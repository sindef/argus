package hostui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/gorilla/websocket"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/ui"
	"argus/internal/wire"
)

func VMAction(ctx context.Context, o Ops, ns, name, act, prompt string) error {
	switch {
	case kube.ValidAction(act):
		return o.Power(ctx, ns, name, act)
	case act == "delete":
		return o.DeleteVM(ctx, ns, name)
	case act == "snapshot":
		return o.SnapshotOp(ctx, ns, name, "", "")
	case act == "rename":
		return o.Meta(ctx, ns, name, act, wire.MetaRequest{Value: prompt})
	case act == "clone", act == "template-copy":
		return o.Meta(ctx, ns, name, "clone", wire.MetaRequest{Value: prompt, Template: act == "template-copy"})
	}
	return ui.ErrUnknownAction
}

func formMap(r *http.Request) map[string]string {
	r.ParseForm()
	m := map[string]string{}
	for k := range r.Form {
		m[k] = r.FormValue(k)
	}
	return m
}

func (s *UI) vmDone(w http.ResponseWriter, r *http.Request, err error, what string) {
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s", ui.Session(r).User, what, r.PathValue("ns"), r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *UI) Action(w http.ResponseWriter, r *http.Request) {
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if !ui.Permits(r, ActionPrivilege(act)) {
		http.Error(w, "you do not have permission for this action", http.StatusForbidden)
		return
	}
	prompt := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	err := VMAction(r.Context(), s.Ops(r), ns, name, act, prompt)
	if err == nil && act == "delete" {
		log.Printf("%s: delete %s/%s", ui.Session(r).User, ns, name)
		ui.Redirect(w, r, "/")
		return
	}
	s.vmDone(w, r, err, act+" "+prompt)
}

func (s *UI) SnapshotOp(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.Ops(r).SnapshotOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("snap"), r.PathValue("op")), "snapshot "+r.PathValue("op")+" "+r.PathValue("snap"))
}

func (s *UI) RemoveDisk(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.Ops(r).RemoveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")), "remove disk "+r.PathValue("disk"))
}

func (s *UI) MoveDiskForm(w http.ResponseWriter, r *http.Request) {
	req := wire.MoveDiskRequest{Disk: r.FormValue("disk"), Pool: r.FormValue("pool")}
	s.vmDone(w, r, s.Ops(r).MoveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), req), "move disk "+req.Disk+" to "+req.Pool)
}

func (s *UI) SetTags(w http.ResponseWriter, r *http.Request) {
	req := wire.TagsRequest{Tags: r.FormValue("tags"), Folder: r.FormValue("folder"), Attributes: r.FormValue("attributes")}
	s.vmDone(w, r, s.Ops(r).SetTags(r.Context(), r.PathValue("ns"), r.PathValue("name"), req), "organize")
}

func (s *UI) VMBackupForm(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.Ops(r).BackupOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), formMap(r)), "backup "+r.PathValue("op"))
}

func (s *UI) ScheduleForm(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.Ops(r).ScheduleOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), formMap(r)), "schedule "+r.PathValue("op"))
}

func (s *UI) Bulk(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	act, sel := r.PathValue("action"), r.Form["sel"]
	if !ui.Permits(r, ActionPrivilege(act)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if len(sel) == 0 {
		ui.Flash(w, r, errors.New("select at least one VM"))
		return
	}
	single := act == "clone" || act == "rename" || act == "template-copy"
	if single && len(sel) != 1 {
		ui.Flash(w, r, errors.New("select exactly one VM"))
		return
	}
	prompt := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	var errs []string
	for _, v := range sel {
		ns, name, ok := strings.Cut(v, "/")
		if !ok {
			continue
		}
		err := VMAction(r.Context(), s.Ops(r), ns, name, act, prompt)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		log.Printf("%s: %s %s/%s %s", ui.Session(r).User, act, ns, name, prompt)
	}
	if len(errs) > 0 {
		ui.Flash(w, r, errors.New(strings.Join(errs, "; ")))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *UI) ConsoleWS(w http.ResponseWriter, r *http.Request) {
	ns, name, kind := r.PathValue("ns"), r.PathValue("name"), r.PathValue("kind")
	s.Record(s.AuditUser(ui.Session(r).Org, ui.Session(r).User), "open console of "+path.Join(r.PathValue("node"), ns, name), true, "")
	backend, err := s.Ops(r).DialConsole(r.Context(), ns, name, kind)
	if err != nil {
		log.Printf("%s: %v", ui.Session(r).User, err)
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
	log.Printf("%s: %s %s/%s", ui.Session(r).User, kind, ns, name)
	done := make(chan struct{}, 2)
	go pipe(client, backend, done)
	go pipe(backend, client, done)
	s.WaitConsole(r, done)
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

func (s *UI) NewVM(w http.ResponseWriter, r *http.Request) {
	f, err := s.Ops(r).VMForm(r.Context())
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "modal"
	}
	slices.SortStableFunc(f.Pools, func(a, b kube.Pool) int { return cmp.Compare(b.Free, a.Free) })
	s.Render(w, r, "vmnew.html", tmpl, map[string]any{"Images": f.Images, "Library": f.Library, "Templates": f.Templates, "Networks": f.Networks, "Pools": f.Pools, "Error": ui.ErrText(err)})
}

func (s *UI) createVM(w http.ResponseWriter, r *http.Request) {
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
			DNS:      ui.Fields(r.FormValue("dns")),
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
	err := ui.IntField(r, "cores", 1, 64, &spec.Cores)
	if err == nil {
		err = ui.IntField(r, "memory", 128, 262144, &spec.MemoryMiB)
	}
	if err == nil {
		err = ui.IntField(r, "disk", 1, 4096, &spec.DiskGiB)
	}
	if err == nil {
		err = CheckVMSpec(spec)
	}
	if err == nil {
		err = s.Ops(r).CreateVM(r.Context(), CreateVMReq{Spec: spec, Pool: r.FormValue("pool")})
	}
	if errors.Is(err, ErrQueued) {
		log.Printf("%s: create vm %s queued", ui.Session(r).User, spec.Name)
		w.Header().Set("HX-Refresh", "true")
		return
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: create vm %s", ui.Session(r).User, spec.Name)
	if base := ui.Base(r); base != "" {
		ui.Redirect(w, r, strings.Replace(base, "/hosts/", "/nodes/", 1)+"/vms/"+kube.Namespace+"/"+spec.Name)
		return
	}
	ui.Redirect(w, r, "/vms/"+kube.Namespace+"/"+spec.Name)
}

var validHostname = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,62}[a-z0-9])?$`)

func CheckVMSpec(spec kube.VMSpec) error {
	switch {
	case !ui.ValidObject.MatchString(spec.Name):
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
	case g.User != "" && !ui.ValidName.MatchString(g.User):
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

func (s *UI) hardwareModal(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	hw, err := s.Ops(r).Hardware(r.Context(), ns, name)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	f, _ := s.Ops(r).VMForm(r.Context())
	s.Render(w, r, "vms.html", "hardware-modal", map[string]any{"HW": hw, "NS": ns, "Name": name, "Images": f.Images, "Networks": f.Networks, "Firmwares": kube.FirmwaresFor(f.Arch)})
}

func (s *UI) hardwareAction(w http.ResponseWriter, r *http.Request) {
	ns, name, op := r.PathValue("ns"), r.PathValue("name"), r.PathValue("op")
	req := HWOpReq{Hardware: kube.Hardware{Firmware: r.FormValue("firmware"), TPM: r.FormValue("tpm") != "", Monitor: r.FormValue("monitor") != "", CDROM: r.FormValue("cdrom"), CPUReserve: strings.TrimSpace(r.FormValue("cpu_reserve")), CPULimit: strings.TrimSpace(r.FormValue("cpu_limit"))}, Network: r.FormValue("network"), NIC: r.FormValue("nic"), Disk: r.FormValue("disk"), Device: r.FormValue("device"), Thick: r.FormValue("thick") == "true"}
	var err error
	switch op {
	case "save":
		var cores, mem int
		var cps int
		if err = ui.IntField(r, "cores", 1, 64, &cores); err == nil {
			err = ui.IntField(r, "memory", 128, 262144, &mem)
		}
		if err == nil {
			err = ui.IntField(r, "cores_per_socket", 1, 64, &cps)
		}
		req.Hardware.Cores, req.Hardware.MemoryMiB, req.Hardware.CoresPerSocket = int64(cores), int64(mem), int64(cps)
		if v := strings.TrimSpace(r.FormValue("memory_reserve")); v != "" && err == nil {
			var reserve int
			err = ui.IntField(r, "memory_reserve", 64, 262144, &reserve)
			req.Hardware.MemoryReserve = int64(reserve)
		}
	case "resize":
		var size int
		err = ui.IntField(r, "size", 1, 65536, &size)
		req.SizeGiB = int64(size)
	}
	if err == nil {
		err = s.Ops(r).HardwareOp(r.Context(), ns, name, op, req)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: hardware %s %s/%s", ui.Session(r).User, r.PathValue("op"), ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func ActionPrivilege(act string) auth.Privilege {
	switch act {
	case "delete":
		return auth.VMDelete
	case "snapshot":
		return auth.VMSnapshot
	case "rename", "tags":
		return auth.VMConfig
	case "clone", "template", "untemplate", "template-copy":
		return auth.VMProvision
	}
	return auth.VMPower
}
