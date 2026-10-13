package manager

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"argus/internal/kube"
)

func (c *Manager) CopyImage(org, image string, src Node, targets []Node) error {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return c.startTask(&Job{Org: org, Kind: "Copy image " + image, From: src.Name, To: strings.Join(names, ", ")}, "image:"+image, 12*time.Hour, func(ctx context.Context, j *Job) error {
		var ex kube.Export
		if err := src.do(ctx, http.MethodPost, APIPath("/api/images/%s/export", image), map[string]string{"baseURL": src.URL}, &ex); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := cleanupCtx()
			defer cancel()
			logCleanup("export of image "+image, src.do(cctx, http.MethodPost, APIPath("/api/images/%s/unexport", image), nil, nil))
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

func (c *Manager) SendImage(org, image, size string, src kube.RegistrySource, targets []Node, creds func() (string, string)) error {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = t.Name
	}
	return c.startTask(&Job{Org: org, Kind: "Send image " + image, From: "Image library", To: strings.Join(names, ", ")}, "image:"+image, 12*time.Hour, func(ctx context.Context, j *Job) error {
		for _, t := range targets {
			src.User, src.Pass = creds()
			if err := c.importTo(ctx, j, t, image, map[string]any{"name": image, "size": size, "registry": src}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *Manager) importTo(ctx context.Context, j *Job, t Node, image string, req map[string]any) error {
	c.update(j, "Copying to "+t.Name, false, false)
	if err := t.do(ctx, http.MethodPost, "/api/images/import", req, nil); err != nil {
		return err
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
		c.update(j, "Copying to "+t.Name+" "+imgs[i].Progress, false, false)
		return imgs[i].Phase == "Succeeded", nil
	})
	logCleanup("import of image "+image+" on "+t.Name, t.do(ctx, http.MethodPost, APIPath("/api/images/%s/imported", image), nil, nil))
	if err != nil {
		return fmt.Errorf("%s: %w", t.Name, err)
	}
	return nil
}

func (c *Manager) Task(org, kind, lock string, run func(ctx context.Context, progress func(string)) error) error {
	return c.startTask(&Job{Org: org, Kind: kind}, lock, 12*time.Hour, func(ctx context.Context, j *Job) error {
		return run(ctx, func(status string) { c.update(j, status, false, false) })
	})
}
