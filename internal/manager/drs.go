package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"time"

	"argus/internal/kube"
)

const (
	drsEvery    = 5 * time.Minute
	drsHeadroom = 0.9
	drsGain     = 0.05
)

var drsTolerance = map[int]float64{1: 0.4, 2: 0.3, 3: 0.2, 4: 0.15, 5: 0.1}

type Rule struct {
	Name  string   `json:"name"`
	Kind  string   `json:"kind"`
	VMs   []string `json:"vms"`
	Hosts []string `json:"hosts,omitempty"`
}

var RuleKinds = map[string]string{"together": "Keep together", "apart": "Keep apart", "hosts": "Run on hosts"}

type Recommendation struct {
	ID      string
	Org     string
	DC      string
	Cluster string
	VM      string
	From    string
	To      string
	Reason  string
	Created time.Time
}

type drsVM struct {
	Name    string
	CPU     float64
	Mem     float64
	Movable bool
}

type drsHost struct {
	Name     string
	CPU      float64
	MemUsed  float64
	MemTotal float64
	Cores    float64
	VMs      []drsVM
}

type drsMove struct {
	VM, From, To, Reason string
}

func (h drsHost) load() float64 {
	if h.MemTotal <= 0 {
		return h.CPU
	}
	return max(h.CPU, h.MemUsed/h.MemTotal)
}

func (h drsHost) fits(v drsVM) bool {
	return h.MemUsed+v.Mem <= drsHeadroom*h.MemTotal
}

func (h drsHost) has(vm string) bool {
	return slices.ContainsFunc(h.VMs, func(v drsVM) bool { return v.Name == vm })
}

func allowed(rules []Rule, vm, host string) bool {
	for _, r := range rules {
		if r.Kind == "hosts" && slices.Contains(r.VMs, vm) && !slices.Contains(r.Hosts, host) {
			return false
		}
	}
	return true
}

func separated(rules []Rule, vm string, h drsHost) bool {
	for _, r := range rules {
		if r.Kind == "apart" && slices.Contains(r.VMs, vm) && slices.ContainsFunc(r.VMs, func(o string) bool { return o != vm && h.has(o) }) {
			return false
		}
	}
	return true
}

func grouped(rules []Rule, vm string) bool {
	return slices.ContainsFunc(rules, func(r Rule) bool { return r.Kind == "together" && slices.Contains(r.VMs, vm) })
}

func byLoad(hosts []drsHost) []drsHost {
	out := slices.Clone(hosts)
	sort.SliceStable(out, func(i, j int) bool { return out[i].load() < out[j].load() })
	return out
}

func target(hosts []drsHost, rules []Rule, v drsVM, from string, ok func(drsHost) bool) (drsHost, bool) {
	for _, h := range byLoad(hosts) {
		if h.Name != from && h.fits(v) && allowed(rules, v.Name, h.Name) && separated(rules, v.Name, h) && ok(h) {
			return h, true
		}
	}
	return drsHost{}, false
}

func ruleMove(hosts []drsHost, rules []Rule) (drsMove, bool) {
	anyHost := func(drsHost) bool { return true }
	for _, r := range rules {
		switch r.Kind {
		case "hosts":
			for _, h := range hosts {
				for _, v := range h.VMs {
					if v.Movable && slices.Contains(r.VMs, v.Name) && !slices.Contains(r.Hosts, h.Name) {
						if t, ok := target(hosts, rules, v, h.Name, anyHost); ok {
							return drsMove{v.Name, h.Name, t.Name, "rule " + r.Name + ": run on " + fmt.Sprint(r.Hosts)}, true
						}
					}
				}
			}
		case "apart":
			for _, h := range hosts {
				var members []drsVM
				for _, v := range h.VMs {
					if slices.Contains(r.VMs, v.Name) {
						members = append(members, v)
					}
				}
				if len(members) < 2 {
					continue
				}
				for _, v := range members[1:] {
					if !v.Movable {
						continue
					}
					if t, ok := target(hosts, rules, v, h.Name, anyHost); ok {
						return drsMove{v.Name, h.Name, t.Name, "rule " + r.Name + ": keep apart"}, true
					}
				}
			}
		case "together":
			count := map[string]int{}
			for _, h := range hosts {
				for _, v := range h.VMs {
					if slices.Contains(r.VMs, v.Name) {
						count[h.Name]++
					}
				}
			}
			if len(count) < 2 {
				continue
			}
			home := ""
			for _, h := range byLoad(hosts) {
				if count[h.Name] > count[home] {
					home = h.Name
				}
			}
			for _, h := range hosts {
				if h.Name == home {
					continue
				}
				for _, v := range h.VMs {
					if v.Movable && slices.Contains(r.VMs, v.Name) {
						if t, ok := target(hosts, rules, v, h.Name, func(t drsHost) bool { return t.Name == home }); ok {
							return drsMove{v.Name, h.Name, t.Name, "rule " + r.Name + ": keep together"}, true
						}
					}
				}
			}
		}
	}
	return drsMove{}, false
}

func spread(hosts []drsHost) float64 {
	if len(hosts) < 2 {
		return 0
	}
	s := byLoad(hosts)
	return s[len(s)-1].load() - s[0].load()
}

func moved(hosts []drsHost, v drsVM, from, to string) []drsHost {
	out := slices.Clone(hosts)
	for i := range out {
		switch out[i].Name {
		case from:
			out[i].CPU -= v.CPU / max(out[i].Cores, 1)
			out[i].MemUsed -= v.Mem
		case to:
			out[i].CPU += v.CPU / max(out[i].Cores, 1)
			out[i].MemUsed += v.Mem
		}
	}
	return out
}

func plan(cs ClusterSettings, hosts []drsHost) (drsMove, bool) {
	if len(hosts) < 2 {
		return drsMove{}, false
	}
	if m, ok := ruleMove(hosts, cs.Rules); ok {
		return m, true
	}
	tol, ok := drsTolerance[cs.Threshold]
	if !ok {
		tol = drsTolerance[3]
	}
	before := spread(hosts)
	if before <= tol {
		return drsMove{}, false
	}
	s := byLoad(hosts)
	hi, lo := s[len(s)-1], s[0]
	best, bestSpread := drsMove{}, before-drsGain
	for _, v := range hi.VMs {
		if !v.Movable || grouped(cs.Rules, v.Name) || !lo.fits(v) || !allowed(cs.Rules, v.Name, lo.Name) || !separated(cs.Rules, v.Name, lo) {
			continue
		}
		if after := spread(moved(hosts, v, hi.Name, lo.Name)); after < bestSpread {
			best, bestSpread = drsMove{v.Name, hi.Name, lo.Name, fmt.Sprintf("balance load %.0f%% → %.0f%% spread", before*100, after*100)}, after
		}
	}
	return best, best.VM != ""
}

func (c *Manager) drsHosts(org, dc, cl string, protected []Protected) ([]drsHost, bool) {
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	var hosts []drsHost
	for _, st := range c.Inventory(org) {
		n := st.Node
		if n.Datacenter != dc || n.Cluster != cl || n.Maintenance || !st.Reachable() {
			continue
		}
		ps := c.tel.Hosts[n.Name]
		if len(ps) == 0 {
			return nil, false
		}
		p := ps[len(ps)-1]
		h := drsHost{Name: n.Name, CPU: float64(p.V[0]) / 100, MemUsed: float64(p.V[1]), MemTotal: float64(p.V[2]), Cores: float64(p.V[3])}
		for _, v := range st.VMs {
			if !v.Running {
				continue
			}
			var cpu float64
			if vs := c.tel.VMs[n.Name+"/"+v.Namespace+"/"+v.Name]; len(vs) > 0 {
				recent := vs[max(0, len(vs)-10):]
				for _, q := range recent {
					cpu += float64(q.V[0])
				}
				cpu /= float64(len(recent))
			}
			ha := slices.ContainsFunc(protected, func(p Protected) bool { return p.VM == v.Name })
			h.VMs = append(h.VMs, drsVM{Name: v.Name, CPU: cpu, Mem: float64(memBytes(v.Memory)), Movable: !v.Template && !ha && !c.Busy(n.Name, v.Name)})
		}
		hosts = append(hosts, h)
	}
	return hosts, true
}

func (c *Manager) Recommendations(org, dc, cl string) []Recommendation {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	var out []Recommendation
	for _, r := range c.st.drs {
		if r.Org == org && r.DC == dc && r.Cluster == cl {
			out = append(out, r)
		}
	}
	return out
}

func (c *Manager) Recommendation(org, id string) (Recommendation, bool) {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	i := slices.IndexFunc(c.st.drs, func(r Recommendation) bool { return r.ID == id && r.Org == org })
	if i < 0 {
		return Recommendation{}, false
	}
	return c.st.drs[i], true
}

func (c *Manager) ApplyRecommendation(ctx context.Context, org, id string) error {
	c.st.mu.Lock()
	i := slices.IndexFunc(c.st.drs, func(r Recommendation) bool { return r.ID == id && r.Org == org })
	var r Recommendation
	if i >= 0 {
		r = c.st.drs[i]
		c.st.drs = slices.Delete(c.st.drs, i, i+1)
	}
	c.st.mu.Unlock()
	if i < 0 {
		return errors.New("the recommendation is no longer current")
	}
	return c.drsMigrate(ctx, r)
}

func (c *Manager) drsMigrate(ctx context.Context, r Recommendation) error {
	src, err := c.NodeIn(ctx, r.Org, r.From)
	if err != nil {
		return err
	}
	dst, err := c.NodeIn(ctx, r.Org, r.To)
	if err != nil {
		return err
	}
	log.Printf("drs: %s/%s: move %s from %s to %s (%s)", r.DC, r.Cluster, r.VM, r.From, r.To, r.Reason)
	return c.LiveMigrate(ctx, src, dst, kube.Namespace, r.VM)
}

func (c *Manager) clusterBusy(org, dc, cl string) bool {
	hosts := map[string]bool{}
	for _, st := range c.Inventory(org) {
		if st.Node.Datacenter == dc && st.Node.Cluster == cl {
			hosts[st.Node.Name] = true
		}
	}
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	return slices.ContainsFunc(c.st.jobs, func(j *Job) bool { return !j.Done && (hosts[j.From] || hosts[j.To]) })
}

func (c *Manager) drsTick(ctx context.Context) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return
	}
	protected, _ := c.Protected(ctx)
	seen := map[[3]string]bool{}
	var recs []Recommendation
	for _, n := range nodes {
		key := [3]string{n.Org, n.Datacenter, n.Cluster}
		if n.Cluster == "" || seen[key] {
			continue
		}
		seen[key] = true
		cs, err := c.ClusterSettings(ctx, n.Org, n.Datacenter, n.Cluster)
		if err != nil || cs.DRS == "" || c.clusterBusy(n.Org, n.Datacenter, n.Cluster) {
			continue
		}
		hosts, ok := c.drsHosts(n.Org, n.Datacenter, n.Cluster, protected)
		if !ok {
			continue
		}
		m, ok := plan(cs, hosts)
		if !ok {
			continue
		}
		b := make([]byte, 8)
		rand.Read(b)
		r := Recommendation{ID: hex.EncodeToString(b), Org: n.Org, DC: n.Datacenter, Cluster: n.Cluster, VM: m.VM, From: m.From, To: m.To, Reason: m.Reason, Created: time.Now()}
		if cs.DRS == "auto" {
			if err := c.drsMigrate(ctx, r); err != nil {
				log.Printf("drs: %s: %v", r.VM, err)
			}
			continue
		}
		recs = append(recs, r)
	}
	c.st.mu.Lock()
	c.st.drs = recs
	c.st.mu.Unlock()
}
