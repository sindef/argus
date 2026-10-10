package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/api/resource"
)

type vmRow struct {
	kube.VM
	Host       string
	Datacenter string
	Cluster    string
	Protected  *manager.Protected
	Replica    bool
}

type hostRow struct {
	manager.NodeState
	Status    string
	VMs       int
	Running   int
	CoresUsed int64
	MemUsed   int64
	Util      *manager.Point
}

type clusterView struct {
	Name      string
	Status    string
	VMs       int
	Hosts     []hostRow
	VMsByHost map[string][]vmRow
}

type datacenterView struct {
	Name     string
	Clusters []*clusterView
}

func superAdmin(r *http.Request) bool {
	return session(r).Org == manager.DefaultOrg && session(r).Role == auth.Admin
}

func groupName(s string) string {
	if s == "" {
		return "Unassigned"
	}
	return s
}

func hostStatus(st manager.NodeState) string {
	switch {
	case !st.Reachable():
		return "Unreachable"
	case st.Stale():
		return "Warning"
	}
	return "Healthy"
}

func worse(a, b string) string {
	rank := map[string]int{"Healthy": 0, "Warning": 1, "Unreachable": 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func (s *Server) managerData(r *http.Request) map[string]any {
	states := s.manager.Inventory(session(r).Org)
	ps, _ := s.manager.Protected(r.Context())
	protected := map[string]manager.Protected{}
	for _, p := range ps {
		protected[p.VM] = p
	}
	var rows []vmRow
	var hosts []hostRow
	var cores, mem, coresUsed, memUsed int64
	hostCount, vmCount := map[string]int{}, map[string]int{}
	for _, st := range states {
		h := hostRow{NodeState: st, Status: hostStatus(st)}
		if p, ok := s.manager.HostLatest(st.Node.Name); ok {
			h.Util = &p
		}
		hostCount[h.Status]++
		if h.Status != "Unreachable" {
			cores += st.Info.CPUCores
			mem += st.Info.MemoryBytes
		}
		for _, v := range st.VMs {
			row := vmRow{VM: v, Host: st.Node.Name, Datacenter: groupName(st.Node.Datacenter), Cluster: groupName(st.Node.Cluster)}
			if p, ok := protected[v.Name]; ok {
				row.Protected = &p
				row.Replica = p.Replica == st.Node.Name
			}
			if row.Replica {
				continue
			}
			rows = append(rows, row)
			h.VMs++
			vmCount[v.Status]++
			if v.Running {
				h.Running++
				h.CoresUsed += v.Cores
				if b, err := parseBytes(v.Memory); err == nil {
					h.MemUsed += b
				}
			}
		}
		coresUsed += h.CoresUsed
		memUsed += h.MemUsed
		hosts = append(hosts, h)
	}
	dcs := map[string]map[string]*clusterView{}
	for _, h := range hosts {
		dc, cl := groupName(h.Node.Datacenter), groupName(h.Node.Cluster)
		if dcs[dc] == nil {
			dcs[dc] = map[string]*clusterView{}
		}
		c := dcs[dc][cl]
		if c == nil {
			c = &clusterView{Name: cl, Status: "Healthy", VMsByHost: map[string][]vmRow{}}
			dcs[dc][cl] = c
		}
		c.Hosts = append(c.Hosts, h)
		c.VMs += h.VMs
		c.Status = worse(c.Status, h.Status)
	}
	for _, v := range rows {
		c := dcs[v.Datacenter][v.Cluster]
		c.VMsByHost[v.Host] = append(c.VMsByHost[v.Host], v)
	}
	var tree []datacenterView
	var clusters []*clusterView
	for dc, cls := range dcs {
		d := datacenterView{Name: dc}
		for _, c := range cls {
			d.Clusters = append(d.Clusters, c)
			clusters = append(clusters, c)
		}
		sort.Slice(d.Clusters, func(i, j int) bool { return d.Clusters[i].Name < d.Clusters[j].Name })
		tree = append(tree, d)
	}
	sort.Slice(tree, func(i, j int) bool { return tree[i].Name < tree[j].Name })
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	clusterCount := map[string]int{}
	for _, c := range clusters {
		clusterCount[c.Status]++
	}
	q := strings.ToLower(strings.TrimSpace(r.FormValue("q")))
	if q != "" {
		rows = slices.DeleteFunc(rows, func(v vmRow) bool {
			return !strings.Contains(strings.ToLower(v.Name+" "+v.DisplayName+" "+v.Host+" "+v.Cluster+" "+strings.Join(v.IPs(), " ")), q)
		})
	}
	vms := 0
	for _, n := range vmCount {
		vms += n
	}
	return map[string]any{
		"Hosts":        hosts,
		"Tree":         tree,
		"Clusters":     clusters,
		"Rows":         rows,
		"Protected":    protected,
		"Jobs":         s.manager.Jobs(session(r).Org),
		"VMs":          vms,
		"VMCount":      vmCount,
		"HostCount":    hostCount,
		"ClusterCount": clusterCount,
		"Cores":        cores,
		"CoresUsed":    coresUsed,
		"Mem":          mem,
		"MemUsed":      memUsed,
		"CanOperate":   session(r).Role.Can(auth.Operator),
		"IsAdmin":      session(r).Role.Can(auth.Admin),
		"Super":        superAdmin(r),
	}
}

func (s *Server) managerHome(w http.ResponseWriter, r *http.Request) {
	data := s.managerData(r)
	data["Nav"] = "home"
	data["Range"] = r.URL.Query().Get("range")
	s.render(w, r, "manager.html", "layout", data)
}

func (s *Server) managerVMs(w http.ResponseWriter, r *http.Request) {
	data := s.managerData(r)
	data["Nav"] = "vms"
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "vm-table"
	}
	s.render(w, r, "manager-vms.html", tmpl, data)
}

func (s *Server) managerTree(w http.ResponseWriter, r *http.Request) {
	data := s.managerData(r)
	data["Nav"] = r.FormValue("nav")
	s.render(w, r, "manager.html", "tree", data)
}

func (s *Server) managerConsole(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	kind := "vnc"
	if r.URL.Query().Get("type") == "serial" {
		kind = "console"
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	page := fmt.Sprintf("/nodes/%s/vms/%s/%s", n.Name, ns, name)
	s.render(w, r, "console.html", "layout", map[string]any{"Namespace": ns, "Name": name, "Kind": kind, "Page": page, "WS": page + "/ws/", "Nav": "vm:" + n.Name + "/" + name})
}

func (s *Server) orgNode(r *http.Request) (manager.Node, error) {
	return s.manager.NodeIn(r.Context(), session(r).Org, r.PathValue("node"))
}

var vmTabs = []string{"summary", "hardware", "snapshots", "logs"}

func (s *Server) managerVM(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var vm kube.VM
	if err := n.Call(r.Context(), http.MethodGet, fmt.Sprintf("/api/vms/%s/%s", ns, name), nil, &vm); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tab := r.URL.Query().Get("tab")
	if !slices.Contains(vmTabs, tab) {
		tab = "summary"
	}
	data := s.managerData(r)
	data["Node"], data["VM"], data["Tab"], data["Tabs"] = n, vm, tab, vmTabs
	data["Ctx"] = map[string]any{"V": vmRow{VM: vm, Host: n.Name}, "Page": fmt.Sprintf("/nodes/%s/vms/%s/%s", n.Name, ns, name), "CanOperate": data["CanOperate"], "IsAdmin": data["IsAdmin"]}
	data["Nav"] = "vm:" + n.Name + "/" + name
	if p, ok := data["Protected"].(map[string]manager.Protected)[name]; ok {
		data["HA"] = p
	}
	switch tab {
	case "summary", "logs":
		var evs []kube.Event
		err = n.Call(r.Context(), http.MethodGet, fmt.Sprintf("/api/vms/%s/%s/events", ns, name), nil, &evs)
		data["Events"] = evs
	case "snapshots":
		var snaps []kube.Snapshot
		err = n.Call(r.Context(), http.MethodGet, fmt.Sprintf("/api/vms/%s/%s/snapshots", ns, name), nil, &snaps)
		data["Snapshots"] = snaps
	}
	data["Error"] = errText(err)
	s.render(w, r, "manager-vm.html", "layout", data)
}

func (s *Server) managerVMAction(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		flash(w, r, err)
		return
	}
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if s.manager.Busy(n.Name, name) {
		flash(w, r, fmt.Errorf("%s has a migration in progress", name))
		return
	}
	path := fmt.Sprintf("/api/vms/%s/%s/", ns, name)
	var body any
	switch {
	case kube.ValidAction(act):
		path += act
	case act == "delete":
		if !session(r).Role.Can(auth.Admin) {
			flash(w, r, errors.New("only admins can delete VMs"))
			return
		}
		path += "delete"
	case act == "snapshot":
		path += "snapshots"
	case act == "rename" || act == "clone":
		path += "meta/" + act
		body = map[string]string{"value": strings.TrimSpace(r.Header.Get("HX-Prompt"))}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err := n.Call(r.Context(), http.MethodPost, path, body, nil); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s on %s", session(r).User, act, ns, name, n.Name)
	go s.manager.Refresh(n.Name)
	if act == "delete" {
		redirect(w, r, "/")
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerSnapshotOp(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err == nil {
		err = n.Call(r.Context(), http.MethodPost, fmt.Sprintf("/api/vms/%s/%s/snapshots/%s/%s", r.PathValue("ns"), r.PathValue("name"), r.PathValue("snap"), r.PathValue("op")), nil, nil)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerConsoleWS(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	backend, err := n.DialConsole(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("kind"))
	if err != nil {
		log.Printf("%s: %v", session(r).User, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()
	up := websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}
	client, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	log.Printf("%s: %s %s/%s on %s", session(r).User, r.PathValue("kind"), r.PathValue("ns"), r.PathValue("name"), n.Name)
	done := make(chan struct{}, 2)
	go pipe(client, backend, done)
	go pipe(backend, client, done)
	s.waitConsole(r, done)
}

func (s *Server) managerMigrate(w http.ResponseWriter, r *http.Request) {
	org := session(r).Org
	src, err := s.orgNode(r)
	var dst manager.Node
	if err == nil {
		dst, err = s.manager.NodeIn(r.Context(), org, r.FormValue("to"))
	}
	if err == nil && src.Name == dst.Name {
		err = errors.New("pick a different host")
	}
	var vm kube.VM
	if err == nil {
		err = src.Call(r.Context(), http.MethodGet, fmt.Sprintf("/api/vms/%s/%s", r.PathValue("ns"), r.PathValue("name")), nil, &vm)
	}
	if err == nil {
		if vm.Running {
			err = s.manager.LiveMigrate(src, dst, r.PathValue("ns"), r.PathValue("name"))
		} else {
			err = s.manager.Migrate(src, dst, r.PathValue("ns"), r.PathValue("name"))
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: migrate %s/%s from %s to %s", session(r).User, r.PathValue("ns"), r.PathValue("name"), src.Name, dst.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerProtect(w http.ResponseWriter, r *http.Request) {
	var interval int
	err := intField(r, "interval", 5, 1440, &interval)
	if err == nil {
		_, err = s.orgNode(r)
	}
	if err == nil {
		_, err = s.manager.NodeIn(r.Context(), session(r).Org, r.FormValue("replica"))
	}
	if err == nil {
		err = s.manager.Protect(r.Context(), r.PathValue("node"), r.PathValue("name"), r.FormValue("replica"), interval)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: protect %s on %s, replica on %s", session(r).User, r.PathValue("name"), r.PathValue("node"), r.FormValue("replica"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerUnprotect(w http.ResponseWriter, r *http.Request) {
	_, err := s.orgNode(r)
	if err == nil {
		err = s.manager.Unprotect(r.Context(), r.PathValue("name"))
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: unprotect %s", session(r).User, r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) managerHosts(w http.ResponseWriter, r *http.Request) {
	s.renderHosts(w, r, nil)
}

func (s *Server) renderHosts(w http.ResponseWriter, r *http.Request, err error) {
	data := s.managerData(r)
	data["Nav"] = "hosts"
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		data["Error"] = err.Error()
	}
	mig, _ := s.manager.MigrationSettings(r.Context())
	ranges := map[string]string{}
	for _, h := range data["Hosts"].([]hostRow) {
		st := h.NodeState
		if start, end, rerr := manager.SlotRange(mig.Subnet, st.Node.Slot); mig.Subnet != "" && rerr == nil {
			ranges[st.Node.Name] = start + " – " + end
		}
	}
	data["Ranges"] = ranges
	data["Migration"] = mig
	s.render(w, r, "hosts.html", "layout", data)
}

func (s *Server) managerAddNode(w http.ResponseWriter, r *http.Request) {
	name, addr := r.FormValue("name"), r.FormValue("url")
	u, err := url.Parse(addr)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		err = errors.New("address must be https://host")
	} else if !validObject.MatchString(name) {
		err = errors.New("invalid host name")
	}
	if err == nil {
		err = s.manager.AddNode(r.Context(), session(r).Org, name, u.Scheme+"://"+u.Host, r.FormValue("token"), r.FormValue("fingerprint"), strings.TrimSpace(r.FormValue("datacenter")), strings.TrimSpace(r.FormValue("cluster")))
	}
	if err != nil {
		s.renderHosts(w, r, err)
		return
	}
	s.manager.Refresh(name)
	log.Printf("%s: added host %s at %s", session(r).User, name, addr)
	http.Redirect(w, r, "/hosts", http.StatusSeeOther)
}

func (s *Server) managerGroupNode(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.SetNodeGroup(r.Context(), session(r).Org, r.PathValue("node"), strings.TrimSpace(r.FormValue("datacenter")), strings.TrimSpace(r.FormValue("cluster"))); err != nil {
		s.renderHosts(w, r, err)
		return
	}
	s.manager.Refresh(r.PathValue("node"))
	http.Redirect(w, r, "/hosts", http.StatusSeeOther)
}

func (s *Server) managerRemoveNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("node")
	if err := s.manager.RemoveNode(r.Context(), session(r).Org, name); err != nil {
		s.renderHosts(w, r, err)
		return
	}
	s.manager.PollAll(r.Context())
	log.Printf("%s: removed host %s", session(r).User, name)
	http.Redirect(w, r, "/hosts", http.StatusSeeOther)
}

func (s *Server) managerSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, "", nil)
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, notice string, err error) {
	org, oerr := s.manager.Org(r.Context(), session(r).Org)
	if err == nil {
		err = oerr
	}
	data := map[string]any{"Nav": "settings", "Org": org, "Notice": notice, "Super": superAdmin(r), "Roles": auth.Roles}
	if org.OIDC != nil {
		var maps []string
		for g, role := range org.OIDC.Roles {
			maps = append(maps, g+"="+string(role))
		}
		sort.Strings(maps)
		data["RoleMap"] = strings.Join(maps, "\n")
	}
	if superAdmin(r) {
		data["Orgs"], _ = s.manager.Orgs(r.Context())
		data["Migration"], _ = s.manager.MigrationSettings(r.Context())
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		data["Error"] = err.Error()
	}
	s.render(w, r, "settings.html", "layout", data)
}

func (s *Server) managerSSO(w http.ResponseWriter, r *http.Request) {
	org, err := s.manager.Org(r.Context(), session(r).Org)
	if err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	issuer := strings.TrimSpace(r.FormValue("issuer"))
	if issuer == "" {
		org.OIDC = nil
	} else {
		if u, perr := url.Parse(issuer); perr != nil || u.Scheme != "https" {
			s.renderSettings(w, r, "", errors.New("issuer must be an https URL"))
			return
		}
		roles := map[string]auth.Role{}
		for _, line := range strings.Split(r.FormValue("roles"), "\n") {
			g, role, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			if !auth.Role(strings.TrimSpace(role)).Valid() {
				s.renderSettings(w, r, "", fmt.Errorf("invalid role in %q", line))
				return
			}
			roles[strings.TrimSpace(g)] = auth.Role(strings.TrimSpace(role))
		}
		secret := r.FormValue("secret")
		if secret == "" && org.OIDC != nil {
			secret = org.OIDC.ClientSecret
		}
		org.OIDC = &manager.OIDC{Issuer: issuer, ClientID: strings.TrimSpace(r.FormValue("client")), ClientSecret: secret, GroupsClaim: strings.TrimSpace(r.FormValue("claim")), Roles: roles}
	}
	if err := s.manager.SaveOrg(r.Context(), org); err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	log.Printf("%s: updated SSO for %s", session(r).User, org.ID)
	s.renderSettings(w, r, "Single sign-on saved.", nil)
}

func (s *Server) managerAddOrg(w http.ResponseWriter, r *http.Request) {
	if !superAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id, name := r.FormValue("id"), strings.TrimSpace(r.FormValue("name"))
	if !validObject.MatchString(id) || name == "" {
		s.renderSettings(w, r, "", errors.New("invalid organization"))
		return
	}
	if _, err := s.manager.Org(r.Context(), id); err == nil {
		s.renderSettings(w, r, "", errors.New("organization already exists"))
		return
	}
	if err := s.manager.SaveOrg(r.Context(), manager.Org{ID: id, Name: name}); err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	pw := auth.RandomPassword()
	if _, err := s.auth.ForOrg(id).Bootstrap(r.Context(), pw); err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	log.Printf("%s: created organization %s", session(r).User, id)
	s.renderSettings(w, r, "Organization "+name+" created. Initial login: admin / "+pw, nil)
}

func (s *Server) managerDeleteOrg(w http.ResponseWriter, r *http.Request) {
	if !superAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := s.manager.DeleteOrg(r.Context(), r.PathValue("id")); err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	log.Printf("%s: deleted organization %s", session(r).User, r.PathValue("id"))
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) managerMigrationSettings(w http.ResponseWriter, r *http.Request) {
	if !superAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	vlan := 0
	if v := r.FormValue("vlan"); v != "" {
		if err := intField(r, "vlan", 0, 4094, &vlan); err != nil {
			s.renderSettings(w, r, "", err)
			return
		}
	}
	if err := s.manager.SetMigrationSettings(r.Context(), manager.MigrationSettings{VLAN: vlan, Subnet: r.FormValue("subnet")}); err != nil {
		s.renderSettings(w, r, "", err)
		return
	}
	log.Printf("%s: migration network vlan %d %s", session(r).User, vlan, r.FormValue("subnet"))
	s.renderSettings(w, r, "Applying the migration network; see Hosts for each host's status.", nil)
}

func parseBytes(q string) (int64, error) {
	v, err := resource.ParseQuantity(q)
	return v.Value(), err
}

func (s *Server) managerVMMetrics(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	cores := float32(1)
	if st, ok := s.manager.NodeState(n.Name); ok {
		for _, v := range st.VMs {
			if v.Namespace == ns && v.Name == name && v.Cores > 0 {
				cores = float32(v.Cores)
			}
		}
	}
	ps := s.manager.VMSeries(n.Name, ns, name)
	cols := map[string][]float32{}
	t := make([]int64, len(ps))
	for i, p := range ps {
		t[i] = p.T
		cols["cpu"] = append(cols["cpu"], min(100, p.V[0]/cores*100))
		cols["mem"] = append(cols["mem"], p.V[1])
		cols["rd"] = append(cols["rd"], p.V[2])
		cols["wr"] = append(cols["wr"], p.V[3])
		cols["rx"] = append(cols["rx"], p.V[4])
		cols["tx"] = append(cols["tx"], p.V[5])
	}
	writeJSON(w, map[string]any{"t": t, "cols": cols})
}

func (s *Server) managerFleetMetrics(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Add(-24 * time.Hour)
	if r.URL.Query().Get("range") == "7d" {
		since = time.Now().Add(-7 * 24 * time.Hour)
	}
	var nodes []string
	if n := r.URL.Query().Get("node"); n != "" {
		if _, err := s.manager.NodeIn(r.Context(), session(r).Org, n); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		nodes = []string{n}
	} else {
		for _, st := range s.manager.Inventory(session(r).Org) {
			nodes = append(nodes, st.Node.Name)
		}
	}
	ps := s.manager.HostSeries(nodes, since)
	cols := map[string][]float32{}
	t := make([]int64, len(ps))
	for i, p := range ps {
		t[i] = p.T
		cols["cpu"] = append(cols["cpu"], p.V[0])
		mem := float32(0)
		if p.V[2] > 0 {
			mem = p.V[1] / p.V[2] * 100
		}
		cols["mem"] = append(cols["mem"], mem)
		cols["poolUsed"] = append(cols["poolUsed"], p.V[4])
		cols["poolFree"] = append(cols["poolFree"], p.V[5])
	}
	writeJSON(w, map[string]any{"t": t, "cols": cols})
}

type hostEvent struct {
	kube.Event
	Host string
}

func fanOut(ctx context.Context, states []manager.NodeState, fn func(context.Context, manager.Node) error) string {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []string
	)
	sem := make(chan struct{}, 8)
	for _, st := range states {
		if !st.Reachable() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(n manager.Node) {
			defer func() { <-sem; wg.Done() }()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := fn(ctx, n); err != nil {
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
			}
		}(st.Node)
	}
	wg.Wait()
	return strings.Join(errs, "; ")
}

func (s *Server) managerEvents(w http.ResponseWriter, r *http.Request) {
	var (
		mu  sync.Mutex
		evs []hostEvent
	)
	errs := fanOut(r.Context(), s.manager.Inventory(session(r).Org), func(ctx context.Context, n manager.Node) error {
		var list []kube.Event
		if err := n.Call(ctx, http.MethodGet, "/api/events", nil, &list); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, e := range list {
			evs = append(evs, hostEvent{Event: e, Host: n.Name})
		}
		return nil
	})
	sort.Slice(evs, func(i, j int) bool { return evs[i].Time.After(evs[j].Time) })
	if len(evs) > 200 {
		evs = evs[:200]
	}
	s.render(w, r, "events.html", "layout", map[string]any{"Events": evs, "Error": errs, "Nav": "events", "Jobs": s.manager.Jobs(session(r).Org)})
}

func (s *Server) managerResolve(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Resolve(r.Context(), session(r).Org, r.PathValue("id")); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: resolved job %s", session(r).User, r.PathValue("id"))
	w.Header().Set("HX-Refresh", "true")
}
