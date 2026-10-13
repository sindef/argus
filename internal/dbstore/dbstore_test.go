package dbstore

import (
	"argus/internal/store"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	db, key := filepath.Join(dir, "argus.db"), filepath.Join(dir, "argus.key")
	s, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Create(ctx, "a", "Opaque", map[string][]byte{"k": []byte("secret-value")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "a", "Opaque", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}
	data, rv, err := s.Get(ctx, "a")
	if err != nil || string(data["k"]) != "secret-value" || rv != "1" {
		t.Fatalf("get: %v %v %s", data, err, rv)
	}
	if err := s.Update(ctx, "a", "1", map[string][]byte{"k": []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, "a", "1", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	s.Close()
	raw, _ := os.ReadFile(db)
	if bytes.Contains(raw, []byte("secret-value")) {
		t.Fatal("plaintext found in the database file")
	}
	os.WriteFile(key, make([]byte, 32), 0o600)
	s2, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, _, err := s2.Get(ctx, "a"); err == nil {
		t.Fatal("record readable with the wrong key")
	}
	os.Chmod(key, 0o644)
	if _, err := Open(db, key); err == nil {
		t.Fatal("world-readable key accepted")
	}
}
