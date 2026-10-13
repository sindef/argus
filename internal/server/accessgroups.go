package server

import (
	"log"
	"net/http"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/ui"
)

type groupRow struct {
	auth.Group
	Fill string
}

func (s *Server) groupsPage(w http.ResponseWriter, r *http.Request) {
	store := s.Users(r)
	gs, err := store.Groups(r.Context())
	us, uerr := store.Users(r.Context())
	if err == nil {
		err = uerr
	}
	var rows []groupRow
	for _, g := range gs {
		rows = append(rows, groupRow{g, ui.Fill(map[string]any{"old": g.Name, "name": g.Name, "role": g.Role, "scopes": strings.Join(g.Scopes, ", "), "members": append([]string{}, g.Members...), "sso": strings.Join(g.SSO, "\n")})})
	}
	local := slices.DeleteFunc(us, func(u auth.User) bool { return u.SSO })
	custom, _ := s.Users(r).Roles(r.Context())
	var roleRows []map[string]any
	for _, c := range custom {
		ps := make([]string, len(c.Privileges))
		for i, p := range c.Privileges {
			ps[i] = string(p)
		}
		roleRows = append(roleRows, map[string]any{"Name": c.Name, "Privileges": c.Privileges, "Fill": ui.Fill(map[string]any{"old": c.Name, "name": c.Name, "privileges": ps})})
	}
	s.Render(w, r, "groups.html", "layout", map[string]any{"Nav": "user-groups", "Groups": rows, "Users": local, "Roles": s.RoleNames(r), "CustomRoles": roleRows,
		"Privileges": auth.PrivilegeLabels, "Scopes": s.ScopeNames(r), "Error": ui.ErrText(err)})
}

func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	var sso []string
	for _, line := range strings.Split(r.FormValue("sso"), "\n") {
		parts := []string{line}
		if !strings.Contains(line, "=") {
			parts = strings.Split(line, ",")
		}
		for _, g := range parts {
			if g = strings.TrimSpace(g); g != "" {
				sso = append(sso, g)
			}
		}
	}
	g := auth.Group{Name: r.FormValue("name"), Role: auth.Role(r.FormValue("role")), Scopes: ui.ScopeList(r.FormValue("scopes")), Members: r.Form["members"], SSO: sso}
	if err := s.Users(r).SaveGroup(r.Context(), r.FormValue("old"), g); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: saved group %s (%s)", ui.Session(r).User, g.Name, g.Role)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.Users(r).DeleteGroup(r.Context(), name); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: deleted group %s", ui.Session(r).User, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) ScopeNames(r *http.Request) []string {
	var scopes []string
	if s.manager != nil {
		gs, _ := s.manager.Groups(r.Context(), ui.Session(r).Org)
		for _, g := range gs {
			scopes = append(scopes, g.Datacenter)
			for _, c := range g.Clusters {
				scopes = append(scopes, g.Datacenter+"/"+c)
			}
		}
		for _, st := range s.manager.Inventory(ui.Session(r).Org) {
			scopes = append(scopes, "host:"+st.Node.Name)
			for _, v := range st.VMs {
				scopes = append(scopes, "vm:"+st.Node.Name+"/"+v.Name)
			}
		}
	}
	return scopes
}

func (s *Server) saveRole(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	role := auth.CustomRole{Name: strings.TrimSpace(r.FormValue("name"))}
	for _, p := range r.Form["privileges"] {
		role.Privileges = append(role.Privileges, auth.Privilege(p))
	}
	if err := s.Users(r).SaveRole(r.Context(), r.FormValue("old"), role); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: saved role %s %v", ui.Session(r).User, role.Name, role.Privileges)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if err := s.Users(r).DeleteRole(r.Context(), r.FormValue("name")); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: deleted role %s", ui.Session(r).User, r.FormValue("name"))
	w.Header().Set("HX-Refresh", "true")
}
