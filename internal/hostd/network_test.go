package hostd

import (
	"strings"
	"testing"
)

func TestUnitKeepsAddressAndMTU(t *testing.T) {
	addr, err := addressNetwork(AddressSpec{Iface: "br0", IPv4: "static", Address: "10.0.0.5/24", IPv6: "off"}, true)
	if err != nil {
		t.Fatal(err)
	}
	withLink := withMTU(addr, 9000)
	if linkMTU(withLink) != 9000 || !strings.Contains(withLink, "Address=10.0.0.5/24") {
		t.Fatalf("lost settings:\n%s", withLink)
	}
	if again := withMTU(withLink, 1500); linkMTU(again) != 1500 || strings.Count(again, "[Link]") != 1 || !strings.Contains(again, "Address=10.0.0.5/24") {
		t.Fatalf("MTU change lost settings:\n%s", again)
	}
	if strings.Contains(withMTU(withLink, 0), "[Link]") {
		t.Error("clearing the MTU kept a Link section")
	}
}
