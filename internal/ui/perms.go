package ui

import (
	"context"
	"net/http"

	"argus/internal/auth"
)

type permKey struct{}

type perms struct {
	priv   auth.Privilege
	grants []auth.Grant
}

func (s *Core) Need(p auth.Privilege, h http.HandlerFunc) http.HandlerFunc {
	return s.Authenticate(func(w http.ResponseWriter, r *http.Request) {
		sess := Session(r)
		grants, err := s.Mode.Grants(r.Context(), sess.Session)
		if err != nil || !auth.AllowsAnywhere(grants, p) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), permKey{}, perms{p, grants})))
	})
}

func Granted(r *http.Request, p auth.Privilege) bool {
	pm, _ := r.Context().Value(permKey{}).(perms)
	return auth.AllowsAnywhere(pm.grants, p)
}

func Perms(r *http.Request) (auth.Privilege, []auth.Grant, bool) {
	pm, ok := r.Context().Value(permKey{}).(perms)
	return pm.priv, pm.grants, ok
}
