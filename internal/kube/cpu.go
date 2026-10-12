package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var nodeGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

const (
	cpuModelLabel = "cpu-model.node.kubevirt.io/"
	hostCPULabel  = "host-model-cpu.node.kubevirt.io/"
)

type CPUInfo struct {
	Models   []string `json:"models,omitempty"`
	Host     string   `json:"host,omitempty"`
	Baseline string   `json:"baseline,omitempty"`
}

func (c *Client) CPUInfo(ctx context.Context) (CPUInfo, error) {
	var info CPUInfo
	nodes, err := c.dyn.Resource(nodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return info, err
	}
	for _, n := range nodes.Items {
		for k, v := range n.GetLabels() {
			if m, ok := strings.CutPrefix(k, cpuModelLabel); ok && v == "true" {
				info.Models = append(info.Models, m)
			}
			if m, ok := strings.CutPrefix(k, hostCPULabel); ok && v == "true" {
				info.Host = m
			}
		}
	}
	slices.Sort(info.Models)
	kv, err := c.dyn.Resource(kvGVR).Namespace(kubevirtNS).Get(ctx, "kubevirt", metav1.GetOptions{})
	if err != nil {
		return info, err
	}
	info.Baseline, _, _ = unstructured.NestedString(kv.Object, "spec", "configuration", "cpuModel")
	return info, nil
}

func (c *Client) SetCPUBaseline(ctx context.Context, model string) error {
	info, err := c.CPUInfo(ctx)
	if err != nil {
		return err
	}
	if model != "" && !slices.Contains(info.Models, model) {
		return fmt.Errorf("this host's CPU cannot run the %s model", model)
	}
	var v any
	if model != "" {
		v = model
	}
	patch, _ := json.Marshal(obj{"spec": obj{"configuration": obj{"cpuModel": v}}})
	_, err = c.dyn.Resource(kvGVR).Namespace(kubevirtNS).Patch(ctx, "kubevirt", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
