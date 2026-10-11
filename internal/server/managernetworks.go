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

type netRow struct {
	manager.Network
	Defined  bool
	Hosts    map[string]string
	Applied  int
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
	rows := map[string]*netRow{}
	for _, d := range defs {
		rows[d.Name] = &netRow{Network: d, Defined: true, Hosts: map[string]string{}}
	}
	for host, nets := range onHost {
		for name, x := range nets {
			if rows[name] == nil {
				rows[name] = &netRow{Network: manager.Network{Name: name, VLAN: x.VLAN, Bridge: x.Bridge}, Hosts: map[string]string{}}
			}
		}
		for name, row := range rows {
			x, ok := nets[name]
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
	s.render(w, r, "networks.html", "layout", map[string]any{"Nav": "networks", "Rows": out, "Reachable": len(onHost), "Bridges": bridges, "AllBridges": all, "CanEdit": session(r).Role.Can(auth.Admin), "Error": errs})
}

func (s *Server) pushNetwork(r *http.Request, op string, n manager.Network) string {
	return fanOut(r.Context(), s.inventory(r), func(ctx context.Context, node manager.Node) error {
		return node.Call(ctx, http.MethodPost, "/api/networks/"+op, networkReq{Name: n.Name, VLAN: n.VLAN, Bridge: n.Bridge}, nil)
	})
}

func (s *Server) managerAddNetwork(w http.ResponseWriter, r *http.Request) {
	req, err := parseNetwork(r)
	if err == nil {
		err = validNetwork(req)
	}
	n := manager.Network{Name: req.Name, VLAN: req.VLAN, Bridge: req.Bridge, CIDR: strings.TrimSpace(r.FormValue("cidr")), Gateway: strings.TrimSpace(r.FormValue("gateway")), Description: strings.TrimSpace(r.FormValue("description"))}
	if _, _, cerr := net.ParseCIDR(n.CIDR); err == nil && n.CIDR != "" && cerr != nil {
		err = errors.New("invalid CIDR")
	}
	if err == nil && n.Gateway != "" && net.ParseIP(n.Gateway) == nil {
		err = errors.New("invalid gateway")
	}
	if err == nil {
		err = s.manager.SaveNetwork(r.Context(), session(r).Org, n)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: define network %s vlan %d", session(r).User, n.Name, n.VLAN)
	if errs := s.pushNetwork(r, "create", n); errs != "" {
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
