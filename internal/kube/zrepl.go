package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type ZDisk struct {
	DataVolume string   `json:"dataVolume"`
	Dataset    string   `json:"dataset"`
	Snapshots  []string `json:"snapshots,omitempty"`
}

func (c *Client) VMDatasets(ctx context.Context, ns, name string) (map[string]ZDisk, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	zvols, err := c.dyn.Resource(zfsVolumeGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pools := map[string]string{}
	for _, z := range zvols.Items {
		pools[z.GetName()], _, _ = unstructured.NestedString(z.Object, "spec", "poolName")
	}
	owned := map[string]bool{}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	for _, t := range templates {
		n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
		owned[n] = true
	}
	out := map[string]ZDisk{}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		vo := asObj(v)
		dv, ok, _ := unstructured.NestedString(vo, "dataVolume", "name")
		if !ok || !owned[dv] {
			continue
		}
		pvc, err := c.dyn.Resource(pvcGVR).Namespace(ns).Get(ctx, dv, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		pv, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
		if pools[pv] == "" {
			return nil, fmt.Errorf("disk %s is not on a ZFS pool", asStr(vo["name"]))
		}
		out[asStr(vo["name"])] = ZDisk{DataVolume: dv, Dataset: pools[pv] + "/" + pv}
	}
	if len(out) != len(owned) {
		return nil, fmt.Errorf("%s has disks outside its volumes", name)
	}
	return out, nil
}

func (c *Client) Freeze(ctx context.Context, ns, name string) error {
	return c.subresource(ctx, ns, "virtualmachineinstances", name, "freeze", `{"unfreezeTimeout":"1m"}`)
}

func (c *Client) Unfreeze(ctx context.Context, ns, name string) error {
	return c.subresource(ctx, ns, "virtualmachineinstances", name, "unfreeze", "{}")
}

func (c *Client) StoppedReplica(ctx context.Context, ns, name string) error {
	if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return fmt.Errorf("power off %s first", name)
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return isReplica(vm.Object, name)
}

func isReplica(o obj, name string) error {
	if l, _, _ := unstructured.NestedString(o, "metadata", "labels", haLabel); l != "true" {
		return fmt.Errorf("%s on this host is not an HA replica; refusing to overwrite it", name)
	}
	return nil
}
