package server

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/hostd"
	"argus/internal/manager"
)

var hostTabs = []struct{ Path, Label, Icon string }{{"dashboard", "Summary", "gauge"}, {"pools", "Pools", "database"}, {"disks", "Disks", "hard-drive"}, {"network/host", "Network", "cable"}}

func (s *Server) managerHostUI(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	role, ok := hostRouteRole(r.Method, "/"+r.PathValue("path"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !session(r).Role.Can(role) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.PathValue("path"), "/download") {
		s.record(s.auditUser(session(r).Org, session(r).User), "download "+n.Name+"/"+r.PathValue("path"), true, "")
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
	base := "/hosts/" + n.Name
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	binary := ct == "application/json" || ct == "application/gzip" || ct == "application/octet-stream"
	isHTML := r.Header.Get("HX-Request") != "" || !binary
	page := r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" && resp.StatusCode == http.StatusOK && isHTML
	if !page {
		for _, k := range []string{"Hx-Refresh", "Hx-Retarget", "Hx-Reswap"} {
			if v := resp.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		for _, k := range []string{"Hx-Redirect", "Hx-Location", "Hx-Push-Url", "Hx-Replace-Url"} {
			if v := resp.Header.Get(k); v != "" && safeRedirect(v, n.Name) {
				w.Header().Set(k, v)
			}
		}
		if !isHTML {
			for _, k := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
				if v := resp.Header.Get(k); v != "" {
					w.Header().Set(k, v)
				}
			}
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "application/octet-stream")
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		w.Write(sanitizeHTML(body, base))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		flash(w, r, err)
		return
	}
	body := sanitizeHTML(raw, base)
	s.render(w, r, "remote.html", "layout", map[string]any{"Nav": "host:" + n.Name, "Host": n.Name, "Node": n, "Tabs": hostTabs, "Tab": r.PathValue("path"), "Body": template.HTML(body)})
}

type hostNetRow struct {
	Node    manager.Node
	Info    HostNetInfo
	Bridges []hostd.Iface
	Mgmt    []string
	Free    []string
	Error   string
}

type freePort struct {
	Name  string
	Hosts []string
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
				if i.Kind == "ether" && i.Master == "" && len(i.Addresses) == 0 && !i.Mgmt && !i.Access {
					row.Free = append(row.Free, i.Name)
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
	byPort := map[string][]string{}
	for _, row := range rows {
		for _, p := range row.Free {
			byPort[p] = append(byPort[p], row.Node.Name)
		}
	}
	var ports []freePort
	for p, hs := range byPort {
		ports = append(ports, freePort{p, hs})
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Name < ports[j].Name })
	s.render(w, r, "hostnets.html", "layout", map[string]any{"Nav": "hostnets", "Rows": rows, "Pre": pre, "Ports": ports, "HostCount": len(rows)})
}

func (s *Server) managerBridgeRollout(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	spec := bridgeSpec(r)
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
	host, path := r.FormValue("host"), "vms/new"
	if r.FormValue("what") == "import" {
		path = "vms/import"
	}
	if host == "" {
		data := s.managerData(r)
		data["What"] = r.FormValue("what")
		s.render(w, r, "manager-vms.html", "pick-host", data)
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
	r.SetPathValue("path", path)
	r.URL.RawQuery = ""
	s.managerHostUI(w, r)
}
