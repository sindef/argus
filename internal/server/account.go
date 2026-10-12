package server

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"argus/internal/auth"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", "auth", s.loginData(r, ""))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login.html", "auth", s.loginData(r, "Too many failed sign-ins, try again later"))
		return
	}
	store, remember := s.auth, false
	if s.manager != nil {
		org := r.FormValue("org")
		if _, err := s.manager.Org(r.Context(), org); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			s.render(w, r, "login.html", "auth", s.loginData(r, "Select your organization"))
			return
		}
		store, remember = s.auth.ForOrg(org), r.FormValue("remember") != ""
		http.SetCookie(w, &http.Cookie{Name: "argus_org", Value: org, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	select {
	case s.limiter.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		w.WriteHeader(http.StatusServiceUnavailable)
		s.render(w, r, "login.html", "auth", s.loginData(r, "Too many sign-ins at once, try again"))
		return
	}
	tok, err := store.Login(r.Context(), r.FormValue("username"), r.FormValue("password"), remember)
	<-s.limiter.slots
	if err != nil {
		log.Printf("login: %v", err)
	}
	if tok == "" {
		s.limiter.fail(ip)
		log.Printf("login: failed for %q from %s", r.FormValue("username"), ip)
		s.record(s.auditUser(r.FormValue("org"), r.FormValue("username")), "sign in from "+ip, false, "invalid username or password")
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", "auth", s.loginData(r, "Invalid username or password"))
		return
	}
	s.limiter.succeed(ip)
	s.record(s.auditUser(r.FormValue("org"), r.FormValue("username")), "sign in from "+ip, true, "")
	s.setSessionCookie(w, tok, remember)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, tok string, remember bool) {
	c := &http.Cookie{Name: s.cookieName(), Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if remember {
		c.MaxAge = 30 * 24 * 3600
	}
	http.SetCookie(w, c)
}

func (s *Server) users(r *http.Request) *auth.Store {
	if s.manager != nil {
		return s.auth.ForOrg(session(r).Org)
	}
	return s.auth
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Path: "/", MaxAge: -1})
	redirect(w, r, "/login")
}

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "password.html", "layout", nil)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("password")
	msg := ""
	switch {
	case len(pw) < 12:
		msg = "Password must be at least 12 characters"
	case pw != r.FormValue("confirm"):
		msg = "Passwords do not match"
	default:
		err := s.users(r).ChangePassword(r.Context(), session(r).Token, r.FormValue("current"), pw)
		if err == nil {
			log.Printf("%s: changed password", session(r).User)
			if session(r).User == "admin" {
				s.banner("")
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		msg = "Could not change password"
		if errors.Is(err, auth.ErrPassword) {
			msg = err.Error()
		}
	}
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, r, "password.html", "layout", map[string]any{"Error": msg})
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, "", nil)
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, notice string, err error) {
	rows, groups, lerr := s.userRows(r)
	if err == nil {
		err = lerr
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	s.render(w, r, "users.html", "layout", map[string]any{"Nav": "users", "Users": rows, "Groups": groups, "Roles": auth.Roles, "Scopes": s.scopeNames(r), "Notice": notice, "Error": errText(err)})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	name, role := r.FormValue("name"), auth.Role(r.FormValue("role"))
	if !validName.MatchString(name) || !role.Valid() {
		s.renderUsers(w, r, "", errors.New("invalid username or role"))
		return
	}
	pw := auth.RandomPassword()
	if err := s.users(r).Add(r.Context(), name, role, pw); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	if sc := scopeList(r.FormValue("scopes")); len(sc) > 0 {
		if err := s.users(r).SetScopes(r.Context(), name, sc); err != nil {
			s.renderUsers(w, r, "", err)
			return
		}
	}
	log.Printf("%s: added user %s (%s)", session(r).User, name, role)
	s.renderUsers(w, r, "Temporary password for "+name+": "+pw, nil)
}

func (s *Server) setRole(w http.ResponseWriter, r *http.Request) {
	name, role := r.PathValue("name"), auth.Role(r.FormValue("role"))
	if !role.Valid() {
		s.renderUsers(w, r, "", errors.New("invalid role"))
		return
	}
	if err := s.users(r).SetRole(r.Context(), name, role); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: set %s role to %s", session(r).User, name, role)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pw := auth.RandomPassword()
	if err := s.users(r).ResetPassword(r.Context(), name, pw); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: reset password for %s", session(r).User, name)
	s.renderUsers(w, r, "Temporary password for "+name+": "+pw, nil)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == session(r).User {
		s.renderUsers(w, r, "", errors.New("cannot delete yourself"))
		return
	}
	if err := s.users(r).Delete(r.Context(), name); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: deleted user %s", session(r).User, name)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) loginData(r *http.Request, msg string) map[string]any {
	data := map[string]any{"Error": msg}
	if s.manager != nil {
		orgs, _ := s.manager.Orgs(r.Context())
		sso := false
		for _, o := range orgs {
			sso = sso || o.SSO()
		}
		org := r.FormValue("org")
		if c, err := r.Cookie("argus_org"); err == nil && org == "" {
			org = c.Value
		}
		data["Orgs"], data["Org"], data["SSO"] = orgs, org, sso
	}
	return data
}

func scopeList(v string) []string {
	var out []string
	for _, sc := range strings.Split(v, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Server) setScopes(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.users(r).SetScopes(r.Context(), name, scopeList(r.FormValue("scopes"))); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: set %s scopes to %q", session(r).User, name, r.FormValue("scopes"))
	w.Header().Set("HX-Refresh", "true")
}
