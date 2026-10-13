package hostui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/ui"
)

func (s *UI) disksPage(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Ops(r).Disks(r.Context())
	s.Render(w, r, "disks.html", "layout", map[string]any{"Nav": "disks", "Disks": ds, "Error": ui.ErrText(err)})
}

func (s *UI) devicesPage(w http.ResponseWriter, r *http.Request) {
	devs, err := s.Ops(r).Devices(r.Context())
	s.Render(w, r, "devices.html", "layout", map[string]any{"Nav": "devices", "D": devs, "Error": ui.ErrText(err)})
}

func (s *UI) deviceAction(w http.ResponseWriter, r *http.Request) {
	req := DeviceReq{Kind: r.FormValue("kind"), ID: r.FormValue("id"), On: r.FormValue("on") == "true"}
	if err := s.Ops(r).DeviceOp(r.Context(), req); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: passthrough %s %s %v", ui.Session(r).User, req.Kind, req.ID, req.On)
	w.Header().Set("HX-Refresh", "true")
}

func (s *UI) diskModal(w http.ResponseWriter, r *http.Request) {
	dd, err := s.Ops(r).Disk(r.Context(), r.PathValue("name"))
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	s.Render(w, r, "disks.html", "disk-modal", map[string]any{"D": dd})
}

func (s *UI) diskAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	confirm := r.FormValue("confirm")
	if confirm == "" {
		confirm = r.Header.Get("HX-Prompt")
	}
	if err := s.Ops(r).DiskAction(r.Context(), name, action, confirm, r.FormValue("test")); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: disk %s %s %s", ui.Session(r).User, name, action, r.FormValue("test"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, "dashboard.html", "layout", map[string]any{"Nav": "dashboard"})
}

func (s *UI) dashboardStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Ops(r).DashStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	ui.WriteJSON(w, st)
}

func (s *UI) poolsPage(w http.ResponseWriter, r *http.Request) {
	pv, err := s.Ops(r).Pools(r.Context())
	s.Render(w, r, "pools.html", "layout", map[string]any{"Nav": "pools", "Pools": pv.Pools, "Importable": pv.Importable, "Error": ui.ErrText(err)})
}

func (s *UI) poolModal(w http.ResponseWriter, r *http.Request) {
	disks, err := s.Ops(r).Disks(r.Context())
	free := slices.DeleteFunc(disks, func(d hostd.Disk) bool { return !d.Free() })
	s.Render(w, r, "pools.html", "pool-modal", map[string]any{"Disks": free, "Pool": r.PathValue("name"), "Error": ui.ErrText(err)})
}

func vdevs(kind string, width int, disks []string) ([]hostd.Vdev, error) {
	if len(disks) == 0 {
		return nil, nil
	}
	if kind == "stripe" || width <= 0 {
		return []hostd.Vdev{{Type: kind, Disks: disks}}, nil
	}
	if len(disks)%width != 0 {
		return nil, fmt.Errorf("%d disks cannot be split into %s groups of %d", len(disks), kind, width)
	}
	var out []hostd.Vdev
	for i := 0; i < len(disks); i += width {
		out = append(out, hostd.Vdev{Type: kind, Disks: disks[i : i+width]})
	}
	return out, nil
}

func (s *UI) poolSpec(r *http.Request) (hostd.PoolSpec, error) {
	r.ParseForm()
	spec := hostd.PoolSpec{Name: r.FormValue("name"), Cache: r.Form["cache_disks"], Spares: r.Form["spare_disks"]}
	width, _ := strconv.Atoi(r.FormValue("data_width"))
	var err error
	if spec.Data, err = vdevs(r.FormValue("data_type"), width, r.Form["data_disks"]); err != nil {
		return spec, err
	}
	if spec.Log, err = vdevs(r.FormValue("log_type"), 0, r.Form["log_disks"]); err != nil {
		return spec, err
	}
	if spec.Special, err = vdevs(r.FormValue("special_type"), 0, r.Form["special_disks"]); err != nil {
		return spec, err
	}
	disks, err := s.Ops(r).Disks(r.Context())
	if err != nil {
		return spec, err
	}
	selected := slices.Concat(r.Form["data_disks"], r.Form["log_disks"], r.Form["special_disks"], spec.Cache, spec.Spares)
	for _, d := range disks {
		if !slices.Contains(selected, d.Name) || d.Blank() {
			continue
		}
		if r.FormValue("wipe-"+d.Name) != d.Name {
			return spec, fmt.Errorf("%s has data; type %s to confirm erasing it", d.Name, d.Name)
		}
		spec.Wipe = append(spec.Wipe, d.Name)
	}
	return spec, nil
}

func (s *UI) poolAction(w http.ResponseWriter, r *http.Request) {
	name, op := r.PathValue("name"), r.PathValue("op")
	req := PoolOpReq{Confirm: r.FormValue("confirm")}
	if req.Confirm == "" {
		req.Confirm = r.Header.Get("HX-Prompt")
	}
	var err error
	if op == "create" || op == "add" {
		req.Spec, err = s.poolSpec(r)
		if op == "create" {
			name = req.Spec.Name
		}
	}
	if err == nil {
		err = s.Ops(r).PoolOp(r.Context(), name, op, req)
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *UI) createPool(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("op", "create")
	s.poolAction(w, r)
}

func Management(hn hostd.HostNet, i hostd.Iface) bool {
	if !hn.Restricted {
		return i.Master == ""
	}
	return i.Master == "" && slices.Contains(hn.Management, i.Name)
}

func (s *UI) hostNetworkPage(w http.ResponseWriter, r *http.Request) {
	info, err := s.Ops(r).Network(AccessCtx(r))
	var free []hostd.Iface
	for _, i := range info.Net.Ifaces {
		if i.Kind == "ether" && !i.Mgmt {
			free = append(free, i)
		}
	}
	s.Render(w, r, "hostnet.html", "layout", map[string]any{"Nav": "hostnet", "Net": info.Net, "Ports": free, "Migration": info.Migration, "Error": ui.ErrText(err)})
}

func (s *UI) ifaceModal(w http.ResponseWriter, r *http.Request) {
	info, err := s.Ops(r).Network(AccessCtx(r))
	hn := info.Net
	var iface *hostd.Iface
	for i := range hn.Ifaces {
		if hn.Ifaces[i].Name == r.PathValue("name") {
			iface = &hn.Ifaces[i]
		}
	}
	if err == nil && iface == nil {
		err = fmt.Errorf("interface %s not found", r.PathValue("name"))
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	s.Render(w, r, "hostnet.html", "iface-modal", map[string]any{"I": iface, "DNS": hn.DNS, "Management": Management(hn, *iface), "Migration": info.Migration == iface.Name})
}

func (s *UI) hostNetworkAction(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	req := NetOpReq{
		Bridge: BridgeSpec(r),
		Delete: r.FormValue("bridge"),
		Address: hostd.AddressSpec{
			Iface: r.FormValue("iface"), IPv4: r.FormValue("ipv4"), IPv6: r.FormValue("ipv6"),
			Address: strings.TrimSpace(r.FormValue("address")), Gateway: strings.TrimSpace(r.FormValue("gateway")),
			Address6: strings.TrimSpace(r.FormValue("address6")), Gateway6: strings.TrimSpace(r.FormValue("gateway6")),
			DNS: ui.Fields(r.FormValue("dns")),
		},
		Iface:      r.FormValue("iface"),
		Migration:  r.FormValue("migration") != "",
		Management: r.FormValue("management") != "",
	}
	err := s.Ops(r).NetworkOp(AccessCtx(r), r.PathValue("op"), req)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: host network %s %s", ui.Session(r).User, r.PathValue("op"), r.FormValue("iface"))
	w.Header().Set("HX-Refresh", "true")
}

func BridgeSpec(r *http.Request) hostd.BridgeSpec {
	spec := hostd.BridgeSpec{Name: r.FormValue("name"), Ports: r.Form["ports"], BondMode: r.FormValue("bond_mode"), Hash: r.FormValue("hash")}
	spec.MTU, _ = strconv.Atoi(r.FormValue("mtu"))
	if len(spec.Ports) > 1 {
		spec.Bond = "bond-" + spec.Name
		spec.Bond = spec.Bond[:min(len(spec.Bond), 15)]
	}
	return spec
}

func (s *UI) ImportModal(w http.ResponseWriter, r *http.Request) {
	f, _ := s.Ops(r).VMForm(r.Context())
	s.Render(w, r, "vmnew.html", "import-modal", map[string]any{"Networks": f.Networks, "Pools": f.Pools, "Images": f.Images, "Arch": f.Arch})
}

func (s *UI) importVM(w http.ResponseWriter, r *http.Request) {
	name, err := s.Ops(r).Import(r.Context(), r.PathValue("source"), r.Header.Get("Content-Type"), r.Body)
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: import vm %s from %s", ui.Session(r).User, name, r.PathValue("source"))
	if base := ui.Base(r); base != "" {
		ui.Redirect(w, r, strings.Replace(base, "/hosts/", "/nodes/", 1)+"/vms/"+kube.Namespace+"/"+name)
		return
	}
	ui.Redirect(w, r, "/vms/"+kube.Namespace+"/"+name)
}

func (s *UI) vcenterVMsOptions(w http.ResponseWriter, r *http.Request) {
	fields := map[string]string{}
	for _, k := range []string{"host", "user", "password", "fingerprint"} {
		fields[k] = r.FormValue(k)
	}
	vms, err := s.Ops(r).VCenterVMs(r.Context(), fields)
	if err != nil {
		w.Header().Set("HX-Retarget", "#import-vm-error")
		ui.Flash(w, r, err)
		return
	}
	s.Render(w, r, "vmnew.html", "vcenter-vms", map[string]any{"VMs": vms})
}

func (s *UI) exportOVF(w http.ResponseWriter, r *http.Request) {
	x, err := s.Ops(r).OVF(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.ovf"`, r.PathValue("name")))
	io.WriteString(w, x)
}

func MediaSize(r *http.Request) int64 {
	var n int64
	if _, err := fmt.Sscan(r.Header.Get("X-Media-Size"), &n); err == nil && n > 0 {
		return n
	}
	return r.ContentLength
}

func (s *UI) mediaMount(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.Ops(r).MountMedia(r.Context(), ns, name, r.Header.Get("Content-Type"), MediaSize(r), r.Body); err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Write([]byte("Media inserted"))
}

func (s *UI) mediaEject(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.Ops(r).EjectMedia(r.Context(), ns, name); err != nil {
		ui.Flash(w, r, err)
		return
	}
	w.Write([]byte("Media ejected"))
}

func (s *UI) backupsPage(w http.ResponseWriter, r *http.Request) {
	c, err := s.Ops(r).BackupCatalog(r.Context())
	if err == nil && c.Error != "" {
		err = errors.New(c.Error)
	}
	s.Render(w, r, "backups.html", "layout", map[string]any{"Nav": "backups", "Tab": "backups", "Catalog": c, "Error": ui.ErrText(err)})
}

func (s *UI) restoreFromCatalog(w http.ResponseWriter, r *http.Request) {
	vm := r.FormValue("vm")
	if !ui.ValidObject.MatchString(vm) {
		ui.Flash(w, r, errors.New("invalid VM name"))
		return
	}
	if err := s.Ops(r).BackupOp(r.Context(), kube.Namespace, vm, "restore", formMap(r)); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: restore %s backup %s as %s", ui.Session(r).User, vm, r.FormValue("stamp"), r.FormValue("target"))
	w.Header().Set("HX-Refresh", "true")
}
