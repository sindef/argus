package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const bridge = "br0"

var nadGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}

type Network struct {
	Name       string
	Bridge     string
	VLAN       int
	SpoofCheck bool
	RateMbps   int
}

type cniPlugin struct {
	Type         string    `json:"type,omitempty"`
	Bridge       string    `json:"bridge,omitempty"`
	VLAN         int       `json:"vlan,omitempty"`
	MacSpoofChk  bool      `json:"macspoofchk,omitempty"`
	IPAM         *struct{} `json:"ipam,omitempty"`
	IngressRate  int64     `json:"ingressRate,omitempty"`
	IngressBurst int64     `json:"ingressBurst,omitempty"`
	EgressRate   int64     `json:"egressRate,omitempty"`
	EgressBurst  int64     `json:"egressBurst,omitempty"`
}

type cniConfig struct {
	CNIVersion string      `json:"cniVersion"`
	Name       string      `json:"name"`
	Plugins    []cniPlugin `json:"plugins,omitempty"`
	cniPlugin
}

func parseNetwork(name, raw string) Network {
	var cfg cniConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		log.Printf("network %s: %v", name, err)
	}
	plugins := cfg.Plugins
	if len(plugins) == 0 {
		plugins = []cniPlugin{cfg.cniPlugin}
	}
	n := Network{Name: name}
	for _, p := range plugins {
		switch p.Type {
		case "bridge":
			n.Bridge, n.VLAN, n.SpoofCheck = p.Bridge, p.VLAN, p.MacSpoofChk
		case "bandwidth":
			n.RateMbps = int(p.EgressRate / 1_000_000)
		}
	}
	return n
}

func networkConfig(n Network) (string, error) {
	if n.Bridge == "" {
		n.Bridge = bridge
	}
	cfg := cniConfig{CNIVersion: "0.3.1", Name: n.Name, Plugins: []cniPlugin{{Type: "bridge", Bridge: n.Bridge, VLAN: n.VLAN, MacSpoofChk: n.SpoofCheck, IPAM: &struct{}{}}}}
	if n.RateMbps > 0 {
		rate := int64(n.RateMbps) * 1_000_000
		cfg.Plugins = append(cfg.Plugins, cniPlugin{Type: "bandwidth", IngressRate: rate, IngressBurst: rate, EgressRate: rate, EgressBurst: rate})
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	list, err := c.dyn.Resource(nadGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Network, 0, len(list.Items))
	for _, n := range list.Items {
		raw, _, _ := unstructured.NestedString(n.Object, "spec", "config")
		out = append(out, parseNetwork(n.GetName(), raw))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) CreateNetwork(ctx context.Context, n Network) error {
	cfg, err := networkConfig(n)
	if err != nil {
		return err
	}
	nad := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8s.cni.cncf.io/v1",
		"kind":       "NetworkAttachmentDefinition",
		"metadata":   map[string]interface{}{"name": n.Name},
		"spec":       map[string]interface{}{"config": cfg},
	}}
	_, err = c.dyn.Resource(nadGVR).Namespace(Namespace).Create(ctx, nad, metav1.CreateOptions{})
	return err
}

func (c *Client) UpdateNetwork(ctx context.Context, n Network) error {
	nad, err := c.dyn.Resource(nadGVR).Namespace(Namespace).Get(ctx, n.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return c.CreateNetwork(ctx, n)
	}
	if err != nil {
		return err
	}
	cfg, err := networkConfig(n)
	if err != nil {
		return err
	}
	unstructured.SetNestedField(nad.Object, cfg, "spec", "config")
	_, err = c.dyn.Resource(nadGVR).Namespace(Namespace).Update(ctx, nad, metav1.UpdateOptions{})
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
