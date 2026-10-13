package managerserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/wire"
)

type complianceRow struct {
	Node  manager.Node
	Diffs []string
	Error string
}

func (s *Server) clusterMembers(r *http.Request, dc, cl string) []manager.NodeState {
	var out []manager.NodeState
	for _, st := range s.inventory(r) {
		if inGroup(st.Node, dc, cl) {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node.Name < out[j].Node.Name })
	return out
}

func profileDiffs(p manager.Profile, st wire.HostSettings, nets []kube.Network, defs []manager.Network) []string {
	var d []string
	if len(p.DNS) > 0 && !slices.Equal(p.DNS, st.DNS) {
		d = append(d, fmt.Sprintf("DNS is %q", strings.Join(st.DNS, ", ")))
	}
	if len(p.NTP) > 0 && !slices.Equal(p.NTP, st.NTP) {
		d = append(d, fmt.Sprintf("NTP is %q", strings.Join(st.NTP, ", ")))
	}
	if p.Syslog != "" && p.Syslog != st.Syslog {
		d = append(d, fmt.Sprintf("syslog is %q", st.Syslog))
	}
	if p.Backup != "" && p.Backup != st.Backup {
		d = append(d, fmt.Sprintf("backup target is %q", st.Backup))
	}
	if p.MigrationBridge != "" && p.MigrationBridge != st.MigrationBridge {
		d = append(d, "migration bridge is "+st.MigrationBridge)
	}
	if p.Networks {
		for _, def := range defs {
			i := slices.IndexFunc(nets, func(n kube.Network) bool { return n.Name == def.Name })
			switch {
			case i < 0:
				d = append(d, "network "+def.Name+" missing")
			case nets[i].VLAN != def.VLAN || nets[i].Bridge != def.BridgeName():
				d = append(d, "network "+def.Name+" differs")
			}
		}
	}
	return d
}

func (s *Server) compliance(r *http.Request, p manager.Profile, dc, cl string) []complianceRow {
	defs, _ := s.manager.Networks(r.Context(), ui.Session(r).Org)
	members := s.clusterMembers(r, dc, cl)
	rows := make([]complianceRow, len(members))
	var wg sync.WaitGroup
	for i, st := range members {
		rows[i].Node = st.Node
		wg.Add(1)
		go func(row *complianceRow) {
			defer wg.Done()
			var hs wire.HostSettings
			var nets []kube.Network
			err := row.Node.Call(r.Context(), http.MethodGet, "/api/host/settings", nil, &hs)
			if err == nil {
				err = row.Node.Call(r.Context(), http.MethodGet, "/api/networks", nil, &nets)
			}
			if err != nil {
				row.Error = err.Error()
				return
			}
			row.Diffs = profileDiffs(p, hs, nets, networksFor(defs, row.Node))
		}(&rows[i])
	}
	wg.Wait()
	return rows
}

func (s *Server) profilePage(w http.ResponseWriter, r *http.Request) {
	dc, cl := r.FormValue("dc"), r.FormValue("cluster")
	p, err := s.manager.Profile(r.Context(), ui.Session(r).Org, dc, cl)
	s.Render(w, r, "profile.html", "layout", map[string]any{"Nav": "profile", "DC": dc, "Cluster": cl, "Profile": p, "Rows": s.compliance(r, p, dc, cl), "Error": ui.ErrText(err)})
}

func (s *Server) profileSave(w http.ResponseWriter, r *http.Request) {
	p := manager.Profile{DNS: ui.Fields(r.FormValue("dns")), NTP: ui.Fields(r.FormValue("ntp")), Syslog: strings.TrimSpace(r.FormValue("syslog")), MigrationBridge: strings.TrimSpace(r.FormValue("migration_bridge")), Backup: strings.TrimSpace(r.FormValue("backup")), Networks: r.FormValue("networks") != ""}
	if err := s.manager.SetProfile(r.Context(), ui.Session(r).Org, r.FormValue("dc"), r.FormValue("cluster"), p); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: host profile for %s/%s saved", ui.Session(r).User, r.FormValue("dc"), r.FormValue("cluster"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) profileApply(w http.ResponseWriter, r *http.Request) {
	dc, cl := r.FormValue("dc"), r.FormValue("cluster")
	p, err := s.manager.Profile(r.Context(), ui.Session(r).Org, dc, cl)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	defs, _ := s.manager.Networks(r.Context(), ui.Session(r).Org)
	u := wire.HostSettingsUpdate{}
	if len(p.DNS) > 0 {
		u.DNS = &p.DNS
	}
	if len(p.NTP) > 0 {
		u.NTP = &p.NTP
	}
	if p.Syslog != "" {
		u.Syslog = &p.Syslog
	}
	if p.MigrationBridge != "" {
		u.MigrationBridge = &p.MigrationBridge
	}
	if p.Backup != "" {
		u.Backup = &p.Backup
	}
	members := s.clusterMembers(r, dc, cl)
	if len(members) == 0 {
		ui.Flash(w, r, errors.New("no hosts in this cluster"))
		return
	}
	errs := fanOut(r.Context(), members, func(ctx context.Context, n manager.Node) error {
		if err := n.Call(ctx, http.MethodPost, "/api/host/settings", u, nil); err != nil {
			return err
		}
		if p.Networks {
			for _, d := range networksFor(defs, n) {
				if err := n.Call(ctx, http.MethodPost, "/api/networks/create", netReq(d), nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if errs != "" {
		ui.Flash(w, r, errors.New(errs))
		return
	}
	log.Printf("%s: host profile for %s/%s applied", ui.Session(r).User, dc, cl)
	w.Header().Set("HX-Refresh", "true")
}
