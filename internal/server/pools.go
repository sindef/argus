package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"

	"argus/internal/hostd"
	"argus/internal/kube"
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
	data := map[string]any{"Nav": "pools"}
	if s.hostd == nil {
		data["Error"] = "host helper not available"
		s.render(w, r, "pools.html", "layout", data)
		return
	}
	hp, err := s.hostd.Pools(r.Context())
	kp, _ := s.kube.Pools(r.Context())
	classes, _ := s.kube.PoolClasses(r.Context())
	var rows []poolRow
	for _, p := range hp.Pools {
		row := poolRow{Pool: p, Class: classes[p.Name]}
		if row.Class == "" && !p.System {
			if cerr := s.kube.EnsurePoolClass(r.Context(), p.Name); cerr == nil {
				row.Class = p.Name
			}
		}
		for _, k := range kp {
			if k.Name == p.Name {
				row.Provisioned, row.Volumes = k.Provisioned, k.Volumes
			}
		}
		rows = append(rows, row)
	}
	data["Pools"], data["Importable"], data["Error"] = rows, hp.Importable, errText(err)
	s.render(w, r, "pools.html", "layout", data)
}

func (s *Server) poolModal(w http.ResponseWriter, r *http.Request) {
	disks, err := s.hostd.Disks(r.Context())
	free := slices.DeleteFunc(disks, func(d hostd.Disk) bool { return !d.Free() })
	s.render(w, r, "pools.html", "pool-modal", map[string]any{"Disks": free, "Pool": r.PathValue("name"), "Error": errText(err)})
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
	disks, err := s.hostd.Disks(r.Context())
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
	var err error
	switch op {
	case "create", "add":
		var spec hostd.PoolSpec
		if spec, err = s.poolSpec(r); err != nil {
			break
		}
		if op == "add" {
			err = s.hostd.AddToPool(r.Context(), name, spec)
		} else if err = s.hostd.CreatePool(r.Context(), spec); err == nil {
			name = spec.Name
			err = s.kube.EnsurePoolClass(r.Context(), name)
		}
	case "import", "force-import":
		if err = s.hostd.ImportPool(r.Context(), name, op == "force-import"); err == nil {
			err = s.kube.EnsurePoolClass(r.Context(), name)
		}
	case "export", "destroy":
		confirm := r.FormValue("confirm")
		if confirm == "" {
			confirm = r.Header.Get("HX-Prompt")
		}
		if err = s.hostd.ReleasePool(r.Context(), name, op, confirm); err == nil {
			err = s.kube.DeletePoolClass(r.Context(), name)
		}
	default:
		err = errors.New("unknown pool operation")
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: pool %s %s", session(r).User, op, name)
	w.Header().Set("HX-Refresh", "true")
}
