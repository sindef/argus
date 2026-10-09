package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
)

type nodeVMs struct {
	Node  manager.Node
	Info  manager.NodeInfo
	VMs   []kube.VM
	Error string
}

func (s *Server) gather(ctx context.Context) ([]nodeVMs, error) {
	nodes, err := s.manager.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := make([]nodeVMs, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n manager.Node) {
			defer wg.Done()
			out[i].Node = n
			info, err := n.Info(ctx)
			if err == nil {
				out[i].Info = info
				out[i].VMs, err = n.VMs(ctx)
			}
			if err != nil {
				out[i].Error = err.Error()
			}
		}(i, n)
	}
	wg.Wait()
	return out, nil
}

func (s *Server) managerData(r *http.Request) map[string]any {
	nodes, err := s.gather(r.Context())
	ps, perr := s.manager.Protected(r.Context())
	if err == nil {
		err = perr
	}
	protected := map[string]manager.Protected{}
	for _, p := range ps {
		protected[p.VM] = p
	}
	return map[string]any{
		"Nodes":      nodes,
		"Protected":  protected,
		"Jobs":       s.manager.Jobs(),
		"CanOperate": session(r).Role.Can(auth.Operator),
		"IsAdmin":    session(r).Role.Can(auth.Admin),
		"Error":      errText(err),
	}
}

func (s *Server) managerVMs(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "manager.html", "layout", s.managerData(r))
}

func (s *Server) managerTable(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "manager.html", "table", s.managerData(r))
}

func (s *Server) managerAction(w http.ResponseWriter, r *http.Request) {
	act := r.PathValue("action")
	if !kube.ValidAction(act) {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	n, err := s.manager.Node(r.Context(), r.PathValue("node"))
	if err == nil {
		err = n.Action(r.Context(), r.PathValue("ns"), r.PathValue("name"), act)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s on %s", session(r).User, act, r.PathValue("ns"), r.PathValue("name"), n.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerMigrate(w http.ResponseWriter, r *http.Request) {
	src, err := s.manager.Node(r.Context(), r.PathValue("node"))
	var dst manager.Node
	if err == nil {
		dst, err = s.manager.Node(r.Context(), r.FormValue("to"))
	}
	if err == nil && src.Name == dst.Name {
		err = errors.New("pick a different node")
	}
	running := false
	if err == nil {
		var vms []kube.VM
		vms, err = src.VMs(r.Context())
		for _, v := range vms {
			if v.Namespace == r.PathValue("ns") && v.Name == r.PathValue("name") {
				running = v.Running
			}
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	if running {
		s.manager.LiveMigrate(src, dst, r.PathValue("ns"), r.PathValue("name"))
	} else {
		s.manager.Migrate(src, dst, r.PathValue("ns"), r.PathValue("name"))
	}
	log.Printf("%s: migrate %s/%s from %s to %s", session(r).User, r.PathValue("ns"), r.PathValue("name"), src.Name, dst.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerNodes(w http.ResponseWriter, r *http.Request) {
	s.renderNodes(w, r, nil)
}

func (s *Server) renderNodes(w http.ResponseWriter, r *http.Request, err error) {
	nodes, gerr := s.gather(r.Context())
	if err == nil {
		err = gerr
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	mig, merr := s.manager.MigrationSettings(r.Context())
	if err == nil {
		err = merr
	}
	ranges := map[string]string{}
	for _, n := range nodes {
		if start, end, rerr := manager.SlotRange(mig.Subnet, n.Node.Slot); mig.Subnet != "" && rerr == nil {
			ranges[n.Node.Name] = start + " – " + end
		}
	}
	s.render(w, r, "nodes.html", "layout", map[string]any{"Nodes": nodes, "Migration": mig, "Ranges": ranges, "IsAdmin": session(r).Role.Can(auth.Admin), "Error": errText(err)})
}

func (s *Server) managerAddNode(w http.ResponseWriter, r *http.Request) {
	name, addr := r.FormValue("name"), r.FormValue("url")
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		err = errors.New("URL must be https://host")
	} else if !validObject.MatchString(name) {
		err = errors.New("invalid node name")
	}
	if err == nil {
		err = s.manager.AddNode(r.Context(), name, u.Scheme+"://"+u.Host, r.FormValue("token"), r.FormValue("fingerprint"))
	}
	if err != nil {
		s.renderNodes(w, r, err)
		return
	}
	log.Printf("%s: added node %s at %s", session(r).User, name, addr)
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) managerRemoveNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")
	if err := s.manager.RemoveNode(r.Context(), name); err != nil {
		s.renderNodes(w, r, err)
		return
	}
	log.Printf("%s: removed node %s", session(r).User, name)
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) managerMigrationSettings(w http.ResponseWriter, r *http.Request) {
	vlan := 0
	if v := r.FormValue("vlan"); v != "" {
		if err := intField(r, "vlan", 0, 4094, &vlan); err != nil {
			s.renderNodes(w, r, err)
			return
		}
	}
	if err := s.manager.SetMigrationSettings(r.Context(), manager.MigrationSettings{VLAN: vlan, Subnet: r.FormValue("subnet")}); err != nil {
		s.renderNodes(w, r, err)
		return
	}
	log.Printf("%s: migration network vlan %d %s", session(r).User, vlan, r.FormValue("subnet"))
	http.Redirect(w, r, "/nodes", http.StatusSeeOther)
}

func (s *Server) managerProtect(w http.ResponseWriter, r *http.Request) {
	var interval int
	err := intField(r, "interval", 5, 1440, &interval)
	if err == nil {
		err = s.manager.Protect(r.Context(), r.PathValue("node"), r.PathValue("name"), r.FormValue("replica"), interval)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: protect %s on %s, replica on %s", session(r).User, r.PathValue("name"), r.PathValue("node"), r.FormValue("replica"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerUnprotect(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Unprotect(r.Context(), r.PathValue("name")); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: unprotect %s", session(r).User, r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}
