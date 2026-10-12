package kube

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

type VMSpec struct {
	Name         string
	Cores        int
	MemoryMiB    int
	DiskGiB      int
	Image        string
	CDROM        string
	Network      string
	UserData     string
	Start        bool
	StorageClass string
	Guest        Guest
}

type Guest struct {
	Hostname string
	User     string
	SSHKeys  []string
	Password string
	Address  string
	Gateway  string
	DNS      []string
}

func (g Guest) userData() string {
	if g.Hostname == "" && g.User == "" {
		return ""
	}
	cfg := obj{}
	if g.Hostname != "" {
		cfg["hostname"] = g.Hostname
		cfg["fqdn"] = g.Hostname
	}
	if g.User != "" {
		u := obj{"name": g.User, "sudo": "ALL=(ALL) NOPASSWD:ALL", "shell": "/bin/bash", "lock_passwd": g.Password == ""}
		if len(g.SSHKeys) > 0 {
			u["ssh_authorized_keys"] = g.SSHKeys
		}
		if g.Password != "" {
			u["passwd"] = sha512Crypt(g.Password, cryptSalt())
			cfg["ssh_pwauth"] = true
		}
		cfg["users"] = []any{"default", u}
	}
	b, _ := json.Marshal(cfg)
	return "#cloud-config\n" + string(b) + "\n"
}

func (g Guest) networkData(mac string) string {
	if g.Address == "" {
		return ""
	}
	eth := obj{"match": obj{"macaddress": mac}, "set-name": "eth0", "addresses": []string{g.Address}}
	if g.Gateway != "" {
		eth["routes"] = []any{obj{"to": "default", "via": g.Gateway}}
	}
	if len(g.DNS) > 0 {
		eth["nameservers"] = obj{"addresses": g.DNS}
	}
	b, _ := json.Marshal(obj{"version": 2, "ethernets": obj{"eth0": eth}})
	return string(b)
}

func storageSpec(sizeGiB int, class string) obj {
	st := obj{"resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", sizeGiB)}}}
	if class != "" {
		st["storageClassName"] = class
	}
	return st
}

type obj = map[string]interface{}

func (c *Client) CreateVM(ctx context.Context, s VMSpec) error {
	if err := c.guard(); err != nil {
		return err
	}
	root := s.Name + "-root"
	source := obj{"blank": obj{}}
	if s.Image != "" {
		source = obj{"pvc": obj{"namespace": Namespace, "name": s.Image}}
	}
	rootBoot, cdBoot := int64(1), int64(2)
	if s.Image == "" && s.CDROM != "" {
		rootBoot, cdBoot = 2, 1
	}
	disks := []interface{}{obj{"name": "root", "bootOrder": rootBoot, "disk": obj{"bus": "virtio"}}}
	volumes := []interface{}{obj{"name": "root", "dataVolume": obj{"name": root}}}
	if s.CDROM != "" {
		disks = append(disks, obj{"name": "cdrom", "bootOrder": cdBoot, "cdrom": obj{"bus": cdromBus()}})
		volumes = append(volumes, obj{"name": "cdrom", "persistentVolumeClaim": obj{"claimName": s.CDROM, "readOnly": true}})
	}
	mac := randomMAC()
	userData, networkData := s.UserData, s.Guest.networkData(mac)
	if userData == "" {
		userData = s.Guest.userData()
	}
	if userData != "" || networkData != "" {
		ci := obj{"userData": userData}
		if userData == "" {
			ci["userData"] = "#cloud-config\n{}\n"
		}
		if networkData != "" {
			ci["networkData"] = networkData
		}
		disks = append(disks, obj{"name": "cloudinit", "disk": obj{"bus": "virtio"}})
		volumes = append(volumes, obj{"name": "cloudinit", "cloudInitNoCloud": ci})
	}
	strategy := "Halted"
	if s.Start {
		strategy = "Always"
	}
	vm := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   obj{"name": s.Name},
		"spec": obj{
			"runStrategy": strategy,
			"dataVolumeTemplates": []interface{}{obj{
				"metadata": obj{"name": root},
				"spec": obj{
					"source":  source,
					"storage": storageSpec(s.DiskGiB, s.StorageClass),
				},
			}},
			"template": obj{
				"metadata": obj{"labels": obj{"kubevirt.io/vm": s.Name}},
				"spec": obj{
					"domain": obj{
						"cpu":    obj{"cores": int64(s.Cores)},
						"memory": obj{"guest": fmt.Sprintf("%dMi", s.MemoryMiB)},
						"devices": obj{
							"autoattachPodInterface": false,
							"disks":                  disks,
							"interfaces":             []interface{}{obj{"name": "nic0", "model": "virtio", "bridge": obj{}, "macAddress": mac}},
						},
					},
					"networks": []interface{}{obj{"name": "nic0", "multus": obj{"networkName": s.Network}}},
					"volumes":  volumes,
				},
			},
		},
	}}
	_, err := c.dyn.Resource(vmGVR).Namespace(Namespace).Create(ctx, vm, metav1.CreateOptions{})
	return err
}

func cdromBus() string {
	if runtime.GOARCH == "arm64" {
		return "scsi"
	}
	return "sata"
}

func Firmwares() []string { return FirmwaresFor(runtime.GOARCH) }

func FirmwaresFor(arch string) []string {
	if arch == "arm64" {
		return []string{"uefi"}
	}
	return []string{"bios", "uefi", "uefi-secure"}
}

func randomMAC() string {
	b := make([]byte, 6)
	rand.Read(b)
	b[0] = b[0]&0xfc | 0x02
	return net.HardwareAddr(b).String()
}

func (c *Client) DeleteVM(ctx context.Context, ns, name string) error {
	fg := metav1.DeletePropagationForeground
	if err := c.dyn.Resource(vmGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg}); err != nil {
		return err
	}
	snaps, err := c.ListSnapshots(ctx, ns, name)
	if err != nil {
		return err
	}
	for _, sn := range snaps {
		if err := c.DeleteSnapshot(ctx, ns, sn.Name); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Client) AddDisk(ctx context.Context, ns, name string, sizeGiB int, class string) error {
	return c.editStoppedVM(ctx, ns, name, func(o obj) error {
		disks, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "disks")
		used := map[string]bool{}
		for _, d := range disks {
			used[asStr(asObj(d)["name"])] = true
		}
		n := 1
		for used[fmt.Sprintf("disk%d", n)] {
			n++
		}
		disk := fmt.Sprintf("disk%d", n)
		dv := fmt.Sprintf("%s-%s", name, disk)
		templates, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
		templates = append(templates, obj{
			"metadata": obj{"name": dv},
			"spec": obj{
				"source":  obj{"blank": obj{}},
				"storage": storageSpec(sizeGiB, class),
			},
		})
		volumes, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		volumes = append(volumes, obj{"name": disk, "dataVolume": obj{"name": dv}})
		disks = append(disks, obj{"name": disk, "disk": obj{"bus": "virtio"}})
		if err := unstructured.SetNestedSlice(o, templates, "spec", "dataVolumeTemplates"); err != nil {
			return err
		}
		if err := unstructured.SetNestedSlice(o, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, disks, "spec", "template", "spec", "domain", "devices", "disks")
	})
}

func (c *Client) RemoveDisk(ctx context.Context, ns, name, disk string) error {
	if disk == "root" {
		return fmt.Errorf("cannot remove the root disk")
	}
	owned := ""
	err := c.editStoppedVM(ctx, ns, name, func(o obj) error {
		disks, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "disks")
		volumes, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		templates, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
		disks = without(disks, func(m obj) bool { return m["name"] == disk })
		dv := ""
		volumes = without(volumes, func(m obj) bool {
			if m["name"] != disk {
				return false
			}
			if v, ok := m["dataVolume"].(obj); ok {
				dv, _ = v["name"].(string)
			}
			return true
		})
		before := len(templates)
		templates = without(templates, func(m obj) bool {
			md, _ := m["metadata"].(obj)
			return dv != "" && md["name"] == dv
		})
		if len(templates) < before {
			owned = dv
		}
		if err := unstructured.SetNestedSlice(o, templates, "spec", "dataVolumeTemplates"); err != nil {
			return err
		}
		if err := unstructured.SetNestedSlice(o, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, disks, "spec", "template", "spec", "domain", "devices", "disks")
	})
	if err != nil || owned == "" {
		return err
	}
	return c.dyn.Resource(dvGVR).Namespace(ns).Delete(ctx, owned, metav1.DeleteOptions{})
}

func (c *Client) editStoppedVM(ctx context.Context, ns, name string, fn func(obj) error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("power off %s first", name)
		}
		vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := fn(vm.Object); err != nil {
			return err
		}
		_, err = c.dyn.Resource(vmGVR).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

func without(items []interface{}, drop func(obj) bool) []interface{} {
	out := items[:0]
	for _, it := range items {
		if m, ok := it.(obj); ok && drop(m) {
			continue
		}
		out = append(out, it)
	}
	return out
}

type Template struct {
	Name      string
	Root      string
	Cores     int64
	MemoryMiB int64
	DiskGiB   int64
}

func (c *Client) Templates(ctx context.Context) ([]Template, error) {
	list, err := c.dyn.Resource(vmGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{LabelSelector: TemplateLabel + "=true"})
	if err != nil {
		return nil, err
	}
	var out []Template
	for _, v := range list.Items {
		t := Template{Name: v.GetName()}
		t.Cores, _, _ = unstructured.NestedInt64(v.Object, "spec", "template", "spec", "domain", "cpu", "cores")
		if m, ok, _ := unstructured.NestedString(v.Object, "spec", "template", "spec", "domain", "memory", "guest"); ok {
			if q, err := resource.ParseQuantity(m); err == nil {
				t.MemoryMiB = q.Value() >> 20
			}
		}
		vols, _, _ := unstructured.NestedSlice(v.Object, "spec", "template", "spec", "volumes")
		for _, vol := range vols {
			if asObj(vol)["name"] == "root" {
				t.Root, _, _ = unstructured.NestedString(asObj(vol), "dataVolume", "name")
			}
		}
		tpls, _, _ := unstructured.NestedSlice(v.Object, "spec", "dataVolumeTemplates")
		for _, d := range tpls {
			if n, _, _ := unstructured.NestedString(asObj(d), "metadata", "name"); n == t.Root {
				if s, ok, _ := unstructured.NestedString(asObj(d), "spec", "storage", "resources", "requests", "storage"); ok {
					if q, err := resource.ParseQuantity(s); err == nil {
						t.DiskGiB = q.Value() >> 30
					}
				}
			}
		}
		if t.Root != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

func (c *Client) MoveDisk(ctx context.Context, ns, name, disk, class string) (string, string, error) {
	var old, moved string
	err := c.editStoppedVM(ctx, ns, name, func(o obj) error {
		volumes, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		templates, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
		vi := slices.IndexFunc(volumes, func(v any) bool { return asObj(v)["name"] == disk })
		if vi < 0 {
			return fmt.Errorf("disk %s not found", disk)
		}
		old, _, _ = unstructured.NestedString(volumes[vi].(obj), "dataVolume", "name")
		ti := slices.IndexFunc(templates, func(t any) bool {
			n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
			return old != "" && n == old
		})
		if ti < 0 {
			return fmt.Errorf("disk %s is not a managed disk and cannot be moved", disk)
		}
		storage, _, _ := unstructured.NestedMap(templates[ti].(obj), "spec", "storage")
		if storage["storageClassName"] == class {
			return fmt.Errorf("disk %s is already on that pool", disk)
		}
		storage["storageClassName"] = class
		if class == "" {
			delete(storage, "storageClassName")
		}
		b := make([]byte, 2)
		rand.Read(b)
		moved = fmt.Sprintf("%s-%s-%x", name, disk, b)
		templates[ti] = obj{"metadata": obj{"name": moved}, "spec": obj{"source": obj{"pvc": obj{"namespace": ns, "name": old}}, "storage": storage}}
		unstructured.SetNestedField(asObj(volumes[vi]), moved, "dataVolume", "name")
		if err := unstructured.SetNestedSlice(o, templates, "spec", "dataVolumeTemplates"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, volumes, "spec", "template", "spec", "volumes")
	})
	return old, moved, err
}

func (c *Client) DataVolumePhase(ctx context.Context, ns, name string) (string, error) {
	dv, err := c.dyn.Resource(dvGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	return phase, nil
}

func (c *Client) DeleteDataVolume(ctx context.Context, ns, name string) error {
	err := c.dyn.Resource(dvGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

var pvcGVR = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

type Hardware struct {
	Cores     int64
	MemoryMiB int64
	Firmware  string
	TPM       bool
	CDROM     string
	Disks     []HardwareDisk
	NICs      []NIC
}

type HardwareDisk struct {
	Name    string
	PVC     string
	SizeGiB int64
}

func (c *Client) Hardware(ctx context.Context, ns, name string) (Hardware, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Hardware{}, err
	}
	o := vm.Object
	hw := Hardware{Firmware: Firmwares()[0]}
	hw.Cores, _, _ = unstructured.NestedInt64(o, "spec", "template", "spec", "domain", "cpu", "cores")
	if m, ok, _ := unstructured.NestedString(o, "spec", "template", "spec", "domain", "memory", "guest"); ok {
		if q, err := resource.ParseQuantity(m); err == nil {
			hw.MemoryMiB = q.Value() >> 20
		}
	}
	if _, ok, _ := unstructured.NestedMap(o, "spec", "template", "spec", "domain", "firmware", "bootloader", "efi"); ok {
		hw.Firmware = "uefi"
		if sb, ok, _ := unstructured.NestedBool(o, "spec", "template", "spec", "domain", "firmware", "bootloader", "efi", "secureBoot"); (!ok || sb) && runtime.GOARCH != "arm64" {
			hw.Firmware = "uefi-secure"
		}
	}
	_, hw.TPM, _ = unstructured.NestedMap(o, "spec", "template", "spec", "domain", "devices", "tpm")
	vols, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
	for _, v := range vols {
		vo := asObj(v)
		if vo["name"] == "cdrom" {
			hw.CDROM, _, _ = unstructured.NestedString(vo, "persistentVolumeClaim", "claimName")
			continue
		}
		pvc, _, _ := unstructured.NestedString(vo, "dataVolume", "name")
		if pvc == "" {
			pvc, _, _ = unstructured.NestedString(vo, "persistentVolumeClaim", "claimName")
		}
		if pvc == "" {
			continue
		}
		d := HardwareDisk{Name: asStr(vo["name"]), PVC: pvc}
		if p, err := c.dyn.Resource(pvcGVR).Namespace(ns).Get(ctx, pvc, metav1.GetOptions{}); err == nil {
			if v, ok, _ := unstructured.NestedString(p.Object, "spec", "resources", "requests", "storage"); ok {
				if q, err := resource.ParseQuantity(v); err == nil {
					d.SizeGiB = q.Value() >> 30
				}
			}
		}
		hw.Disks = append(hw.Disks, d)
	}
	hw.NICs = toVM(vm, nil).NICs
	return hw, nil
}

func (c *Client) SetHardware(ctx context.Context, ns, name string, hw Hardware) error {
	return c.editStoppedVM(ctx, ns, name, func(o obj) error {
		dom := []string{"spec", "template", "spec", "domain"}
		unstructured.SetNestedField(o, hw.Cores, append(dom, "cpu", "cores")...)
		unstructured.SetNestedField(o, fmt.Sprintf("%dMi", hw.MemoryMiB), append(dom, "memory", "guest")...)
		unstructured.RemoveNestedField(o, append(dom, "firmware", "bootloader")...)
		unstructured.RemoveNestedField(o, append(dom, "features", "smm")...)
		switch hw.Firmware {
		case "uefi":
			unstructured.SetNestedField(o, obj{"secureBoot": false}, append(dom, "firmware", "bootloader", "efi")...)
		case "uefi-secure":
			if runtime.GOARCH == "arm64" {
				return errors.New("Secure Boot is not available on arm64 hosts")
			}
			unstructured.SetNestedField(o, obj{"secureBoot": true}, append(dom, "firmware", "bootloader", "efi")...)
			unstructured.SetNestedField(o, obj{"enabled": true}, append(dom, "features", "smm")...)
		}
		if hw.TPM {
			unstructured.SetNestedField(o, obj{}, append(dom, "devices", "tpm")...)
		} else {
			unstructured.RemoveNestedField(o, append(dom, "devices", "tpm")...)
		}
		disks, _, _ := unstructured.NestedSlice(o, append(dom, "devices", "disks")...)
		vols, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		disks = without(disks, func(m obj) bool { return m["name"] == "cdrom" })
		vols = without(vols, func(m obj) bool { return m["name"] == "cdrom" })
		if hw.CDROM != "" {
			disks = append(disks, obj{"name": "cdrom", "cdrom": obj{"bus": cdromBus()}})
			vols = append(vols, obj{"name": "cdrom", "persistentVolumeClaim": obj{"claimName": hw.CDROM, "readOnly": true}})
		}
		if err := unstructured.SetNestedSlice(o, disks, append(dom, "devices", "disks")...); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, vols, "spec", "template", "spec", "volumes")
	})
}

func (c *Client) AddNIC(ctx context.Context, ns, name, network string) error {
	return c.editStoppedVM(ctx, ns, name, func(o obj) error {
		ifaces, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "interfaces")
		nets, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "networks")
		used := map[string]bool{}
		for _, i := range ifaces {
			used[asStr(asObj(i)["name"])] = true
		}
		n := 0
		for used[fmt.Sprintf("nic%d", n)] {
			n++
		}
		nic := fmt.Sprintf("nic%d", n)
		ifaces = append(ifaces, obj{"name": nic, "model": "virtio", "bridge": obj{}, "macAddress": randomMAC()})
		nets = append(nets, obj{"name": nic, "multus": obj{"networkName": network}})
		if err := unstructured.SetNestedSlice(o, ifaces, "spec", "template", "spec", "domain", "devices", "interfaces"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, nets, "spec", "template", "spec", "networks")
	})
}

func (c *Client) RemoveNIC(ctx context.Context, ns, name, nic string) error {
	return c.editStoppedVM(ctx, ns, name, func(o obj) error {
		ifaces, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "interfaces")
		if len(ifaces) <= 1 {
			return errors.New("a VM needs at least one network adapter")
		}
		nets, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "networks")
		ifaces = without(ifaces, func(m obj) bool { return m["name"] == nic })
		nets = without(nets, func(m obj) bool { return m["name"] == nic })
		if err := unstructured.SetNestedSlice(o, ifaces, "spec", "template", "spec", "domain", "devices", "interfaces"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, nets, "spec", "template", "spec", "networks")
	})
}

func (c *Client) ResizeDisk(ctx context.Context, ns, name, disk string, sizeGiB int64) error {
	hw, err := c.Hardware(ctx, ns, name)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(hw.Disks, func(d HardwareDisk) bool { return d.Name == disk })
	if i < 0 {
		return fmt.Errorf("disk %s not found", disk)
	}
	if sizeGiB <= hw.Disks[i].SizeGiB {
		return fmt.Errorf("disks can only grow; %s is %d GiB", disk, hw.Disks[i].SizeGiB)
	}
	size := fmt.Sprintf("%dGi", sizeGiB)
	patch, _ := json.Marshal(obj{"spec": obj{"resources": obj{"requests": obj{"storage": size}}}})
	if _, err := c.dyn.Resource(pvcGVR).Namespace(ns).Patch(ctx, hw.Disks[i].PVC, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return err
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	for _, t := range templates {
		if n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name"); n == hw.Disks[i].PVC {
			unstructured.SetNestedField(asObj(t), size, "spec", "storage", "resources", "requests", "storage")
		}
	}
	unstructured.SetNestedSlice(vm.Object, templates, "spec", "dataVolumeTemplates")
	_, err = c.dyn.Resource(vmGVR).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{})
	return err
}

type ImportedVM struct {
	Name      string
	Cores     int64
	MemoryMiB int64
	Firmware  string
	Network   string
	Class     string
	Disks     []ImportedDisk
	MACs      []string
	Compat    bool
	CDROM     string
}

type ImportedDisk struct {
	SizeBytes int64
	Source    map[string]any
}

func (c *Client) CreateImportedVM(ctx context.Context, v ImportedVM) ([]string, error) {
	if err := c.guard(); err != nil {
		return nil, err
	}
	if len(v.Disks) == 0 {
		return nil, errors.New("no disks to import")
	}
	bus, model := "virtio", "virtio"
	if v.Compat {
		bus, model = "sata", "e1000e"
	}
	var templates, disks, volumes, ifaces, networks []any
	var dvs []string
	for i, d := range v.Disks {
		disk := "root"
		if i > 0 {
			disk = fmt.Sprintf("disk%d", i)
		}
		dv := v.Name + "-" + disk
		gib := int((d.SizeBytes + 1<<30 - 1) >> 30)
		templates = append(templates, obj{"metadata": obj{"name": dv}, "spec": obj{"source": d.Source, "storage": storageSpec(gib, v.Class)}})
		dk := obj{"name": disk, "disk": obj{"bus": bus}}
		if i == 0 {
			dk["bootOrder"] = int64(1)
		}
		disks = append(disks, dk)
		volumes = append(volumes, obj{"name": disk, "dataVolume": obj{"name": dv}})
		dvs = append(dvs, dv)
	}
	if v.CDROM != "" {
		disks = append(disks, obj{"name": "cdrom", "cdrom": obj{"bus": cdromBus()}})
		volumes = append(volumes, obj{"name": "cdrom", "persistentVolumeClaim": obj{"claimName": v.CDROM, "readOnly": true}})
	}
	macs := v.MACs
	if len(macs) == 0 {
		macs = []string{""}
	}
	for i, mac := range macs {
		if hw, err := net.ParseMAC(mac); err == nil && len(hw) == 6 && hw[0]&1 == 0 {
			mac = hw.String()
		} else {
			mac = randomMAC()
		}
		nic := fmt.Sprintf("nic%d", i)
		ifaces = append(ifaces, obj{"name": nic, "model": model, "bridge": obj{}, "macAddress": mac})
		networks = append(networks, obj{"name": nic, "multus": obj{"networkName": v.Network}})
	}
	domain := obj{
		"cpu":    obj{"cores": v.Cores},
		"memory": obj{"guest": fmt.Sprintf("%dMi", v.MemoryMiB)},
		"devices": obj{
			"autoattachPodInterface": false,
			"disks":                  disks,
			"interfaces":             ifaces,
		},
	}
	if v.Firmware == "efi" {
		domain["firmware"] = obj{"bootloader": obj{"efi": obj{"secureBoot": false}}}
	}
	vm := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   obj{"name": v.Name},
		"spec": obj{
			"runStrategy":         "Halted",
			"dataVolumeTemplates": templates,
			"template": obj{
				"metadata": obj{"labels": obj{"kubevirt.io/vm": v.Name}},
				"spec": obj{
					"domain":   domain,
					"networks": networks,
					"volumes":  volumes,
				},
			},
		},
	}}
	_, err := c.dyn.Resource(vmGVR).Namespace(Namespace).Create(ctx, vm, metav1.CreateOptions{})
	return dvs, err
}
