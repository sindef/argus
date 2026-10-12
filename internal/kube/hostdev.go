package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const devicePrefix = "argus.io/"

func PCIResource(vendor, device string) string { return devicePrefix + "pci-" + vendor + "-" + device }
func USBResource(vendor, product string) string {
	return devicePrefix + "usb-" + vendor + "-" + product
}

func (c *Client) SetPermittedDevices(ctx context.Context, pci, usb [][2]string) error {
	pcis, usbs := []any{}, []any{}
	for _, d := range pci {
		pcis = append(pcis, obj{"pciVendorSelector": strings.ToUpper(d[0] + ":" + d[1]), "resourceName": PCIResource(d[0], d[1])})
	}
	for _, d := range usb {
		usbs = append(usbs, obj{"resourceName": USBResource(d[0], d[1]), "selectors": []any{obj{"vendor": d[0], "product": d[1]}}})
	}
	patch, _ := json.Marshal(obj{"spec": obj{"configuration": obj{"permittedHostDevices": obj{"pciHostDevices": pcis, "usb": usbs}}}})
	_, err := c.dyn.Resource(kvGVR).Namespace(kubevirtNS).Patch(ctx, "kubevirt", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

type DeviceResource struct {
	Name  string
	Label string
	Count int64
}

func (c *Client) DeviceResources(ctx context.Context) ([]DeviceResource, error) {
	nodes, err := c.dyn.Resource(nodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []DeviceResource
	for _, n := range nodes.Items {
		alloc, _, _ := unstructured.NestedStringMap(n.Object, "status", "allocatable")
		for name, v := range alloc {
			if q, err := resource.ParseQuantity(v); err == nil && strings.HasPrefix(name, devicePrefix) && q.Value() > 0 {
				out = append(out, DeviceResource{Name: name, Label: name, Count: q.Value()})
			}
		}
	}
	slices.SortFunc(out, func(a, b DeviceResource) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (c *Client) setHostDevices(ctx context.Context, ns, name string, fn func([]any) ([]any, error)) error {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if run, _, _ := unstructured.NestedString(vm.Object, "spec", "runStrategy"); run != "Halted" {
		return errors.New("power off the VM first")
	}
	devs, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "hostDevices")
	if devs, err = fn(devs); err != nil {
		return err
	}
	patch, _ := json.Marshal(obj{"spec": obj{"template": obj{"spec": obj{"domain": obj{"devices": obj{"hostDevices": devs}}}}}})
	_, err = c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) AddHostDevice(ctx context.Context, ns, name, resource string) error {
	if !strings.HasPrefix(resource, devicePrefix) {
		return errors.New("unknown device")
	}
	return c.setHostDevices(ctx, ns, name, func(devs []any) ([]any, error) {
		for i := 0; ; i++ {
			n := fmt.Sprintf("hostdev%d", i)
			if !slices.ContainsFunc(devs, func(d any) bool { return d.(map[string]any)["name"] == n }) {
				return append(devs, obj{"name": n, "deviceName": resource}), nil
			}
		}
	})
}

func (c *Client) RemoveHostDevice(ctx context.Context, ns, name, dev string) error {
	return c.setHostDevices(ctx, ns, name, func(devs []any) ([]any, error) {
		return slices.DeleteFunc(devs, func(d any) bool { return d.(map[string]any)["name"] == dev }), nil
	})
}
