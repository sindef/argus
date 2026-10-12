package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"argus/internal/auth"
)

type userRow struct {
	auth.User
	Access string
	Scope  []string
	In     []string
	Fill   string
}

type groupRow struct {
	auth.Group
	Fill string
}

func fill(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Server) userRows(r *http.Request) ([]userRow, []auth.Group, error) {
	store := s.users(r)
	us, err := store.Users(r.Context())
	if err != nil {
		return nil, nil, err
	}
	gs, err := store.Groups(r.Context())
	var rows []userRow
	for _, u := range us {
		role, scopes := store.Access(r.Context(), u)
		in := u.Groups
		if !u.SSO {
			in = nil
			for _, g := range gs {
				if slices.Contains(g.Members, u.Name) {
					in = append(in, g.Name)
				}
			}
		}
		rows = append(rows, userRow{User: u, Access: string(role), Scope: scopes, In: in,
			Fill: fill(map[string]any{"action": "/users/" + u.Name + "/update", "name": u.Name, "role": u.Role, "scopes": strings.Join(u.Scopes, ", "), "groups": append([]string{}, in...), "access": string(role), "via": strings.Join(in, ", ")})})
	}
	return rows, gs, err
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	store, name := s.users(r), r.PathValue("name")
	role := auth.Role(r.FormValue("role"))
	err := error(nil)
	if !role.Valid() {
		err = errors.New("invalid role")
	}
	us, _ := store.Users(r.Context())
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == name })
	if err == nil && i < 0 {
		err = auth.ErrNotFound
	}
	if err == nil && us[i].SSO {
		err = auth.ErrSSO
	}
	if err == nil && us[i].Role != role {
		err = store.SetRole(r.Context(), name, role)
	}
	if sc := scopeList(r.FormValue("scopes")); err == nil && s.manager != nil && !slices.Equal(us[i].Scopes, sc) {
		err = store.SetScopes(r.Context(), name, sc)
	}
	if err == nil && s.manager != nil {
		err = store.SetMembership(r.Context(), name, r.Form["groups"])
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: updated user %s (%s, groups %v)", session(r).User, name, role, r.Form["groups"])
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) groupsPage(w http.ResponseWriter, r *http.Request) {
	store := s.users(r)
	gs, err := store.Groups(r.Context())
	us, uerr := store.Users(r.Context())
	if err == nil {
		err = uerr
	}
	var rows []groupRow
	for _, g := range gs {
		rows = append(rows, groupRow{g, fill(map[string]any{"old": g.Name, "name": g.Name, "role": g.Role, "scopes": strings.Join(g.Scopes, ", "), "members": append([]string{}, g.Members...), "sso": strings.Join(g.SSO, "\n")})})
	}
	local := slices.DeleteFunc(us, func(u auth.User) bool { return u.SSO })
	s.render(w, r, "groups.html", "layout", map[string]any{"Nav": "user-groups", "Groups": rows, "Users": local, "Roles": auth.Roles, "Scopes": s.scopeNames(r), "Error": errText(err)})
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
	g := auth.Group{Name: r.FormValue("name"), Role: auth.Role(r.FormValue("role")), Scopes: scopeList(r.FormValue("scopes")), Members: r.Form["members"], SSO: sso}
	if err := s.users(r).SaveGroup(r.Context(), r.FormValue("old"), g); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: saved group %s (%s)", session(r).User, g.Name, g.Role)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.users(r).DeleteGroup(r.Context(), name); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: deleted group %s", session(r).User, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) scopeNames(r *http.Request) []string {
	var scopes []string
	if s.manager != nil {
		gs, _ := s.manager.Groups(r.Context(), session(r).Org)
		for _, g := range gs {
			scopes = append(scopes, g.Datacenter)
			for _, c := range g.Clusters {
				scopes = append(scopes, g.Datacenter+"/"+c)
			}
		}
	}
	return scopes
}
