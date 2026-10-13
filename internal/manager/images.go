package manager

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/wire"
)

func (c *Manager) CopyImage(ctx context.Context, org, image string, src Node, targets []Node) error {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return c.startTask(ctx, &Job{Org: org, Kind: "Copy image " + image, From: src.Name, To: strings.Join(names, ", ")}, "image:"+image, 12*time.Hour, func(ctx context.Context, j *Job) error {
		var ex kube.Export
		if err := src.do(ctx, http.MethodPost, APIPath("/api/images/%s/export", image), wire.ExportRequest{BaseURL: src.URL}, &ex); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := lifecycle.CleanupContext()
			defer cancel()
			lifecycle.LogCleanup("export of image "+image, src.do(cctx, http.MethodPost, APIPath("/api/images/%s/unexport", image), nil, nil))
		}()
		size, _ := ex.Spec["size"].(string)
		for _, t := range targets {
			if err := c.importTo(ctx, j, t, image, map[string]any{"name": image, "size": size, "export": ex}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Manager) SendImage(ctx context.Context, org, image, size string, targets []Node, source func(context.Context, Node) (kube.RegistrySource, error)) error {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return c.startTask(ctx, &Job{Org: org, Kind: "Send image " + image, From: "Image library", To: strings.Join(names, ", ")}, "image:"+image, 12*time.Hour, func(ctx context.Context, j *Job) error {
		for _, t := range targets {
			src, err := source(ctx, t)
			if err != nil {
				return err
			}
			if err := c.importTo(ctx, j, t, image, map[string]any{"name": image, "size": size, "registry": src}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Manager) ImportImage(ctx context.Context, org, from string, req wire.ImageImportRequest, targets []Node) error {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return c.startTask(ctx, &Job{Org: org, Kind: "Import image " + req.Name, From: from, To: strings.Join(names, ", ")}, "image:"+req.Name, 12*time.Hour, func(ctx context.Context, j *Job) error {
		for _, t := range targets {
			if err := c.importTo(ctx, j, t, req.Name, req); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Manager) importTo(ctx context.Context, j *Job, t Node, image string, req any) error {
	return c.Pull(ctx, func(s string) { c.update(j, s, false, false) }, t, image, req, false)
}

func (c *Manager) Pull(ctx context.Context, progress func(string), t Node, image string, req any, reuse bool) error {
	post := true
	if reuse {
		var imgs []kube.Image
		if err := t.do(ctx, http.MethodGet, "/api/images", nil, &imgs); err != nil {
			return err
		}
		post = !slices.ContainsFunc(imgs, func(x kube.Image) bool { return x.Name == image })
	}
	progress("Copying " + image + " to " + t.Name)
	if post {
		if err := t.do(ctx, http.MethodPost, "/api/images/import", req, nil); err != nil {
			return err
		}
	}
	err := waitFor(ctx, 6*time.Hour, func() (bool, error) {
		var imgs []kube.Image
		if reachable := t.do(ctx, http.MethodGet, "/api/images", nil, &imgs) == nil; !reachable {
			return false, nil
		}
		i := slices.IndexFunc(imgs, func(x kube.Image) bool { return x.Name == image })
		if i < 0 {
			return false, fmt.Errorf("%s disappeared on %s", image, t.Name)
		}
		progress(strings.TrimSpace("Copying " + image + " to " + t.Name + " " + strings.TrimSuffix(imgs[i].Progress, "N/A")))
		return imgs[i].Phase == "Succeeded", nil
	})
	if post {
		lifecycle.LogCleanup("import of image "+image+" on "+t.Name, t.do(ctx, http.MethodPost, APIPath("/api/images/%s/imported", image), nil, nil))
	}
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	return nil
}

func (c *Manager) Task(ctx context.Context, j Job, lock string, run func(ctx context.Context, progress func(string)) error) error {
	return c.startTask(ctx, &j, lock, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return run(ctx, func(status string) { c.update(j, status, false, false) })
	})
}
