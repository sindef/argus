package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"

	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/ui"
)

type poolRow struct {
	hostd.Pool
	Class       string
	Provisioned int64
	Volumes     int
}

func (s *Server) poolClass(ctx context.Context, pool string) (string, error) {
	if pool == "" {
		return "", nil
	}
	classes, err := s.kube.PoolClasses(ctx)
	if err != nil {
		return "", err
	}
	c, ok := classes[pool]
	if !ok {
		return "", fmt.Errorf("pool %s is not available for volumes", pool)
	}
	return c, nil
}

func (s *Server) poolChoices(ctx context.Context) []kube.Pool {
	pools, _ := s.kube.Pools(ctx)
	return slices.DeleteFunc(pools, func(p kube.Pool) bool { return p.StorageClass == "" })
}

func (s *Server) poolsPage(w http.ResponseWriter, r *http.Request) {
	pv, err := s.ops(r).Pools(r.Context())
	s.Render(w, r, "pools.html", "layout", map[string]any{"Nav": "pools", "Pools": pv.Pools, "Importable": pv.Importable, "Error": ui.ErrText(err)})
}

func (s *Server) poolModal(w http.ResponseWriter, r *http.Request) {
	disks, err := s.ops(r).Disks(r.Context())
	free := slices.DeleteFunc(disks, func(d hostd.Disk) bool { return !d.Free() })
	s.Render(w, r, "pools.html", "pool-modal", map[string]any{"Disks": free, "Pool": r.PathValue("name"), "Error": ui.ErrText(err)})
}

func vdevs(kind string, width int, disks []string) ([]hostd.Vdev, error) {
	if len(disks) == 0 {
		return nil, nil
	}
	if kind == "stripe" || width <= 0 {
		return []hostd.Vdev{{Type: kind, Disks: disks}}, nil
	}
	if len(disks)%width != 0 {
		return nil, fmt.Errorf("%d disks cannot be split into %s groups of %d", len(disks), kind, width)
	}
	var out []hostd.Vdev
	for i := 0; i < len(disks); i += width {
		out = append(out, hostd.Vdev{Type: kind, Disks: disks[i : i+width]})
	}
	return out, nil
}

func (s *Server) poolSpec(r *http.Request) (hostd.PoolSpec, error) {
	r.ParseForm()
	spec := hostd.PoolSpec{Name: r.FormValue("name"), Cache: r.Form["cache_disks"], Spares: r.Form["spare_disks"]}
	width, _ := strconv.Atoi(r.FormValue("data_width"))
	var err error
	if spec.Data, err = vdevs(r.FormValue("data_type"), width, r.Form["data_disks"]); err != nil {
		return spec, err
	}
	if spec.Log, err = vdevs(r.FormValue("log_type"), 0, r.Form["log_disks"]); err != nil {
		return spec, err
	}
	if spec.Special, err = vdevs(r.FormValue("special_type"), 0, r.Form["special_disks"]); err != nil {
		return spec, err
	}
	disks, err := s.ops(r).Disks(r.Context())
	if err != nil {
		return spec, err
	}
	selected := slices.Concat(r.Form["data_disks"], r.Form["log_disks"], r.Form["special_disks"], spec.Cache, spec.Spares)
	for _, d := range disks {
		if !slices.Contains(selected, d.Name) || d.Blank() {
			continue
		}
		if r.FormValue("wipe-"+d.Name) != d.Name {
			return spec, fmt.Errorf("%s has data; type %s to confirm erasing it", d.Name, d.Name)
		}
		spec.Wipe = append(spec.Wipe, d.Name)
	}
	return spec, nil
}

func (s *Server) poolAction(w http.ResponseWriter, r *http.Request) {
	name, op := r.PathValue("name"), r.PathValue("op")
	req := poolOpReq{Confirm: r.FormValue("confirm")}
	if req.Confirm == "" {
		req.Confirm = r.Header.Get("HX-Prompt")
	}
	var err error
	if op == "create" || op == "add" {
		req.Spec, err = s.poolSpec(r)
		if op == "create" {
			name = req.Spec.Name
		}
	}
	if err == nil {
		err = s.ops(r).PoolOp(r.Context(), name, op, req)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: pool %s %s", ui.Session(r).User, op, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("op", "create")
	s.poolAction(w, r)
}
