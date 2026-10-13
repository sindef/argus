package manager

import "testing"

func TestCompileFirewall(t *testing.T) {
	vms := []fwVM{
		{host: "a", macs: []string{"02:00:00:00:00:01"}, tags: []string{"db"}, nets: []string{"lan"}},
		{host: "b", name: "w1", macs: []string{"02:00:00:00:00:02"}, tags: []string{"web"}},
		{host: "b", macs: []string{"02:00:00:00:00:03"}},
	}
	p := FirewallPolicy{DefaultDrop: true, Rules: []FirewallRule{
		{Name: "web to db", Source: "tag:web", Dest: "tag:db", Proto: "tcp", Ports: []int{5432}, Allow: true},
		{Name: "ghost", Source: "tag:none", Dest: "tag:db", Proto: "any", Allow: true},
		{Name: "admin net", Source: "cidr:10.9.0.0/16", Dest: "tag:db", Proto: "tcp", Ports: []int{22}, Allow: true},
		{Name: "w1 to lan", Source: "vm:b/w1", Dest: "net:lan", Proto: "tcp", Ports: []int{80}, Allow: true},
	}}
	out := compileFirewall(p, vms)
	if len(out["b"]) != 0 {
		t.Fatalf("untagged and unprotected VMs must be left alone: %+v", out["b"])
	}
	db := out["a"]
	if len(db) != 1 || !db[0].Drop || len(db[0].Rules) != 3 {
		t.Fatalf("db rules: %+v", db)
	}
	if r := db[0].Rules[0]; r.Any || len(r.SrcMACs) != 1 || r.SrcMACs[0] != "02:00:00:00:00:02" {
		t.Fatalf("web source not resolved to its MAC: %+v", r)
	}
	if r := db[0].Rules[2]; len(r.SrcMACs) != 1 || r.SrcMACs[0] != "02:00:00:00:00:02" {
		t.Fatalf("VM source or network destination not resolved: %+v", r)
	}
	for _, r := range db[0].Rules {
		if r.Any {
			t.Fatal("a rule with an empty source tag became an allow-all")
		}
	}
}
