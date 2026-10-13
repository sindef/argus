package managerserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"argus/internal/auth"
	"argus/internal/dbstore"
	"argus/internal/manager"
	"argus/internal/tlscert"
)

const (
	reached   = "handler"
	forbidden = "forbidden"
	login     = "login"
	unauth    = "unauthorized"
	invalid   = "invalid"
)

type harness struct {
	s       *Server
	cookies map[string]string
	tokens  map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := dbstore.Open(filepath.Join(dir, "argus.db"), filepath.Join(dir, "argus.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(db)
	pki, err := tlscert.Load(context.Background(), db, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(store, Options{Manager: manager.New(db, ""), Certs: tlscert.NewHolder(pki.Cert), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	org := store.ForOrg(manager.DefaultOrg)
	if err := org.SaveRole(ctx, "", auth.CustomRole{Name: "viewer", Privileges: []auth.Privilege{auth.VMView}}); err != nil {
		t.Fatal(err)
	}
	h := &harness{s: s, cookies: map[string]string{}, tokens: map[string]string{}}
	for name, role := range map[string]auth.Role{"admin": auth.Admin, "operator": auth.Operator, "readonly": auth.ReadOnly, "scoped": auth.Admin, "viewer": "viewer"} {
		if err := org.Add(ctx, name, role, "correct-horse-battery-staple"); err != nil {
			t.Fatal(err)
		}
	}
	if err := org.SetScopes(ctx, "scoped", []string{"Home/Lab"}); err != nil {
		t.Fatal(err)
	}
	users, err := org.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		u.MustChange = false
		h.cookies[u.Name] = org.StartSession(ctx, u, false)
	}
	for _, scope := range []string{"", auth.ReadScope, auth.MetricsScope} {
		tk := auth.Token{Name: "t-" + scope, Scope: scope, Org: manager.DefaultOrg, User: "operator"}
		tok, err := store.AddToken(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		h.tokens[scope] = tok
	}
	return h
}

func (h *harness) outcome(t *testing.T, method, path, who, origin string) (out string) {
	t.Helper()
	r := httptest.NewRequest(method, "https://argus.test"+path, strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	} else if method != http.MethodGet {
		r.Header.Set("Origin", "https://argus.test")
	}
	switch {
	case strings.HasPrefix(who, "token:"):
		r.Header.Set("Authorization", "Bearer "+h.tokens[strings.TrimPrefix(who, "token:")])
	case who != "":
		r.AddCookie(&http.Cookie{Name: h.s.Cookie, Value: h.cookies[who]})
	}
	w := httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			out = reached
		}
	}()
	h.s.ServeHTTP(w, r)
	loc := w.Header().Get("Location")
	switch {
	case w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "invalid path"):
		return invalid
	case w.Code == http.StatusUnauthorized:
		return unauth
	case w.Code == http.StatusForbidden:
		return forbidden
	case w.Code == http.StatusSeeOther && strings.HasPrefix(loc, "/login"):
		return login
	}
	return reached
}

type routeCase struct {
	method, path, who, origin, want string
}

func run(t *testing.T, h *harness, cases []routeCase) {
	t.Helper()
	for _, c := range cases {
		if got := h.outcome(t, c.method, c.path, c.who, c.origin); got != c.want {
			t.Errorf("%s %s as %q: got %s, want %s", c.method, c.path, c.who, got, c.want)
		}
	}
}

func TestManagerRouteAuth(t *testing.T) {
	h := newHarness(t)
	run(t, h, []routeCase{
		{"GET", "/", "", "", login},
		{"GET", "/networks", "viewer", "", reached},
		{"POST", "/networks", "viewer", "", forbidden},
		{"POST", "/networks", "admin", "", reached},
		{"GET", "/hosts/node-01/disks", "viewer", "", forbidden},
		{"GET", "/hosts/node-01/disks", "operator", "", forbidden},
		{"GET", "/hosts/node-01/disks", "admin", "", reached},
		{"GET", "/users", "scoped", "", forbidden},
		{"GET", "/users", "admin", "", reached},
		{"POST", "/alarm-rules/save", "operator", "", forbidden},
		{"POST", "/alarm-rules/save", "admin", "", reached},
		{"GET", "/api/v1/vms", "token:", "", reached},
		{"DELETE", "/api/v1/hosts/node-01/vms/web", "token:", "", reached},
		{"POST", "/api/v1/networks", "token:", "", forbidden},
		{"PATCH", "/api/v1/hosts/node-01/vms/web", "token:read", "", forbidden},
		{"GET", "/api/v1/hosts", "token:metrics", "", forbidden},
	})
}
