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
