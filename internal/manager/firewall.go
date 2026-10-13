package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
)

type FirewallRule struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Dest   string `json:"dest"`
	Proto  string `json:"proto"`
	Ports  []int  `json:"ports,omitempty"`
	Allow  bool   `json:"allow"`
}

type FirewallPolicy struct {
	DefaultDrop bool           `json:"defaultDrop"`
	Rules       []FirewallRule `json:"rules"`
}

func firewallKey(org string) string { return "argus-firewall-" + org }

func (c *Manager) Firewall(ctx context.Context, org string) (FirewallPolicy, error) {
	var p FirewallPolicy
	_, err := auth.LoadJSON(ctx, c.store, firewallKey(org), "policy", &p)
	return p, err
}

func validSelector(s string, cidr bool) error {
	switch {
	case s == "any":
		return nil
	case strings.HasPrefix(s, "tag:") && len(s) > 4:
		return nil
	case cidr && strings.HasPrefix(s, "cidr:"):
		if _, err := netip.ParsePrefix(strings.TrimPrefix(s, "cidr:")); err != nil {
			return fmt.Errorf("invalid CIDR in %q", s)
		}
		return nil
	}
	return fmt.Errorf("selector %q must be any, tag:NAME%s", s, map[bool]string{true: " or cidr:A.B.C.D/N"}[cidr])
}

func (c *Manager) SetFirewall(ctx context.Context, org string, p FirewallPolicy) error {
	for _, r := range p.Rules {
		if r.Name == "" {
			return errors.New("every rule needs a name")
		}
		if err := validSelector(r.Source, true); err != nil {
			return err
		}
		if err := validSelector(r.Dest, false); err != nil {
			return err
		}
		if !slices.Contains([]string{"any", "tcp", "udp", "icmp"}, r.Proto) {
			return fmt.Errorf("rule %s: protocol must be any, tcp, udp or icmp", r.Name)
		}
		for _, port := range r.Ports {
			if port < 1 || port > 65535 || r.Proto != "tcp" && r.Proto != "udp" {
				return fmt.Errorf("rule %s: ports need tcp or udp and must be 1-65535", r.Name)
			}
		}
	}
	return auth.PutJSON(ctx, c.store, firewallKey(org), "policy", p)
}

type fwVM struct {
	host string
	macs []string
	tags []string
}

func selects(sel string, tags []string) bool {
	return sel == "any" || strings.HasPrefix(sel, "tag:") && slices.Contains(tags, strings.TrimPrefix(sel, "tag:"))
}

func compileFirewall(p FirewallPolicy, vms []fwVM) map[string][]hostd.FWVM {
	out := map[string][]hostd.FWVM{}
	for _, v := range vms {
		var rules []hostd.FWRule
		for _, r := range p.Rules {
			if !selects(r.Dest, v.tags) {
				continue
			}
			fr := hostd.FWRule{Proto: r.Proto, Ports: r.Ports, Allow: r.Allow}
			switch {
			case r.Source == "any":
				fr.Any = true
			case strings.HasPrefix(r.Source, "cidr:"):
				fr.SrcCIDRs = []string{strings.TrimPrefix(r.Source, "cidr:")}
			default:
				for _, o := range vms {
					if selects(r.Source, o.tags) {
						fr.SrcMACs = append(fr.SrcMACs, o.macs...)
					}
				}
				if len(fr.SrcMACs) == 0 {
					continue
				}
			}
			rules = append(rules, fr)
		}
		if len(rules) == 0 && !slices.ContainsFunc(p.Rules, func(r FirewallRule) bool { return selects(r.Dest, v.tags) }) {
			continue
		}
		for _, mac := range v.macs {
			out[v.host] = append(out[v.host], hostd.FWVM{MAC: mac, Rules: rules, Drop: p.DefaultDrop})
		}
	}
	return out
}

func (c *Manager) alignFirewall(ctx context.Context, n Node) {
	p, err := c.Firewall(ctx, n.Org)
	if err != nil {
		return
	}
	var vms []fwVM
	for _, st := range c.Inventory(n.Org) {
		for _, v := range st.VMs {
			var macs []string
			for _, nic := range v.NICs {
				if nic.MAC != "" {
					macs = append(macs, strings.ToLower(nic.MAC))
				}
			}
			vms = append(vms, fwVM{host: st.Node.Name, macs: macs, tags: v.Tags})
		}
	}
	rules := compileFirewall(p, vms)[n.Name]
	if rules == nil {
		rules = []hostd.FWVM{}
	}
	b, _ := json.Marshal(rules)
	if st, ok := c.NodeState(n.Name); ok {
		b = append(b, st.Info.BootTime.String()...)
	}
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	c.st.mu.Lock()
	same := c.st.fwPushed[n.Name] == h || c.st.fwPushed[n.Name] == "" && len(rules) == 0
	c.st.mu.Unlock()
	if same {
		return
	}
	if n.Call(ctx, http.MethodPost, "/api/host/firewall", rules, nil) == nil {
		c.st.mu.Lock()
		c.st.fwPushed[n.Name] = h
		c.st.mu.Unlock()
	}
}
