package kube

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const displayNameAnnotation = "argus.io/display-name"

var (
	cloneGVR = schema.GroupVersionResource{Group: "clone.kubevirt.io", Version: "v1beta1", Resource: "virtualmachineclones"}
	eventGVR = schema.GroupVersionResource{Version: "v1", Resource: "events"}
)

type Event struct {
	Time    time.Time
	Type    string
	Reason  string
	Message string
	Object  string
}

func (c *Client) Rename(ctx context.Context, ns, name, display string) error {
	value, err := json.Marshal(display)
	if err != nil {
		return err
	}
	if display == "" {
		value = []byte("null")
	}
	patch := []byte(`{"metadata":{"annotations":{"` + displayNameAnnotation + `":` + string(value) + `}}}`)
	_, err = c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) Clone(ctx context.Context, ns, name, target string) error {
	if err := c.guard(); err != nil {
		return err
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	macs := obj{}
	ifaces, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	for _, i := range ifaces {
		if n, ok := i.(obj)["name"].(string); ok {
			macs[n] = strings.ReplaceAll(randomMAC(), ":", "-")
		}
	}
	cl := &unstructured.Unstructured{Object: obj{
		"apiVersion": "clone.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineClone",
		"metadata":   obj{"generateName": "argus-clone-"},
		"spec": obj{
			"source":            obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": name},
			"target":            obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": target},
			"newMacAddresses":   macs,
			"annotationFilters": []string{"*", "!" + displayNameAnnotation},
		},
	}}
	_, err = c.dyn.Resource(cloneGVR).Namespace(ns).Create(ctx, cl, metav1.CreateOptions{})
	return err
}

func (c *Client) Events(ctx context.Context, ns, name string, limit int) ([]Event, error) {
	list, err := c.dyn.Resource(eventGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range list.Items {
		obj, _, _ := unstructured.NestedString(e.Object, "involvedObject", "name")
		kind, _, _ := unstructured.NestedString(e.Object, "involvedObject", "kind")
		if name != "" && obj != name && !(kind == "Pod" && len(obj) > len("virt-launcher-"+name) && obj[:len("virt-launcher-"+name)] == "virt-launcher-"+name) {
			continue
		}
		ev := Event{Object: kind + "/" + obj}
		ev.Type, _, _ = unstructured.NestedString(e.Object, "type")
		ev.Reason, _, _ = unstructured.NestedString(e.Object, "reason")
		ev.Message, _, _ = unstructured.NestedString(e.Object, "message")
		for _, f := range []string{"lastTimestamp", "eventTime", "firstTimestamp"} {
			if ts, ok, _ := unstructured.NestedString(e.Object, f); ok && ts != "" {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					ev.Time = t
					break
				}
			}
		}
		if ev.Time.IsZero() {
			ev.Time = e.GetCreationTimestamp().Time
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type NodeInfo struct {
	Name          string    `json:"name"`
	Version       string    `json:"version"`
	CAFingerprint string    `json:"caFingerprint"`
	RunningVMs    int       `json:"runningVMs"`
	CPUCores      int64     `json:"cpuCores"`
	MemoryBytes   int64     `json:"memoryBytes"`
	BootTime      time.Time `json:"bootTime"`
}
