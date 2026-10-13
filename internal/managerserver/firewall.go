package managerserver

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"argus/internal/auth"
	"argus/internal/manager"
	"argus/internal/ui"
)

type fwChoice struct{ Value, Label string }

type fwChoices struct {
	VMs, Tags, Networks, Hosts []fwChoice
}

func (s *Server) firewallChoices(r *http.Request) fwChoices {
	var c fwChoices
	add := func(list *[]fwChoice, value, label string) {
		if !slices.ContainsFunc(*list, func(x fwChoice) bool { return x.Value == value }) {
			*list = append(*list, fwChoice{value, label})
		}
	}
	for _, st := range s.manager.Inventory(ui.Session(r).Org) {
		add(&c.Hosts, st.Node.Name, st.Node.Name)
		for _, n := range st.Networks {
			add(&c.Networks, n.Name, n.Name)
		}
		for _, v := range st.VMs {
			add(&c.VMs, st.Node.Name+"/"+v.Name, v.Label()+" ("+st.Node.Name+")")
			for _, t := range v.Tags {
				add(&c.Tags, t, t)
			}
			for _, n := range v.NICs {
				add(&c.Networks, n.Network, n.Network)
			}
		}
	}
	if defs, err := s.manager.Networks(r.Context(), ui.Session(r).Org); err == nil {
		for _, d := range defs {
			add(&c.Networks, d.Name, d.Name)
		}
	}
	for _, l := range []*[]fwChoice{&c.VMs, &c.Tags, &c.Networks, &c.Hosts} {
		slices.SortFunc(*l, func(a, b fwChoice) int { return strings.Compare(a.Label, b.Label) })
	}
	return c
}

func (c fwChoices) label(sel string) string {
	kind, val, _ := strings.Cut(sel, ":")
	find := func(list []fwChoice) string {
		if i := slices.IndexFunc(list, func(x fwChoice) bool { return x.Value == val }); i >= 0 {
			return list[i].Label
		}
		return val
	}
	switch kind {
	case "any":
		return "Any VM"
	case "vm":
		return find(c.VMs)
	case "tag":
		return "VMs tagged " + val
	case "net":
		return "VMs on " + val
	case "host":
		return "VMs on host " + val
	case "cidr":
		return val
	}
	return sel
}

func (s *Server) firewallPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.manager.Firewall(r.Context(), ui.Session(r).Org)
	choices := s.firewallChoices(r)
	type row struct {
		manager.FirewallRule
		Fill, SourceLabel, DestLabel string
	}
	var rows []row
	for i, rule := range p.Rules {
		ports := make([]string, len(rule.Ports))
		for j, n := range rule.Ports {
			ports[j] = strconv.Itoa(n)
		}
		rows = append(rows, row{rule, ui.Fill(map[string]any{"i": strconv.Itoa(i), "name": rule.Name, "source": rule.Source, "dest": rule.Dest, "proto": rule.Proto,
			"ports": strings.Join(ports, ", "), "verdict": map[bool]string{true: "allow", false: "deny"}[rule.Allow]}), choices.label(rule.Source), choices.label(rule.Dest)})
	}
	s.Render(w, r, "firewall.html", "layout", map[string]any{"Nav": "firewall", "Policy": p, "Rows": rows, "Choices": choices, "Error": ui.ErrText(err)})
}

func (s *Server) firewallAction(w http.ResponseWriter, r *http.Request) {
	org := ui.Session(r).Org
	p, err := s.manager.Firewall(r.Context(), org)
	i, ierr := strconv.Atoi(r.FormValue("i"))
	if err == nil && r.PathValue("op") != "default" && r.PathValue("op") != "save" && (ierr != nil || i < 0 || i >= len(p.Rules)) {
		err = errors.New("unknown rule")
	}
	if err == nil {
		switch r.PathValue("op") {
		case "default":
			p.DefaultDrop = r.FormValue("drop") != ""
		case "save":
			rule := manager.FirewallRule{Name: strings.TrimSpace(r.FormValue("name")), Source: strings.TrimSpace(r.FormValue("source")), Dest: strings.TrimSpace(r.FormValue("dest")),
				Proto: r.FormValue("proto"), Allow: r.FormValue("verdict") == "allow"}
			for _, f := range strings.FieldsFunc(r.FormValue("ports"), func(c rune) bool { return c == ',' || c == ' ' }) {
				n, perr := strconv.Atoi(f)
				if perr != nil {
					err = errors.New("ports must be numbers")
				}
				rule.Ports = append(rule.Ports, n)
			}
			if ierr == nil && i >= 0 && i < len(p.Rules) {
				p.Rules[i] = rule
			} else {
				p.Rules = append(p.Rules, rule)
			}
		case "delete":
			p.Rules = slices.Delete(p.Rules, i, i+1)
		case "move":
			j := i - 1
			if r.FormValue("dir") == "down" {
				j = i + 1
			}
			if j >= 0 && j < len(p.Rules) {
				p.Rules[i], p.Rules[j] = p.Rules[j], p.Rules[i]
			}
		default:
			err = errors.New("unknown operation")
		}
	}
	if err == nil {
		err = s.permit(r, auth.NetworkConfig, auth.Object{})
	}
	if err == nil {
		err = s.manager.SetFirewall(r.Context(), org, p)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	go s.manager.PollAll(context.Background())
	w.Header().Set("HX-Refresh", "true")
}
