package manager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"argus/internal/auth"
)

type AlarmRule struct {
	Name     string  `json:"name"`
	Metric   string  `json:"metric"`
	Warn     float64 `json:"warn"`
	Critical float64 `json:"critical"`
	Minutes  int     `json:"minutes"`
	Scope    string  `json:"scope,omitempty"`
	Tag      string  `json:"tag,omitempty"`
}

type RuleHit struct {
	Key, Severity, Host, Object, Message string
}

var AlarmMetrics = []struct{ ID, Label, Unit string }{
	{"host-cpu", "Host CPU", "%"},
	{"host-memory", "Host memory", "%"},
	{"host-pool", "Host storage", "%"},
	{"vm-cpu", "VM CPU", "%"},
	{"vm-memory", "VM memory", "%"},
	{"vm-disk", "VM disk I/O", "MB/s"},
	{"vm-net", "VM network", "Mbit/s"},
}

func alarmRulesKey(org string) string { return "argus-alarm-rules-" + org }

func (c *Manager) AlarmRules(ctx context.Context, org string) ([]AlarmRule, error) {
	var rs []AlarmRule
	_, err := auth.LoadJSON(ctx, c.store, alarmRulesKey(org), "rules", &rs)
	return rs, err
}

func (c *Manager) SaveAlarmRule(ctx context.Context, org, old string, r AlarmRule, remove bool) error {
	if !remove {
		switch {
		case r.Name == "" || len(r.Name) > 64:
			return errors.New("name is required")
		case !slices.ContainsFunc(AlarmMetrics, func(x struct{ ID, Label, Unit string }) bool { return x.ID == r.Metric }):
			return errors.New("unknown metric")
		case r.Warn <= 0 && r.Critical <= 0:
			return errors.New("set a warning or critical threshold")
		case r.Warn > 0 && r.Critical > 0 && r.Critical < r.Warn:
			return errors.New("critical must not be below warning")
		case r.Minutes < 0 || r.Minutes > 1440:
			return errors.New("duration must be 0-1440 minutes")
		}
	}
	var rs []AlarmRule
	rv, err := auth.LoadJSON(ctx, c.store, alarmRulesKey(org), "rules", &rs)
	if err != nil {
		return err
	}
	if !remove && old != r.Name && slices.ContainsFunc(rs, func(x AlarmRule) bool { return x.Name == r.Name }) {
		return fmt.Errorf("alarm %s already exists", r.Name)
	}
	rs = slices.DeleteFunc(rs, func(x AlarmRule) bool { return x.Name == old || x.Name == r.Name })
	if !remove {
		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return auth.SaveJSON(ctx, c.store, alarmRulesKey(org), "rules", rv, rs)
}

func window(ps []Point, minutes int, now time.Time) []Point {
	if len(ps) == 0 || now.Unix()-ps[len(ps)-1].T > 15*60 {
		return nil
	}
	cut := now.Add(-time.Duration(minutes) * time.Minute).Unix()
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= cut })
	return ps[min(i, len(ps)-1):]
}

func (r AlarmRule) judge(ps []Point, value func(Point) (float64, bool)) (string, float64) {
	low := 0.0
	for i, p := range ps {
		v, ok := value(p)
		if !ok {
			return "", 0
		}
		if i == 0 || v < low {
			low = v
		}
	}
	switch {
	case len(ps) == 0:
		return "", 0
	case r.Critical > 0 && low >= r.Critical:
		return "critical", low
	case r.Warn > 0 && low >= r.Warn:
		return "warning", low
	}
	return "", 0
}

func (c *Manager) EvaluateAlarmRules(ctx context.Context) []RuleHit {
	c.inv.mu.RLock()
	states := make([]NodeState, 0, len(c.inv.nodes))
	for _, st := range c.inv.nodes {
		states = append(states, st)
	}
	c.inv.mu.RUnlock()
	rules := map[string][]AlarmRule{}
	for _, st := range states {
		if _, ok := rules[st.Node.Org]; !ok {
			rules[st.Node.Org], _ = c.AlarmRules(ctx, st.Node.Org)
		}
	}
	var out []RuleHit
	now := time.Now()
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	for _, st := range states {
		n := st.Node
		for _, r := range rules[n.Org] {
			if r.Scope != "" && !auth.ScopeCovers(r.Scope, auth.Object{Datacenter: n.Datacenter, Cluster: n.Cluster, Host: n.Name}) {
				continue
			}
			metric := AlarmMetrics[slices.IndexFunc(AlarmMetrics, func(x struct{ ID, Label, Unit string }) bool { return x.ID == r.Metric })]
			hit := func(key, object string, ps []Point, value func(Point) (float64, bool)) {
				if sev, v := r.judge(window(ps, r.Minutes, now), value); sev != "" {
					out = append(out, RuleHit{Key: "rule:" + r.Name + ":" + key, Severity: sev, Host: n.Name, Object: object, Message: fmt.Sprintf("%s %.0f %s (alarm %s)", metric.Label, v, metric.Unit, r.Name)})
				}
			}
			switch r.Metric {
			case "host-cpu":
				hit(n.Name, "host", c.tel.Hosts[n.Name], func(p Point) (float64, bool) { return float64(p.V[0]), true })
			case "host-memory":
				hit(n.Name, "host", c.tel.Hosts[n.Name], func(p Point) (float64, bool) { return 100 * float64(p.V[1]) / float64(p.V[2]), p.V[2] > 0 })
			case "host-pool":
				hit(n.Name, "host", c.tel.Hosts[n.Name], func(p Point) (float64, bool) {
					return 100 * float64(p.V[4]) / float64(p.V[4]+p.V[5]), p.V[4]+p.V[5] > 0
				})
			default:
				for _, vm := range st.VMs {
					if !vm.Running || r.Tag != "" && !slices.Contains(vm.Tags, r.Tag) {
						continue
					}
					ps := c.tel.VMs[n.Name+"/"+vm.Namespace+"/"+vm.Name]
					mem, _ := resource.ParseQuantity(vm.Memory)
					key, object := n.Name+"/"+vm.Name, "VM "+vm.Label()
					switch r.Metric {
					case "vm-cpu":
						hit(key, object, ps, func(p Point) (float64, bool) { return 100 * float64(p.V[0]) / float64(vm.Cores), vm.Cores > 0 })
					case "vm-memory":
						hit(key, object, ps, func(p Point) (float64, bool) { return 100 * float64(p.V[1]) / float64(mem.Value()), mem.Value() > 0 })
					case "vm-disk":
						hit(key, object, ps, func(p Point) (float64, bool) { return float64(p.V[2]+p.V[3]) / 1e6, true })
					case "vm-net":
						hit(key, object, ps, func(p Point) (float64, bool) { return float64(p.V[4]+p.V[5]) * 8 / 1e6, true })
					}
				}
			}
		}
	}
	return out
}
