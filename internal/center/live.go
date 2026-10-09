package center

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"argus/internal/kube"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const settingsKey = "argus-settings"

type MigrationSettings struct {
	VLAN   int    `json:"vlan"`
	Subnet string `json:"subnet"`
}

func (c *Center) MigrationSettings(ctx context.Context) (MigrationSettings, error) {
	var m MigrationSettings
	data, _, err := c.store.Get(ctx, settingsKey)
	if apierrors.IsNotFound(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data["migration"], &m)
}

func (c *Center) SetMigrationSettings(ctx context.Context, m MigrationSettings) error {
	_, n, err := net.ParseCIDR(m.Subnet)
	if err != nil || n.IP.To4() == nil {
		return errors.New("subnet must be an IPv4 CIDR")
	}
	if ones, _ := n.Mask.Size(); ones > 24 {
		return errors.New("subnet must be /24 or larger")
	}
	if m.VLAN < 0 || m.VLAN > 4094 {
		return errors.New("VLAN must be 0-4094")
	}
	m.Subnet = n.String()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	data := map[string][]byte{"migration": b}
	_, rv, err := c.store.Get(ctx, settingsKey)
	switch {
	case apierrors.IsNotFound(err):
		err = c.store.Create(ctx, settingsKey, "Opaque", data)
	case err == nil:
		err = c.store.Update(ctx, settingsKey, rv, data)
	}
	if err != nil {
		return err
	}
	return c.PushMigrationNetwork(ctx)
}

func (c *Center) PushMigrationNetwork(ctx context.Context) error {
	m, err := c.MigrationSettings(ctx)
	if err != nil || m.Subnet == "" {
		return err
	}
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for _, n := range ns {
		used[n.Slot] = true
	}
	changed := false
	for i := range ns {
		if ns[i].Slot == 0 {
			slot := 1
			for used[slot] {
				slot++
			}
			ns[i].Slot, used[slot], changed = slot, true, true
		}
	}
	if changed {
		if err := c.save(ctx, ns, rv); err != nil {
			return err
		}
	}
	var errs []string
	for _, n := range ns {
		start, end, err := SlotRange(m.Subnet, n.Slot)
		if err == nil {
			err = n.do(ctx, http.MethodPost, "/api/migration-network", kube.MigrationNetwork{VLAN: m.VLAN, Subnet: m.Subnet, RangeStart: start, RangeEnd: end}, nil)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func SlotRange(subnet string, slot int) (string, string, error) {
	_, n, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", "", err
	}
	ones, bits := n.Mask.Size()
	if slot < 1 || (slot+1)*16 > 1<<(bits-ones) {
		return "", "", fmt.Errorf("no address block %d in %s", slot, subnet)
	}
	base := binary.BigEndian.Uint32(n.IP.To4())
	ip := func(off int) string {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, base+uint32(off))
		return net.IP(b).String()
	}
	return ip(slot * 16), ip(slot*16 + 15), nil
}

func (c *Center) syncCAs(ctx context.Context) error {
	ns, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	cas := make([]string, len(ns))
	for i, n := range ns {
		var res struct{ CA string }
		if err := n.do(ctx, http.MethodGet, "/api/kubevirt-ca", nil, &res); err != nil {
			return err
		}
		cas[i] = strings.TrimSpace(res.CA)
	}
	for i, n := range ns {
		var others []string
		for j, ca := range cas {
			if j != i && ca != "" {
				others = append(others, ca)
			}
		}
		if err := n.do(ctx, http.MethodPost, "/api/kubevirt-external-ca", map[string]string{"bundle": strings.Join(others, "\n") + "\n"}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Center) LiveMigrate(src, dst Node, ns, vm string) {
	j := &Job{VM: vm, From: src.Name, To: dst.Name, Started: time.Now(), Status: "Starting", Live: true}
	c.mu.Lock()
	c.jobs = append(c.jobs, j)
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		if err := c.liveMigrate(ctx, j, src, dst, ns, vm); err != nil {
			c.update(j, "Failed: "+err.Error(), true, true)
			return
		}
		c.update(j, "Completed", true, false)
	}()
}

func (c *Center) liveMigrate(ctx context.Context, j *Job, src, dst Node, ns, vm string) error {
	if err := c.checkNetworks(ctx, src, dst, ns, vm); err != nil {
		return err
	}
	c.update(j, "Exchanging certificates", false, false)
	if err := c.syncCAs(ctx); err != nil {
		return err
	}
	var spec map[string]any
	if err := src.do(ctx, http.MethodGet, fmt.Sprintf("/api/vms/%s/%s/spec", ns, vm), nil, &spec); err != nil {
		return err
	}
	id := make([]byte, 8)
	rand.Read(id)
	migrationID := hex.EncodeToString(id)

	c.update(j, "Preparing target", false, false)
	var recv struct{ SyncAddress string }
	cleanup := func() {
		bg := context.Background()
		src.do(bg, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/migration-cleanup", ns, vm), nil, nil)
		dst.do(bg, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/migration-cleanup", kube.Namespace, vm), nil, nil)
	}
	rollback := func() {
		cleanup()
		dst.do(context.Background(), http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/delete", kube.Namespace, vm), nil, nil)
	}
	if err := dst.do(ctx, http.MethodPost, "/api/receive", map[string]any{"name": vm, "migrationID": migrationID, "spec": spec}, &recv); err != nil {
		rollback()
		return err
	}
	c.update(j, "Migrating", false, false)
	if err := src.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/send", ns, vm), map[string]string{"migrationID": migrationID, "connectURL": recv.SyncAddress}, nil); err != nil {
		rollback()
		return err
	}
	for {
		var st struct{ Phase string }
		if err := src.do(ctx, http.MethodGet, fmt.Sprintf("/api/vms/%s/%s/migration", ns, vm), nil, &st); err != nil {
			rollback()
			return err
		}
		switch st.Phase {
		case "Succeeded":
			c.update(j, "Cleaning up", false, false)
			cleanup()
			return src.do(ctx, http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/delete", ns, vm), nil, nil)
		case "Failed":
			rollback()
			return errors.New("migration failed")
		}
		c.update(j, "Migrating ("+st.Phase+")", false, false)
		select {
		case <-ctx.Done():
			rollback()
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
