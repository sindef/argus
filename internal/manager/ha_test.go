package manager

import (
	"context"
	"path/filepath"
	"testing"

	"argus/internal/filestore"
)

func TestUnprotectOnlyMatchesOwnHosts(t *testing.T) {
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := New(fs, filepath.Join(t.TempDir(), "m.gob"))
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
