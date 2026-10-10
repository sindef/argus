package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

const (
	kubevirtNS       = "kubevirt"
	migrationNetwork = "argus-migration"
)

var (
	vmimGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstancemigrations"}
	kvGVR    = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
	leaseGVR = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}
	podGVR   = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
)

const syncController = "virt-synchronization-controller"

func (c *Client) syncLeaseAge(ctx context.Context) (time.Duration, error) {
	l, err := c.dyn.Resource(leaseGVR).Namespace(kubevirtNS).Get(ctx, syncController, metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	renew, _, _ := unstructured.NestedString(l.Object, "spec", "renewTime")
	t, err := time.Parse(time.RFC3339Nano, renew)
	if err != nil {
		return 0, err
	}
	return time.Since(t), nil
}

func (c *Client) ensureSyncController(ctx context.Context) error {
	if age, err := c.syncLeaseAge(ctx); err == nil && age < 30*time.Second {
		return nil
	}
	err := c.dyn.Resource(podGVR).Namespace(kubevirtNS).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "kubevirt.io=" + syncController})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		if age, err := c.syncLeaseAge(ctx); err == nil && age < 10*time.Second {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("migration sync controller not healthy")
		case <-time.After(3 * time.Second):
		}
	}
}

type MigrationNetwork struct {
	VLAN       int    `json:"vlan"`
	Subnet     string `json:"subnet"`
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`
}

func (c *Client) migrationActive(ctx context.Context) (bool, error) {
	list, err := c.dyn.Resource(vmimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, m := range list.Items {
		if phase, _, _ := unstructured.NestedString(m.Object, "status", "phase"); phase != "Succeeded" && phase != "Failed" {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) SetMigrationNetwork(ctx context.Context, m MigrationNetwork) (string, error) {
	cfg := obj{
		"cniVersion": "0.3.1",
		"name":       migrationNetwork,
		"type":       "bridge",
		"bridge":     bridge,
		"ipam": obj{
			"type":   "host-local",
			"ranges": []interface{}{[]interface{}{obj{"subnet": m.Subnet, "rangeStart": m.RangeStart, "rangeEnd": m.RangeEnd}}},
		},
	}
	if m.VLAN > 0 {
		cfg["vlan"] = m.VLAN
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	res := c.dyn.Resource(nadGVR).Namespace(kubevirtNS)
	nad, err := res.Get(ctx, migrationNetwork, metav1.GetOptions{})
	changed := true
	switch {
	case apierrors.IsNotFound(err):
		nad = &unstructured.Unstructured{Object: obj{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata":   obj{"name": migrationNetwork},
			"spec":       obj{"config": string(b)},
		}}
		err = nil
	case err == nil:
		old, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
		changed = old != string(b)
	}
	if err != nil {
		return "", err
	}
	if changed {
		if active, err := c.migrationActive(ctx); err != nil || active {
			return "", errors.New("a migration is in progress on this host; retry when it finishes")
		}
		unstructured.SetNestedField(nad.Object, string(b), "spec", "config")
		if nad.GetResourceVersion() == "" {
			_, err = res.Create(ctx, nad, metav1.CreateOptions{})
		} else {
			_, err = res.Update(ctx, nad, metav1.UpdateOptions{})
		}
		if err != nil {
			return "", err
		}
	}
	patch := []byte(`{"spec":{"configuration":{"migrations":{"network":"` + migrationNetwork + `"}}}}`)
	if _, err := c.dyn.Resource(kvGVR).Namespace(kubevirtNS).Patch(ctx, "kubevirt", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return "", err
	}
	if changed {
		if err := c.dyn.Resource(podGVR).Namespace(kubevirtNS).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-handler"}); err != nil {
			return "", err
		}
	}
	return c.waitMigrationAddress(ctx, m)
}

func (c *Client) waitMigrationAddress(ctx context.Context, m MigrationNetwork) (string, error) {
	_, subnet, err := net.ParseCIDR(m.Subnet)
	if err != nil {
		return "", err
	}
	lo, hi := net.ParseIP(m.RangeStart).To4(), net.ParseIP(m.RangeEnd).To4()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		pods, err := c.dyn.Resource(podGVR).Namespace(kubevirtNS).List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-handler"})
		if err == nil {
			for _, p := range pods.Items {
				if p.GetDeletionTimestamp() != nil {
					continue
				}
				var status []struct {
					Name string   `json:"name"`
					IPs  []string `json:"ips"`
				}
				json.Unmarshal([]byte(p.GetAnnotations()["k8s.v1.cni.cncf.io/network-status"]), &status)
				for _, st := range status {
					if st.Name != kubevirtNS+"/"+migrationNetwork {
						continue
					}
					for _, a := range st.IPs {
						ip := net.ParseIP(a).To4()
						if ip != nil && subnet.Contains(ip) && bytes.Compare(ip, lo) >= 0 && bytes.Compare(ip, hi) <= 0 {
							return a, nil
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", errors.New("virt-handler did not get a migration address in range")
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *Client) RemoveMigrationNetwork(ctx context.Context) error {
	if active, err := c.migrationActive(ctx); err != nil || active {
		return errors.New("a migration is in progress on this host")
	}
	patch := []byte(`{"spec":{"configuration":{"migrations":{"network":null}}}}`)
	if _, err := c.dyn.Resource(kvGVR).Namespace(kubevirtNS).Patch(ctx, "kubevirt", types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return err
	}
	err := c.dyn.Resource(nadGVR).Namespace(kubevirtNS).Delete(ctx, migrationNetwork, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.dyn.Resource(podGVR).Namespace(kubevirtNS).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-handler"})
}

func (c *Client) HasMigrationNetwork(ctx context.Context) bool {
	_, err := c.dyn.Resource(nadGVR).Namespace(kubevirtNS).Get(ctx, migrationNetwork, metav1.GetOptions{})
	return err == nil
}

func (c *Client) KubeVirtCA(ctx context.Context) (string, error) {
	cm, err := c.dyn.Resource(cmGVR).Namespace(kubevirtNS).Get(ctx, "kubevirt-ca", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	ca, _, _ := unstructured.NestedString(cm.Object, "data", "ca-bundle")
	return ca, nil
}

func (c *Client) SetExternalCA(ctx context.Context, bundle string) error {
	res := c.dyn.Resource(cmGVR).Namespace(kubevirtNS)
	cm, err := res.Get(ctx, "kubevirt-external-ca", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm = &unstructured.Unstructured{Object: obj{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   obj{"name": "kubevirt-external-ca"},
			"data":       obj{"ca-bundle": bundle},
		}}
		_, err = res.Create(ctx, cm, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	unstructured.SetNestedField(cm.Object, bundle, "data", "ca-bundle")
	_, err = res.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (c *Client) VMSpec(ctx context.Context, ns, name string) (obj, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if err := migratable(vm); err != nil {
		return nil, err
	}
	spec, _, _ := unstructured.NestedMap(vm.Object, "spec")
	return spec, nil
}

func sendName(vm string) string { return "argus-send-" + vm }
func recvName(vm string) string { return "argus-recv-" + vm }

func (c *Client) Receive(ctx context.Context, ns, name, migrationID string, spec obj) (string, error) {
	if err := c.guard(); err != nil {
		return "", err
	}
	if !c.HasMigrationNetwork(ctx) {
		return "", errors.New("migration network not configured")
	}
	if err := c.ensureSyncController(ctx); err != nil {
		return "", err
	}
	if _, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("%s already exists", name)
	}
	templates, _, _ := unstructured.NestedSlice(spec, "dataVolumeTemplates")
	for _, t := range templates {
		m := t.(obj)
		unstructured.SetNestedField(m, "true", "metadata", "annotations", "cdi.kubevirt.io/storage.usePopulator")
		unstructured.SetNestedMap(m, obj{"blank": obj{}}, "spec", "source")
		unstructured.RemoveNestedField(m, "spec", "sourceRef")
	}
	unstructured.SetNestedSlice(spec, templates, "dataVolumeTemplates")
	spec["runStrategy"] = "WaitAsReceiver"
	delete(spec, "running")
	vm := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   obj{"name": name, "annotations": obj{"kubevirt.io/restore-run-strategy": "Always"}},
		"spec":       spec,
	}}
	if _, err := c.dyn.Resource(vmGVR).Namespace(ns).Create(ctx, vm, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return "", errors.New("target VM did not start waiting")
		case <-time.After(2 * time.Second):
		}
	}
	mig := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstanceMigration",
		"metadata":   obj{"name": recvName(name)},
		"spec":       obj{"vmiName": name, "receive": obj{"migrationID": migrationID}},
	}}
	if _, err := c.dyn.Resource(vmimGVR).Namespace(ns).Create(ctx, mig, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	for {
		m, err := c.dyn.Resource(vmimGVR).Namespace(ns).Get(ctx, recvName(name), metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if addrs, _, _ := unstructured.NestedStringSlice(m.Object, "status", "synchronizationAddresses"); len(addrs) > 0 {
			return addrs[0], nil
		}
		select {
		case <-ctx.Done():
			return "", errors.New("target did not become ready to receive")
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) Send(ctx context.Context, ns, name, migrationID, connectURL string) error {
	if !c.HasMigrationNetwork(ctx) {
		return errors.New("migration network not configured")
	}
	if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err != nil {
		return fmt.Errorf("%s is not running", name)
	}
	if err := c.ensureSyncController(ctx); err != nil {
		return err
	}
	mig := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstanceMigration",
		"metadata":   obj{"name": sendName(name)},
		"spec":       obj{"vmiName": name, "sendTo": obj{"migrationID": migrationID, "connectURL": connectURL}},
	}}
	_, err := c.dyn.Resource(vmimGVR).Namespace(ns).Create(ctx, mig, metav1.CreateOptions{})
	return err
}

func (c *Client) MigrationPhase(ctx context.Context, ns, name string) (string, error) {
	for _, n := range []string{sendName(name), recvName(name)} {
		m, err := c.dyn.Resource(vmimGVR).Namespace(ns).Get(ctx, n, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		phase, _, _ := unstructured.NestedString(m.Object, "status", "phase")
		return phase, nil
	}
	return "", errors.New("no migration")
}

func (c *Client) CleanupMigration(ctx context.Context, ns, name string) error {
	for _, n := range []string{sendName(name), recvName(name)} {
		if err := c.dyn.Resource(vmimGVR).Namespace(ns).Delete(ctx, n, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
