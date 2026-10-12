package hostd

import (
	"strings"
	"testing"
)

func TestRenderFirewall(t *testing.T) {
	out, err := renderFirewall([]FWVM{{MAC: "02:00:00:00:00:01", Drop: true, Rules: []FWRule{
		{SrcMACs: []string{"02:00:00:00:00:02"}, Proto: "tcp", Ports: []int{22, 443}, Allow: true},
		{SrcCIDRs: []string{"10.0.0.0/8", "fd00::/8"}, Proto: "icmp", Allow: true},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ether daddr 02:00:00:00:00:01 jump vm0",
		"ether saddr 02:00:00:00:00:02 meta l4proto tcp th dport { 22, 443 } accept",
		"ip saddr 10.0.0.0/8 meta l4proto { icmp, ipv6-icmp } accept",
		"ip6 saddr fd00::/8 meta l4proto { icmp, ipv6-icmp } accept",
		"\t\tdrop\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	empty, _ := renderFirewall([]FWVM{{MAC: "02:00:00:00:00:01", Rules: []FWRule{{Allow: true}}}})
	if strings.Contains(empty, "accept\n\t}") && !strings.Contains(empty, "arp accept\n\t}") {
		t.Error("rule with no sources matched everything")
	}
	if _, err := renderFirewall([]FWVM{{MAC: "02:00:00:00:00:01", Rules: []FWRule{{SrcCIDRs: []string{"10.0.0.0/8; flush ruleset"}}}}}); err == nil {
		t.Error("injected CIDR accepted")
	}
}
