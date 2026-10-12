package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"argus/internal/hostd"
	"argus/internal/manager"
)

type switchPort struct {
	hostd.BridgePort
	Host, VM, Network string
}

func (s *Server) switchHosts(r *http.Request, name string) []string {
	var hosts []string
	for _, h := range s.fleetHostNets(r.Context(), session(r).Org) {
		if slices.ContainsFunc(h.Bridges, func(b hostd.Iface) bool { return b.Name == name }) {
			hosts = append(hosts, h.Node.Name)
		}
	}
	return hosts
}

func (s *Server) managerSwitchPorts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	hosts := s.switchHosts(r, name)
	states := slices.DeleteFunc(s.inventory(r), func(st manager.NodeState) bool { return !slices.Contains(hosts, st.Node.Name) })
	type owner struct{ vm, network string }
	byMAC := map[string]owner{}
	for _, st := range states {
		for _, v := range st.VMs {
			for _, nic := range v.NICs {
				byMAC[strings.ToLower(nic.MAC)] = owner{v.Label(), nic.Network[strings.Index(nic.Network, "/")+1:]}
			}
		}
	}
	var (
		mu    sync.Mutex
		ports []switchPort
	)
	errs := fanOut(r.Context(), states, func(ctx context.Context, n manager.Node) error {
		var ps []hostd.BridgePort
		if err := n.Call(ctx, http.MethodGet, manager.APIPath("/api/host/network/bridges/%s/ports", name), nil, &ps); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range ps {
			sp := switchPort{BridgePort: p, Host: n.Name}
			for _, m := range p.MACs {
				if o, ok := byMAC[strings.ToLower(m)]; ok {
					sp.VM, sp.Network = o.vm, o.network
				}
			}
			ports = append(ports, sp)
		}
		return nil
	})
	sort.Slice(ports, func(i, j int) bool {
		a, b := ports[i], ports[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Uplink != b.Uplink {
			return a.Uplink
		}
		return a.VM+a.Name < b.VM+b.Name
	})
	s.render(w, r, "networks.html", "switch-ports", map[string]any{"Name": name, "Ports": ports, "Error": errs})
}

func (s *Server) managerSwitchTeaming(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	t := hostd.Teaming{Mode: r.FormValue("mode"), Hash: r.FormValue("hash"), Primary: strings.TrimSpace(r.FormValue("primary"))}
	if v := r.FormValue("mtu"); v != "" {
		var err error
		if t.MTU, err = strconv.Atoi(v); err != nil {
			flash(w, r, errors.New("invalid MTU"))
			return
		}
	}
	hosts := s.switchHosts(r, name)
	sort.Strings(hosts)
	var out []string
	defer func() { w.Write([]byte(template.HTMLEscapeString(strings.Join(out, "\n")))) }()
	for i, h := range hosts {
		n, err := s.nodeIn(r, h)
		if err == nil {
			err = s.rolloutHostNet(r.Context(), n, "/api/host/network/teaming", netOpReq{Iface: name, Teaming: t})
		}
		if err != nil {
			out = append(out, h+": "+strings.TrimPrefix(err.Error(), h+": "))
			if rest := hosts[i+1:]; len(rest) > 0 {
				out = append(out, "Stopped; not changed: "+strings.Join(rest, ", "))
			}
			return
		}
		log.Printf("%s: teaming of %s on %s set to %+v", session(r).User, name, h, t)
		out = append(out, fmt.Sprintf("%s: %s updated and confirmed", h, name))
	}
}
