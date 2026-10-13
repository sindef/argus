package server

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

func newHarness(t *testing.T, managed bool) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := dbstore.Open(filepath.Join(dir, "argus.db"), filepath.Join(dir, "argus.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(db)
	o := Options{}
	org := store
	if managed {
		pki, err := tlscert.Load(context.Background(), db, nil)
		if err != nil {
			t.Fatal(err)
		}
		o.Manager, o.Certs, o.DataDir = manager.New(db, ""), tlscert.NewHolder(pki.Cert), dir
		org = store.ForOrg(manager.DefaultOrg)
	}
	s, err := New(nil, store, o)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
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
		tk := auth.Token{Name: "t-" + scope, Scope: scope}
		if managed {
			tk.Org, tk.User = manager.DefaultOrg, "admin"
		}
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
		r.AddCookie(&http.Cookie{Name: h.s.cookieName(), Value: h.cookies[who]})
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

func TestHostRouteAuth(t *testing.T) {
	h := newHarness(t, false)
	run(t, h, []routeCase{
		{"GET", "/vms", "", "", login},
		{"GET", "/api/vms", "", "", unauth},
		{"GET", "/vms", "readonly", "", reached},
		{"POST", "/vms/default/web/start", "readonly", "", forbidden},
		{"POST", "/vms/default/web/start", "operator", "", reached},
		{"GET", "/users", "operator", "", forbidden},
		{"GET", "/users", "admin", "", reached},
		{"GET", "/disks", "operator", "", forbidden},
		{"GET", "/disks", "admin", "", reached},
		{"POST", "/vms/default/web/start", "operator", "https://evil.test", forbidden},
		{"GET", "/vms/default/web%20x", "admin", "", invalid},
		{"GET", "/api/vms", "token:", "", reached},
		{"GET", "/vms", "token:", "", unauth},
		{"POST", "/api/vms/default/web/start", "token:read", "", forbidden},
		{"GET", "/api/vms", "token:read", "", reached},
		{"GET", "/api/vms", "token:metrics", "", forbidden},
		{"GET", "/api/prometheus", "token:metrics", "", reached},
	})
}

func TestManagerRouteAuth(t *testing.T) {
	h := newHarness(t, true)
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
		{"PATCH", "/api/v1/hosts/node-01/vms/web", "token:read", "", forbidden},
		{"GET", "/api/v1/hosts", "token:metrics", "", forbidden},
	})
}
