package manager

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/wire"
)

const settingsKey = "argus-settings"

type MigrationSettings struct {
	VLAN     int    `json:"vlan"`
	Subnet   string `json:"subnet"`
	Reserved []int  `json:"reserved,omitempty"`
}

func (c *Manager) MigrationSettings(ctx context.Context) (MigrationSettings, error) {
	var m MigrationSettings
	_, err := auth.LoadJSON(ctx, c.store, settingsKey, "migration", &m)
	return m, err
}

func (c *Manager) SetMigrationSettings(ctx context.Context, m MigrationSettings) error {
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
	if old, err := c.MigrationSettings(ctx); err == nil && old.Subnet == m.Subnet {
		m.Reserved = old.Reserved
	}
	return c.saveMigrationSettings(ctx, m, true)
}

func (c *Manager) saveMigrationSettings(ctx context.Context, m MigrationSettings, push bool) error {
	err := auth.PutJSON(ctx, c.store, settingsKey, "migration", m)
	if err != nil || !push {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := c.PushMigrationNetwork(ctx); err != nil {
			log.Printf("migration network: %v", err)
		}
	}()
	return nil
}

func (c *Manager) PushMigrationNetwork(ctx context.Context) error {
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
	for _, r := range m.Reserved {
		used[r] = true
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
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		errs    []string
		applied = map[string]string{}
	)
	for _, n := range ns {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			start, end, err := SlotRange(m.Subnet, n.Slot)
			var res struct{ Address string }
			if err == nil {
				err = n.do(ctx, http.MethodPost, "/api/migration-network", kube.MigrationNetwork{VLAN: m.VLAN, Subnet: m.Subnet, RangeStart: start, RangeEnd: end}, &res)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err.Error())
				return
			}
			applied[n.Name] = res.Address
		}(n)
	}
	wg.Wait()
	if err := c.recordMigrationAddresses(ctx, applied); err != nil {
		errs = append(errs, err.Error())
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
	size := 1 << (bits - ones)
	base := binary.BigEndian.Uint32(n.IP.To4())
	ip := func(off int) string {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, base+uint32(off))
		return net.IP(b).String()
	}
	end := slot*16 + 15
	if end == size-1 {
		end--
	}
	return ip(slot * 16), ip(end), nil
}

func (c *Manager) recordMigrationAddresses(ctx context.Context, applied map[string]string) error {
	ns, rv, err := c.load(ctx)
	if err != nil {
		return err
	}
	for i := range ns {
		ns[i].MigrationAddress = applied[ns[i].Name]
	}
	return c.save(ctx, ns, rv)
}

func (c *Manager) releaseSlot(ctx context.Context, n Node) {
	if n.Slot == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := n.do(cctx, http.MethodPost, "/api/migration-network/remove", nil, nil); err == nil {
		return
	}
	m, err := c.MigrationSettings(ctx)
	if err != nil || m.Subnet == "" {
		return
	}
	m.Reserved = append(m.Reserved, n.Slot)
	if err := c.saveMigrationSettings(ctx, m, false); err != nil {
		log.Printf("migration: reserve slot %d of %s: %v", n.Slot, n.Name, err)
	}
}

func (c *Manager) syncCAs(ctx context.Context, src, dst Node) error {
	ns, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	cas := map[string]string{}
	for _, n := range ns {
		var res struct{ CA string }
		err := n.do(ctx, http.MethodGet, "/api/kubevirt-ca", nil, &res)
		if err != nil && (n.Name == src.Name || n.Name == dst.Name) {
			return err
		}
		if err == nil && strings.TrimSpace(res.CA) != "" {
			cas[n.Name] = strings.TrimSpace(res.CA)
		}
	}
	for _, n := range []Node{src, dst} {
		var others []string
		for name, ca := range cas {
			if name != n.Name {
				others = append(others, ca)
			}
		}
		sort.Strings(others)
		if err := n.do(ctx, http.MethodPost, "/api/kubevirt-external-ca", wire.ExternalCARequest{Bundle: strings.Join(others, "\n") + "\n"}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Manager) LiveMigrate(ctx context.Context, src, dst Node, ns, vm string) error {
	if err := c.unprotected(ctx, src, vm); err != nil {
		return err
	}
	return c.startJob(ctx, &Job{Org: src.Org, Namespace: ns, VM: vm, From: src.Name, To: dst.Name, Live: true}, 6*time.Hour, func(ctx context.Context, j *Job) error {
		return c.liveMigrate(ctx, j, src, dst, ns, vm)
	})
}

func vmState(ctx context.Context, n Node, ns, vm string) (kube.VM, error) {
	var v kube.VM
	return v, n.do(ctx, http.MethodGet, vmPathIn(ns, vm, ""), nil, &v)
}

func (c *Manager) liveMigrate(ctx context.Context, j *Job, src, dst Node, ns, vm string) error {
	if err := c.preflight(ctx, src, dst, ns, vm); err != nil {
		return err
	}
	meta, err := c.carry(ctx, src, dst, ns, vm)
	if err != nil {
		return err
	}
	c.progress(j, "Exchanging certificates")
	if err := c.syncCAs(ctx, src, dst); err != nil {
		return err
	}
	var spec map[string]any
	if err := src.do(ctx, http.MethodGet, vmPathIn(ns, vm, "spec"), nil, &spec); err != nil {
		return err
	}
	op := wire.OperationRequest{Operation: j.ID}
	cleanup := func(cctx context.Context) {
		lifecycle.LogCleanup("migration of "+vm+" on "+src.Name, src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "migration-cleanup"), op, nil))
		lifecycle.LogCleanup("migration of "+vm+" on "+dst.Name, dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, vm, "migration-cleanup"), op, nil))
	}
	abort := func(cause error) error {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		cleanup(cctx)
		if err := dst.do(cctx, http.MethodPost, vmPathIn(kube.Namespace, vm, "abort"), op, nil); err != nil {
			return fmt.Errorf("%w; target cleanup failed: %w", cause, err)
		}
		return cause
	}
	sourceRunning := func(cctx context.Context) bool {
		v, err := vmState(cctx, src, ns, vm)
		return err == nil && v.Running
	}

	c.progress(j, "Preparing target")
	var recv wire.ReceiveResponse
	if err := dst.do(ctx, http.MethodPost, "/api/receive", wire.ReceiveRequest{Name: vm, MigrationID: j.ID, Spec: spec, Metadata: meta}, &recv); err != nil {
		return abort(err)
	}
	c.progress(j, "Migrating")
	sendErr := src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "send"), wire.SendRequest{MigrationID: j.ID, ConnectURL: recv.SyncAddress}, nil)
	phasePath := vmPathIn(ns, vm, "migration") + "?operation=" + j.ID
	lastPhase, since := "", time.Now()
	for {
		var st struct{ Phase string }
		err := src.do(ctx, http.MethodGet, phasePath, nil, &st)
		switch {
		case err != nil:
			c.progress(j, "Migrating (source status unavailable, retrying)")
		case st.Phase == "Succeeded":
			return c.finishLive(ctx, j, src, dst, ns, vm, cleanup)
		case st.Phase == "Failed" || (st.Phase == "" && sendErr != nil):
			cause := errors.New("migration failed")
			if sendErr != nil {
				cause = sendErr
			}
			if !sourceRunning(ctx) {
				return fmt.Errorf("%w: %w", cause, errUnknown)
			}
			return abort(cause)
		default:
			var tst struct{ Phase string }
			if dst.do(ctx, http.MethodGet, vmPathIn(kube.Namespace, vm, "migration")+"?operation="+j.ID, nil, &tst) == nil && tst.Phase == "Failed" {
				cctx, cancel := lifecycle.CleanupContext()
				err := src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "migration-cleanup"), op, nil)
				running := err == nil && sourceRunning(cctx)
				cancel()
				if !running {
					return fmt.Errorf("target failed to receive: %w", errUnknown)
				}
				return abort(errors.New("target failed to receive"))
			}
			if st.Phase != lastPhase {
				lastPhase, since = st.Phase, time.Now()
			} else if (st.Phase == "Pending" || st.Phase == "Scheduling") && time.Since(since) > 90*time.Second {
				c.progress(j, "Migrating (stalled in "+st.Phase+", checking sync controllers)")
				lifecycle.LogCleanup("sync controller on "+src.Name, src.do(ctx, http.MethodPost, "/api/sync-controller", nil, nil))
				lifecycle.LogCleanup("sync controller on "+dst.Name, dst.do(ctx, http.MethodPost, "/api/sync-controller", nil, nil))
				since = time.Now()
			}
			c.progress(j, "Migrating ("+st.Phase+")")
		}
		select {
		case <-ctx.Done():
			return c.cancelLive(j, src, dst, ns, vm, op, abort, sourceRunning)
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Manager) finishLive(ctx context.Context, j *Job, src, dst Node, ns, vm string, cleanup func(context.Context)) error {
	c.progress(j, "Confirming target")
	for i := 0; i < 60; i++ {
		if v, err := vmState(ctx, dst, kube.Namespace, vm); err == nil && v.Running {
			c.progress(j, "Cleaning up")
			cleanup(ctx)
			return src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "delete"), nil, nil)
		}
		select {
		case <-ctx.Done():
			return errUnknown
		case <-time.After(5 * time.Second):
		}
	}
	return errUnknown
}

func (c *Manager) cancelLive(j *Job, src, dst Node, ns, vm string, op wire.OperationRequest, abort func(error) error, sourceRunning func(context.Context) bool) error {
	c.progress(j, "Cancelling")
	cctx, cancel := lifecycle.CleanupContext()
	defer cancel()
	if err := src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "migration-cleanup"), op, nil); err != nil {
		return fmt.Errorf("cancel failed: %w: %w", err, errUnknown)
	}
	time.Sleep(10 * time.Second)
	target, err := vmState(cctx, dst, kube.Namespace, vm)
	if err != nil || target.Running || !sourceRunning(cctx) {
		return fmt.Errorf("timed out: %w", errUnknown)
	}
	return abort(errors.New("timed out; cancelled"))
}
