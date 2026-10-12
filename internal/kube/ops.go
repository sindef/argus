package kube

import (
	"context"
	"encoding/json"
	"log"
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

func (c *Client) Clone(ctx context.Context, ns, name, target string, template bool) error {
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
		if n := asStr(asObj(i)["name"]); n != "" {
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
			"labelFilters":      []string{"*", "!" + haLabel, "!" + TemplateLabel},
		},
	}}
	if template {
		unstructured.SetNestedStringSlice(cl.Object, []string{`{"op":"add","path":"/spec/runStrategy","value":"Halted"}`}, "spec", "patches")
	}
	created, err := c.dyn.Resource(cloneGVR).Namespace(ns).Create(ctx, cl, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	go c.finishClone(ns, created.GetName(), target, template)
	return nil
}

func (c *Client) finishClone(ns, clone, target string, template bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	for ctx.Err() == nil {
		cur, err := c.dyn.Resource(cloneGVR).Namespace(ns).Get(ctx, clone, metav1.GetOptions{})
		if err != nil {
			log.Printf("clone %s: %v", target, err)
			return
		}
		switch phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase"); phase {
		case "Succeeded":
			if template {
				if err := c.setTemplate(ctx, ns, target, true); err != nil {
					log.Printf("clone %s: mark as template: %v", target, err)
				}
			}
			c.dyn.Resource(cloneGVR).Namespace(ns).Delete(ctx, clone, metav1.DeleteOptions{})
			return
		case "Failed":
			log.Printf("clone %s failed", target)
			return
		}
		time.Sleep(5 * time.Second)
	}
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
	Arch          string    `json:"arch,omitempty"`
	MemoryBytes   int64     `json:"memoryBytes"`
	BootTime      time.Time `json:"bootTime"`
	CPU           CPUInfo   `json:"cpu"`
}
