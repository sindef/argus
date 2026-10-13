package manager

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestInterruptedJobKeepsLocks(t *testing.T) {
	fs, _ := testStore(t)
	ctx := context.Background()
	b, _ := json.Marshal([]*Job{{ID: "j1", VM: "web", From: "a1", To: "a2", Status: "Copying", Live: true}})
	fs.Create(ctx, jobsKey, "Opaque", map[string][]byte{"jobs": b})
	c := New(fs, filepath.Join(t.TempDir(), "m.gob"), 1)
	c.loadJobs(ctx)
	if !c.Busy("a1", "web") || !c.Busy("a2", "web") {
		t.Fatal("interrupted migration released its locks")
	}
	err := c.startJob(context.Background(), &Job{VM: "web", From: "a2", To: "a1"}, time.Minute, func(context.Context, *Job) error { return nil })
	if err == nil {
		t.Fatal("new operation accepted on a VM with an unresolved migration")
	}
}
