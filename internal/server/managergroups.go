package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"argus/internal/manager"
)

var groupTitles = map[string]string{
	"add-datacenter":    "New datacenter",
	"add-cluster":       "New cluster",
	"rename-datacenter": "Rename datacenter",
	"rename-cluster":    "Rename cluster",
	"move-host":         "Move host",
}

func (s *Server) groupNames(r *http.Request) ([]string, []string) {
	gs, _ := s.manager.Groups(r.Context(), session(r).Org)
	var dcs, cls []string
	for _, g := range gs {
		dcs = append(dcs, g.Datacenter)
		cls = append(cls, g.Clusters...)
	}
	return dcs, cls
}

func (s *Server) groupModal(w http.ResponseWriter, r *http.Request) {
	op := r.PathValue("op")
	title, ok := groupTitles[op]
	if !ok {
		http.NotFound(w, r)
		return
	}
	dcs, cls := s.groupNames(r)
	data := map[string]any{"Op": op, "Title": title, "DC": r.FormValue("dc"), "Cluster": r.FormValue("cluster"), "Host": r.FormValue("host"), "DCs": dcs, "Clusters": cls}
	switch op {
	case "rename-datacenter":
		data["Value"] = data["DC"]
	case "rename-cluster":
		data["Value"] = data["Cluster"]
	case "move-host":
		title += " " + r.FormValue("host")
		data["Title"] = title
	}
	s.render(w, r, "manager.html", "group-modal", data)
}

func (s *Server) groupAction(w http.ResponseWriter, r *http.Request) {
	org, op := session(r).Org, r.PathValue("op")
	dc, cl, name := r.FormValue("dc"), r.FormValue("cluster"), strings.TrimSpace(r.FormValue("name"))
	var err error
	switch op {
	case "add-datacenter":
		err = s.manager.AddDatacenter(r.Context(), org, name)
	case "add-cluster":
		err = s.manager.AddCluster(r.Context(), org, dc, name)
	case "rename-datacenter":
		err = s.manager.RenameDatacenter(r.Context(), org, dc, name)
	case "rename-cluster":
		err = s.manager.RenameCluster(r.Context(), org, dc, cl, name)
	case "delete-datacenter":
		err = s.manager.DeleteGroup(r.Context(), org, dc, "")
	case "delete-cluster":
		err = s.manager.DeleteGroup(r.Context(), org, dc, cl)
	case "move-host":
		err = s.manager.MoveNode(r.Context(), org, r.FormValue("host"), strings.TrimSpace(r.FormValue("datacenter")), strings.TrimSpace(r.FormValue("cluster")))
	case "apply-networks":
		err = s.applyClusterNetworks(r, dc, cl)
	default:
		err = errors.New("unknown operation")
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s %s", session(r).User, op, dc, cl, name)
	s.manager.PollAll(r.Context())
	w.Header().Set("HX-Refresh", "true")
}

func inGroup(n manager.Node, dc, cl string) bool {
	return groupName(n.Datacenter) == dc && groupName(n.Cluster) == cl
}

func (s *Server) applyClusterNetworks(r *http.Request, dc, cl string) error {
	defs, err := s.manager.Networks(r.Context(), session(r).Org)
	if err != nil {
		return err
	}
	var members []manager.NodeState
	for _, st := range s.manager.Inventory(session(r).Org) {
		if inGroup(st.Node, dc, cl) {
			members = append(members, st)
		}
	}
	if len(members) == 0 {
		return errors.New("no reachable hosts in this cluster")
	}
	errs := fanOut(r.Context(), members, func(ctx context.Context, n manager.Node) error {
		for _, d := range defs {
			if err := n.Call(ctx, http.MethodPost, "/api/networks/create", networkReq{Name: d.Name, VLAN: d.VLAN, Bridge: d.Bridge}, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if errs != "" {
		return errors.New(errs)
	}
	return nil
}
