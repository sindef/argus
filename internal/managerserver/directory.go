package managerserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"argus/internal/auth"
	"argus/internal/manager"
)

func (s *Server) directoryLogin(r *http.Request, store *auth.Store) (auth.User, bool, error) {
	org, err := s.manager.Org(r.Context(), r.FormValue("org"))
	switch {
	case errors.Is(err, manager.ErrOrgNotFound):
		return auth.User{}, false, nil
	case err != nil:
		return auth.User{}, false, err
	case !org.Directory():
		return auth.User{}, false, nil
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	dn, groups, err := org.LDAP.Authenticate(r.Context(), name, r.FormValue("password"))
	if err != nil {
		return auth.User{}, false, fmt.Errorf("directory: %w", err)
	}
	role, scopes, matched, ok := store.SSOAccess(r.Context(), groups)
	if !ok {
		return auth.User{}, false, fmt.Errorf("directory: %s is in no mapped group", dn)
	}
	if err := store.UpsertSSO(r.Context(), name, "ldap|"+dn, role, scopes, matched); err != nil {
		return auth.User{}, false, err
	}
	return auth.User{Name: name, Role: role, Scopes: scopes, SSO: true}, true, nil
}
