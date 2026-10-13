package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"argus/internal/auth"
)

var ValidName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

func (s *Core) loginPage(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "login.html", "auth", s.LoginView(r, ""))
}

func (s *Core) login(w http.ResponseWriter, r *http.Request) {
	ip, acct := ClientIP(r), accountKey(r.FormValue("org"), r.FormValue("username"))
	if s.Limiter.Blocked(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.Render(w, r, "login.html", "auth", s.LoginView(r, "Too many failed sign-ins, try again later"))
		return
	}
	if d := s.Limiter.backoff(acct); d > 0 {
		w.WriteHeader(http.StatusTooManyRequests)
		s.Render(w, r, "login.html", "auth", s.LoginView(r, fmt.Sprintf("Too many failed sign-ins for this account, try again in %s", d.Round(time.Second))))
		return
	}
	store, remember, err := s.Mode.LoginStore(w, r)
	if err != nil {
		w.WriteHeader(http.StatusUnauthorized)
		s.Render(w, r, "login.html", "auth", s.LoginView(r, "Select your organization"))
		return
	}
	select {
	case s.Limiter.slots <- struct{}{}:
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		w.WriteHeader(http.StatusServiceUnavailable)
		s.Render(w, r, "login.html", "auth", s.LoginView(r, "Too many sign-ins at once, try again"))
		return
	}
	u, ok, err := store.Verify(r.Context(), r.FormValue("username"), r.FormValue("password"))
	if !ok && err == nil {
		u, ok, err = s.Mode.Directory(r, store)
	}
	<-s.Limiter.slots
	if err != nil {
		log.Printf("login: %v", err)
	}
	if !ok {
		s.Limiter.Fail(ip)
		s.Limiter.Fail(acct)
		log.Printf("login: failed for %q from %s", r.FormValue("username"), ip)
		s.Record(s.AuditUser(r.FormValue("org"), r.FormValue("username")), "sign in from "+ip, false, "invalid username or password")
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusUnauthorized)
		s.Render(w, r, "login.html", "auth", s.LoginView(r, "Invalid username or password"))
		return
	}
	if u.TOTP != "" {
		data := s.LoginView(r, "")
		data["Pending"] = s.mfaStart(r.FormValue("org"), u.Name, remember)
		s.Render(w, r, "login.html", "auth", data)
		return
	}
	s.Limiter.Succeed(ip)
	s.Limiter.Succeed(acct)
	s.Record(s.AuditUser(r.FormValue("org"), r.FormValue("username")), "sign in from "+ip, true, "")
	s.SetSessionCookie(w, store.StartSession(r.Context(), u, remember), remember)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Core) SetSessionCookie(w http.ResponseWriter, tok string, remember bool) {
	c := &http.Cookie{Name: s.Cookie, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
	if remember {
		c.MaxAge = 30 * 24 * 3600
	}
	http.SetCookie(w, c)
}

func (s *Core) Users(r *http.Request) *auth.Store {
	if s.MultiOrg {
		return s.Auth.ForOrg(Session(r).Org)
	}
	return s.Auth
}

func (s *Core) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.Cookie); err == nil {
		s.Auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.Cookie, Path: "/", MaxAge: -1})
	Redirect(w, r, "/login")
}

func (s *Core) passwordPage(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "password.html", "layout", s.passwordData(r, ""))
}

func (s *Core) changePassword(w http.ResponseWriter, r *http.Request) {
	pw := r.FormValue("password")
	msg := ""
	switch {
	case len(pw) < 12:
		msg = "Password must be at least 12 characters"
	case pw != r.FormValue("confirm"):
		msg = "Passwords do not match"
	default:
		err := s.Users(r).ChangePassword(r.Context(), Session(r).Token, r.FormValue("current"), pw)
		if err == nil {
			log.Printf("%s: changed password", Session(r).User)
			if Session(r).User == "admin" {
				s.Banner("")
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
	s.Render(w, r, "password.html", "layout", s.passwordData(r, msg))
}

func (s *Core) usersPage(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, "", nil)
}

func (s *Core) renderUsers(w http.ResponseWriter, r *http.Request, notice string, err error) {
	Failed(w, r, err)
	rows, groups, lerr := s.userRows(r)
	if err == nil {
		err = lerr
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	s.Render(w, r, "users.html", "layout", map[string]any{"Nav": "users", "Users": rows, "Groups": groups, "Roles": s.RoleNames(r), "Scopes": s.Mode.ScopeNames(r), "Notice": notice, "Error": ErrText(err)})
}

func (s *Core) addUser(w http.ResponseWriter, r *http.Request) {
	name, role := r.FormValue("name"), auth.Role(r.FormValue("role"))
	if !ValidName.MatchString(name) || !s.Users(r).ValidRole(r.Context(), role) {
		s.renderUsers(w, r, "", errors.New("invalid username or role"))
		return
	}
	pw := auth.RandomPassword()
	if err := s.Users(r).Add(r.Context(), name, role, pw); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	if sc := ScopeList(r.FormValue("scopes")); len(sc) > 0 {
		if err := s.Users(r).SetScopes(r.Context(), name, sc); err != nil {
			s.renderUsers(w, r, "", err)
			return
		}
	}
	log.Printf("%s: added user %s (%s)", Session(r).User, name, role)
	s.renderUsers(w, r, "Temporary password for "+name+": "+pw, nil)
}

func (s *Core) setRole(w http.ResponseWriter, r *http.Request) {
	name, role := r.PathValue("name"), auth.Role(r.FormValue("role"))
	if !s.Users(r).ValidRole(r.Context(), role) {
		s.renderUsers(w, r, "", errors.New("invalid role"))
		return
	}
	if err := s.Users(r).SetRole(r.Context(), name, role); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: set %s role to %s", Session(r).User, name, role)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Core) resetPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pw := auth.RandomPassword()
	if err := s.Users(r).ResetPassword(r.Context(), name, pw); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: reset password for %s", Session(r).User, name)
	s.renderUsers(w, r, "Temporary password for "+name+": "+pw, nil)
}

func (s *Core) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == Session(r).User {
		s.renderUsers(w, r, "", errors.New("cannot delete yourself"))
		return
	}
	if err := s.Users(r).Delete(r.Context(), name); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: deleted user %s", Session(r).User, name)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Core) LoginView(r *http.Request, msg string) map[string]any {
	data := map[string]any{"Error": msg}
	s.Mode.LoginData(r, data)
	return data
}

func ScopeList(v string) []string {
	var out []string
	for _, sc := range strings.Split(v, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			out = append(out, sc)
		}
	}
	return out
}

func (s *Core) setScopes(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.Users(r).SetScopes(r.Context(), name, ScopeList(r.FormValue("scopes"))); err != nil {
		Flash(w, r, err)
		return
	}
	log.Printf("%s: set %s scopes to %q", Session(r).User, name, r.FormValue("scopes"))
	w.Header().Set("HX-Refresh", "true")
}

type userRow struct {
	auth.User
	Access string
	Scope  []string
	In     []string
	Fill   string
}

func Fill(v map[string]any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (s *Core) userRows(r *http.Request) ([]userRow, []auth.Group, error) {
	store := s.Users(r)
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
			Fill: Fill(map[string]any{"action": "/users/" + u.Name + "/update", "name": u.Name, "role": u.Role, "scopes": strings.Join(u.Scopes, ", "), "groups": append([]string{}, in...), "access": string(role), "via": strings.Join(in, ", ")})})
	}
	return rows, gs, err
}

func (s *Core) updateUser(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	store, name := s.Users(r), r.PathValue("name")
	role := auth.Role(r.FormValue("role"))
	err := error(nil)
	if !s.Users(r).ValidRole(r.Context(), role) {
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
	if sc := ScopeList(r.FormValue("scopes")); err == nil && s.MultiOrg && !slices.Equal(us[i].Scopes, sc) {
		err = store.SetScopes(r.Context(), name, sc)
	}
	if err == nil && s.MultiOrg {
		err = store.SetMembership(r.Context(), name, r.Form["groups"])
	}
	if err != nil {
		Flash(w, r, err)
		return
	}
	log.Printf("%s: updated user %s (%s, groups %v)", Session(r).User, name, role, r.Form["groups"])
	w.Header().Set("HX-Refresh", "true")
}

func (s *Core) RoleNames(r *http.Request) []auth.Role {
	out := slices.Clone(auth.Roles)
	custom, _ := s.Users(r).Roles(r.Context())
	for _, c := range custom {
		out = append(out, auth.Role(c.Name))
	}
	return out
}
