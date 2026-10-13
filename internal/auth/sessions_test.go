package auth

import (
	"context"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type memSecrets struct {
	mu sync.Mutex
	m  map[string]map[string][]byte
}

func (m *memSecrets) Get(_ context.Context, name string) (map[string][]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.m[name]
	if !ok {
		return nil, "", apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
	}
	return d, "1", nil
}

func (m *memSecrets) Create(_ context.Context, name, _ string, data map[string][]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[name] = data
	return nil
}

func (m *memSecrets) Update(_ context.Context, name, _ string, data map[string][]byte) error {
	return m.Create(context.Background(), name, "", data)
}

func restart(sec Secrets) *Store {
	s := NewStore(sec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.PersistSessions(ctx)
	return s
}

func TestSessionRevocationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	sec := &memSecrets{m: map[string]map[string][]byte{}}
	s := NewStore(sec)
	if err := s.Add(ctx, "root", Admin, "pw-root-123"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "ops", Admin, "pw-ops-1234"); err != nil {
		t.Fatal(err)
	}
	kept, _ := s.Login(ctx, "root", "pw-root-123", false)
	out, _ := s.Login(ctx, "root", "pw-root-123", false)
	demoted, _ := s.Login(ctx, "ops", "pw-ops-1234", false)
	s.flushSessions(ctx)
	s.Logout(out)
	if err := s.SetRole(ctx, "ops", ReadOnly); err != nil {
		t.Fatal(err)
	}

	r := restart(sec)
	if _, ok := r.Session(kept); !ok {
		t.Error("valid session lost across restart")
	}
	if _, ok := r.Session(out); ok {
		t.Error("logged-out session came back after restart")
	}
	if _, ok := r.Session(demoted); ok {
		t.Error("session of demoted user came back after restart")
	}
}

func TestSSOIdentityIsBySubject(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&memSecrets{m: map[string]map[string][]byte{}})
	if err := s.UpsertSSO(ctx, "alice", "idp|1", Admin, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSSO(ctx, "alice", "idp|2", Admin, nil, nil); err == nil {
		t.Fatal("a second identity took over an existing SSO user name")
	}
	if err := s.UpsertSSO(ctx, "alice.smith", "idp|1", Operator, nil, nil); err != nil {
		t.Fatal(err)
	}
	us, _ := s.Users(ctx)
	if len(us) != 1 || us[0].Name != "alice.smith" || us[0].Role != Operator {
		t.Fatalf("rename by subject failed: %+v", us)
	}
}

func TestCustomRoleChangeRefreshesSessions(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&memSecrets{m: map[string]map[string][]byte{}})
	if err := s.SaveRole(ctx, "", CustomRole{Name: "boss", Privileges: []Privilege{Administer}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "ann", "boss", "pw-ann-12345"); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.Login(ctx, "ann", "pw-ann-12345", false)
	if sess, _ := s.Session(tok); sess.Role != Admin {
		t.Fatalf("role before downgrade: %s", sess.Role)
	}
	if err := s.SaveRole(ctx, "boss", CustomRole{Name: "boss", Privileges: []Privilege{VMView}}); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.Session(tok); sess.Role != ReadOnly {
		t.Errorf("role after downgrade: %s", sess.Role)
	}
}

func TestDeletedUserLosesTokensAndGroups(t *testing.T) {
	ctx := context.Background()
	s := NewStore(&memSecrets{m: map[string]map[string][]byte{}}).ForOrg("acme")
	for _, n := range []string{"root", "worker"} {
		if err := s.Add(ctx, n, Admin, "pw-"+n+"-12345"); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := s.AddToken(ctx, Token{Name: "ci", Org: "acme", User: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGroup(ctx, "", Group{Name: "ops", Role: Admin, Members: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "worker"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(ctx, "worker", ReadOnly, "pw-worker-67890"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.CheckToken(ctx, tok); ok {
		t.Error("deleted user's token still valid")
	}
	if gs, _ := s.Groups(ctx); len(gs) != 1 || len(gs[0].Members) != 0 {
		t.Errorf("deleted user's group membership kept: %+v", gs)
	}
}
