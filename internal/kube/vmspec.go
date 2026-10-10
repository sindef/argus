package kube

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
)

type VMSpec struct {
	Name      string
	Cores     int
	MemoryMiB int
	DiskGiB   int
	Image     string
	CDROM     string
	Network   string
	UserData  string
	Start     bool
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
		disks = append(disks, obj{"name": "cdrom", "bootOrder": cdBoot, "cdrom": obj{"bus": "sata"}})
		volumes = append(volumes, obj{"name": "cdrom", "persistentVolumeClaim": obj{"claimName": s.CDROM, "readOnly": true}})
	}
	if s.UserData != "" {
		disks = append(disks, obj{"name": "cloudinit", "disk": obj{"bus": "virtio"}})
		volumes = append(volumes, obj{"name": "cloudinit", "cloudInitNoCloud": obj{"userData": s.UserData}})
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
					"storage": obj{"resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", s.DiskGiB)}}},
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
							"interfaces":             []interface{}{obj{"name": "nic0", "model": "virtio", "bridge": obj{}, "macAddress": randomMAC()}},
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

func randomMAC() string {
	b := make([]byte, 6)
	rand.Read(b)
	b[0] = b[0]&0xfc | 0x02
	return net.HardwareAddr(b).String()
}

func (c *Client) DeleteVM(ctx context.Context, ns, name string) error {
	fg := metav1.DeletePropagationForeground
	return c.dyn.Resource(vmGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
}

func (c *Client) AddDisk(ctx context.Context, ns, name string, sizeGiB int) error {
	return c.editStoppedVM(ctx, ns, name, func(o obj) error {
		disks, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "disks")
		used := map[string]bool{}
		for _, d := range disks {
			used[d.(obj)["name"].(string)] = true
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
				"storage": obj{"resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", sizeGiB)}}},
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
