package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	vmSnapGVR    = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinesnapshots"}
	vmRestoreGVR = schema.GroupVersionResource{Group: "snapshot.kubevirt.io", Version: "v1beta1", Resource: "virtualmachinerestores"}
)

type Snapshot struct {
	Name    string
	Created time.Time
	Phase   string
	Ready   bool
}

func (c *Client) ListSnapshots(ctx context.Context, ns, vm string) ([]Snapshot, error) {
	list, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, s := range list.Items {
		if src, _, _ := unstructured.NestedString(s.Object, "spec", "source", "name"); src != vm {
			continue
		}
		snap := Snapshot{Name: s.GetName(), Created: s.GetCreationTimestamp().Time}
		snap.Phase, _, _ = unstructured.NestedString(s.Object, "status", "phase")
		snap.Ready, _, _ = unstructured.NestedBool(s.Object, "status", "readyToUse")
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

func (c *Client) CreateSnapshot(ctx context.Context, ns, vm string) (string, error) {
	name := fmt.Sprintf("%s-%s", vm, time.Now().UTC().Format("20060102-150405"))
	snap := &unstructured.Unstructured{Object: obj{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineSnapshot",
		"metadata":   obj{"name": name},
		"spec":       obj{"source": obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm}},
	}}
	_, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Create(ctx, snap, metav1.CreateOptions{})
	return name, err
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
	_, err := c.dyn.Resource(vmRestoreGVR).Namespace(ns).Create(ctx, restore, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteSnapshot(ctx context.Context, ns, snapshot string) error {
	return c.dyn.Resource(vmSnapGVR).Namespace(ns).Delete(ctx, snapshot, metav1.DeleteOptions{})
}
