package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const volumeLabel = "argus.io/volume"

var (
	zfsNodeGVR   = schema.GroupVersionResource{Group: "zfs.openebs.io", Version: "v1", Resource: "zfsnodes"}
	zfsVolumeGVR = schema.GroupVersionResource{Group: "zfs.openebs.io", Version: "v1", Resource: "zfsvolumes"}
	volSnapGVR   = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
)

type Pool struct {
	Name        string
	Free        int64
	Used        int64
	Provisioned int64
	Volumes     int
	Snapshots   int
}

type Volume struct {
	Name      string
	Size      int64
	Phase     string
	Progress  string
	Created   time.Time
	Image     bool
	Owner     string
	UsedBy    string
	Snapshots []string
}

func (v Volume) Standalone() bool { return !v.Image && v.Owner == "" }

func quantity(s string) int64 {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

func (c *Client) Pools(ctx context.Context) ([]Pool, error) {
	nodes, err := c.dyn.Resource(zfsNodeGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	vols, err := c.dyn.Resource(zfsVolumeGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	snaps, err := c.dyn.Resource(volSnapGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Pool
	for _, n := range nodes.Items {
		pools, _, _ := unstructured.NestedSlice(n.Object, "pools")
		for _, p := range pools {
			m := p.(obj)
			pool := Pool{Name: fmt.Sprint(m["name"]), Free: quantity(fmt.Sprint(m["free"])), Used: quantity(fmt.Sprint(m["used"])), Snapshots: len(snaps.Items)}
			for _, v := range vols.Items {
				if name, _, _ := unstructured.NestedString(v.Object, "spec", "poolName"); name == pool.Name {
					capacity, _, _ := unstructured.NestedString(v.Object, "spec", "capacity")
					pool.Provisioned += quantity(capacity)
					pool.Volumes++
				}
			}
			out = append(out, pool)
		}
	}
	return out, nil
}

func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	dvs, err := c.dyn.Resource(dvGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	vms, err := c.dyn.Resource(vmGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	snaps, err := c.dyn.Resource(volSnapGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{LabelSelector: volumeLabel})
	if err != nil {
		return nil, err
	}
	usedBy := map[string]string{}
	for _, vm := range vms.Items {
		vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		for _, v := range vols {
			if dv, ok := v.(obj)["dataVolume"].(obj); ok {
				usedBy[fmt.Sprint(dv["name"])] = vm.GetName()
			}
		}
	}
	bySource := map[string][]string{}
	for _, s := range snaps.Items {
		bySource[s.GetLabels()[volumeLabel]] = append(bySource[s.GetLabels()[volumeLabel]], s.GetName())
	}
	out := make([]Volume, 0, len(dvs.Items))
	for _, dv := range dvs.Items {
		o := dv.Object
		v := Volume{Name: dv.GetName(), Created: dv.GetCreationTimestamp().Time, Image: dv.GetLabels()[imageLabel] == "true", UsedBy: usedBy[dv.GetName()], Snapshots: bySource[dv.GetName()]}
		size, _, _ := unstructured.NestedString(o, "spec", "storage", "resources", "requests", "storage")
		v.Size = quantity(size)
		v.Phase, _, _ = unstructured.NestedString(o, "status", "phase")
		v.Progress, _, _ = unstructured.NestedString(o, "status", "progress")
		for _, ref := range dv.GetOwnerReferences() {
			if ref.Kind == "VirtualMachine" {
				v.Owner = ref.Name
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) CreateVolume(ctx context.Context, name string, sizeGiB int, url string) error {
	source := obj{"blank": obj{}}
	if url != "" {
		source = obj{"http": obj{"url": url}}
	}
	dv := &unstructured.Unstructured{Object: obj{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata":   obj{"name": name, "labels": obj{volumeLabel: "true"}},
		"spec": obj{
			"source":  source,
			"storage": obj{"resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", sizeGiB)}}},
		},
	}}
	_, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Create(ctx, dv, metav1.CreateOptions{})
	return err
}

func (c *Client) standaloneVolume(ctx context.Context, name string) (Volume, error) {
	vols, err := c.Volumes(ctx)
	if err != nil {
		return Volume{}, err
	}
	for _, v := range vols {
		if v.Name == name {
			if !v.Standalone() {
				return v, fmt.Errorf("%s is not a standalone volume", name)
			}
			return v, nil
		}
	}
	return Volume{}, fmt.Errorf("volume %s not found", name)
}

func (c *Client) DeleteVolume(ctx context.Context, name string) error {
	v, err := c.standaloneVolume(ctx, name)
	if err != nil {
		return err
	}
	if v.UsedBy != "" {
		return fmt.Errorf("%s is attached to %s", name, v.UsedBy)
	}
	if len(v.Snapshots) > 0 {
		return fmt.Errorf("delete the snapshots of %s first", name)
	}
	return c.dyn.Resource(dvGVR).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *Client) AttachVolume(ctx context.Context, ns, vm, volume string) error {
	v, err := c.standaloneVolume(ctx, volume)
	if err != nil {
		return err
	}
	if v.UsedBy != "" {
		return fmt.Errorf("%s is attached to %s", volume, v.UsedBy)
	}
	return c.editStoppedVM(ctx, ns, vm, func(o obj) error {
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
		volumes, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		volumes = append(volumes, obj{"name": disk, "dataVolume": obj{"name": volume}})
		disks = append(disks, obj{"name": disk, "disk": obj{"bus": "virtio"}})
		if err := unstructured.SetNestedSlice(o, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return err
		}
		return unstructured.SetNestedSlice(o, disks, "spec", "template", "spec", "domain", "devices", "disks")
	})
}

func (c *Client) SnapshotVolume(ctx context.Context, name string) error {
	if _, err := c.standaloneVolume(ctx, name); err != nil {
		return err
	}
	snap := &unstructured.Unstructured{Object: obj{
		"apiVersion": "snapshot.storage.k8s.io/v1",
		"kind":       "VolumeSnapshot",
		"metadata":   obj{"name": fmt.Sprintf("%s-%s", name, time.Now().UTC().Format("20060102-150405")), "labels": obj{volumeLabel: name}},
		"spec":       obj{"volumeSnapshotClassName": "zfs", "source": obj{"persistentVolumeClaimName": name}},
	}}
	_, err := c.dyn.Resource(volSnapGVR).Namespace(Namespace).Create(ctx, snap, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteVolumeSnapshot(ctx context.Context, name string) error {
	s, err := c.dyn.Resource(volSnapGVR).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if s.GetLabels()[volumeLabel] == "" {
		return fmt.Errorf("%s is not a volume snapshot", name)
	}
	return c.dyn.Resource(volSnapGVR).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
