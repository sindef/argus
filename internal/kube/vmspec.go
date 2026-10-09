package kube

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
							"interfaces":             []interface{}{obj{"name": "nic0", "model": "virtio", "bridge": obj{}}},
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

func (c *Client) DeleteVM(ctx context.Context, ns, name string) error {
	fg := metav1.DeletePropagationForeground
	return c.dyn.Resource(vmGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &fg})
}
