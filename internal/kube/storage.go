package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const volumeLabel = "argus.io/volume"

const zfsProvisioner = "zfs.csi.openebs.io"

var (
	scGVR        = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	profileGVR   = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "storageprofiles"}
	zfsNodeGVR   = schema.GroupVersionResource{Group: "zfs.openebs.io", Version: "v1", Resource: "zfsnodes"}
	zfsVolumeGVR = schema.GroupVersionResource{Group: "zfs.openebs.io", Version: "v1", Resource: "zfsvolumes"}
	volSnapGVR   = schema.GroupVersionResource{Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"}
)

type Pool struct {
	Name         string
	StorageClass string
	Free         int64
	Used         int64
	Provisioned  int64
	Volumes      int
	Snapshots    int
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
	Dataset   string
	Allocated int64
	SnapUsed  int64
	Thick     bool
}

func (v Volume) Standalone() bool { return !v.Image && v.Owner == "" }

func (v Volume) Pool() string {
	pool, _, _ := strings.Cut(v.Dataset, "/")
	return pool
}

func (c *Client) volumeDatasets(ctx context.Context) map[string]string {
	out := map[string]string{}
	pvcs, err := c.dyn.Resource(pvcGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	zvols, err := c.dyn.Resource(zfsVolumeGVR).Namespace(zfsNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	pools := map[string]string{}
	for _, z := range zvols.Items {
		pools[z.GetName()], _, _ = unstructured.NestedString(z.Object, "spec", "poolName")
	}
	for _, p := range pvcs.Items {
		if pv, _, _ := unstructured.NestedString(p.Object, "spec", "volumeName"); pools[pv] != "" {
			out[p.GetName()] = pools[pv] + "/" + pv
		}
	}
	return out
}

func (v Volume) SizeGiB() int64 { return (v.Size + 1<<30 - 1) >> 30 }

func (c *Client) ResizeVolume(ctx context.Context, name string, sizeGiB int) error {
	vols, err := c.Volumes(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vols, func(v Volume) bool { return v.Name == name })
	switch {
	case i < 0:
		return fmt.Errorf("volume %s not found", name)
	case !vols[i].Standalone():
		return errors.New("resize VM disks from the VM's hardware settings")
	case int64(sizeGiB) <= vols[i].SizeGiB():
		return fmt.Errorf("volumes can only grow; %s is %d GiB", name, vols[i].SizeGiB())
	}
	patch, _ := json.Marshal(obj{"spec": obj{"resources": obj{"requests": obj{"storage": fmt.Sprintf("%dGi", sizeGiB)}}}})
	_, err = c.dyn.Resource(pvcGVR).Namespace(Namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

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
	classes, _ := c.PoolClasses(ctx)
	var out []Pool
	for _, n := range nodes.Items {
		pools, _, _ := unstructured.NestedSlice(n.Object, "pools")
		for _, p := range pools {
			m := asObj(p)
			pool := Pool{Name: fmt.Sprint(m["name"]), StorageClass: classes[fmt.Sprint(m["name"])], Free: quantity(fmt.Sprint(m["free"])), Used: quantity(fmt.Sprint(m["used"])), Snapshots: len(snaps.Items)}
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
			if dv, ok := asObj(v)["dataVolume"].(obj); ok {
				usedBy[fmt.Sprint(dv["name"])] = vm.GetName()
			}
		}
	}
	bySource := map[string][]string{}
	for _, s := range snaps.Items {
		bySource[s.GetLabels()[volumeLabel]] = append(bySource[s.GetLabels()[volumeLabel]], s.GetName())
	}
	datasets := c.volumeDatasets(ctx)
	out := make([]Volume, 0, len(dvs.Items))
	for _, dv := range dvs.Items {
		o := dv.Object
		v := Volume{Name: dv.GetName(), Created: dv.GetCreationTimestamp().Time, Image: dv.GetLabels()[imageLabel] == "true", UsedBy: usedBy[dv.GetName()], Snapshots: bySource[dv.GetName()], Dataset: datasets[dv.GetName()]}
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

func (c *Client) CreateVolume(ctx context.Context, name string, sizeGiB int, url, class string) error {
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
			"storage": storageSpec(sizeGiB, class),
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
	return c.editVM(ctx, ns, vm, func(o obj, running bool) error {
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
		volumes, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
		vol, dk := diskEntry(disk, volume, running)
		volumes, disks = append(volumes, vol), append(disks, dk)
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

func (c *Client) PoolClasses(ctx context.Context) (map[string]string, error) {
	list, err := c.dyn.Resource(scGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, sc := range list.Items {
		if prov, _, _ := unstructured.NestedString(sc.Object, "provisioner"); prov != zfsProvisioner {
			continue
		}
		if pool, _, _ := unstructured.NestedString(sc.Object, "parameters", "poolname"); pool != "" {
			if _, ok := out[pool]; !ok || sc.GetName() == "zfs" {
				out[pool] = sc.GetName()
			}
		}
	}
	return out, nil
}

func (c *Client) EnsurePoolClass(ctx context.Context, pool string) error {
	classes, err := c.PoolClasses(ctx)
	if err != nil {
		return err
	}
	if _, ok := classes[pool]; !ok {
		sc := &unstructured.Unstructured{Object: obj{
			"apiVersion":           "storage.k8s.io/v1",
			"kind":                 "StorageClass",
			"metadata":             obj{"name": pool},
			"provisioner":          zfsProvisioner,
			"allowVolumeExpansion": true,
			"volumeBindingMode":    "Immediate",
			"parameters":           obj{"poolname": pool, "fstype": "ext4", "compression": "lz4", "thinprovision": "yes"},
		}}
		if _, err := c.dyn.Resource(scGVR).Create(ctx, sc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	name := pool
	if cls, ok := classes[pool]; ok {
		name = cls
	}
	spec := obj{"cloneStrategy": "copy", "claimPropertySets": []any{obj{"accessModes": []any{"ReadWriteOnce"}, "volumeMode": "Block"}}}
	res := c.dyn.Resource(profileGVR)
	prof, err := res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = res.Create(ctx, &unstructured.Unstructured{Object: obj{"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "StorageProfile", "metadata": obj{"name": name}, "spec": spec}}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	unstructured.SetNestedField(prof.Object, spec, "spec")
	_, err = res.Update(ctx, prof, metav1.UpdateOptions{})
	return err
}

func (c *Client) DeletePoolClass(ctx context.Context, pool string) error {
	classes, err := c.PoolClasses(ctx)
	if err != nil {
		return err
	}
	name, ok := classes[pool]
	if !ok || name == "zfs" {
		return nil
	}
	err = c.dyn.Resource(scGVR).Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

var (
	zfsNamespace = "openebs"
	zfsSnapGVR   = schema.GroupVersionResource{Group: "zfs.openebs.io", Version: "v1", Resource: "zfssnapshots"}
	pvGVR        = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
)

func (c *Client) ZFSOrphans(ctx context.Context) (datasets, keep []string, err error) {
	pvs, err := c.dyn.Resource(pvGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	vols, err := c.dyn.Resource(zfsVolumeGVR).Namespace(zfsNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	snaps, err := c.dyn.Resource(zfsSnapGVR).Namespace(zfsNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	bound := map[string]bool{}
	for _, pv := range pvs.Items {
		bound[pv.GetName()] = true
	}
	for _, s := range snaps.Items {
		keep = append(keep, s.GetName())
		if s.GetDeletionTimestamp() == nil {
			bound[s.GetLabels()["openebs.io/persistent-volume"]] = true
		}
	}
	for _, v := range vols.Items {
		pool, _, _ := unstructured.NestedString(v.Object, "spec", "poolName")
		if !bound[v.GetName()] && v.GetDeletionTimestamp() == nil && time.Since(v.GetCreationTimestamp().Time) > 10*time.Minute && pool != "" {
			datasets = append(datasets, pool+"/"+v.GetName())
		}
	}
	return datasets, keep, nil
}

func (c *Client) DeleteZFSVolume(ctx context.Context, name string) error {
	return c.dyn.Resource(zfsVolumeGVR).Namespace(zfsNamespace).Delete(ctx, name, metav1.DeleteOptions{})
}
