package manager

import (
	"context"
	"math/rand"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/wire"
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
	Networks []kube.Network
	Images   []kube.Image
	HostNet  HostNetwork
	Error    string
	Observed time.Time
}

type HostNetwork struct {
	Net       hostd.HostNet `json:"net"`
	Migration string        `json:"migration"`
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

func (m *Manager) pollLoop(ctx context.Context) {
	for {
		m.PollAll(ctx)
		m.tel.save(false)
		select {
		case <-ctx.Done():
			m.tel.save(true)
			return
		case <-time.After(pollEvery + time.Duration(rand.Intn(5000))*time.Millisecond):
		}
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
		defer m.alignFirewall(ctx, n)
		var sample kube.MetricsSample
		if n.Call(ctx, "GET", "/api/metrics", nil, &sample) == nil {
			m.tel.record(n.Name, info.CPUCores, sample)
		}
		var nets []kube.Network
		if n.Call(ctx, "GET", "/api/networks", nil, &nets) == nil {
			st.Networks = nets
		}
		var imgs []kube.Image
		if n.Call(ctx, "GET", "/api/images", nil, &imgs) == nil {
			st.Images = imgs
		}
		var hn HostNetwork
		if n.Call(ctx, "GET", "/api/host/network", nil, &hn) == nil {
			st.HostNet = hn
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
	if n.Call(ctx, http.MethodPost, "/api/host/cpu-baseline", wire.CPUBaselineRequest{Model: want}, nil) == nil {
		info.CPU.Baseline = want
	}
}
