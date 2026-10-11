package server

import (
	"testing"

	"argus/internal/auth"
)

func TestHostRoutePolicy(t *testing.T) {
	cases := []struct {
		method, path string
		role         auth.Role
		known        bool
	}{
		{"GET", "/dashboard", auth.ReadOnly, true},
		{"GET", "/vms/default/web/disks/root/download", auth.Operator, true},
		{"POST", "/vms/default/web/hardware/save", auth.Operator, true},
		{"POST", "/pools/fast/destroy", auth.Admin, true},
		{"GET", "/api/node", "", false},
		{"POST", "/dashboard", "", false},
	}
	for _, c := range cases {
		role, ok := hostRouteRole(c.method, c.path)
		if ok != c.known || role != c.role {
			t.Errorf("%s %s: got %q %v, want %q %v", c.method, c.path, role, ok, c.role, c.known)
		}
	}
	if auth.ReadOnly.Can(auth.Operator) {
		t.Fatal("read-only must not satisfy operator routes")
	}
}
