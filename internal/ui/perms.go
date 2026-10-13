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

type objectKey struct{}

func WithObject(ctx context.Context, o auth.Object) context.Context {
	return context.WithValue(ctx, objectKey{}, o)
}

func Permits(r *http.Request, p auth.Privilege) bool {
	_, grants, _ := Perms(r)
	if o, ok := r.Context().Value(objectKey{}).(auth.Object); ok {
		o.VM = r.PathValue("name")
		return auth.Allows(grants, p, o)
	}
	return auth.AllowsAnywhere(grants, p)
}

func Perms(r *http.Request) (auth.Privilege, []auth.Grant, bool) {
	pm, ok := r.Context().Value(permKey{}).(perms)
	return pm.priv, pm.grants, ok
}
