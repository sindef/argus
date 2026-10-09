package server

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"time"

	"argus/internal/auth"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login.html", "layout", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	tok, err := s.auth.Login(r.Context(), r.FormValue("username"), r.FormValue("password"))
	if err != nil {
		log.Printf("login: %v", err)
	}
	if tok == "" {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, r, "login.html", "layout", map[string]any{"Error": "Invalid username or password"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
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
		err := s.auth.ChangePassword(r.Context(), session(r).Token, r.FormValue("current"), pw)
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

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, "", nil)
}

func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, notice string, err error) {
	us, lerr := s.auth.Users(r.Context())
	if err == nil {
		err = lerr
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
	}
	s.render(w, r, "users.html", "layout", map[string]any{"Users": us, "Roles": auth.Roles, "Notice": notice, "Error": errText(err)})
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	name, role := r.FormValue("name"), auth.Role(r.FormValue("role"))
	if !validName.MatchString(name) || !role.Valid() {
		s.renderUsers(w, r, "", errors.New("invalid username or role"))
		return
	}
	pw := auth.RandomPassword()
	if err := s.auth.Add(r.Context(), name, role, pw); err != nil {
		s.renderUsers(w, r, "", err)
		return
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
	if err := s.auth.SetRole(r.Context(), name, role); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: set %s role to %s", session(r).User, name, role)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pw := auth.RandomPassword()
	if err := s.auth.ResetPassword(r.Context(), name, pw); err != nil {
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
	if err := s.auth.Delete(r.Context(), name); err != nil {
		s.renderUsers(w, r, "", err)
		return
	}
	log.Printf("%s: deleted user %s", session(r).User, name)
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}
