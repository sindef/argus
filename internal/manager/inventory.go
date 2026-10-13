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

func (c *Manager) Inventory(org string) []NodeState {
	c.inv.mu.RLock()
	defer c.inv.mu.RUnlock()
	var out []NodeState
	for _, s := range c.inv.nodes {
		if s.Node.Org == org {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node.Name < out[j].Node.Name })
	return out
}

func (c *Manager) NodeState(name string) (NodeState, bool) {
	c.inv.mu.RLock()
	defer c.inv.mu.RUnlock()
	s, ok := c.inv.nodes[name]
	return s, ok
}

func (c *Manager) pollLoop(ctx context.Context) {
	for {
		c.PollAll(ctx)
		c.tel.save(false)
		select {
		case <-ctx.Done():
			c.tel.save(true)
			return
		case <-time.After(pollEvery + time.Duration(rand.Intn(5000))*time.Millisecond):
		}
	}
}

func (c *Manager) PollAll(ctx context.Context) {
	nodes, err := c.Nodes(ctx)
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
			c.poll(ctx, n)
		}(n)
	}
	wg.Wait()
	c.inv.mu.Lock()
	for name := range c.inv.nodes {
		if !known[name] {
			delete(c.inv.nodes, name)
		}
	}
	c.inv.mu.Unlock()
}

func (c *Manager) Refresh(name string) {
	if n, err := c.Node(context.Background(), name); err == nil {
		c.poll(context.Background(), n)
	}
}

func (c *Manager) poll(ctx context.Context, n Node) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	c.inv.mu.RLock()
	st := c.inv.nodes[n.Name]
	c.inv.mu.RUnlock()
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
		c.alignCPU(ctx, n, &st.Info)
		defer c.alignFirewall(ctx, n)
		var sample kube.MetricsSample
		if n.Call(ctx, "GET", "/api/metrics", nil, &sample) == nil {
			c.tel.record(n.Name, info.CPUCores, sample)
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
	c.inv.mu.Lock()
	c.inv.nodes[n.Name] = st
	c.inv.mu.Unlock()
}

func (c *Manager) alignCPU(ctx context.Context, n Node, info *kube.NodeInfo) {
	if n.Datacenter == "" || n.Cluster == "" {
		return
	}
	cs, err := c.ClusterSettings(ctx, n.Org, n.Datacenter, n.Cluster)
	want := cs.CPUModel
	if err != nil || info.CPU.Baseline == want || want != "" && !slices.Contains(info.CPU.Models, want) {
		return
	}
	if n.Call(ctx, http.MethodPost, "/api/host/cpu-baseline", wire.CPUBaselineRequest{Model: want}, nil) == nil {
		info.CPU.Baseline = want
	}
}
