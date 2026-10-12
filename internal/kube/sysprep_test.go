package kube

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestUnattend(t *testing.T) {
	g := WindowsGuest{ComputerName: "web01", AdminPassword: `p<&>"w`, Domain: "corp.example", DomainUser: "joiner", DomainPassword: "j", Commands: []string{`cmd /c echo a&b`}}
	u := g.Unattend()
	var v struct{}
	if err := xml.Unmarshal([]byte(u), &v); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, u)
	}
	for _, want := range []string{"<ComputerName>web01</ComputerName>", "<Value>p&lt;&amp;&gt;&#34;w</Value>", "<JoinDomain>corp.example</JoinDomain>", "<Path>cmd /c echo a&amp;b</Path>", "<TimeZone>UTC</TimeZone>"} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q", want)
		}
	}
	if (WindowsGuest{ComputerName: "this-name-is-too-long", AdminPassword: "x"}).Validate() == nil {
		t.Error("16-character computer name accepted")
	}
}
