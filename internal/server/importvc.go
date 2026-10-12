package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"argus/internal/kube"
	"argus/internal/vsphere"
)

func vcenterTLS(host, fingerprint string) (*tls.Config, error) {
	e := &esxiSource{Host: host}
	if err := e.pinCert(); err != nil {
		return nil, err
	}
	seen := hex.EncodeToString(e.pin[:])
	if strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(fingerprint)) != seen {
		return nil, fmt.Errorf("confirm the vCenter certificate fingerprint before credentials are sent; this host presents %s", seen)
	}
	return &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 || sha256.Sum256(raw[0]) != e.pin {
			return errors.New("vCenter certificate changed since the import started")
		}
		return nil
	}}, nil
}

func dialVCenter(ctx context.Context, f func(string) string) (*vsphere.Client, *tls.Config, error) {
	host := strings.TrimSpace(f("host"))
	if host == "" {
		return nil, nil, errors.New("vCenter host required")
	}
	tc, err := vcenterTLS(host, f("fingerprint"))
	if err != nil {
		return nil, nil, err
	}
	c, err := vsphere.Dial(ctx, host, f("user"), f("password"), tc)
	return c, tc, err
}

func (s *Server) vcenterVMs(ctx context.Context, f func(string) string) ([]vsphere.VM, error) {
	c, _, err := dialVCenter(ctx, f)
	if err != nil {
		return nil, err
	}
	defer c.Logout()
	return c.VMs(ctx)
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (s *Server) importVCenter(ctx context.Context, f func(string) string) (string, error) {
	c, tc, err := dialVCenter(ctx, f)
	if err != nil {
		return "", err
	}
	cfg, err := c.Config(ctx, f("vm"))
	if err != nil {
		c.Logout()
		return "", err
	}
	class, err := s.poolClass(ctx, f("pool"))
	if err != nil {
		c.Logout()
		return "", err
	}
	v := kube.ImportedVM{Name: f("name"), Network: f("network"), Class: class, Cores: max(cfg.Cores, 1), MemoryMiB: max(cfg.MemoryMiB, 128), Firmware: cfg.Firmware,
		CDROM: f("cdrom"), Compat: compatDrivers(f("drivers"), cfg.GuestID)}
	if v.Name == "" {
		v.Name = strings.Trim(strings.ToLower(nonName.ReplaceAllString(cfg.Name, "-")), "-")
	}
	for _, n := range cfg.NICs {
		v.MACs = append(v.MACs, n.MAC)
	}
	var total int64
	for _, d := range cfg.Disks {
		v.Disks = append(v.Disks, kube.ImportedDisk{SizeBytes: d.Capacity, Source: map[string]any{"upload": map[string]any{}}})
		total += d.Capacity
	}
	if !validObject.MatchString(v.Name) || v.Network == "" {
		c.Logout()
		return "", errors.New("valid VM name and network required")
	}
	dvs, err := s.kube.CreateImportedVM(ctx, v)
	if err != nil {
		c.Logout()
		return "", err
	}
	go func() {
		defer c.Logout()
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		err := s.vcenterCopy(ctx, c, tc, f("vm"), cfg.Power == "poweredOn", dvs, total)
		log.Printf("import of %s from vCenter: %v", v.Name, err)
	}()
	return v.Name, nil
}

func (s *Server) vcenterCopy(ctx context.Context, c *vsphere.Client, tc *tls.Config, vm string, running bool, dvs []string, total int64) error {
	snap := ""
	if running {
		var err error
		if snap, err = c.Snapshot(ctx, vm, "argus-import"); err != nil {
			return err
		}
		defer c.RemoveSnapshot(context.Background(), snap)
	}
	lease, err := c.Export(ctx, vm, snap)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			lease.Abort(context.Background())
		}
	}()
	if len(lease.Disks) != len(dvs) {
		return fmt.Errorf("vCenter exported %d disks, expected %d", len(lease.Disks), len(dvs))
	}
	var copied atomic.Int64
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(30 * time.Second):
				lease.Progress(ctx, int(100*copied.Load()/max(total, 1)))
			}
		}
	}()
	for i, d := range lease.Disks {
		resp, err := lease.Open(ctx, d, tc)
		if err != nil {
			return err
		}
		err = s.kube.Upload(ctx, dvs[i], countingReader{resp.Body, &copied})
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("disk %d: %w", i, err)
		}
	}
	ok = true
	return lease.Complete(ctx)
}

func (s *Server) vcenterVMsOptions(w http.ResponseWriter, r *http.Request) {
	fields := map[string]string{}
	for _, k := range []string{"host", "user", "password", "fingerprint"} {
		fields[k] = r.FormValue(k)
	}
	vms, err := s.ops(r).VCenterVMs(r.Context(), fields)
	if err != nil {
		w.Header().Set("HX-Retarget", "#import-vm-error")
		flash(w, r, err)
		return
	}
	s.render(w, r, "vmnew.html", "vcenter-vms", map[string]any{"VMs": vms})
}
