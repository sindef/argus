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
		if err := src.do(ctx, http.MethodPost, "/api/images/"+image+"/export", map[string]string{"baseURL": src.URL}, &ex); err != nil {
			return err
		}
		defer func() {
			cctx, cancel := cleanupCtx()
			defer cancel()
			src.do(cctx, http.MethodPost, "/api/images/"+image+"/unexport", nil, nil)
		}()
		size, _ := ex.Spec["size"].(string)
		for _, t := range targets {
			c.update(j, "Copying to "+t.Name, false, false)
			if err := t.do(ctx, http.MethodPost, "/api/images/import", map[string]any{"name": image, "size": size, "export": ex}, nil); err != nil {
				return err
			}
			err := waitFor(ctx, 6*time.Hour, func() (bool, error) {
				var imgs []kube.Image
				if err := t.do(ctx, http.MethodGet, "/api/images", nil, &imgs); err != nil {
					return false, nil
				}
				i := slices.IndexFunc(imgs, func(x kube.Image) bool { return x.Name == image })
				if i < 0 {
					return false, fmt.Errorf("%s disappeared on %s", image, t.Name)
				}
				c.update(j, "Copying to "+t.Name+" "+imgs[i].Progress, false, false)
				return imgs[i].Phase == "Succeeded", nil
			})
			t.do(ctx, http.MethodPost, "/api/images/"+image+"/imported", nil, nil)
			if err != nil {
				return fmt.Errorf("%s: %w", t.Name, err)
			}
		}
		return nil
	})
}
