package managerserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/wire"
)

type switchRow struct {
	Name     string
	Uplinks  []switchUplink
	Missing  []string
	Networks []*netRow
	Mismatch bool
}

type switchUplink struct {
	Host                        string
	Ports                       []hostd.Iface
	Mode, Hash, Primary, Active string
	MTU                         int
}

func (u switchUplink) policy() string {
	return fmt.Sprint(u.Mode, u.Hash, u.Primary, u.MTU)
}

func switches(hosts []hostNetRow, nets []*netRow) []switchRow {
	byName := map[string]*switchRow{}
	var reachable []string
	for _, h := range hosts {
		if h.Error != "" {
			continue
		}
		reachable = append(reachable, h.Node.Name)
		ifaces := map[string]hostd.Iface{}
		for _, i := range h.Info.Net.Ifaces {
			ifaces[i.Name] = i
		}
		for _, b := range h.Bridges {
			if byName[b.Name] == nil {
				byName[b.Name] = &switchRow{Name: b.Name}
			}
			up := switchUplink{Host: h.Node.Name, MTU: b.MTU}
			for _, p := range b.Ports {
				switch i := ifaces[p]; i.Kind {
				case "bond":
					up.Mode, up.Hash, up.Primary, up.Active = i.BondMode, i.Hash, i.Primary, i.Active
					for _, q := range i.Ports {
						up.Ports = append(up.Ports, ifaces[q])
					}
				case "ether":
					up.Ports = append(up.Ports, i)
				}
			}
			sw := byName[b.Name]
			sw.Mismatch = sw.Mismatch || len(sw.Uplinks) > 0 && sw.Uplinks[0].policy() != up.policy()
			sw.Uplinks = append(sw.Uplinks, up)
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
	org := ui.Session(r).Org
	states := s.inventory(r)
	defs, err := s.manager.Networks(r.Context(), org)
	onHost := map[string]map[string]kube.Network{}
	for _, st := range states {
		if !st.Reachable() {
			continue
		}
		onHost[st.Node.Name] = map[string]kube.Network{}
		for _, x := range st.Networks {
			onHost[st.Node.Name][x.Name] = x
		}
	}
	errs := stateErrors(states)
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
	s.Render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Rows": out, "Reachable": len(onHost), "Switches": switches(s.fleetHostNets(org), out), "Scopes": s.ScopeNames(r), "AllBridges": all, "Error": errs})
}

func networksFor(defs []manager.Network, n manager.Node) []manager.Network {
	return slices.DeleteFunc(slices.Clone(defs), func(d manager.Network) bool { return !inScope(d.Scopes, n) })
}

func (s *Server) pushNetwork(r *http.Request, op string, n manager.Network) string {
	var targets []manager.NodeState
	for _, st := range s.inventory(r) {
		if s.permit(r, auth.NetworkConfig, objectOf(st.Node, "")) == nil {
			targets = append(targets, st)
		}
	}
	return fanOut(r.Context(), targets, func(ctx context.Context, node manager.Node) error {
		defer s.manager.Refresh(node.Name)
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
	req, err := hostui.ParseNetwork(r)
	if err == nil {
		err = hostui.ValidNetwork(req)
	}
	n := manager.Network{Name: req.Name, VLAN: req.VLAN, Bridge: req.Bridge, SpoofCheck: req.SpoofCheck, RateMbps: req.RateMbps, CIDR: strings.TrimSpace(r.FormValue("cidr")), Gateway: strings.TrimSpace(r.FormValue("gateway")), Description: strings.TrimSpace(r.FormValue("description")), Scopes: ui.ScopeList(r.FormValue("scopes"))}
	if err == nil {
		err = checkNetworkDef(n)
	}
	op := "create"
	if r.FormValue("edit") != "" {
		op = "update"
	}
	if err == nil {
		err = s.permitNetwork(r, n.Name, n.Scopes, true)
	}
	if err == nil {
		err = s.manager.SaveNetwork(r.Context(), ui.Session(r).Org, n, op == "update")
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: define network %s vlan %d", ui.Session(r).User, n.Name, n.VLAN)
	if errs := s.pushNetwork(r, op, n); errs != "" {
		ui.Flash(w, r, errors.New(errs))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerNetworkOp(w http.ResponseWriter, r *http.Request) {
	org, name, op := ui.Session(r).Org, r.PathValue("name"), r.PathValue("op")
	defs, err := s.manager.Networks(r.Context(), org)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	n := manager.Network{Name: name}
	for _, d := range defs {
		if d.Name == name {
			n = d
		}
	}
	if err := s.permitNetwork(r, name, nil, false); err != nil {
		ui.Flash(w, r, err)
		return
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
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) permitNetwork(r *http.Request, name string, scopes []string, save bool) error {
	defs, err := s.manager.Networks(r.Context(), ui.Session(r).Org)
	if err != nil {
		return err
	}
	var objs []auth.Object
	if save {
		objs = scopeObjects(scopes)
	}
	for _, d := range defs {
		if d.Name == name {
			objs = append(objs, scopeObjects(d.Scopes)...)
		}
	}
	if len(objs) == 0 {
		objs = scopeObjects(nil)
	}
	return s.permit(r, auth.NetworkConfig, objs...)
}

func checkNetworkDef(n manager.Network) error {
	if err := hostui.ValidNetwork(netReq(n)); err != nil {
		return err
	}
	if _, _, err := net.ParseCIDR(n.CIDR); n.CIDR != "" && err != nil {
		return errors.New("invalid CIDR")
	}
	if n.Gateway != "" && net.ParseIP(n.Gateway) == nil {
		return errors.New("invalid gateway")
	}
	return nil
}

func netReq(n manager.Network) wire.NetworkRequest {
	return wire.NetworkRequest{Name: n.Name, VLAN: n.VLAN, Bridge: n.Bridge, SpoofCheck: n.SpoofCheck, RateMbps: n.RateMbps}
}
