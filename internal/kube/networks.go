package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const bridge = "br0"

var nadGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}

type Network struct {
	Name   string
	Bridge string
	VLAN   int
}

type cniConfig struct {
	CNIVersion string   `json:"cniVersion"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Bridge     string   `json:"bridge"`
	VLAN       int      `json:"vlan,omitempty"`
	IPAM       struct{} `json:"ipam"`
}

func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	list, err := c.dyn.Resource(nadGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(list.Items))
	for _, n := range list.Items {
		raw, _, _ := unstructured.NestedString(n.Object, "spec", "config")
		var cfg cniConfig
		json.Unmarshal([]byte(raw), &cfg)
		out = append(out, Network{Name: n.GetName(), Bridge: cfg.Bridge, VLAN: cfg.VLAN})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) CreateNetwork(ctx context.Context, name string, vlan int, br string) error {
	if br == "" {
		br = bridge
	}
	cfg, err := json.Marshal(cniConfig{CNIVersion: "0.3.1", Name: name, Type: "bridge", Bridge: br, VLAN: vlan})
	if err != nil {
		return err
	}
	nad := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.cni.cncf.io/v1",
		"kind":       "NetworkAttachmentDefinition",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"config": string(cfg)},
	}}
	_, err = c.dyn.Resource(nadGVR).Namespace(Namespace).Create(ctx, nad, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	vms, err := c.ListVMs(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		for _, n := range vm.NICs {
			if n.Network == name || n.Network == Namespace+"/"+name {
				return fmt.Errorf("%s is used by %s", name, vm.Name)
			}
		}
	}
	return c.dyn.Resource(nadGVR).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
}
