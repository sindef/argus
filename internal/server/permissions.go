package server

import (
	"net/http"
	"strings"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
)

func objectOf(n manager.Node, vm string) auth.Object {
	return auth.Object{Datacenter: groupName(n.Datacenter), Cluster: groupName(n.Cluster), Host: n.Name, VM: vm}
}

func (s *Server) can(r *http.Request, p auth.Privilege, n manager.Node, vm string) bool {
	_, grants, ok := ui.Perms(r)
	if !ok {
		return inScope(ui.Session(r).Scopes, n)
	}
	return auth.Allows(grants, p, objectOf(n, vm))
}

func pathVM(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/vms/") {
		return r.PathValue("name")
	}
	return ""
}

func (s *Server) inventory(r *http.Request) []manager.NodeState {
	all := s.manager.Inventory(ui.Session(r).Org)
	_, grants, ok := ui.Perms(r)
	if !ok {
		var out []manager.NodeState
		for _, st := range all {
			if inScope(ui.Session(r).Scopes, st.Node) {
				out = append(out, st)
			}
		}
		return out
	}
	var out []manager.NodeState
	for _, st := range all {
		o := objectOf(st.Node, "")
		if !auth.ReachesHost(grants, auth.VMView, o) && !auth.Allows(grants, auth.HostView, o) {
			continue
		}
		var vms []kube.VM
		for _, v := range st.VMs {
			if auth.Allows(grants, auth.VMView, objectOf(st.Node, v.Name)) {
				vms = append(vms, v)
			}
		}
		st.VMs = vms
		out = append(out, st)
	}
	return out
}

func (s *Server) nodeIn(r *http.Request, name string) (manager.Node, error) {
	n, err := s.manager.NodeIn(r.Context(), ui.Session(r).Org, name)
	if err != nil {
		return n, err
	}
	p := auth.VMView
	if priv, _, ok := ui.Perms(r); ok {
		p = priv
	}
	if !s.can(r, p, n, pathVM(r)) {
		return manager.Node{}, errNodeNotFound(name)
	}
	return n, nil
}

func actionPrivilege(act string) auth.Privilege {
	switch act {
	case "delete":
		return auth.VMDelete
	case "snapshot":
		return auth.VMSnapshot
	case "rename", "tags":
		return auth.VMConfig
	case "clone", "template", "untemplate", "template-copy":
		return auth.VMProvision
	}
	return auth.VMPower
}
