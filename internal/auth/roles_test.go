package auth

import "testing"

func TestAllows(t *testing.T) {
	web := Object{Datacenter: "Home", Cluster: "Lab", Host: "n1", VM: "web"}
	db := Object{Datacenter: "Home", Cluster: "Lab", Host: "n1", VM: "db"}
	other := Object{Datacenter: "Away", Cluster: "Edge", Host: "n9", VM: "web"}
	host := Object{Datacenter: "Home", Cluster: "Lab", Host: "n1"}
	gs := []Grant{
		{[]Privilege{VMView, VMPower}, []string{"vm:n1/web"}},
		{[]Privilege{VMView}, []string{"Home/Lab"}},
	}
	cases := []struct {
		p    Privilege
		o    Object
		want bool
	}{
		{VMPower, web, true}, {VMPower, db, false}, {VMView, db, true}, {VMView, other, false},
		{VMDelete, web, false}, {VMPower, host, false}, {HostView, host, false},
	}
	for _, c := range cases {
		if got := Allows(gs, c.p, c.o); got != c.want {
			t.Errorf("%s on %+v = %v, want %v", c.p, c.o, got, c.want)
		}
	}
	if !ReachesHost(gs, VMPower, host) || ReachesHost(gs, VMPower, Object{Host: "n9"}) {
		t.Error("VM-scoped grant must reach only its own host")
	}
	if !Allows([]Grant{{[]Privilege{VMDelete}, nil}}, VMDelete, other) {
		t.Error("unscoped grant must cover everything")
	}
}
