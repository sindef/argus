package server

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
)

type switchRow struct {
	Name     string
	Uplinks  []switchUplink
	Missing  []string
	Networks []*netRow
}

type switchUplink struct {
	Host  string
	Ports []string
}

func switches(hosts []hostNetRow, nets []*netRow) []switchRow {
	byName := map[string]*switchRow{}
	var reachable []string
	for _, h := range hosts {
		if h.Error != "" {
			continue
		}
		reachable = append(reachable, h.Node.Name)
		for _, b := range h.Bridges {
			if byName[b.Name] == nil {
				byName[b.Name] = &switchRow{Name: b.Name}
			}
			byName[b.Name].Uplinks = append(byName[b.Name].Uplinks, switchUplink{h.Node.Name, b.Ports})
		}
	}
	var out []switchRow
	for _, sw := range byName {
		for _, h := range reachable {
			if !slices.ContainsFunc(sw.Uplinks, func(u switchUplink) bool { return u.Host == h }) {
				sw.Missing = append(sw.Missing, h)
			}
		}
		for _, n := range nets {
			if n.BridgeName() == sw.Name {
				sw.Networks = append(sw.Networks, n)
			}
		}
		out = append(out, *sw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type netRow struct {
	manager.Network
	Defined  bool
	Hosts    map[string]string
	Applied  int
	InScope  int
	Attached int
}

func (s *Server) managerNetworks(w http.ResponseWriter, r *http.Request) {
	org := session(r).Org
	states := s.inventory(r)
	defs, err := s.manager.Networks(r.Context(), org)
	var mu sync.Mutex
	onHost := map[string]map[string]kube.Network{}
	errs := fanOut(r.Context(), states, func(ctx context.Context, n manager.Node) error {
		var nets []kube.Network
		if err := n.Call(ctx, http.MethodGet, "/api/networks", nil, &nets); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		onHost[n.Name] = map[string]kube.Network{}
		for _, x := range nets {
			onHost[n.Name][x.Name] = x
		}
		return nil
	})
	attached := map[string]int{}
	bridges := map[string][]string{}
	for _, st := range states {
		for _, v := range st.VMs {
			for _, nic := range v.NICs {
				attached[nic.Network[strings.Index(nic.Network, "/")+1:]]++
			}
		}
		seen := map[string]bool{}
		for _, x := range onHost[st.Node.Name] {
			if !seen[x.Bridge] {
				seen[x.Bridge] = true
				bridges[st.Node.Name] = append(bridges[st.Node.Name], x.Bridge)
			}
		}
	}
	nodes := map[string]manager.Node{}
	for _, st := range states {
		nodes[st.Node.Name] = st.Node
	}
	rows := map[string]*netRow{}
	for _, d := range defs {
		rows[d.Name] = &netRow{Network: d, Defined: true, Hosts: map[string]string{}}
	}
	for _, nets := range onHost {
		for name, x := range nets {
			if rows[name] == nil {
				rows[name] = &netRow{Network: manager.Network{Name: name, VLAN: x.VLAN, Bridge: x.Bridge}, Hosts: map[string]string{}}
			}
		}
	}
	for host, nets := range onHost {
		for name, row := range rows {
			x, ok := nets[name]
			if !inScope(row.Scopes, nodes[host]) {
				continue
			}
			row.InScope++
			switch {
			case !ok:
				row.Hosts[host] = "Missing"
			case x.VLAN != row.VLAN || x.Bridge != row.BridgeName():
				row.Hosts[host] = "Mismatch"
			default:
				row.Hosts[host] = "Applied"
				row.Applied++
			}
		}
	}
	var out []*netRow
	for _, row := range rows {
		row.Attached = attached[row.Name]
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if err != nil {
		errs = err.Error()
	}
	var all []string
	for _, bs := range bridges {
		for _, b := range bs {
			if !slices.Contains(all, b) {
				all = append(all, b)
			}
		}
	}
	sort.Strings(all)
	s.render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Rows": out, "Reachable": len(onHost), "Switches": switches(s.fleetHostNets(r.Context(), org), out), "Scopes": s.scopeNames(r), "AllBridges": all, "CanEdit": session(r).Role.Can(auth.Admin), "Error": errs})
}

func networksFor(defs []manager.Network, n manager.Node) []manager.Network {
	return slices.DeleteFunc(slices.Clone(defs), func(d manager.Network) bool { return !inScope(d.Scopes, n) })
}

func (s *Server) pushNetwork(r *http.Request, op string, n manager.Network) string {
	return fanOut(r.Context(), s.inventory(r), func(ctx context.Context, node manager.Node) error {
		req := netReq(n)
		if op == "delete" || inScope(n.Scopes, node) {
			return node.Call(ctx, http.MethodPost, "/api/networks/"+op, req, nil)
		}
		var nets []kube.Network
		if err := node.Call(ctx, http.MethodGet, "/api/networks", nil, &nets); err != nil || !slices.ContainsFunc(nets, func(x kube.Network) bool { return x.Name == n.Name }) {
			return err
		}
		return node.Call(ctx, http.MethodPost, "/api/networks/delete", req, nil)
	})
}

func (s *Server) managerAddNetwork(w http.ResponseWriter, r *http.Request) {
	req, err := parseNetwork(r)
	if err == nil {
		err = validNetwork(req)
	}
	n := manager.Network{Name: req.Name, VLAN: req.VLAN, Bridge: req.Bridge, SpoofCheck: req.SpoofCheck, RateMbps: req.RateMbps, CIDR: strings.TrimSpace(r.FormValue("cidr")), Gateway: strings.TrimSpace(r.FormValue("gateway")), Description: strings.TrimSpace(r.FormValue("description")), Scopes: scopeList(r.FormValue("scopes"))}
	if _, _, cerr := net.ParseCIDR(n.CIDR); err == nil && n.CIDR != "" && cerr != nil {
		err = errors.New("invalid CIDR")
	}
	if err == nil && n.Gateway != "" && net.ParseIP(n.Gateway) == nil {
		err = errors.New("invalid gateway")
	}
	op := "create"
	if r.FormValue("edit") != "" {
		op = "update"
	}
	if err == nil {
		err = s.manager.SaveNetwork(r.Context(), session(r).Org, n, op == "update")
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: define network %s vlan %d", session(r).User, n.Name, n.VLAN)
	if errs := s.pushNetwork(r, op, n); errs != "" {
		flash(w, r, errors.New(errs))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerNetworkOp(w http.ResponseWriter, r *http.Request) {
	org, name, op := session(r).Org, r.PathValue("name"), r.PathValue("op")
	defs, err := s.manager.Networks(r.Context(), org)
	if err != nil {
		flash(w, r, err)
		return
	}
	n := manager.Network{Name: name}
	for _, d := range defs {
		if d.Name == name {
			n = d
		}
	}
	var errs string
	switch op {
	case "apply":
		errs = s.pushNetwork(r, "create", n)
	case "delete":
		errs = s.pushNetwork(r, "delete", n)
		if errs == "" {
			err = s.manager.DeleteNetwork(r.Context(), org, name)
		}
	default:
		err = errors.New("unknown operation")
	}
	if err == nil && errs != "" {
		err = errors.New(errs)
	}
	log.Printf("%s: network %s %s", session(r).User, op, name)
	if err != nil {
		flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func netReq(n manager.Network) networkReq {
	return networkReq{Name: n.Name, VLAN: n.VLAN, Bridge: n.Bridge, SpoofCheck: n.SpoofCheck, RateMbps: n.RateMbps}
}
