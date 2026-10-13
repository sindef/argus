package manager

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"argus/internal/dbstore"
)

func TestUnprotectOnlyMatchesOwnHosts(t *testing.T) {
	fs, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	c := New(fs, filepath.Join(t.TempDir(), "m.gob"), 1)
	ctx := context.Background()
	c.updateHA(ctx, func([]Protected) ([]Protected, error) {
		return []Protected{{VM: "web", Owner: "a1", Replica: "a2"}, {VM: "web", Owner: "b1", Replica: "b2"}}, nil
	})
	if err := c.Unprotect(ctx, "c9", "web"); err == nil {
		t.Fatal("unprotect through an unrelated host succeeded")
	}
	if err := c.Unprotect(ctx, "a1", "web"); err != nil {
		t.Fatal(err)
	}
	ps, _ := c.Protected(ctx)
	if len(ps) != 1 || ps[0].Owner != "b1" {
		t.Fatalf("other organization's record changed: %+v", ps)
	}
}

func TestFailoverIsManualByDefault(t *testing.T) {
	fs, _ := testStore(t)
	c := New(fs, filepath.Join(t.TempDir(), "m.gob"), 1)
	ctx := context.Background()
	c.save(ctx, []Node{{Name: "a1", Org: "o"}, {Name: "a2", Org: "o"}}, "")
	c.updateHA(ctx, func([]Protected) ([]Protected, error) {
		return []Protected{{VM: "web", Owner: "a1", Replica: "a2", LastSync: time.Now()}}, nil
	})
	c.lastSeen["a1"], c.lastSeen["a2"] = time.Now(), time.Now()
	if err := c.ManualFailover(ctx, "a1", "web"); err == nil {
		t.Fatal("failed over while the owner is reachable")
	}
	c.lastSeen["a1"] = time.Now().Add(-time.Hour)
	c.failover(ctx)
	if ps, _ := c.Protected(ctx); ps[0].Owner != "a1" {
		t.Fatal("automatic failover ran while disabled")
	}
	if err := c.ManualFailover(ctx, "a1", "web"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := c.Protected(ctx); ps[0].Owner != "a2" || ps[0].Replica != "a1" {
		t.Fatalf("manual failover did not swap roles: %+v", ps[0])
	}
}

func testStore(t *testing.T) (*dbstore.Store, error) {
	dir := t.TempDir()
	return dbstore.Open(filepath.Join(dir, "argus.db"), filepath.Join(dir, "argus.key"))
}
