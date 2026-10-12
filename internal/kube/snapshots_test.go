package kube

import (
	"testing"
	"time"
)

func TestSnapshotTree(t *testing.T) {
	at := func(m int) time.Time { return time.Unix(int64(m)*60, 0) }
	got := snapshotTree([]Snapshot{
		{Name: "c", Parent: "a", Created: at(3)},
		{Name: "a", Created: at(1)},
		{Name: "b", Parent: "a", Created: at(2)},
		{Name: "d", Parent: "b", Created: at(4)},
		{Name: "orphan", Parent: "gone", Created: at(5)},
	})
	want := []struct {
		name  string
		depth int
	}{{"a", 0}, {"b", 1}, {"d", 2}, {"c", 1}, {"orphan", 0}}
	if len(got) != len(want) {
		t.Fatalf("got %d snapshots", len(got))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Depth != w.depth {
			t.Errorf("%d: got %s/%d, want %s/%d", i, got[i].Name, got[i].Depth, w.name, w.depth)
		}
	}
}
