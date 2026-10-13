package hostd

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type FWRule struct {
	SrcMACs  []string `json:"srcMACs,omitempty"`
	SrcCIDRs []string `json:"srcCIDRs,omitempty"`
	Any      bool     `json:"any,omitempty"`
	Proto    string   `json:"proto,omitempty"`
	Ports    []int    `json:"ports,omitempty"`
	Allow    bool     `json:"allow"`
}

type FWVM struct {
	MAC   string   `json:"mac"`
	Rules []FWRule `json:"rules"`
	Drop  bool     `json:"drop"`
}

const fwTable = "argus_fw"

func set(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return "{ " + strings.Join(items, ", ") + " }"
}

func renderRule(r FWRule) ([]string, error) {
	proto := ""
	switch r.Proto {
	case "", "any":
	case "icmp":
		proto = "meta l4proto { icmp, ipv6-icmp }"
	case "tcp", "udp":
		proto = "meta l4proto " + r.Proto
		if len(r.Ports) > 0 {
			ps := make([]string, len(r.Ports))
			for i, p := range r.Ports {
				if p < 1 || p > 65535 {
					return nil, fmt.Errorf("invalid port %d", p)
				}
				ps[i] = strconv.Itoa(p)
			}
			proto += " th dport " + set(ps)
		}
	default:
		return nil, fmt.Errorf("invalid protocol %q", r.Proto)
	}
	verdict := "drop"
	if r.Allow {
		verdict = "accept"
	}
	var srcs []string
	if len(r.SrcMACs) > 0 {
		for _, m := range r.SrcMACs {
			if _, err := net.ParseMAC(m); err != nil {
				return nil, fmt.Errorf("invalid MAC %q", m)
			}
		}
		srcs = append(srcs, "ether saddr "+set(r.SrcMACs))
	}
	var v4, v6 []string
	for _, c := range r.SrcCIDRs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", c)
		}
		if p.Addr().Is4() {
			v4 = append(v4, p.String())
		} else {
			v6 = append(v6, p.String())
		}
	}
	if len(v4) > 0 {
		srcs = append(srcs, "ip saddr "+set(v4))
	}
	if len(v6) > 0 {
		srcs = append(srcs, "ip6 saddr "+set(v6))
	}
	if r.Any {
		srcs = []string{""}
	}
	var out []string
	for _, s := range srcs {
		out = append(out, strings.TrimSpace(strings.Join([]string{s, proto, verdict}, " ")))
	}
	return out, nil
}

func renderFirewall(vms []FWVM) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "table bridge %s {}\ndelete table bridge %s\ntable bridge %s {\n", fwTable, fwTable, fwTable)
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority 0; policy accept;\n")
	for i, v := range vms {
		if _, err := net.ParseMAC(v.MAC); err != nil {
			return "", fmt.Errorf("invalid MAC %q", v.MAC)
		}
		fmt.Fprintf(&b, "\t\tether daddr %s jump vm%d\n", strings.ToLower(v.MAC), i)
	}
	b.WriteString("\t}\n")
	for i, v := range vms {
		fmt.Fprintf(&b, "\tchain vm%d {\n\t\tct state established,related accept\n\t\tether type arp accept\n", i)
		for _, r := range v.Rules {
			lines, err := renderRule(r)
			if err != nil {
				return "", err
			}
			for _, l := range lines {
				fmt.Fprintf(&b, "\t\t%s\n", l)
			}
		}
		if v.Drop {
			b.WriteString("\t\tdrop\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

func applyFirewall(ctx context.Context, vms []FWVM) error {
	rules, err := renderFirewall(vms)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "argus-fw-*.nft")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(rules); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if out, err := exec.CommandContext(ctx, "nft", "-f", f.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
