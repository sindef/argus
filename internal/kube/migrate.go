package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const exportLabel = "argus.io/export"

var vmExportGVR = schema.GroupVersionResource{Group: "export.kubevirt.io", Version: "v1", Resource: "virtualmachineexports"}

type Export struct {
	Spec  obj          `json:"spec"`
	Disks []ExportDisk `json:"disks"`
	Token string       `json:"token"`
	CA    string       `json:"ca"`
}

type ExportDisk struct {
	DataVolume string `json:"dataVolume"`
	URL        string `json:"url"`
}

type DiskStatus struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Progress string `json:"progress"`
}

func exportName(vm string) string { return "argus-" + vm }

func (c *Client) ExportVM(ctx context.Context, ns, name, baseURL, caPEM string) (Export, error) {
	if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return Export{}, fmt.Errorf("power off %s first", name)
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Export{}, err
	}
	if err := migratable(vm); err != nil {
		return Export{}, err
	}
	return c.export(ctx, ns, exportName(name), baseURL, caPEM, vm, obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": name})
}

func (c *Client) export(ctx context.Context, ns, exp, baseURL, caPEM string, vm *unstructured.Unstructured, source obj) (Export, error) {
	b := make([]byte, 24)
	rand.Read(b)
	token := hex.EncodeToString(b)
	for _, gvr := range []schema.GroupVersionResource{vmExportGVR, secGVR} {
		if err := c.dyn.Resource(gvr).Namespace(ns).Delete(ctx, exp, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return Export{}, err
		}
	}
	if err := c.waitGone(ctx, ns, exp); err != nil {
		return Export{}, err
	}
	if err := c.Secrets(ns).Create(ctx, exp, "Opaque", map[string][]byte{"token": []byte(token)}); err != nil {
		return Export{}, err
	}
	ve := &unstructured.Unstructured{Object: obj{
		"apiVersion": "export.kubevirt.io/v1",
		"kind":       "VirtualMachineExport",
		"metadata":   obj{"name": exp, "labels": obj{exportLabel: "true"}},
		"spec": obj{
			"source":         source,
			"tokenSecretRef": exp,
			"ttlDuration":    "13h",
		},
	}}
	if _, err := c.dyn.Resource(vmExportGVR).Namespace(ns).Create(ctx, ve, metav1.CreateOptions{}); err != nil {
		return Export{}, err
	}
	if err := c.waitExportReady(ctx, ns, exp); err != nil {
		return Export{}, err
	}
	data, _, err := c.Secrets(ns).Get(ctx, exp)
	if err != nil {
		return Export{}, err
	}
	spec, _, _ := unstructured.NestedMap(vm.Object, "spec")
	ex := Export{Spec: spec, Token: string(data["token"]), CA: caPEM}
	templates, _, _ := unstructured.NestedSlice(spec, "dataVolumeTemplates")
	for _, t := range templates {
		dv, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
		ex.Disks = append(ex.Disks, ExportDisk{DataVolume: dv, URL: fmt.Sprintf("%s/export/%s/%s/%s", baseURL, ns, exp, dv)})
	}
	return ex, nil
}

func (c *Client) waitGone(ctx context.Context, ns, exp string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		_, err := c.dyn.Resource(vmExportGVR).Namespace(ns).Get(ctx, exp, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("previous export did not go away")
		case <-time.After(time.Second):
		}
	}
}

func migratable(vm *unstructured.Unstructured) error {
	owned := map[string]bool{}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	for _, t := range templates {
		n, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
		owned[n] = true
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		if _, ok := v.(obj)["persistentVolumeClaim"]; ok {
			return fmt.Errorf("eject the CD-ROM from %s first", vm.GetName())
		}
		if dv, ok := v.(obj)["dataVolume"].(obj); ok && !owned[dv["name"].(string)] {
			return fmt.Errorf("disk %v of %s cannot be migrated", dv["name"], vm.GetName())
		}
	}
	return nil
}

func (c *Client) waitExportReady(ctx context.Context, ns, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		ve, err := c.dyn.Resource(vmExportGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if phase, _, _ := unstructured.NestedString(ve.Object, "status", "phase"); phase == "Ready" {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("export did not become ready")
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) ExportSource(ctx context.Context, ns, export, volume string) (string, string, error) {
	ve, err := c.dyn.Resource(vmExportGVR).Namespace(ns).Get(ctx, export, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if ve.GetLabels()[exportLabel] != "true" {
		return "", "", errors.New("not an argus export")
	}
	cert, _, _ := unstructured.NestedString(ve.Object, "status", "links", "internal", "cert")
	vols, _, _ := unstructured.NestedSlice(ve.Object, "status", "links", "internal", "volumes")
	for _, v := range vols {
		if v.(obj)["name"] != volume {
			continue
		}
		formats, _, _ := unstructured.NestedSlice(v.(obj), "formats")
		for _, f := range formats {
			if f.(obj)["format"] == "gzip" {
				return f.(obj)["url"].(string), cert, nil
			}
		}
	}
	return "", "", fmt.Errorf("volume %s not exported", volume)
}

func (c *Client) DeleteExport(ctx context.Context, ns, name string) error {
	exp := exportName(name)
	err := c.dyn.Resource(vmExportGVR).Namespace(ns).Delete(ctx, exp, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = c.dyn.Resource(secGVR).Namespace(ns).Delete(ctx, exp, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.deleteSnapshot(ctx, ns, name)
}

func importName(vm string) string { return "argus-import-" + vm }

func (c *Client) ImportVM(ctx context.Context, ns, name, operation string, ex Export) error {
	if err := c.guard(); err != nil {
		return err
	}
	if _, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return fmt.Errorf("%s already exists", name)
	}
	if err := c.FinishImport(ctx, ns, name); err != nil {
		return err
	}
	src := importName(name)
	cm := &unstructured.Unstructured{Object: obj{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   obj{"name": src},
		"data":       obj{"ca.pem": ex.CA},
	}}
	if _, err := c.dyn.Resource(cmGVR).Namespace(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return err
	}
	err := c.createImportVM(ctx, ns, name, operation, src, ex)
	if err != nil {
		c.FinishImport(context.Background(), ns, name)
	}
	return err
}

func (c *Client) createImportVM(ctx context.Context, ns, name, operation, src string, ex Export) error {
	if err := c.Secrets(ns).Create(ctx, src, "Opaque", map[string][]byte{"token": []byte("x-kubevirt-export-token: " + ex.Token)}); err != nil {
		return err
	}
	urls := map[string]string{}
	for _, d := range ex.Disks {
		urls[d.DataVolume] = d.URL
	}
	spec := ex.Spec
	templates, _, _ := unstructured.NestedSlice(spec, "dataVolumeTemplates")
	for _, t := range templates {
		dv, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
		u, ok := urls[dv]
		if !ok {
			return fmt.Errorf("no export for disk %s", dv)
		}
		source := obj{"http": obj{"url": u, "certConfigMap": src, "secretExtraHeaders": []interface{}{src}}}
		if err := unstructured.SetNestedMap(t.(obj), source, "spec", "source"); err != nil {
			return err
		}
		unstructured.RemoveNestedField(t.(obj), "spec", "sourceRef")
	}
	if err := unstructured.SetNestedSlice(spec, templates, "dataVolumeTemplates"); err != nil {
		return err
	}
	spec["runStrategy"] = "Halted"
	delete(spec, "running")
	vm := &unstructured.Unstructured{Object: obj{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   obj{"name": name, "annotations": obj{operationAnnotation: operation}},
		"spec":       spec,
	}}
	_, err := c.dyn.Resource(vmGVR).Namespace(ns).Create(ctx, vm, metav1.CreateOptions{})
	return err
}

func (c *Client) FinishImport(ctx context.Context, ns, name string) error {
	src := importName(name)
	for _, gvr := range []schema.GroupVersionResource{cmGVR, secGVR} {
		if err := c.dyn.Resource(gvr).Namespace(ns).Delete(ctx, src, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Client) DiskStatus(ctx context.Context, ns, name string) ([]DiskStatus, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	var out []DiskStatus
	for _, t := range templates {
		n, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
		st := DiskStatus{Name: n}
		if dv, err := c.dyn.Resource(dvGVR).Namespace(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			st.Phase, _, _ = unstructured.NestedString(dv.Object, "status", "phase")
			st.Progress, _, _ = unstructured.NestedString(dv.Object, "status", "progress")
		}
		out = append(out, st)
	}
	return out, nil
}

func RenameSpec(spec obj, from, to string) map[string]string {
	names := map[string]string{}
	templates, _, _ := unstructured.NestedSlice(spec, "dataVolumeTemplates")
	for _, t := range templates {
		dv, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
		n := to + "-" + dv
		if strings.HasPrefix(dv, from) {
			n = to + strings.TrimPrefix(dv, from)
		}
		names[dv] = n
		unstructured.SetNestedField(t.(obj), n, "metadata", "name")
	}
	unstructured.SetNestedSlice(spec, templates, "dataVolumeTemplates")
	vols, _, _ := unstructured.NestedSlice(spec, "template", "spec", "volumes")
	for _, v := range vols {
		if dv, ok, _ := unstructured.NestedString(v.(obj), "dataVolume", "name"); ok && names[dv] != "" {
			unstructured.SetNestedField(v.(obj), names[dv], "dataVolume", "name")
		}
	}
	unstructured.SetNestedSlice(spec, vols, "template", "spec", "volumes")
	ifaces, _, _ := unstructured.NestedSlice(spec, "template", "spec", "domain", "devices", "interfaces")
	for _, i := range ifaces {
		i.(obj)["macAddress"] = randomMAC()
	}
	unstructured.SetNestedSlice(spec, ifaces, "template", "spec", "domain", "devices", "interfaces")
	return names
}
