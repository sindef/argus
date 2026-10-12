package netguard

import (
	"net/netip"
	"testing"
)

func TestAllowed(t *testing.T) {
	for ip, want := range map[string]bool{
		"192.168.1.10": true, "10.0.0.5": true, "203.0.113.7": true, "2001:db8::1": true,
		"127.0.0.1": false, "::1": false, "169.254.169.254": false, "fe80::1": false, "0.0.0.0": false,
		"10.42.0.12": false, "10.43.0.1": false, "::ffff:127.0.0.1": false, "::ffff:169.254.169.254": false, "224.0.0.1": false,
	} {
		if got := Allowed(netip.MustParseAddr(ip)); got != want {
			t.Errorf("%s: got %v, want %v", ip, got, want)
		}
	}
}
