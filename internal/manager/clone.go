package manager

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"argus/internal/kube"
	"argus/internal/wire"
)

func RenameExport(ex *kube.Export, from, to string) {
	names := kube.RenameSpec(ex.Spec, from, to)
	for i, d := range ex.Disks {
		if n, ok := names[d.DataVolume]; ok {
			ex.Disks[i].DataVolume = n
		}
	}
}

func (c *Manager) CopyVM(src, dst Node, ns, vm, name, pool string, template bool) error {
	kind := "Clone " + vm + " as " + name
	if template {
		kind = "Create template " + name + " from " + vm
	}
	return c.startJob(&Job{Org: src.Org, Kind: kind, Namespace: ns, VM: vm, From: src.Name, To: dst.Name}, 12*time.Hour, func(ctx context.Context, j *Job) error {
		if src.Name == dst.Name && pool == "" {
			c.update(j, "Cloning", false, false)
			if err := src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "meta/clone"), wire.MetaRequest{Value: name, Template: template}, nil); err != nil {
				return err
			}
			err := waitFor(ctx, 10*time.Minute, func() (bool, error) {
				return src.do(ctx, http.MethodGet, vmPathIn(kube.Namespace, name, ""), nil, nil) == nil, nil
			})
			if err != nil {
				return fmt.Errorf("the clone of %s did not start; check the host's events", vm)
			}
			return c.waitDisks(ctx, j, dst, name)
		}
		if pool == "" {
			if err := c.preflight(ctx, src, dst, ns, vm); err != nil {
				return err
			}
		}
		c.update(j, "Exporting", false, false)
		base := src.URL
		if src.Name == dst.Name {
			base = "self"
		}
		var ex kube.Export
		if err := src.do(ctx, http.MethodPost, vmPathIn(ns, vm, "snapshot-export"), wire.ExportRequest{BaseURL: base}, &ex); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := cleanupCtx()
			defer cancel()
			logCleanup("export of "+vm, src.do(cctx, http.MethodPost, vmPathIn(ns, vm, "unexport"), nil, nil))
		}()
		RenameExport(&ex, vm, name)
		return c.importAndWait(ctx, j, dst, name, pool, template, ex)
	})
}

func (c *Manager) Deploy(org string, dst Node, name, pool, from string, ex kube.Export) error {
	return c.startTask(&Job{Org: org, Kind: "Deploy " + name + " from template " + from, To: dst.Name}, "deploy:"+dst.Name+"/"+name, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return c.importAndWait(ctx, j, dst, name, pool, false, ex)
	})
}

func (n Node) Stream(ctx context.Context, url string, h http.Header) (*http.Response, error) {
	if !strings.HasPrefix(url, n.URL+"/") {
		return nil, fmt.Errorf("%s is not on %s", url, n.Name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := n.streaming().Do(req)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", n.Name, resp.Status)
	}
	return resp, err
}
