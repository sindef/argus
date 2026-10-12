package server

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
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
	Host         string    `json:"host"`
	Name         string    `json:"name"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	Running      bool      `json:"running"`
	Cores        int64     `json:"cores"`
	Memory       string    `json:"memory"`
	StorageBytes int64     `json:"storageBytes"`
	Tags         []string  `json:"tags,omitempty"`
	Template     bool      `json:"template"`
	OS           string    `json:"os,omitempty"`
	IPs          []string  `json:"ips,omitempty"`
	Started      time.Time `json:"started,omitzero"`
	Restart      bool      `json:"restartRequired"`
	Agent        bool      `json:"agent"`
}

func toV1VM(host string, v kube.VM) v1VM {
	return v1VM{Host: host, Name: v.Name, DisplayName: v.DisplayName, Status: v.Status, Running: v.Running, Cores: v.Cores, Memory: v.Memory,
		StorageBytes: v.StorageBytes, Tags: v.Tags, Template: v.Template, OS: v.OS, IPs: v.IPs(), Started: v.Started, Restart: v.PendingRestart, Agent: v.Agent}
}

type v1NewVM struct {
	Name      string   `json:"name"`
	Cores     int      `json:"cores"`
	MemoryMiB int      `json:"memoryMiB"`
	DiskGiB   int      `json:"diskGiB"`
	Image     string   `json:"image"`
	CDROM     string   `json:"cdrom"`
	Network   string   `json:"network"`
	Pool      string   `json:"pool"`
	Start     bool     `json:"start"`
	UserData  string   `json:"userData"`
	Hostname  string   `json:"hostname"`
	User      string   `json:"user"`
	SSHKeys   []string `json:"sshKeys"`
	Password  string   `json:"password"`
	Address   string   `json:"address"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
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

func (s *Server) apiV1Routes() {
	for pattern, r := range map[string]struct {
		role auth.Privilege
		h    http.HandlerFunc
	}{
		"GET /api/v1/openapi.json":                                              {auth.VMView, s.v1Spec},
		"GET /api/v1/hosts":                                                     {auth.HostView, s.v1Hosts},
		"GET /api/v1/vms":                                                       {auth.VMView, s.v1VMs},
		"GET /api/v1/tasks":                                                     {auth.VMView, s.v1Tasks},
		"POST /api/v1/hosts/{node}/vms":                                         {auth.VMProvision, s.v1CreateVM},
		"GET /api/v1/hosts/{node}/vms/{name}":                                   {auth.VMView, s.v1VM},
		"DELETE /api/v1/hosts/{node}/vms/{name}":                                {auth.VMDelete, s.v1VMCall("delete")},
		"POST /api/v1/hosts/{node}/vms/{name}/power":                            {auth.VMPower, s.v1Power},
		"POST /api/v1/hosts/{node}/vms/{name}/migrate":                          {auth.VMMigrate, s.v1Migrate},
		"GET /api/v1/hosts/{node}/vms/{name}/snapshots":                         {auth.VMView, s.v1Snapshots},
		"POST /api/v1/hosts/{node}/vms/{name}/snapshots":                        {auth.VMSnapshot, s.v1VMCall("snapshots")},
		"POST /api/v1/hosts/{node}/vms/{name}/snapshots/{snap}/restore":         {auth.VMSnapshot, s.v1SnapshotOp("restore")},
		"DELETE /api/v1/hosts/{node}/vms/{name}/snapshots/{snap}":               {auth.VMSnapshot, s.v1SnapshotOp("delete")},
		"POST /api/v1/hosts/{node}/vms/{name}/backups":                          {auth.VMBackup, s.v1Backup},
		"GET /api/v1/hosts/{node}/vms/{name}/backups":                           {auth.VMView, s.v1Backup},
		"DELETE /api/v1/hosts/{node}/vms/{name}/backups/{id}":                   {auth.VMBackup, s.v1Backup},
		"GET /api/v1/hosts/{node}/vms/{name}/backups/{id}/disks/{disk}/changes": {auth.VMBackup, s.v1Backup},
		"GET /api/v1/hosts/{node}/vms/{name}/backups/{id}/disks/{disk}/data":    {auth.VMBackup, s.v1Backup},
	} {
		s.mux.HandleFunc(pattern, s.need(r.role, r.h))
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
	writeJSON(w, out)
}

func (s *Server) v1VMs(w http.ResponseWriter, r *http.Request) {
	out := []v1VM{}
	for _, st := range s.inventory(r) {
		for _, v := range st.VMs {
			out = append(out, toV1VM(st.Node.Name, v))
		}
	}
	writeJSON(w, out)
}

func (s *Server) v1Tasks(w http.ResponseWriter, r *http.Request) {
	out := []v1Task{}
	for _, j := range s.manager.Jobs(session(r).Org) {
		out = append(out, v1Task{ID: j.ID, Kind: j.Kind, VM: j.VM, From: j.From, To: j.To, Started: j.Started, Status: j.Status, Done: j.Done, Failed: j.Failed})
	}
	writeJSON(w, out)
}

func (s *Server) v1Node(w http.ResponseWriter, r *http.Request) (manager.Node, bool) {
	n, err := s.orgNode(r)
	if err != nil {
		apiError(w, r, http.StatusNotFound, err)
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
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	writeJSON(w, toV1VM(n.Name, vm))
}

func (s *Server) v1Call(w http.ResponseWriter, r *http.Request, n manager.Node, path string, body any) {
	if s.manager.Busy(n.Name, r.PathValue("name")) {
		apiError(w, r, http.StatusConflict, errors.New("a migration of this VM is in progress"))
		return
	}
	if err := n.Call(r.Context(), http.MethodPost, path, body, nil); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSONCode(w, http.StatusAccepted, map[string]string{})
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
	if err := decodeJSON(r.Body, &req); err != nil || !kube.ValidAction(req.Action) {
		apiError(w, r, http.StatusBadRequest, errors.New("action must be one of start, stop, poweroff, restart, pause, unpause"))
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
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	type v1Snap struct {
		Name    string    `json:"name"`
		Created time.Time `json:"created"`
		Ready   bool      `json:"ready"`
		Auto    bool      `json:"auto"`
	}
	out := []v1Snap{}
	for _, sn := range snaps {
		out = append(out, v1Snap{sn.Name, sn.Created, sn.Ready, sn.Auto})
	}
	writeJSON(w, out)
}

func (s *Server) v1Migrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To string `json:"to"`
	}
	if err := decodeJSON(r.Body, &req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
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
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSONCode(w, http.StatusAccepted, map[string]string{})
}

func (s *Server) v1CreateVM(w http.ResponseWriter, r *http.Request) {
	var req v1NewVM
	if err := decodeJSON(r.Body, &req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
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
	if err := checkVMSpec(spec); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := (remoteOps{n}).CreateVM(r.Context(), createVMReq{Spec: spec, Pool: req.Pool}); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSONCode(w, http.StatusCreated, map[string]string{"host": n.Name, "name": spec.Name})
}

func (s *Server) accountTokens(w http.ResponseWriter, r *http.Request) {
	s.renderAccountTokens(w, r, "", nil)
}

func (s *Server) renderAccountTokens(w http.ResponseWriter, r *http.Request, notice string, err error) {
	sess := session(r)
	ts, terr := s.auth.Tokens(r.Context())
	ts = slices.DeleteFunc(ts, func(t auth.Token) bool { return t.Org != sess.Org || t.User != sess.User })
	if err == nil {
		err = terr
	}
	failed(w, r, err)
	s.render(w, r, "tokens.html", "layout", map[string]any{"Tokens": ts, "Notice": notice, "Error": errText(err)})
}

func (s *Server) createAccountToken(w http.ResponseWriter, r *http.Request) {
	sess, name := session(r), r.FormValue("name")
	if !validName.MatchString(name) {
		s.renderAccountTokens(w, r, "", errors.New("invalid token name"))
		return
	}
	t := auth.Token{Name: name, Org: sess.Org, User: sess.User}
	if r.FormValue("read") != "" {
		t.Scope = auth.ReadScope
	}
	tok, err := s.auth.AddToken(r.Context(), t)
	if err != nil {
		s.renderAccountTokens(w, r, "", err)
		return
	}
	s.renderAccountTokens(w, r, "Token "+name+" (shown once): "+tok, nil)
}

func (s *Server) deleteAccountToken(w http.ResponseWriter, r *http.Request) {
	sess := session(r)
	if err := s.auth.RemoveToken(r.Context(), sess.Org, sess.User, r.PathValue("name")); err != nil {
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
		apiError(w, r, http.StatusConflict, err)
		return
	}
	defer resp.Body.Close()
	copyStream(w, resp)
}
