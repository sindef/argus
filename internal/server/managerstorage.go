package server

import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync"

	"argus/internal/kube"
	"argus/internal/manager"
)

type hostPool struct {
	kube.Pool
	Host    string
	Cluster string
	Status  string
}

func (p hostPool) Total() int64 { return p.Used + p.Free }

func (s *Server) fleetStorage(r *http.Request) ([]hostPool, []hostVolume, string) {
	var (
		mu    sync.Mutex
		pools []hostPool
		vols  []hostVolume
	)
	errs := fanOut(r.Context(), s.manager.Inventory(session(r).Org), func(ctx context.Context, n manager.Node) error {
		var st storageInfo
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
			vols = append(vols, hostVolume{Volume: v, Host: n.Name})
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
	pools, _, errs := s.fleetStorage(r)
	var total, used int64
	status := map[string]int{}
	for _, p := range pools {
		total += p.Total()
		used += p.Used
		status[p.Status]++
	}
	s.render(w, r, "storage.html", "layout", map[string]any{"Nav": "storage", "Pools": pools, "Total": total, "Used": used, "Status": status, "Error": errs})
}

func (s *Server) managerVolumes(w http.ResponseWriter, r *http.Request) {
	data := s.managerData(r)
	_, vols, errs := s.fleetStorage(r)
	attachable := map[string][]string{}
	for _, h := range data["Hosts"].([]hostRow) {
		attachable[h.Node.Name] = stoppedVMs(h.NodeState.VMs)
	}
	data["Nav"], data["Volumes"], data["Attachable"], data["Error"] = "volumes", vols, attachable, errs
	s.render(w, r, "volumes.html", "layout", data)
}

func (s *Server) managerVolumeOp(w http.ResponseWriter, r *http.Request) {
	node, op := r.PathValue("node"), r.PathValue("op")
	if node == "" {
		node, op = r.FormValue("host"), "create"
	}
	n, err := s.manager.NodeIn(r.Context(), session(r).Org, node)
	if err == nil {
		err = n.Call(r.Context(), http.MethodPost, "/api/volumes/"+op, formVolume(r), nil)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: volume %s %s on %s", session(r).User, op, r.FormValue("name"), n.Name)
	go s.manager.Refresh(n.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerRemoveDisk(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err == nil {
		err = n.Call(r.Context(), http.MethodPost, "/api/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"/disks/"+r.PathValue("disk")+"/delete", nil, nil)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	go s.manager.Refresh(n.Name)
	w.Header().Set("HX-Refresh", "true")
}
