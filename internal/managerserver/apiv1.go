package managerserver

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"argus/internal/auth"
	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/web"
)

type v1Host struct {
	Name        string `json:"name"`
	Datacenter  string `json:"datacenter"`
	Cluster     string `json:"cluster"`
	Arch        string `json:"arch"`
	Reachable   bool   `json:"reachable"`
	Maintenance bool   `json:"maintenance"`
	Error       string `json:"error,omitempty"`
	VMs         int    `json:"vms"`
}

type v1VM struct {
	Host         string            `json:"host"`
	Name         string            `json:"name"`
	DisplayName  string            `json:"displayName,omitempty"`
	Status       string            `json:"status"`
	Running      bool              `json:"running"`
	Cores        int64             `json:"cores"`
	Memory       string            `json:"memory"`
	MemoryMiB    int64             `json:"memoryMiB"`
	Networks     []string          `json:"networks,omitempty"`
	StorageBytes int64             `json:"storageBytes"`
	Tags         []string          `json:"tags,omitempty"`
	Folder       string            `json:"folder,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
	Template     bool              `json:"template"`
	OS           string            `json:"os,omitempty"`
	IPs          []string          `json:"ips,omitempty"`
	Started      time.Time         `json:"started,omitzero"`
	Restart      bool              `json:"restartRequired"`
	Agent        bool              `json:"agent"`
}

func toV1VM(host string, v kube.VM) v1VM {
	var nets []string
	for _, nic := range v.NICs {
		nets = append(nets, nic.Network[strings.Index(nic.Network, "/")+1:])
	}
	mem, _ := resource.ParseQuantity(v.Memory)
	return v1VM{MemoryMiB: mem.Value() >> 20, Networks: nets, Host: host, Name: v.Name, DisplayName: v.DisplayName, Status: v.Status, Running: v.Running, Cores: v.Cores, Memory: v.Memory,
		StorageBytes: v.StorageBytes, Tags: v.Tags, Folder: v.Folder, Attributes: v.Attributes, Template: v.Template, OS: v.OS, IPs: v.IPs(), Started: v.Started, Restart: v.PendingRestart, Agent: v.Agent}
}

type v1NewVM struct {
	Name      string     `json:"name"`
	Cores     int        `json:"cores"`
	MemoryMiB int        `json:"memoryMiB"`
	DiskGiB   int        `json:"diskGiB"`
	Image     string     `json:"image"`
	CDROM     string     `json:"cdrom"`
	Network   string     `json:"network"`
	Pool      string     `json:"pool"`
	Start     bool       `json:"start"`
	UserData  string     `json:"userData"`
	Hostname  string     `json:"hostname"`
	User      string     `json:"user"`
	SSHKeys   []string   `json:"sshKeys"`
	Password  string     `json:"password"`
	Address   string     `json:"address"`
	Gateway   string     `json:"gateway"`
	DNS       []string   `json:"dns"`
	Windows   *v1Windows `json:"windows,omitempty"`
}

type v1Windows struct {
	ComputerName   string   `json:"computerName"`
	AdminPassword  string   `json:"adminPassword"`
	TimeZone       string   `json:"timeZone"`
	ProductKey     string   `json:"productKey"`
	Domain         string   `json:"domain"`
	DomainUser     string   `json:"domainUser"`
	DomainPassword string   `json:"domainPassword"`
	Commands       []string `json:"commands"`
	VirtIO         bool     `json:"virtio"`
}

type v1Task struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	VM      string    `json:"vm,omitempty"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	Started time.Time `json:"started"`
	Status  string    `json:"status"`
	Done    bool      `json:"done"`
	Failed  bool      `json:"failed"`
}

func (s *Server) apiV1Routes() []ui.Route {
	return []ui.Route{
		{Pattern: "GET /api/v1/openapi.json", Priv: auth.VMView, H: s.v1Spec},
		{Pattern: "GET /api/v1/hosts", Priv: auth.HostView, H: s.v1Hosts},
		{Pattern: "GET /api/v1/vms", Priv: auth.VMView, H: s.v1VMs},
		{Pattern: "GET /api/v1/tasks", Priv: auth.VMView, H: s.v1Tasks},
		{Pattern: "POST /api/v1/hosts/{node}/vms", Priv: auth.VMProvision, H: s.v1CreateVM},
		{Pattern: "GET /api/v1/hosts/{node}/vms/{name}", Priv: auth.VMView, H: s.v1VM},
		{Pattern: "PATCH /api/v1/hosts/{node}/vms/{name}", Priv: auth.VMConfig, H: s.v1UpdateVM},
		{Pattern: "GET /api/v1/hosts/{node}/images", Priv: auth.VMView, H: s.v1Images},
		{Pattern: "GET /api/v1/networks", Priv: auth.VMView, H: s.v1Networks},
		{Pattern: "POST /api/v1/networks", Priv: auth.NetworkConfig, H: s.v1SaveNetwork},
		{Pattern: "GET /api/v1/networks/{name}", Priv: auth.VMView, H: s.v1Network},
		{Pattern: "PUT /api/v1/networks/{name}", Priv: auth.NetworkConfig, H: s.v1SaveNetwork},
		{Pattern: "DELETE /api/v1/networks/{name}", Priv: auth.NetworkConfig, H: s.v1DeleteNetwork},
		{Pattern: "DELETE /api/v1/hosts/{node}/vms/{name}", Priv: auth.VMDelete, H: s.v1VMCall("delete")},
		{Pattern: "POST /api/v1/hosts/{node}/vms/{name}/power", Priv: auth.VMPower, H: s.v1Power},
		{Pattern: "POST /api/v1/hosts/{node}/vms/{name}/migrate", Priv: auth.VMMigrate, H: s.v1Migrate},
		{Pattern: "GET /api/v1/hosts/{node}/vms/{name}/snapshots", Priv: auth.VMView, H: s.v1Snapshots},
		{Pattern: "POST /api/v1/hosts/{node}/vms/{name}/snapshots", Priv: auth.VMSnapshot, H: s.v1VMCall("snapshots")},
		{Pattern: "POST /api/v1/hosts/{node}/vms/{name}/snapshots/{snap}/restore", Priv: auth.VMSnapshot, H: s.v1SnapshotOp("restore")},
		{Pattern: "DELETE /api/v1/hosts/{node}/vms/{name}/snapshots/{snap}", Priv: auth.VMSnapshot, H: s.v1SnapshotOp("delete")},
		{Pattern: "POST /api/v1/hosts/{node}/vms/{name}/backups", Priv: auth.VMBackup, H: s.v1Backup},
		{Pattern: "GET /api/v1/hosts/{node}/vms/{name}/backups", Priv: auth.VMView, H: s.v1Backup},
		{Pattern: "DELETE /api/v1/hosts/{node}/vms/{name}/backups/{id}", Priv: auth.VMBackup, H: s.v1Backup},
		{Pattern: "GET /api/v1/hosts/{node}/vms/{name}/backups/{id}/disks/{disk}/changes", Priv: auth.VMBackup, H: s.v1Backup},
		{Pattern: "GET /api/v1/hosts/{node}/vms/{name}/backups/{id}/disks/{disk}/data", Priv: auth.VMBackup, H: s.v1Backup},
	}
}

func (s *Server) v1Spec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	http.ServeFileFS(w, r, web.FS, "static/openapi.json")
}

func (s *Server) v1Hosts(w http.ResponseWriter, r *http.Request) {
	out := []v1Host{}
	for _, st := range s.inventory(r) {
		out = append(out, v1Host{Name: st.Node.Name, Datacenter: st.Node.Datacenter, Cluster: st.Node.Cluster, Arch: st.Arch(), Reachable: st.Reachable(),
			Maintenance: st.Node.Maintenance, Error: st.Error, VMs: len(st.VMs)})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1VMs(w http.ResponseWriter, r *http.Request) {
	out := []v1VM{}
	for _, st := range s.inventory(r) {
		for _, v := range st.VMs {
			out = append(out, toV1VM(st.Node.Name, v))
		}
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1Tasks(w http.ResponseWriter, r *http.Request) {
	out := []v1Task{}
	for _, j := range s.visibleJobs(r) {
		out = append(out, v1Task{ID: j.ID, Kind: j.Kind, VM: j.VM, From: j.From, To: j.To, Started: j.Started, Status: j.Status, Done: j.Done, Failed: j.Failed})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1Node(w http.ResponseWriter, r *http.Request) (manager.Node, bool) {
	n, err := s.orgNode(r)
	if err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return n, false
	}
	return n, true
}

func (s *Server) v1VM(w http.ResponseWriter, r *http.Request) {
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	var vm kube.VM
	if err := n.Call(r.Context(), http.MethodGet, manager.APIPath("/api/vms/%s/%s", kube.Namespace, r.PathValue("name")), nil, &vm); err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	ui.WriteJSON(w, toV1VM(n.Name, vm))
}

func (s *Server) v1Call(w http.ResponseWriter, r *http.Request, n manager.Node, path string, body any) {
	if s.manager.Busy(n.Name, r.PathValue("name")) {
		ui.APIError(w, r, http.StatusConflict, errors.New("a migration of this VM is in progress"))
		return
	}
	if err := n.Call(r.Context(), http.MethodPost, path, body, nil); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSONCode(w, http.StatusAccepted, map[string]string{})
}

func (s *Server) v1VMCall(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if n, ok := s.v1Node(w, r); ok {
			s.v1Call(w, r, n, manager.APIPath("/api/vms/%s/%s/%s", kube.Namespace, r.PathValue("name"), op), nil)
		}
	}
}

func (s *Server) v1SnapshotOp(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if n, ok := s.v1Node(w, r); ok {
			s.v1Call(w, r, n, manager.APIPath("/api/vms/%s/%s/snapshots/%s/%s", kube.Namespace, r.PathValue("name"), r.PathValue("snap"), op), nil)
		}
	}
}

func (s *Server) v1Power(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := ui.DecodeJSON(r.Body, &req); err != nil || !kube.ValidAction(req.Action) {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("action must be one of start, stop, poweroff, restart, pause, unpause"))
		return
	}
	if n, ok := s.v1Node(w, r); ok {
		s.v1Call(w, r, n, manager.APIPath("/api/vms/%s/%s/%s", kube.Namespace, r.PathValue("name"), req.Action), nil)
	}
}

func (s *Server) v1Snapshots(w http.ResponseWriter, r *http.Request) {
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	var snaps []kube.Snapshot
	if err := n.Call(r.Context(), http.MethodGet, manager.APIPath("/api/vms/%s/%s/snapshots", kube.Namespace, r.PathValue("name")), nil, &snaps); err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	type v1Snap struct {
		Name    string    `json:"name"`
		Created time.Time `json:"created"`
		Ready   bool      `json:"ready"`
		Auto    bool      `json:"auto"`
		Parent  string    `json:"parent,omitempty"`
		Current bool      `json:"current"`
	}
	out := []v1Snap{}
	for _, sn := range snaps {
		out = append(out, v1Snap{sn.Name, sn.Created, sn.Ready, sn.Auto, sn.Parent, sn.Current})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1Migrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To string `json:"to"`
	}
	if err := ui.DecodeJSON(r.Body, &req); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	src, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	dst, err := s.nodeIn(r, req.To)
	if err == nil && dst.Name == src.Name {
		err = errors.New("to must be a different host")
	}
	if err == nil {
		err = s.migrate(r, src, dst, r.PathValue("name"))
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSONCode(w, http.StatusAccepted, map[string]string{})
}

func (s *Server) v1CreateVM(w http.ResponseWriter, r *http.Request) {
	var req v1NewVM
	if err := ui.DecodeJSON(r.Body, &req); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	spec := kube.VMSpec{Name: req.Name, Cores: req.Cores, MemoryMiB: req.MemoryMiB, DiskGiB: req.DiskGiB, Image: req.Image, CDROM: req.CDROM, Network: req.Network,
		UserData: req.UserData, Start: req.Start, Guest: kube.Guest{Hostname: strings.TrimSpace(req.Hostname), User: strings.TrimSpace(req.User),
			SSHKeys: slices.DeleteFunc(req.SSHKeys, func(k string) bool { return strings.TrimSpace(k) == "" }), Password: req.Password,
			Address: req.Address, Gateway: req.Gateway, DNS: req.DNS}}
	if w := req.Windows; w != nil {
		spec.Windows = &kube.WindowsGuest{ComputerName: w.ComputerName, AdminPassword: w.AdminPassword, TimeZone: w.TimeZone, ProductKey: w.ProductKey,
			Domain: w.Domain, DomainUser: w.DomainUser, DomainPassword: w.DomainPassword, Commands: w.Commands, VirtIO: w.VirtIO}
	}
	if err := hostui.CheckVMSpec(spec); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	err := (remoteOps{n, s}).CreateVM(r.Context(), hostui.CreateVMReq{Spec: spec, Pool: req.Pool})
	if errors.Is(err, hostui.ErrQueued) {
		ui.WriteJSONCode(w, http.StatusAccepted, map[string]string{"host": n.Name, "name": spec.Name})
		return
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSONCode(w, http.StatusCreated, map[string]string{"host": n.Name, "name": spec.Name})
}

func (s *Server) accountTokens(w http.ResponseWriter, r *http.Request) {
	s.renderAccountTokens(w, r, "", nil)
}

func (s *Server) renderAccountTokens(w http.ResponseWriter, r *http.Request, notice string, err error) {
	sess := ui.Session(r)
	ts, terr := s.Auth.Tokens(r.Context())
	ts = slices.DeleteFunc(ts, func(t auth.Token) bool { return t.Org != sess.Org || t.User != sess.User })
	if err == nil {
		err = terr
	}
	ui.Failed(w, r, err)
	s.Render(w, r, "tokens.html", "layout", map[string]any{"Tokens": ts, "Notice": notice, "Error": ui.ErrText(err)})
}

func (s *Server) createAccountToken(w http.ResponseWriter, r *http.Request) {
	sess, name := ui.Session(r), r.FormValue("name")
	if !ui.ValidName.MatchString(name) {
		s.renderAccountTokens(w, r, "", errors.New("invalid token name"))
		return
	}
	t := auth.Token{Name: name, Org: sess.Org, User: sess.User}
	if r.FormValue("read") != "" {
		t.Scope = auth.ReadScope
	}
	tok, err := s.Auth.AddToken(r.Context(), t)
	if err != nil {
		s.renderAccountTokens(w, r, "", err)
		return
	}
	s.renderAccountTokens(w, r, "Token "+name+" (shown once): "+tok, nil)
}

func (s *Server) deleteAccountToken(w http.ResponseWriter, r *http.Request) {
	sess := ui.Session(r)
	if err := s.Auth.RemoveToken(r.Context(), sess.Org, sess.User, r.PathValue("name")); err != nil {
		s.renderAccountTokens(w, r, "", err)
		return
	}
	http.Redirect(w, r, "/account/tokens", http.StatusSeeOther)
}

func (s *Server) v1Backup(w http.ResponseWriter, r *http.Request) {
	n, ok := s.v1Node(w, r)
	if !ok {
		return
	}
	path := manager.APIPath("/api/vms/%s/%s/cbt", kube.Namespace, r.PathValue("name"))
	if id := r.PathValue("id"); id != "" {
		path += manager.APIPath("/%s", id)
	}
	if disk := r.PathValue("disk"); disk != "" {
		path += manager.APIPath("/disks/%s/", disk) + r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	}
	if base := r.URL.Query().Get("base"); base != "" {
		path += "?" + url.Values{"base": {base}}.Encode()
	}
	h := http.Header{}
	for _, k := range []string{"Range", "If-Range"} {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	resp, err := n.Open(r.Context(), r.Method, path, h, nil)
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	defer resp.Body.Close()
	ui.CopyStream(w, resp)
}
