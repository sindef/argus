package managerserver

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync"

	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/wire"
)

type hostPool struct {
	kube.Pool
	Host    string
	Cluster string
	Status  string
}

func (p hostPool) Total() int64 { return p.Used + p.Free }

func (s *Server) fleetStorage(r *http.Request) ([]hostPool, []hostui.HostVolume, string) {
	var (
		mu    sync.Mutex
		pools []hostPool
		vols  []hostui.HostVolume
	)
	errs := fanOut(r.Context(), s.inventory(r), func(ctx context.Context, n manager.Node) error {
		var st wire.StorageInfo
		if err := n.Call(ctx, http.MethodGet, "/api/storage", nil, &st); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range st.Pools {
			hp := hostPool{Pool: p, Host: n.Name, Cluster: groupName(n.Cluster), Status: "Healthy"}
			if hp.Total() > 0 && hp.Used*100/hp.Total() >= 80 {
				hp.Status = "Warning"
			}
			pools = append(pools, hp)
		}
		for _, v := range st.Volumes {
			vols = append(vols, hostui.HostVolume{Volume: v, Host: n.Name})
		}
		return nil
	})
	sort.Slice(pools, func(i, j int) bool { return pools[i].Host < pools[j].Host })
	sort.Slice(vols, func(i, j int) bool {
		if vols[i].Host != vols[j].Host {
			return vols[i].Host < vols[j].Host
		}
		return vols[i].Name < vols[j].Name
	})
	return pools, vols, errs
}

func (s *Server) managerStorage(w http.ResponseWriter, r *http.Request) {
	data := s.managerData(r)
	pools, vols, errs := s.fleetStorage(r)
	var total, used int64
	status := map[string]int{}
	for _, p := range pools {
		total += p.Total()
		used += p.Used
		status[p.Status]++
	}
	attachable := map[string][]string{}
	for _, h := range data["Hosts"].([]hostRow) {
		attachable[h.Node.Name] = hostui.StoppedVMs(h.NodeState.VMs)
	}
	data["Nav"], data["Pools"], data["Total"], data["Used"], data["Status"], data["Error"] = "storage", pools, total, used, status, errs
	data["Volumes"], data["Attachable"], data["PoolNames"], data["PoolFilter"] = vols, attachable, hostui.VolumePools(vols), r.FormValue("pool")
	s.Render(w, r, "storage.html", "layout", data)
}

func (s *Server) managerVolumes(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/storage#s-volumes", http.StatusSeeOther)
}

func (s *Server) managerVolumeOp(w http.ResponseWriter, r *http.Request) {
	node, op := r.PathValue("node"), r.PathValue("op")
	if node == "" {
		node, op = r.FormValue("host"), "create"
	}
	n, err := s.nodeIn(r, node)
	if err == nil {
		err = n.Call(r.Context(), http.MethodPost, "/api/volumes/"+op, hostui.FormVolume(r), nil)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: volume %s %s on %s", ui.Session(r).User, op, r.FormValue("name"), n.Name)
	go s.manager.Refresh(n.Name)
	w.Header().Set("HX-Refresh", "true")
}
