package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"argus/internal/manager"
)

func (s *Server) firewallPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.manager.Firewall(r.Context(), session(r).Org)
	type row struct {
		manager.FirewallRule
		Fill string
	}
	var rows []row
	for i, rule := range p.Rules {
		ports := make([]string, len(rule.Ports))
		for j, n := range rule.Ports {
			ports[j] = strconv.Itoa(n)
		}
		rows = append(rows, row{rule, fill(map[string]any{"i": strconv.Itoa(i), "name": rule.Name, "source": rule.Source, "dest": rule.Dest, "proto": rule.Proto,
			"ports": strings.Join(ports, ", "), "action": map[bool]string{true: "allow", false: "deny"}[rule.Allow]})})
	}
	var tags []string
	for _, st := range s.manager.Inventory(session(r).Org) {
		for _, v := range st.VMs {
			for _, t := range v.Tags {
				if !slices.Contains(tags, t) {
					tags = append(tags, t)
				}
			}
		}
	}
	slices.Sort(tags)
	s.render(w, r, "firewall.html", "layout", map[string]any{"Nav": "firewall", "Policy": p, "Rows": rows, "Tags": tags, "Error": errText(err)})
}

func (s *Server) firewallAction(w http.ResponseWriter, r *http.Request) {
	org := session(r).Org
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
				Proto: r.FormValue("proto"), Allow: r.FormValue("action") == "allow"}
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
		err = s.manager.SetFirewall(r.Context(), org, p)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: firewall %s", session(r).User, r.PathValue("op"))
	go s.manager.PollAll(context.Background())
	w.Header().Set("HX-Refresh", "true")
}
