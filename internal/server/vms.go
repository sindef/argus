package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"

	"github.com/gorilla/websocket"
)

type selection map[string]bool

func (sel selection) Has(ns, name string) bool { return sel[ns+"/"+name] }

var powerActions = [][3]string{
	{"start", "Power on", "play"},
	{"stop", "Power off", "power"},
	{"restart", "Restart", "rotate-cw"},
	{"pause", "Pause", "pause"},
	{"unpause", "Resume", "play"},
}

var drawerTabs = []string{"overview", "hardware", "snapshots", "backups", "logs"}

func filterVMs(vms []kube.VM, q string) []kube.VM {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return vms
	}
	out := vms[:0]
	for _, v := range vms {
		hay := strings.ToLower(v.Name + " " + v.DisplayName + " " + strings.Join(v.IPs(), " ") + " " + strings.Join(v.Tags, " "))
		if strings.Contains(hay, q) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) vmData(r *http.Request) map[string]any {
	vms, err := s.kube.ListVMs(r.Context())
	sel := selection{}
	for _, v := range r.Form["sel"] {
		sel[v] = true
	}
	running, cores := 0, int64(0)
	for _, v := range vms {
		if v.Running {
			running++
			cores += v.Cores
		}
	}
	return map[string]any{
		"Total":        len(vms),
		"Running":      running,
		"Cores":        cores,
		"VMs":          filterVMs(vms, r.FormValue("q")),
		"Selected":     sel,
		"PowerActions": powerActions,
		"CanOperate":   session(r).Role.Can(auth.Operator),
		"Error":        errText(err),
	}
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	data := s.vmData(r)
	data["Nav"] = "vms"
	if ns, name := r.PathValue("ns"), r.PathValue("name"); name != "" {
		data["Open"] = kube.VM{Namespace: ns, Name: name}
	}
	s.render(w, r, "vms.html", "layout", data)
}

func (s *Server) vmTable(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.render(w, r, "vms.html", "table", s.vmData(r))
}

func (s *Server) navVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.kube.ListVMs(r.Context())
	s.render(w, r, "vms.html", "nav-vms", map[string]any{"VMs": vms, "CanOperate": session(r).Role.Can(auth.Operator)})
}

func (s *Server) vmDrawer(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tab := r.URL.Query().Get("tab")
	if !slices.Contains(drawerTabs, tab) {
		tab = "overview"
	}
	data := map[string]any{"VM": vm, "Tab": tab, "Tabs": drawerTabs, "CanOperate": session(r).Role.Can(auth.Operator)}
	switch tab {
	case "snapshots":
		data["Snapshots"], err = s.kube.ListSnapshots(r.Context(), vm.Namespace, vm.Name)
	case "logs":
		data["Events"], err = s.kube.Events(r.Context(), vm.Namespace, vm.Name, 50)
	case "hardware":
		pools := s.poolChoices(r.Context())
		data["Move"] = map[string]any{"P": "/vms/" + vm.Namespace + "/" + vm.Name, "Pools": pools}
		data["PoolOf"] = poolNames(pools)
	case "backups":
		data["Backups"] = map[string]any{"B": s.vmBackupInfo(r.Context(), vm.Namespace, vm.Name), "P": "/vms/" + vm.Namespace + "/" + vm.Name, "H": "/vms/" + vm.Namespace + "/" + vm.Name, "Name": vm.Name, "CanOperate": data["CanOperate"]}
	}
	data["Error"] = errText(err)
	s.render(w, r, "vms.html", "drawer", data)
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	act, sel := r.PathValue("action"), r.Form["sel"]
	if !s.permits(r, actionPrivilege(act)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if len(sel) == 0 {
		flash(w, r, errors.New("select at least one VM"))
		return
	}
	single := act == "clone" || act == "rename" || act == "template-copy"
	if single && len(sel) != 1 {
		flash(w, r, errors.New("select exactly one VM"))
		return
	}
	prompt := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	var errs []string
	for _, v := range sel {
		ns, name, ok := strings.Cut(v, "/")
		if !ok {
			continue
		}
		err := vmAction(r.Context(), s.ops(r), ns, name, act, prompt)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		log.Printf("%s: %s %s/%s %s", session(r).User, act, ns, name, prompt)
	}
	if len(errs) > 0 {
		flash(w, r, errors.New(strings.Join(errs, "; ")))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	kind := "vnc"
	if r.URL.Query().Get("type") == "serial" {
		kind = "console"
	}
	s.render(w, r, "console.html", "layout", map[string]any{"Namespace": r.PathValue("ns"), "Name": r.PathValue("name"), "Kind": kind, "Page": "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name"), "Media": "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/media", "WS": "/ws/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/"})
}

func (s *Server) consoleWS(w http.ResponseWriter, r *http.Request) {
	s.record(s.auditUser(session(r).Org, session(r).User), "open console of "+r.PathValue("ns")+"/"+r.PathValue("name"), true, "")
	ns, name, kind := r.PathValue("ns"), r.PathValue("name"), r.PathValue("kind")
	backend, err := s.kube.DialConsole(r.Context(), ns, name, kind)
	if err != nil {
		log.Printf("%s: %v", session(r).User, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()
	up := websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}
	client, err := up.Upgrade(w, r, nil)
	if err == nil {
		client.SetReadLimit(1 << 20)
	}
	if err != nil {
		return
	}
	defer client.Close()
	log.Printf("%s: %s %s/%s", session(r).User, kind, ns, name)
	done := make(chan struct{}, 2)
	go pipe(client, backend, done)
	go pipe(backend, client, done)
	s.waitConsole(r, done)
}

func (s *Server) sessionValid(r *http.Request) bool {
	if tok := session(r).Token; tok != "" {
		_, ok := s.auth.Session(tok)
		return ok
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		t, valid := s.auth.CheckToken(r.Context(), tok)
		return valid && t.Scope == ""
	}
	return false
}

func (s *Server) waitConsole(r *http.Request, done <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if !s.sessionValid(r) {
				log.Printf("%s: console closed, session revoked", session(r).User)
				return
			}
		}
	}
}

func pipe(dst, src *websocket.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		_, msg, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return
		}
	}
}

func (s *Server) newVM(w http.ResponseWriter, r *http.Request) {
	f, err := s.ops(r).VMForm(r.Context())
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "modal"
	}
	slices.SortStableFunc(f.Pools, func(a, b kube.Pool) int { return cmp.Compare(b.Free, a.Free) })
	s.render(w, r, "vmnew.html", tmpl, map[string]any{"Images": f.Images, "Templates": f.Templates, "Networks": f.Networks, "Pools": f.Pools, "Error": errText(err)})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	spec := kube.VMSpec{
		Name:     r.FormValue("name"),
		Image:    r.FormValue("image"),
		CDROM:    r.FormValue("cdrom"),
		Network:  r.FormValue("network"),
		UserData: r.FormValue("userdata"),
		Start:    r.FormValue("start") != "",
		Guest: kube.Guest{
			Hostname: strings.TrimSpace(r.FormValue("hostname")),
			User:     strings.TrimSpace(r.FormValue("user")),
			SSHKeys:  slices.DeleteFunc(strings.Split(strings.TrimSpace(r.FormValue("ssh_keys")), "\n"), func(k string) bool { return strings.TrimSpace(k) == "" }),
			Password: r.FormValue("password"),
			Address:  map[bool]string{true: strings.TrimSpace(r.FormValue("address"))}[r.FormValue("ipmode") == "static"],
			Gateway:  strings.TrimSpace(r.FormValue("gateway")),
			DNS:      fields(r.FormValue("dns")),
		},
	}
	if r.FormValue("os") == "windows" {
		spec.Guest = kube.Guest{}
		spec.Windows = &kube.WindowsGuest{
			ComputerName:   strings.TrimSpace(r.FormValue("computer_name")),
			AdminPassword:  r.FormValue("admin_password"),
			TimeZone:       strings.TrimSpace(r.FormValue("time_zone")),
			ProductKey:     strings.TrimSpace(r.FormValue("product_key")),
			Domain:         strings.TrimSpace(r.FormValue("domain")),
			DomainUser:     strings.TrimSpace(r.FormValue("domain_user")),
			DomainPassword: r.FormValue("domain_password"),
			Commands:       slices.DeleteFunc(strings.Split(strings.ReplaceAll(r.FormValue("commands"), "\r", ""), "\n"), func(c string) bool { return strings.TrimSpace(c) == "" }),
			VirtIO:         r.FormValue("virtio") != "",
		}
	}
	err := intField(r, "cores", 1, 64, &spec.Cores)
	if err == nil {
		err = intField(r, "memory", 128, 262144, &spec.MemoryMiB)
	}
	if err == nil {
		err = intField(r, "disk", 1, 4096, &spec.DiskGiB)
	}
	if err == nil {
		err = checkVMSpec(spec)
	}
	if err == nil {
		err = s.ops(r).CreateVM(r.Context(), createVMReq{Spec: spec, Pool: r.FormValue("pool")})
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: create vm %s", session(r).User, spec.Name)
	if base, ok := r.Context().Value(baseKey{}).(string); ok {
		redirect(w, r, strings.Replace(base, "/hosts/", "/nodes/", 1)+"/vms/"+kube.Namespace+"/"+spec.Name)
		return
	}
	redirect(w, r, "/vms/"+kube.Namespace+"/"+spec.Name)
}

func intField(r *http.Request, field string, min, max int, dst *int) error {
	n, err := strconv.Atoi(r.FormValue(field))
	if err != nil || n < min || n > max {
		return fmt.Errorf("%s must be %d-%d", field, min, max)
	}
	*dst = n
	return nil
}

func (s *Server) addDisk(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var size int
	var err error
	if vol := r.FormValue("volume"); r.FormValue("source") == "volume" {
		err = s.kube.AttachVolume(r.Context(), ns, name, vol)
	} else if err = intField(r, "size", 1, 4096, &size); err == nil {
		var class string
		if class, err = s.poolClass(r.Context(), r.FormValue("pool")); err == nil {
			err = s.kube.AddDisk(r.Context(), ns, name, size, class)
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: add disk (%s) to %s/%s", session(r).User, r.FormValue("source"), ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) newDisk(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		flash(w, r, err)
		return
	}
	vols, _ := s.kube.Volumes(r.Context())
	var free []kube.Volume
	for _, v := range vols {
		if v.Standalone() && v.UsedBy == "" {
			free = append(free, v)
		}
	}
	s.render(w, r, "vms.html", "disk-modal", map[string]any{"VM": vm, "Volumes": free, "Pools": s.poolChoices(r.Context())})
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	evs, err := s.kube.Events(r.Context(), kube.Namespace, "", 200)
	s.render(w, r, "events.html", "layout", map[string]any{"Events": evs, "Error": errText(err), "Nav": "events"})
}

var validHostname = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,62}[a-z0-9])?$`)

func checkVMSpec(spec kube.VMSpec) error {
	switch {
	case !validObject.MatchString(spec.Name):
		return errors.New("invalid name")
	case spec.Network == "":
		return errors.New("network required")
	case spec.Cores < 1 || spec.Cores > 64:
		return errors.New("cores must be 1-64")
	case spec.MemoryMiB < 128 || spec.MemoryMiB > 262144:
		return errors.New("memory must be 128-262144 MiB")
	case spec.DiskGiB < 1 || spec.DiskGiB > 4096:
		return errors.New("disk must be 1-4096 GiB")
	}
	if spec.Windows != nil {
		return spec.Windows.Validate()
	}
	return validGuest(spec.Guest)
}

func validGuest(g kube.Guest) error {
	switch {
	case g.Hostname != "" && !validHostname.MatchString(g.Hostname):
		return errors.New("invalid hostname")
	case g.User != "" && !validName.MatchString(g.User):
		return errors.New("invalid user name")
	case (len(g.SSHKeys) > 0 || g.Password != "") && g.User == "":
		return errors.New("SSH keys and password need a user")
	}
	if g.Address != "" {
		if _, _, err := net.ParseCIDR(g.Address); err != nil {
			return errors.New("address must be a CIDR, e.g. 192.168.1.20/24")
		}
	}
	if g.Gateway != "" && net.ParseIP(g.Gateway) == nil {
		return errors.New("invalid gateway")
	}
	for _, d := range g.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("invalid DNS server %q", d)
		}
	}
	for _, k := range g.SSHKeys {
		if !strings.HasPrefix(strings.TrimSpace(k), "ssh-") && !strings.HasPrefix(strings.TrimSpace(k), "ecdsa-") {
			return errors.New("SSH keys must be OpenSSH public keys")
		}
	}
	return nil
}

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$`)

func parseTags(v string) ([]string, error) {
	var tags []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t == "" || slices.Contains(tags, t) {
			continue
		}
		if !validTag.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q", t)
		}
		tags = append(tags, t)
	}
	return tags, nil
}

var (
	validFolder = regexp.MustCompile(`^[^/\x00-\x1f]{1,64}(/[^/\x00-\x1f]{1,64}){0,7}$`)
	validAttr   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)
)

func (s *Server) organize(ctx context.Context, ns, name, tagList, folder, attrs string) error {
	tags, err := parseTags(tagList)
	if err != nil {
		return err
	}
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder != "" && !validFolder.MatchString(folder) {
		return errors.New("folder must be up to 8 levels separated by /")
	}
	m := map[string]string{}
	for _, line := range strings.Split(attrs, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !validAttr.MatchString(k) || len(v) > 256 {
			return fmt.Errorf("attribute %q must be name=value", line)
		}
		m[k] = v
	}
	if len(m) > 32 {
		return errors.New("at most 32 attributes")
	}
	return s.kube.SetTags(ctx, ns, name, tags, folder, m)
}

func (s *Server) hardwareModal(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	hw, err := s.ops(r).Hardware(r.Context(), ns, name)
	if err != nil {
		flash(w, r, err)
		return
	}
	f, _ := s.ops(r).VMForm(r.Context())
	s.render(w, r, "vms.html", "hardware-modal", map[string]any{"HW": hw, "NS": ns, "Name": name, "Images": f.Images, "Networks": f.Networks, "Firmwares": kube.FirmwaresFor(f.Arch)})
}

func (s *Server) hardwareAction(w http.ResponseWriter, r *http.Request) {
	ns, name, op := r.PathValue("ns"), r.PathValue("name"), r.PathValue("op")
	req := hwOpReq{Hardware: kube.Hardware{Firmware: r.FormValue("firmware"), TPM: r.FormValue("tpm") != "", Monitor: r.FormValue("monitor") != "", CDROM: r.FormValue("cdrom"), CPUReserve: strings.TrimSpace(r.FormValue("cpu_reserve")), CPULimit: strings.TrimSpace(r.FormValue("cpu_limit"))}, Network: r.FormValue("network"), NIC: r.FormValue("nic"), Disk: r.FormValue("disk"), Device: r.FormValue("device"), Thick: r.FormValue("thick") == "true"}
	var err error
	switch op {
	case "save":
		var cores, mem int
		var cps int
		if err = intField(r, "cores", 1, 64, &cores); err == nil {
			err = intField(r, "memory", 128, 262144, &mem)
		}
		if err == nil {
			err = intField(r, "cores_per_socket", 1, 64, &cps)
		}
		req.Hardware.Cores, req.Hardware.MemoryMiB, req.Hardware.CoresPerSocket = int64(cores), int64(mem), int64(cps)
		if v := strings.TrimSpace(r.FormValue("memory_reserve")); v != "" && err == nil {
			var reserve int
			err = intField(r, "memory_reserve", 64, 262144, &reserve)
			req.Hardware.MemoryReserve = int64(reserve)
		}
	case "resize":
		var size int
		err = intField(r, "size", 1, 65536, &size)
		req.SizeGiB = int64(size)
	}
	if err == nil {
		err = s.ops(r).HardwareOp(r.Context(), ns, name, op, req)
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: hardware %s %s/%s", session(r).User, r.PathValue("op"), ns, name)
	w.Header().Set("HX-Refresh", "true")
}
