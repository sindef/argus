package manager

import (
	"context"
	"math/rand"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"argus/internal/kube"
)

const (
	pollEvery   = 30 * time.Second
	pollTimeout = 10 * time.Second
	pollWorkers = 8
)

type NodeState struct {
	Node     Node
	Info     kube.NodeInfo
	VMs      []kube.VM
	Error    string
	Observed time.Time
}

func (s NodeState) Reachable() bool { return s.Error == "" && !s.Observed.IsZero() }

func (s NodeState) Arch() string {
	if s.Info.Arch == "" {
		return "amd64"
	}
	return s.Info.Arch
}

func (s NodeState) Stale() bool { return time.Since(s.Observed) > 3*pollEvery }

type inventory struct {
	mu    sync.RWMutex
	nodes map[string]NodeState
}

func (m *Manager) Inventory(org string) []NodeState {
	m.inv.mu.RLock()
	defer m.inv.mu.RUnlock()
	var out []NodeState
	for _, s := range m.inv.nodes {
		if s.Node.Org == org {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node.Name < out[j].Node.Name })
	return out
}

func (m *Manager) NodeState(name string) (NodeState, bool) {
	m.inv.mu.RLock()
	defer m.inv.mu.RUnlock()
	s, ok := m.inv.nodes[name]
	return s, ok
}

func (m *Manager) pollLoop() {
	for {
		m.PollAll(context.Background())
		m.tel.save()
		time.Sleep(pollEvery + time.Duration(rand.Intn(5000))*time.Millisecond)
	}
}

func (m *Manager) PollAll(ctx context.Context) {
	nodes, err := m.Nodes(ctx)
	if err != nil {
		return
	}
	known := map[string]bool{}
	sem := make(chan struct{}, pollWorkers)
	var wg sync.WaitGroup
	for _, n := range nodes {
		known[n.Name] = true
		wg.Add(1)
		sem <- struct{}{}
		go func(n Node) {
			defer func() { <-sem; wg.Done() }()
			m.poll(ctx, n)
		}(n)
	}
	wg.Wait()
	m.inv.mu.Lock()
	for name := range m.inv.nodes {
		if !known[name] {
			delete(m.inv.nodes, name)
		}
	}
	m.inv.mu.Unlock()
}

func (m *Manager) Refresh(name string) {
	if n, err := m.Node(context.Background(), name); err == nil {
		m.poll(context.Background(), n)
	}
}

func (m *Manager) poll(ctx context.Context, n Node) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	m.inv.mu.RLock()
	st := m.inv.nodes[n.Name]
	m.inv.mu.RUnlock()
	st.Node = n
	info, err := n.Info(ctx)
	var vms []kube.VM
	if err == nil {
		vms, err = n.VMs(ctx)
	}
	if err != nil {
		st.Error = err.Error()
	} else {
		st.Info, st.VMs, st.Error, st.Observed = info, vms, "", time.Now()
		m.alignCPU(ctx, n, &st.Info)
		var sample kube.MetricsSample
		if n.Call(ctx, "GET", "/api/metrics", nil, &sample) == nil {
			m.tel.record(n.Name, info.CPUCores, sample)
		}
	}
	m.inv.mu.Lock()
	m.inv.nodes[n.Name] = st
	m.inv.mu.Unlock()
}

func (m *Manager) alignCPU(ctx context.Context, n Node, info *kube.NodeInfo) {
	if n.Datacenter == "" || n.Cluster == "" {
		return
	}
	cs, err := m.ClusterSettings(ctx, n.Org, n.Datacenter, n.Cluster)
	want := cs.CPUModel
	if err != nil || info.CPU.Baseline == want || want != "" && !slices.Contains(info.CPU.Models, want) {
		return
	}
	if n.Call(ctx, http.MethodPost, "/api/host/cpu-baseline", map[string]string{"model": want}, nil) == nil {
		info.CPU.Baseline = want
	}
}
