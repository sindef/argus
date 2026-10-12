package manager

import (
	"testing"
	"time"

	"argus/internal/kube"
)

func TestAdmitAndPriority(t *testing.T) {
	c := &Manager{promoted: map[string]time.Time{}, inv: inventory{nodes: map[string]NodeState{
		"a": {Node: Node{Name: "a"}, Observed: time.Now(), Info: kube.NodeInfo{MemoryBytes: 16 << 30}, VMs: []kube.VM{{Name: "db", Memory: "8Gi", Running: true}, {Name: "web", Memory: "4Gi", Running: true}}},
		"b": {Node: Node{Name: "b"}, Observed: time.Now(), Info: kube.NodeInfo{MemoryBytes: 16 << 30}, VMs: []kube.VM{{Name: "cache", Memory: "4Gi", Running: true}}},
	}}}
	if err := c.admit(nil, "a", "db", "b"); err != nil {
		t.Fatalf("db fits on b: %v", err)
	}
	ps := []Protected{{VM: "db", Owner: "a", Replica: "b"}}
	if err := c.admit(ps, "a", "web", "b"); err == nil {
		t.Fatal("db+web+cache exceed 90% of b and were admitted")
	}

	high := Protected{VM: "db", Owner: "b", Replica: "a", Priority: "high"}
	low := Protected{VM: "web", Owner: "a", Replica: "b", Priority: "low"}
	c.promoted["db"] = time.Now()
	st := c.inv.nodes["b"]
	st.VMs = append(st.VMs, kube.VM{Name: "db", Memory: "8Gi"})
	c.inv.nodes["b"] = st
	if !c.waitingOnHigher([]Protected{high, low}, low) {
		t.Fatal("low priority did not wait for the high priority VM to start")
	}
	st.VMs[len(st.VMs)-1].Running = true
	c.inv.nodes["b"] = st
	if c.waitingOnHigher([]Protected{high, low}, low) {
		t.Fatal("low priority still waiting after the high priority VM started")
	}
}
