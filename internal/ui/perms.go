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

type Can struct {
	Power, Console, Config, Provision, Delete, Snapshot, Backup, Migrate, Images, Networks, Host, Admin bool
}

func Capabilities(grants []auth.Grant, o *auth.Object) Can {
	has := func(p auth.Privilege) bool {
		if o == nil {
			return auth.AllowsAnywhere(grants, p)
		}
		return auth.Allows(grants, p, *o)
	}
	return Can{Power: has(auth.VMPower), Console: has(auth.VMConsole), Config: has(auth.VMConfig), Provision: has(auth.VMProvision), Delete: has(auth.VMDelete),
		Snapshot: has(auth.VMSnapshot), Backup: has(auth.VMBackup), Migrate: has(auth.VMMigrate), Images: has(auth.ImageConfig), Networks: has(auth.NetworkConfig),
		Host: has(auth.HostConfig), Admin: has(auth.Administer)}
}

func (s *Core) UserGrants(r *http.Request) []auth.Grant {
	if _, g, ok := Perms(r); ok {
		return g
	}
	g, _ := s.Mode.Grants(r.Context(), Session(r).Session)
	return g
}

func (s *Core) Can(r *http.Request) Can {
	var o *auth.Object
	if obj, ok := r.Context().Value(objectKey{}).(auth.Object); ok {
		obj.VM = r.PathValue("name")
		o = &obj
	}
	return Capabilities(s.UserGrants(r), o)
}
