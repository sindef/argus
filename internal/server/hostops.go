package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"runtime"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/vsphere"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type dashStats struct {
	Host    hostd.Stats `json:"host"`
	VMs     int         `json:"vms"`
	Running int         `json:"running"`
	VCPUs   int         `json:"vcpus"`
	VMMem   int64       `json:"vmMem"`
}

type poolsView struct {
	Pools      []poolRow          `json:"pools"`
	Importable []hostd.Importable `json:"importable"`
}

type poolOpReq struct {
	Spec    hostd.PoolSpec `json:"spec"`
	Confirm string         `json:"confirm"`
}

type netOpReq struct {
	Bridge     hostd.BridgeSpec  `json:"bridge"`
	Delete     string            `json:"delete"`
	Address    hostd.AddressSpec `json:"address"`
	Iface      string            `json:"iface"`
	Migration  bool              `json:"migration"`
	Management bool              `json:"management"`
	Teaming    hostd.Teaming     `json:"teaming"`
}

type vmForm struct {
	Images    []kube.Image    `json:"images"`
	Templates []kube.Template `json:"templates"`
	Networks  []kube.Network  `json:"networks"`
	Pools     []kube.Pool     `json:"pools"`
	Arch      string          `json:"arch"`
}

type createVMReq struct {
	Spec kube.VMSpec `json:"spec"`
	Pool string      `json:"pool"`
}

type hwOpReq struct {
	Hardware kube.Hardware `json:"hardware"`
	Network  string        `json:"network"`
	NIC      string        `json:"nic"`
	Disk     string        `json:"disk"`
	SizeGiB  int64         `json:"sizeGiB"`
	Device   string        `json:"device"`
}

type hostOps interface {
	DashStats(ctx context.Context) (dashStats, error)
	Disks(ctx context.Context) ([]hostd.Disk, error)
	Disk(ctx context.Context, name string) (hostd.DiskDetail, error)
	DiskAction(ctx context.Context, name, action, confirm, test string) error
	Pools(ctx context.Context) (poolsView, error)
	PoolOp(ctx context.Context, name, op string, req poolOpReq) error
	Network(ctx context.Context) (HostNetInfo, error)
	NetworkOp(ctx context.Context, op string, req netOpReq) error
	VMForm(ctx context.Context) (vmForm, error)
	CreateVM(ctx context.Context, req createVMReq) error
	Hardware(ctx context.Context, ns, name string) (kube.Hardware, error)
	HardwareOp(ctx context.Context, ns, name, op string, req hwOpReq) error
	Import(ctx context.Context, source, contentType string, body io.Reader) (string, error)
	VCenterVMs(ctx context.Context, fields map[string]string) ([]vsphere.VM, error)
	DownloadDisk(w http.ResponseWriter, r *http.Request, ns, name, disk string)
	OVF(ctx context.Context, ns, name string) (string, error)
	Devices(ctx context.Context) (hostd.Devices, error)
	DeviceOp(ctx context.Context, req deviceReq) error
}

type deviceReq struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	On   bool   `json:"on"`
}

type opsKey struct{}

func (s *Server) ops(r *http.Request) hostOps {
	if o, ok := r.Context().Value(opsKey{}).(hostOps); ok {
		return o
	}
	return localOps{s}
}

type localOps struct{ s *Server }

var errNoHostd = errors.New("host helper not available")

func (l localOps) DashStats(ctx context.Context) (dashStats, error) {
	if l.s.hostd == nil {
		return dashStats{}, errNoHostd
	}
	st, err := l.s.hostd.Stats(ctx)
	if err != nil {
		return dashStats{}, err
	}
	out := dashStats{Host: st}
	vms, _ := l.s.kube.ListVMs(ctx)
	out.VMs = len(vms)
	for _, v := range vms {
		if v.Running {
			out.Running++
			out.VCPUs += int(v.Cores)
			out.VMMem += quantityBytes(v.Memory)
		}
	}
	return out, nil
}

func (l localOps) Disks(ctx context.Context) ([]hostd.Disk, error) {
	if l.s.hostd == nil {
		return nil, errNoHostd
	}
	return l.s.hostd.Disks(ctx)
}

func (l localOps) Disk(ctx context.Context, name string) (hostd.DiskDetail, error) {
	if l.s.hostd == nil {
		return hostd.DiskDetail{}, errNoHostd
	}
	return l.s.hostd.Disk(ctx, name)
}

func (l localOps) DiskAction(ctx context.Context, name, action, confirm, test string) error {
	if l.s.hostd == nil {
		return errNoHostd
	}
	return l.s.hostd.DiskAction(ctx, name, action, confirm, test)
}

func (l localOps) Pools(ctx context.Context) (poolsView, error) {
	if l.s.hostd == nil {
		return poolsView{}, errNoHostd
	}
	hp, err := l.s.hostd.Pools(ctx)
	kp, _ := l.s.kube.Pools(ctx)
	classes, _ := l.s.kube.PoolClasses(ctx)
	out := poolsView{Importable: hp.Importable}
	for _, p := range hp.Pools {
		row := poolRow{Pool: p, Class: classes[p.Name]}
		if row.Class == "" && !p.System {
			if cerr := l.s.kube.EnsurePoolClass(ctx, p.Name); cerr == nil {
				row.Class = p.Name
			}
		}
		for _, k := range kp {
			if k.Name == p.Name {
				row.Provisioned, row.Volumes = k.Provisioned, k.Volumes
			}
		}
		out.Pools = append(out.Pools, row)
	}
	return out, err
}

func (l localOps) PoolOp(ctx context.Context, name, op string, req poolOpReq) error {
	s := l.s
	if s.hostd == nil {
		return errNoHostd
	}
	switch op {
	case "create":
		if err := s.hostd.CreatePool(ctx, req.Spec); err != nil {
			return err
		}
		return s.kube.EnsurePoolClass(ctx, req.Spec.Name)
	case "add":
		return s.hostd.AddToPool(ctx, name, req.Spec)
	case "import", "force-import":
		if err := s.hostd.ImportPool(ctx, name, op == "force-import"); err != nil {
			return err
		}
		return s.kube.EnsurePoolClass(ctx, name)
	case "export", "destroy":
		if err := s.hostd.ReleasePool(ctx, name, op, req.Confirm); err != nil {
			return err
		}
		return s.kube.DeletePoolClass(ctx, name)
	}
	return errors.New("unknown pool operation")
}

func (l localOps) Network(ctx context.Context) (HostNetInfo, error) {
	if l.s.hostd == nil {
		return HostNetInfo{}, errNoHostd
	}
	hn, err := l.s.hostd.Network(ctx)
	return HostNetInfo{Net: hn, Migration: l.s.kube.MigrationBridge(ctx)}, err
}

func (l localOps) NetworkOp(ctx context.Context, op string, req netOpReq) error {
	s := l.s
	if s.hostd == nil {
		return errNoHostd
	}
	switch op {
	case "bridge":
		return s.hostd.CreateBridge(ctx, req.Bridge)
	case "delete":
		nets, err := s.kube.ListNetworks(ctx)
		if err != nil {
			return err
		}
		for _, n := range nets {
			if n.Bridge == req.Delete {
				return fmt.Errorf("network %s uses %s; delete it first", n.Name, req.Delete)
			}
		}
		if s.kube.MigrationBridge(ctx) == req.Delete {
			return fmt.Errorf("%s carries live migration; move that role first", req.Delete)
		}
		return s.hostd.DeleteBridge(ctx, req.Delete)
	case "address":
		return s.hostd.SetAddress(ctx, req.Address)
	case "teaming":
		return s.hostd.SetTeaming(ctx, req.Iface, req.Teaming)
	case "roles":
		return l.setRoles(ctx, req)
	case "confirm", "rollback":
		return s.hostd.NetworkDecision(ctx, op)
	}
	return errors.New("unknown network operation")
}

func (l localOps) setRoles(ctx context.Context, req netOpReq) error {
	hn, err := l.s.hostd.Network(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(hn.Ifaces, func(i hostd.Iface) bool { return i.Name == req.Iface })
	if i < 0 || hn.Ifaces[i].Master != "" {
		return fmt.Errorf("%s cannot take roles", req.Iface)
	}
	iface := hn.Ifaces[i]
	if req.Migration && l.s.kube.MigrationBridge(ctx) != iface.Name {
		if iface.Kind != "bridge" {
			return errors.New("live migration needs a bridge")
		}
		if err := l.s.kube.SetMigrationBridge(ctx, iface.Name); err != nil {
			return err
		}
	}
	if req.Management != management(hn, iface) {
		return l.s.hostd.SetManagement(ctx, iface.Name, req.Management)
	}
	return nil
}

func (l localOps) VMForm(ctx context.Context) (vmForm, error) {
	imgs, err := l.s.kube.ListImages(ctx)
	nets, nerr := l.s.kube.ListNetworks(ctx)
	tpls, _ := l.s.kube.Templates(ctx)
	return vmForm{Images: imgs, Templates: tpls, Networks: nets, Pools: l.s.poolChoices(ctx), Arch: runtime.GOARCH}, errors.Join(err, nerr)
}

func (l localOps) CreateVM(ctx context.Context, req createVMReq) error {
	spec := req.Spec
	var err error
	if spec.StorageClass, err = l.s.poolClass(ctx, req.Pool); err != nil {
		return err
	}
	if tpl, ok := strings.CutPrefix(spec.Image, "tpl:"); ok {
		tpls, _ := l.s.kube.Templates(ctx)
		i := slices.IndexFunc(tpls, func(t kube.Template) bool { return t.Name == tpl })
		if i < 0 {
			return fmt.Errorf("template %s not found", tpl)
		}
		if int64(spec.DiskGiB) < tpls[i].DiskGiB {
			return fmt.Errorf("disk must be at least %d GiB for template %s", tpls[i].DiskGiB, tpl)
		}
		spec.Image = tpls[i].Root
	}
	return l.s.kube.CreateVM(ctx, spec)
}

func (l localOps) Hardware(ctx context.Context, ns, name string) (kube.Hardware, error) {
	hw, err := l.s.kube.Hardware(ctx, ns, name)
	if err != nil || l.s.hostd == nil {
		return hw, err
	}
	if devs, err := l.s.hostd.Devices(ctx); err == nil {
		labels := deviceLabels(devs)
		for i, c := range hw.DeviceChoices {
			hw.DeviceChoices[i].Label = cmp.Or(labels[c.Name], c.Name)
		}
	}
	return hw, nil
}

func deviceLabels(devs hostd.Devices) map[string]string {
	labels := map[string]string{}
	for _, d := range devs.PCI {
		labels[kube.PCIResource(d.Vendor, d.Device)] = cmp.Or(d.Name, "PCI "+d.Vendor+":"+d.Device)
	}
	for _, u := range devs.USB {
		labels[kube.USBResource(u.Vendor, u.Product)] = cmp.Or(u.Name, "USB "+u.Vendor+":"+u.Product)
	}
	return labels
}

func (l localOps) Devices(ctx context.Context) (hostd.Devices, error) {
	if l.s.hostd == nil {
		return hostd.Devices{}, errNoHostd
	}
	return l.s.hostd.Devices(ctx)
}

func (l localOps) DeviceOp(ctx context.Context, req deviceReq) error {
	if l.s.hostd == nil {
		return errNoHostd
	}
	if err := l.s.hostd.SetPassthrough(ctx, req.Kind, req.ID, req.On); err != nil {
		return err
	}
	devs, err := l.s.hostd.Devices(ctx)
	if err != nil {
		return err
	}
	var pci, usb [][2]string
	for _, d := range devs.PCI {
		if k := [2]string{d.Vendor, d.Device}; d.Passthrough && !slices.Contains(pci, k) {
			pci = append(pci, k)
		}
	}
	for _, u := range devs.USB {
		if k := [2]string{u.Vendor, u.Product}; u.Passthrough && !slices.Contains(usb, k) {
			usb = append(usb, k)
		}
	}
	return l.s.kube.SetPermittedDevices(ctx, pci, usb)
}

func (l localOps) HardwareOp(ctx context.Context, ns, name, op string, req hwOpReq) error {
	k := l.s.kube
	switch op {
	case "save":
		if !slices.Contains(kube.Firmwares(), req.Hardware.Firmware) {
			return errors.New("invalid firmware")
		}
		return k.SetHardware(ctx, ns, name, req.Hardware)
	case "nic-add":
		return k.AddNIC(ctx, ns, name, req.Network)
	case "nic-remove":
		return k.RemoveNIC(ctx, ns, name, req.NIC)
	case "resize":
		return k.ResizeDisk(ctx, ns, name, req.Disk, req.SizeGiB)
	case "device-add":
		return k.AddHostDevice(ctx, ns, name, req.Device)
	case "device-remove":
		return k.RemoveHostDevice(ctx, ns, name, req.Device)
	}
	return errors.New("unknown operation")
}

func (l localOps) Import(ctx context.Context, source, contentType string, body io.Reader) (string, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return "", errors.New("expected a multipart form")
	}
	mr := multipart.NewReader(body, params["boundary"])
	if source == "ova" {
		return l.s.importOVA(ctx, mr)
	}
	form, err := mr.ReadForm(1 << 20)
	if err != nil {
		return "", err
	}
	defer form.RemoveAll()
	f := func(k string) string {
		if v := form.Value[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if source == "vcenter" {
		return l.s.importVCenter(ctx, f)
	}
	return l.s.importESXi(ctx, f)
}

func (l localOps) VCenterVMs(ctx context.Context, fields map[string]string) ([]vsphere.VM, error) {
	return l.s.vcenterVMs(ctx, func(k string) string { return fields[k] })
}

func (l localOps) DownloadDisk(w http.ResponseWriter, r *http.Request, ns, name, disk string) {
	l.s.streamDisk(w, r, ns, name, disk)
}

type remoteOps struct{ n manager.Node }

func (o remoteOps) get(ctx context.Context, path string, out any) error {
	return o.n.Call(ctx, http.MethodGet, path, nil, out)
}

func (o remoteOps) post(ctx context.Context, path string, in any) error {
	return o.n.Call(ctx, http.MethodPost, path, in, nil)
}

func (o remoteOps) DashStats(ctx context.Context) (dashStats, error) {
	var out dashStats
	return out, o.get(ctx, "/api/host/dashboard", &out)
}

func (o remoteOps) Devices(ctx context.Context) (hostd.Devices, error) {
	var out hostd.Devices
	return out, o.get(ctx, "/api/host/devices", &out)
}

func (o remoteOps) DeviceOp(ctx context.Context, req deviceReq) error {
	return o.post(ctx, "/api/host/devices", req)
}

func (o remoteOps) Disks(ctx context.Context) ([]hostd.Disk, error) {
	var out []hostd.Disk
	return out, o.get(ctx, "/api/host/disks", &out)
}

func (o remoteOps) Disk(ctx context.Context, name string) (hostd.DiskDetail, error) {
	var out hostd.DiskDetail
	return out, o.get(ctx, manager.APIPath("/api/host/disks/%s", name), &out)
}

func (o remoteOps) DiskAction(ctx context.Context, name, action, confirm, test string) error {
	return o.post(ctx, manager.APIPath("/api/host/disks/%s/%s", name, action), map[string]string{"confirm": confirm, "test": test})
}

func (o remoteOps) Pools(ctx context.Context) (poolsView, error) {
	var out poolsView
	return out, o.get(ctx, "/api/host/pools", &out)
}

func (o remoteOps) PoolOp(ctx context.Context, name, op string, req poolOpReq) error {
	if name == "" {
		name = req.Spec.Name
	}
	return o.post(ctx, manager.APIPath("/api/host/pools/%s/%s", name, op), req)
}

func (o remoteOps) Network(ctx context.Context) (HostNetInfo, error) {
	var out HostNetInfo
	return out, o.get(ctx, "/api/host/network", &out)
}

func (o remoteOps) NetworkOp(ctx context.Context, op string, req netOpReq) error {
	if op == "bridge" {
		return o.post(ctx, "/api/host/network/bridge", req.Bridge)
	}
	return o.post(ctx, manager.APIPath("/api/host/network/%s", op), req)
}

func (o remoteOps) VMForm(ctx context.Context) (vmForm, error) {
	var out vmForm
	return out, o.get(ctx, "/api/host/vm-form", &out)
}

func (o remoteOps) CreateVM(ctx context.Context, req createVMReq) error {
	return o.post(ctx, "/api/vms", req)
}

func (o remoteOps) Hardware(ctx context.Context, ns, name string) (kube.Hardware, error) {
	var out kube.Hardware
	return out, o.get(ctx, manager.APIPath("/api/vms/%s/%s/hardware", ns, name), &out)
}

func (o remoteOps) HardwareOp(ctx context.Context, ns, name, op string, req hwOpReq) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/hardware/%s", ns, name, op), req)
}

func (o remoteOps) Import(ctx context.Context, source, contentType string, body io.Reader) (string, error) {
	resp, err := o.n.Open(ctx, http.MethodPost, manager.APIPath("/api/vms/import/%s", source), http.Header{"Content-Type": {contentType}}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct{ Name string }
	return out.Name, decodeJSON(resp.Body, &out)
}

func (o remoteOps) VCenterVMs(ctx context.Context, fields map[string]string) ([]vsphere.VM, error) {
	var out []vsphere.VM
	return out, o.n.Call(ctx, http.MethodPost, "/api/vms/import/vcenter-vms", fields, &out)
}

func (o remoteOps) DownloadDisk(w http.ResponseWriter, r *http.Request, ns, name, disk string) {
	path := manager.APIPath("/api/vms/%s/%s/disks/%s/download", ns, name, disk)
	if r.URL.Query().Get("format") == "vmdk" {
		path += "?format=vmdk"
	}
	resp, err := o.n.Open(r.Context(), http.MethodGet, path, nil, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	io.Copy(w, resp.Body)
}

func (s *Server) apiOp(w http.ResponseWriter, r *http.Request, in any, fn func() (any, error)) {
	if in != nil {
		if err := decodeJSON(r.Body, in); err != nil {
			apiError(w, r, http.StatusBadRequest, err)
			return
		}
	}
	out, err := fn()
	if apierrors.IsNotFound(err) {
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	if out == nil {
		out = map[string]string{}
	}
	writeJSON(w, out)
}

func (s *Server) hostAPIRoutes() {
	l := localOps{s}
	routes := map[string]http.HandlerFunc{
		"GET /api/host/dashboard": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.DashStats(r.Context()) })
		},
		"GET /api/host/devices": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.Devices(r.Context()) })
		},
		"POST /api/host/devices": func(w http.ResponseWriter, r *http.Request) {
			var req deviceReq
			s.apiOp(w, r, &req, func() (any, error) { return nil, l.DeviceOp(r.Context(), req) })
		},
		"GET /api/host/disks": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.Disks(r.Context()) })
		},
		"GET /api/host/disks/{name}": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.Disk(r.Context(), r.PathValue("name")) })
		},
		"POST /api/host/disks/{name}/{action}": func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Confirm, Test string }
			s.apiOp(w, r, &req, func() (any, error) {
				return nil, l.DiskAction(r.Context(), r.PathValue("name"), r.PathValue("action"), req.Confirm, req.Test)
			})
		},
		"GET /api/host/pools": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.Pools(r.Context()) })
		},
		"POST /api/host/pools/{name}/{op}": func(w http.ResponseWriter, r *http.Request) {
			var req poolOpReq
			s.apiOp(w, r, &req, func() (any, error) { return nil, l.PoolOp(r.Context(), r.PathValue("name"), r.PathValue("op"), req) })
		},
		"POST /api/host/network/{op}": func(w http.ResponseWriter, r *http.Request) {
			var req netOpReq
			var in any = &req
			if r.PathValue("op") == "bridge" {
				in = &req.Bridge
			}
			s.apiOp(w, r, in, func() (any, error) { return nil, l.NetworkOp(accessCtx(r), r.PathValue("op"), req) })
		},
		"GET /api/host/network/bridges/{name}/ports": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) {
				if s.hostd == nil {
					return nil, errNoHostd
				}
				return s.hostd.BridgePorts(r.Context(), r.PathValue("name"))
			})
		},
		"POST /api/host/firewall": func(w http.ResponseWriter, r *http.Request) {
			var vms []hostd.FWVM
			s.apiOp(w, r, &vms, func() (any, error) {
				if s.hostd == nil {
					return nil, errNoHostd
				}
				return nil, s.hostd.Firewall(r.Context(), vms)
			})
		},
		"POST /api/host/cpu-baseline": func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			s.apiOp(w, r, &req, func() (any, error) { return nil, s.kube.SetCPUBaseline(r.Context(), req.Model) })
		},
		"GET /api/host/vm-form": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.VMForm(r.Context()) })
		},
		"POST /api/vms": func(w http.ResponseWriter, r *http.Request) {
			var req createVMReq
			s.apiOp(w, r, &req, func() (any, error) { return nil, l.CreateVM(r.Context(), req) })
		},
		"GET /api/vms/{ns}/{name}/hardware": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) { return l.Hardware(r.Context(), r.PathValue("ns"), r.PathValue("name")) })
		},
		"POST /api/vms/{ns}/{name}/hardware/{op}": func(w http.ResponseWriter, r *http.Request) {
			var req hwOpReq
			s.apiOp(w, r, &req, func() (any, error) {
				return nil, l.HardwareOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), req)
			})
		},
		"POST /api/vms/import/{source}": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) {
				name, err := l.Import(r.Context(), r.PathValue("source"), r.Header.Get("Content-Type"), r.Body)
				return map[string]string{"name": name}, err
			})
		},
		"POST /api/vms/import/vcenter-vms": func(w http.ResponseWriter, r *http.Request) {
			var fields map[string]string
			s.apiOp(w, r, &fields, func() (any, error) { return l.VCenterVMs(r.Context(), fields) })
		},
		"GET /api/vms/{ns}/{name}/ovf": func(w http.ResponseWriter, r *http.Request) {
			x, err := l.OVF(r.Context(), r.PathValue("ns"), r.PathValue("name"))
			if err != nil {
				apiError(w, r, http.StatusNotFound, err)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, x)
		},
		"GET /api/vms/{ns}/{name}/disks/{disk}/download": func(w http.ResponseWriter, r *http.Request) {
			s.streamDisk(w, r, r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk"))
		},
	}
	for pattern, h := range routes {
		s.mux.HandleFunc(pattern, s.require(auth.Admin, h))
	}
}

func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

func accessCtx(r *http.Request) context.Context {
	return hostd.WithAccess(r.Context(), r.Host)
}
