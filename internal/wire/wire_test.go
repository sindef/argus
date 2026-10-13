package wire

import (
	"os"
	"regexp"
	"testing"
)

func TestStateOfHostScript(t *testing.T) {
	out, err := os.ReadFile("../../os/rootfs/usr/sbin/argus-host")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]HostState{"Rebooting": HostBusy, "Shutting down": HostBusy, "Verifying ": HostBusy, "Updating to ": HostBusy,
		"Rebooting into ": HostBusy, "Cancelled: ": HostCancelled, "Update failed: invalid image reference": HostFailed, "Update failed: ": HostFailed, "Update failed: no verified digest": HostFailed, "Update failed, see log": HostFailed, "Unknown request": HostFailed}
	seen := 0
	for _, m := range regexp.MustCompile(`status "([^"$]*)`).FindAllStringSubmatch(string(out), -1) {
		w, ok := want[m[1]]
		if !ok {
			t.Errorf("argus-host writes %q; add it to the state table", m[1])
			continue
		}
		seen++
		if got := StateOf(m[1] + "x"); got != w {
			t.Errorf("StateOf(%q) = %s, want %s", m[1], got, w)
		}
	}
	if seen == 0 || StateOf("") != HostIdle {
		t.Error("no statuses parsed or empty status not idle")
	}
}
