package server

import (
	"context"
	"net/http"
	"strings"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
)

type permKey struct{}

type perms struct {
	priv   auth.Privilege
	grants []auth.Grant
}

func (s *Server) need(p auth.Privilege, h http.HandlerFunc) http.HandlerFunc {
	return s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		sess := session(r)
		grants := auth.RoleGrants(sess.Role)
		var err error
		if s.manager != nil {
			grants, err = s.auth.ForOrg(sess.Org).Grants(r.Context(), sess.User)
		}
		if err != nil || !auth.AllowsAnywhere(grants, p) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), permKey{}, perms{p, grants})))
	})
}

func objectOf(n manager.Node, vm string) auth.Object {
	return auth.Object{Datacenter: groupName(n.Datacenter), Cluster: groupName(n.Cluster), Host: n.Name, VM: vm}
}

func (s *Server) can(r *http.Request, p auth.Privilege, n manager.Node, vm string) bool {
	pm, ok := r.Context().Value(permKey{}).(perms)
	if !ok {
		return inScope(session(r).Scopes, n)
	}
	return auth.Allows(pm.grants, p, objectOf(n, vm))
}

func granted(r *http.Request, p auth.Privilege) bool {
	pm, _ := r.Context().Value(permKey{}).(perms)
	return auth.AllowsAnywhere(pm.grants, p)
}

func pathVM(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/vms/") {
		return r.PathValue("name")
	}
	return ""
}

func (s *Server) inventory(r *http.Request) []manager.NodeState {
	all := s.manager.Inventory(session(r).Org)
	pm, ok := r.Context().Value(permKey{}).(perms)
	if !ok {
		var out []manager.NodeState
		for _, st := range all {
			if inScope(session(r).Scopes, st.Node) {
				out = append(out, st)
			}
		}
		return out
	}
	var out []manager.NodeState
	for _, st := range all {
		o := objectOf(st.Node, "")
		if !auth.ReachesHost(pm.grants, auth.VMView, o) && !auth.Allows(pm.grants, auth.HostView, o) {
			continue
		}
		var vms []kube.VM
		for _, v := range st.VMs {
			if auth.Allows(pm.grants, auth.VMView, objectOf(st.Node, v.Name)) {
				vms = append(vms, v)
			}
		}
		st.VMs = vms
		out = append(out, st)
	}
	return out
}

func (s *Server) nodeIn(r *http.Request, name string) (manager.Node, error) {
	n, err := s.manager.NodeIn(r.Context(), session(r).Org, name)
	if err != nil {
		return n, err
	}
	p := auth.VMView
	if pm, ok := r.Context().Value(permKey{}).(perms); ok {
		p = pm.priv
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
