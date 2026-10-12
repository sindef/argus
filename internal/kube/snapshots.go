package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var (
	vmSnapGVR    = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinesnapshots"}
	vmRestoreGVR = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinerestores"}
)

type Snapshot struct {
	Name     string
	Created  time.Time
	Phase    string
	Ready    bool
	Auto     bool
	Quiesced bool
	Parent   string
	Current  bool
	Depth    int
}

const (
	autoLabel         = "argus.io/auto"
	BackupAnnotation  = "argus.io/backup"
	parentAnnotation  = "argus.io/parent"
	currentAnnotation = "argus.io/current"
)

func (c *Client) vmSnapshots(ctx context.Context, ns, vm string) ([]unstructured.Unstructured, error) {
	list, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(list.Items, func(s unstructured.Unstructured) bool {
		src, _, _ := unstructured.NestedString(s.Object, "spec", "source", "name")
		return src != vm || strings.HasPrefix(s.GetName(), "argus-")
	}), nil
}

func (c *Client) ListSnapshots(ctx context.Context, ns, vm string) ([]Snapshot, error) {
	items, err := c.vmSnapshots(ctx, ns, vm)
	if err != nil {
		return nil, err
	}
	var all []Snapshot
	for _, s := range items {
		snap := Snapshot{Name: s.GetName(), Created: s.GetCreationTimestamp().Time, Auto: s.GetLabels()[autoLabel] == "true",
			Parent: s.GetAnnotations()[parentAnnotation], Current: s.GetAnnotations()[currentAnnotation] == "true"}
		snap.Phase, _, _ = unstructured.NestedString(s.Object, "status", "phase")
		snap.Ready, _, _ = unstructured.NestedBool(s.Object, "status", "readyToUse")
		ind, _, _ := unstructured.NestedStringSlice(s.Object, "status", "indications")
		snap.Quiesced = slices.Contains(ind, "GuestAgent")
		all = append(all, snap)
	}
	return snapshotTree(all), nil
}

func snapshotTree(all []Snapshot) []Snapshot {
	sort.Slice(all, func(i, j int) bool { return all[i].Created.Before(all[j].Created) })
	names := map[string]bool{}
	for _, s := range all {
		names[s.Name] = true
	}
	children := map[string][]Snapshot{}
	for _, s := range all {
		p := s.Parent
		if !names[p] {
			p = ""
		}
		children[p] = append(children[p], s)
	}
	var out []Snapshot
	var walk func(string, int)
	walk = func(parent string, depth int) {
		for _, s := range children[parent] {
			s.Depth = depth
			out = append(out, s)
			walk(s.Name, depth+1)
		}
	}
	walk("", 0)
	return out
}

func (c *Client) annotateSnapshot(ctx context.Context, ns, name string, ann obj) error {
	patch, _ := json.Marshal(obj{"metadata": obj{"annotations": ann}})
	_, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) markCurrent(ctx context.Context, ns string, items []unstructured.Unstructured, name string) error {
	for _, s := range items {
		if s.GetName() != name && s.GetAnnotations()[currentAnnotation] == "true" {
			if err := c.annotateSnapshot(ctx, ns, s.GetName(), obj{currentAnnotation: nil}); err != nil {
				return err
			}
		}
	}
	if name == "" {
		return nil
	}
	return c.annotateSnapshot(ctx, ns, name, obj{currentAnnotation: "true"})
}

func (c *Client) CreateSnapshot(ctx context.Context, ns, vm string) (string, error) {
	return c.createSnapshot(ctx, ns, vm, fmt.Sprintf("%s-%s", vm, time.Now().UTC().Format("20060102-150405")), nil)
}

func (c *Client) CreateAutoSnapshot(ctx context.Context, ns, vm string) (string, error) {
	return c.createSnapshot(ctx, ns, vm, fmt.Sprintf("%s-auto-%s", vm, time.Now().UTC().Format("20060102-150405")), obj{autoLabel: "true"})
}

const TagsAnnotation = "argus.io/tags"

func (c *Client) SetTags(ctx context.Context, ns, vm string, tags []string) error {
	var v any = strings.Join(tags, ",")
	if len(tags) == 0 {
		v = nil
	}
	patch, _ := json.Marshal(obj{"metadata": obj{"annotations": obj{TagsAnnotation: v}}})
	_, err := c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, vm, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) SetBackupPolicy(ctx context.Context, ns, vm, policy string) error {
	var v any = policy
	if policy == "" {
		v = nil
	}
	patch, _ := json.Marshal(obj{"metadata": obj{"annotations": obj{BackupAnnotation: v}}})
	_, err := c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, vm, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) createSnapshot(ctx context.Context, ns, vm, name string, labels obj) (string, error) {
	items, err := c.vmSnapshots(ctx, ns, vm)
	if err != nil {
		return "", err
	}
	parent, current, tracked := "", "", false
	var newest time.Time
	for _, s := range items {
		a := s.GetAnnotations()
		if _, ok := a[parentAnnotation]; ok {
			tracked = true
		}
		if a[currentAnnotation] == "true" {
			current, tracked = s.GetName(), true
		}
		if s.GetCreationTimestamp().After(newest) {
			parent, newest = s.GetName(), s.GetCreationTimestamp().Time
		}
	}
	if tracked {
		parent = current
	}
	meta := obj{"name": name, "annotations": obj{parentAnnotation: parent}}
	if labels != nil {
		meta["labels"] = labels
	}
	snap := &unstructured.Unstructured{Object: obj{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineSnapshot",
		"metadata":   meta,
		"spec":       obj{"source": obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm}},
	}}
	if _, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{}); err != nil {
		return name, err
	}
	return name, c.markCurrent(ctx, ns, items, name)
}

func (c *Client) RestoreSnapshot(ctx context.Context, ns, vm, snapshot string) error {
	if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, vm, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return fmt.Errorf("power off %s first", vm)
	}
	restore := &unstructured.Unstructured{Object: obj{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineRestore",
		"metadata":   obj{"name": fmt.Sprintf("%s-%d", snapshot, time.Now().Unix())},
		"spec": obj{
			"target":                     obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm},
			"virtualMachineSnapshotName": snapshot,
		},
	}}
	if _, err := c.dyn.Resource(vmRestoreGVR).Namespace(ns).Create(ctx, restore, metav1.CreateOptions{}); err != nil {
		return err
	}
	items, err := c.vmSnapshots(ctx, ns, vm)
	if err != nil {
		return err
	}
	return c.markCurrent(ctx, ns, items, snapshot)
}

func (c *Client) DeleteSnapshot(ctx context.Context, ns, snapshot string) error {
	snap, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Get(ctx, snapshot, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Delete(ctx, snapshot, metav1.DeleteOptions{}); err != nil {
		return err
	}
	vm, _, _ := unstructured.NestedString(snap.Object, "spec", "source", "name")
	items, err := c.vmSnapshots(ctx, ns, vm)
	if err != nil {
		return err
	}
	parent := snap.GetAnnotations()[parentAnnotation]
	for _, s := range items {
		if s.GetAnnotations()[parentAnnotation] == snapshot {
			if err := c.annotateSnapshot(ctx, ns, s.GetName(), obj{parentAnnotation: parent}); err != nil {
				return err
			}
		}
	}
	if snap.GetAnnotations()[currentAnnotation] == "true" && parent != "" {
		return c.annotateSnapshot(ctx, ns, parent, obj{currentAnnotation: "true"})
	}
	return nil
}
