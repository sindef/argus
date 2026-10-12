package manager

import "testing"

const gib = 1 << 30

func host(name string, cpu, memUsed float64, vms ...drsVM) drsHost {
	return drsHost{Name: name, CPU: cpu, MemUsed: memUsed * gib, MemTotal: 64 * gib, Cores: 16, VMs: vms}
}

func vm(name string, cores, memGiB float64) drsVM {
	return drsVM{Name: name, CPU: cores, Mem: memGiB * gib, Movable: true}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name  string
		cs    ClusterSettings
		hosts []drsHost
		want  drsMove
	}{
		{"balanced", ClusterSettings{}, []drsHost{host("a", 0.3, 20, vm("x", 2, 8)), host("b", 0.25, 18)}, drsMove{}},
		{"imbalance moves best fit", ClusterSettings{}, []drsHost{host("a", 0.9, 30, vm("x", 1, 4), vm("y", 6, 8)), host("b", 0.1, 10)}, drsMove{VM: "y", From: "a", To: "b"}},
		{"target memory full", ClusterSettings{}, []drsHost{host("a", 0.9, 30, vm("y", 6, 8)), host("b", 0.1, 55)}, drsMove{}},
		{"pinned host rule blocks", ClusterSettings{Rules: []Rule{{Name: "p", Kind: "hosts", VMs: []string{"y"}, Hosts: []string{"a"}}}},
			[]drsHost{host("a", 0.9, 30, vm("y", 6, 8)), host("b", 0.1, 10)}, drsMove{}},
		{"hosts rule violation", ClusterSettings{Rules: []Rule{{Name: "p", Kind: "hosts", VMs: []string{"y"}, Hosts: []string{"b"}}}},
			[]drsHost{host("a", 0.2, 10, vm("y", 1, 2)), host("b", 0.2, 10), host("c", 0.1, 5)}, drsMove{VM: "y", From: "a", To: "b"}},
		{"apart violation", ClusterSettings{Rules: []Rule{{Name: "ha", Kind: "apart", VMs: []string{"x", "y"}}}},
			[]drsHost{host("a", 0.2, 10, vm("x", 1, 2), vm("y", 1, 2)), host("b", 0.2, 10)}, drsMove{VM: "y", From: "a", To: "b"}},
		{"together violation", ClusterSettings{Rules: []Rule{{Name: "app", Kind: "together", VMs: []string{"x", "y", "z"}}}},
			[]drsHost{host("a", 0.2, 10, vm("x", 1, 2), vm("y", 1, 2)), host("b", 0.2, 10, vm("z", 1, 2))}, drsMove{VM: "z", From: "b", To: "a"}},
		{"apart blocks balancing", ClusterSettings{Rules: []Rule{{Name: "ha", Kind: "apart", VMs: []string{"y", "w"}}}},
			[]drsHost{host("a", 0.9, 30, vm("y", 6, 8)), host("b", 0.1, 10, vm("w", 0, 1))}, drsMove{}},
		{"unmovable stays", ClusterSettings{}, []drsHost{host("a", 0.9, 30, drsVM{Name: "y", CPU: 6, Mem: 8 * gib}), host("b", 0.1, 10)}, drsMove{}},
	}
	for _, c := range cases {
		got, _ := plan(c.cs, c.hosts)
		got.Reason = ""
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
