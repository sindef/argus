package managerserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"argus/internal/auth"
	"argus/internal/dbstore"
	"argus/internal/manager"
	"argus/internal/registry"
	"argus/internal/tlscert"
	"argus/web"
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
	s, err := New(store, Options{Manager: manager.New(db, "", 1), Certs: tlscert.NewHolder(pki.Cert), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.manager.EnsureDefaultOrg(ctx); err != nil {
		t.Fatal(err)
	}
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

func TestScopedFleetMutations(t *testing.T) {
	h := newHarness(t)
	post := func(who, path, form string) string {
		r := httptest.NewRequest(http.MethodPost, "https://argus.test"+path, strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://argus.test")
		r.AddCookie(&http.Cookie{Name: h.s.Cookie, Value: h.cookies[who]})
		w := httptest.NewRecorder()
		h.s.ServeHTTP(w, r)
		return w.Body.String()
	}
	for _, c := range []struct {
		who, path, form string
		denied          bool
	}{
		{"scoped", "/groups/rename-datacenter", "dc=Other&name=Taken", true},
		{"scoped", "/groups/add-datacenter", "name=New", true},
		{"scoped", "/groups/rolling-update", "dc=Home&cluster=Other", true},
		{"admin", "/groups/rename-datacenter", "dc=Other&name=Taken", false},
		{"scoped", "/firewall/default", "deny=on", true},
		{"admin", "/firewall/default", "deny=on", false},
		{"scoped", "/networks", "name=lan2&bridge=br0", true},
		{"scoped", "/networks", "name=lan2&bridge=br0&scopes=Home/Lab", false},
		{"scoped", "/alarm-rules/save", "name=r1&metric=host-cpu&warn=90", true},
		{"scoped", "/alarm-rules/save", "name=r1&metric=host-cpu&warn=90&scope=Home/Lab", false},
		{"scoped", "/images/source-add", "name=s&kind=http&url=https://example.test/", true},
	} {
		if got := strings.Contains(post(c.who, c.path, c.form), "not permitted"); got != c.denied {
			t.Errorf("%s %s %s: denied=%v, want %v", c.who, c.path, c.form, got, c.denied)
		}
	}
}

func TestDeletedOrganizationCredentials(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.s.manager.SaveOrg(ctx, manager.Org{ID: "acme", Name: "Acme"}); err != nil {
		t.Fatal(err)
	}
	acme := h.s.Auth.ForOrg("acme")
	if _, err := acme.Bootstrap(ctx, "acme-admin-pass-1"); err != nil {
		t.Fatal(err)
	}
	h.tokens["acme"], _ = h.s.Auth.AddToken(ctx, auth.Token{Name: "a", Org: "acme", User: "admin"})
	if got := h.outcome(t, "GET", "/api/v1/vms", "token:acme", ""); got != reached {
		t.Fatalf("token before deletion: %s", got)
	}
	if err := h.s.manager.DeleteOrg(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if got := h.outcome(t, "GET", "/api/v1/vms", "token:acme", ""); got != unauth {
		t.Errorf("token after deletion: %s", got)
	}
	r := httptest.NewRequest(http.MethodPost, "https://argus.test/settings/orgs", strings.NewReader("id=acme&name=Acme"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://argus.test")
	r.AddCookie(&http.Cookie{Name: h.s.Cookie, Value: h.cookies["admin"]})
	w := httptest.NewRecorder()
	h.s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "belonged to a deleted organization") {
		t.Errorf("recreating a deleted organization ID was not refused")
	}
	if _, err := h.s.manager.Org(ctx, "acme"); err == nil {
		t.Errorf("deleted organization was recreated")
	}
	r = httptest.NewRequest(http.MethodPost, "https://argus.test/settings/orgs", strings.NewReader("id=beta&name=Beta"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://argus.test")
	r.AddCookie(&http.Cookie{Name: h.s.Cookie, Value: h.cookies["admin"]})
	h.s.ServeHTTP(httptest.NewRecorder(), r)
	if _, err := h.s.manager.Org(ctx, "beta"); err != nil {
		t.Errorf("new organization not created: %v", err)
	}
}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	b, err := web.FS.ReadFile("static/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			if m != "parameters" {
				documented[strings.ToUpper(m)+" /api/v1"+strings.ReplaceAll(p, "{host}", "{node}")] = true
			}
		}
	}
	served := map[string]bool{}
	for _, rt := range newHarness(t).s.apiV1Routes() {
		served[rt.Pattern] = true
		if !documented[rt.Pattern] && rt.Pattern != "GET /api/v1/openapi.json" {
			t.Errorf("%s is not in openapi.json", rt.Pattern)
		}
	}
	for d := range documented {
		if !served[d] {
			t.Errorf("openapi.json documents %s, which is not served", d)
		}
	}
}

func TestRegistryUsesCurrentGrants(t *testing.T) {
	h := newHarness(t)
	can := func(user string, a registry.Access) bool {
		r := httptest.NewRequest(http.MethodGet, "https://argus.test/v2/", nil)
		r.SetBasicAuth("default/"+user, "correct-horse-battery-staple")
		return h.s.registryAuthorize(r, "default/img", a)
	}
	for _, c := range []struct {
		user       string
		pull, push bool
	}{{"operator", true, true}, {"viewer", true, false}, {"scoped", true, false}, {"readonly", true, false}} {
		if can(c.user, registry.Pull) != c.pull || can(c.user, registry.Push) != c.push {
			t.Errorf("%s: pull/push not %v/%v", c.user, c.pull, c.push)
		}
	}
	if err := h.s.Auth.ForOrg(manager.DefaultOrg).Delete(context.Background(), "operator"); err != nil {
		t.Fatal(err)
	}
	if can("operator", registry.Push) {
		t.Error("deleted user kept cached push access")
	}
}
