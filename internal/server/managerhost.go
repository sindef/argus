package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/hostd"
	"argus/internal/manager"
)

var hostTabs = []struct{ Path, Label string }{{"dashboard", "Summary"}, {"pools", "Pools"}, {"disks", "Disks"}, {"network/host", "Network"}}

func (s *Server) managerHostUI(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	path := "/api/ui/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	h := http.Header{}
	for _, k := range []string{"Content-Type", "HX-Request", "HX-Prompt", "HX-Target"} {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set(BaseHeader, "/hosts/"+n.Name)
	resp, err := n.Proxy(r.Context(), r.Method, path, r.Body, h)
	if err != nil {
		flash(w, r, fmt.Errorf("%s: %w", n.Name, err))
		return
	}
	defer resp.Body.Close()
	page := r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" && resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html")
	if !page {
		for k, v := range resp.Header {
			if strings.HasPrefix(k, "Hx-") || k == "Content-Type" || k == "Content-Length" || k == "Content-Disposition" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		flash(w, r, err)
		return
	}
	s.render(w, r, "remote.html", "layout", map[string]any{"Nav": "host:" + n.Name, "Host": n.Name, "Tabs": hostTabs, "Tab": r.PathValue("path"), "Body": template.HTML(body)})
}

type hostNetRow struct {
	Node    manager.Node
	Info    HostNetInfo
	Bridges []hostd.Iface
	Mgmt    []string
	Error   string
}

func (s *Server) fleetHostNets(ctx context.Context, org string) []hostNetRow {
	var (
		mu   sync.Mutex
		rows []hostNetRow
	)
	nodes, _ := s.manager.NodesFor(ctx, org)
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n manager.Node) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			row := hostNetRow{Node: n}
			if err := n.Call(cctx, http.MethodGet, "/api/host/network", nil, &row.Info); err != nil {
				row.Error = err.Error()
			}
			for _, i := range row.Info.Net.Ifaces {
				if i.Kind == "bridge" {
					row.Bridges = append(row.Bridges, i)
				}
				if i.Master == "" && (!row.Info.Net.Restricted || slices.Contains(row.Info.Net.Management, i.Name)) {
					row.Mgmt = append(row.Mgmt, i.Name)
				}
			}
			mu.Lock()
			rows = append(rows, row)
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Node.Name < rows[j].Node.Name })
	return rows
}

func (s *Server) managerHostNets(w http.ResponseWriter, r *http.Request) {
	rows := s.fleetHostNets(r.Context(), session(r).Org)
	pre := map[string]bool{}
	if dc := r.FormValue("dc"); dc != "" {
		for _, row := range rows {
			pre[row.Node.Name] = inGroup(row.Node, dc, r.FormValue("cluster"))
		}
	}
	s.render(w, r, "hostnets.html", "layout", map[string]any{"Nav": "hostnets", "Rows": rows, "Pre": pre})
}

func (s *Server) managerBridgeRollout(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	spec := hostd.BridgeSpec{Name: r.FormValue("name"), Ports: strings.Fields(strings.ReplaceAll(r.FormValue("ports"), ",", " ")), Bond: r.FormValue("bond"), BondMode: r.FormValue("bond_mode")}
	hosts := r.Form["hosts"]
	sort.Strings(hosts)
	if len(hosts) == 0 || spec.Name == "" || len(spec.Ports) == 0 {
		flash(w, r, errors.New("name, ports and at least one host are required"))
		return
	}
	var out []string
	defer func() { w.Write([]byte(template.HTMLEscapeString(strings.Join(out, "\n")))) }()
	for i, name := range hosts {
		n, err := s.nodeIn(r, name)
		if err == nil {
			err = s.rolloutBridge(r.Context(), n, spec)
		}
		if err != nil {
			out = append(out, name+": "+strings.TrimPrefix(err.Error(), name+": "))
			if rest := hosts[i+1:]; len(rest) > 0 {
				out = append(out, "Stopped; not changed: "+strings.Join(rest, ", "))
			}
			return
		}
		log.Printf("%s: bridge %s rolled out to %s", session(r).User, spec.Name, name)
		out = append(out, name+": "+spec.Name+" created and confirmed")
	}
}

func (s *Server) rolloutBridge(ctx context.Context, n manager.Node, spec hostd.BridgeSpec) error {
	if err := n.Call(ctx, http.MethodPost, "/api/host/network/bridge", spec, nil); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var info HostNetInfo
	if err := n.Call(pctx, http.MethodGet, "/api/host/network", nil, &info); err != nil {
		return fmt.Errorf("unreachable after the change, it rolls back automatically: %w", err)
	}
	return n.Call(ctx, http.MethodPost, "/api/host/network/confirm", nil, nil)
}

func (s *Server) managerNewVM(w http.ResponseWriter, r *http.Request) {
	host := r.FormValue("host")
	if host == "" {
		s.render(w, r, "manager-vms.html", "pick-host", s.managerData(r))
		return
	}
	if host == "auto" {
		n, err := s.manager.LeastLoaded(session(r).Org)
		if err != nil {
			flash(w, r, err)
			return
		}
		host = n.Name
	}
	r.SetPathValue("node", host)
	r.SetPathValue("path", "vms/new")
	r.URL.RawQuery = ""
	s.managerHostUI(w, r)
}
