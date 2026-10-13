package kube

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"time"

	"argus/internal/lifecycle"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
)

const (
	haLabel             = "argus.io/ha"
	haStoppedAnnotation = "argus.io/ha-stopped"
)

type HAVM struct {
	Namespace   string
	Name        string
	Running     bool
	UserStopped bool
}

func (c *Client) SetHA(ctx context.Context, ns, name string, enabled bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		labels := vm.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		strategy := "Manual"
		if enabled {
			labels[haLabel] = "true"
		} else {
			delete(labels, haLabel)
			strategy = "Halted"
			if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
				strategy = "Always"
			}
		}
		vm.SetLabels(labels)
		unstructured.SetNestedField(vm.Object, strategy, "spec", "runStrategy")
		unstructured.RemoveNestedField(vm.Object, "spec", "running")
		_, err = c.dyn.Resource(vmGVR).Namespace(ns).Update(ctx, vm, metav1.UpdateOptions{})
		return err
	})
}

func (c *Client) HAVMs(ctx context.Context) ([]HAVM, error) {
	vms, err := c.dyn.Resource(vmGVR).List(ctx, metav1.ListOptions{LabelSelector: haLabel + "=true"})
	if err != nil {
		return nil, err
	}
	vmis, err := c.dyn.Resource(vmiGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	running := map[string]bool{}
	for _, i := range vmis.Items {
		if phase, _, _ := unstructured.NestedString(i.Object, "status", "phase"); phase != "Failed" && phase != "Succeeded" {
			running[i.GetNamespace()+"/"+i.GetName()] = true
		}
	}
	out := make([]HAVM, 0, len(vms.Items))
	for _, v := range vms.Items {
		out = append(out, HAVM{Namespace: v.GetNamespace(), Name: v.GetName(), Running: running[v.GetNamespace()+"/"+v.GetName()], UserStopped: v.GetAnnotations()[haStoppedAnnotation] == "true"})
	}
	return out, nil
}

func snapshotName(vm string) string { return "argus-repl-" + vm }

func (c *Client) ExportVMSnapshot(ctx context.Context, ns, name, baseURL, caPEM string) (Export, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Export{}, err
	}
	if err := migratable(vm); err != nil {
		return Export{}, err
	}
	if err := c.DeleteExport(ctx, ns, name); err != nil {
		return Export{}, err
	}
	return c.snapshotExport(ctx, ns, vm, exportName(name), snapshotName(name), baseURL, caPEM)
}

func backupName(vm string) string { return "argus-backup-" + vm }

func (c *Client) BackupExport(ctx context.Context, ns, name string) (Export, error) {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Export{}, err
	}
	if err := c.DeleteBackupExport(ctx, ns, name); err != nil {
		return Export{}, err
	}
	return c.snapshotExport(ctx, ns, vm, backupName(name), backupName(name), "", "")
}

func (c *Client) BackupSource(ctx context.Context, ns, name, volume string) (string, string, error) {
	return c.ExportSource(ctx, ns, backupName(name), volume)
}

func (c *Client) DeleteBackupExport(ctx context.Context, ns, name string) error {
	for _, gvr := range []schema.GroupVersionResource{vmExportGVR, secGVR, vmSnapGVR} {
		if err := c.dyn.Resource(gvr).Namespace(ns).Delete(ctx, backupName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Client) snapshotExport(ctx context.Context, ns string, vm *unstructured.Unstructured, exp, snap, baseURL, caPEM string) (Export, error) {
	name := vm.GetName()
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		_, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Get(wctx, snap, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		select {
		case <-wctx.Done():
			return Export{}, errors.New("previous export snapshot is still being deleted")
		case <-time.After(2 * time.Second):
		}
	}
	vs := &unstructured.Unstructured{Object: obj{
		"apiVersion": "snapshot.kubevirt.io/v1beta1",
		"kind":       "VirtualMachineSnapshot",
		"metadata":   obj{"name": snap},
		"spec":       obj{"source": obj{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": name}},
	}}
	if _, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Create(ctx, vs, metav1.CreateOptions{}); err != nil {
		return Export{}, err
	}
	if err := c.waitSnapshotReady(ctx, ns, snap); err != nil {
		return Export{}, err
	}
	return c.export(ctx, ns, exp, baseURL, caPEM, vm, obj{"apiGroup": "snapshot.kubevirt.io", "kind": "VirtualMachineSnapshot", "name": snap})
}

func (c *Client) waitSnapshotReady(ctx context.Context, ns, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		s, err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if ready, _, _ := unstructured.NestedBool(s.Object, "status", "readyToUse"); ready {
			return nil
		}
		if phase, _, _ := unstructured.NestedString(s.Object, "status", "phase"); phase == "Failed" {
			return fmt.Errorf("snapshot of %s failed", name)
		}
		select {
		case <-ctx.Done():
			return errors.New("snapshot did not become ready")
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) deleteSnapshot(ctx context.Context, ns, vm string) error {
	err := c.dyn.Resource(vmSnapGVR).Namespace(ns).Delete(ctx, snapshotName(vm), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) CheckBackupable(ctx context.Context, ns, name string) error {
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
	for _, t := range templates {
		n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
		owned[n] = true
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, v := range volumes {
		vo := asObj(v)
		if dv, ok := vo["dataVolume"].(obj); ok && !owned[asStr(dv["name"])] {
			return fmt.Errorf("disk %s is an attached volume, which backups do not include; detach it first", vo["name"])
		}
		if _, ok := vo["persistentVolumeClaim"]; ok && vo["name"] != "cdrom" {
			return fmt.Errorf("disk %s is an attached volume, which backups do not include; detach it first", vo["name"])
		}
	}
	return nil
}

var genSuffix = regexp.MustCompile(`-g[0-9]+$`)

func replGenName(vm string) string { return "argus-replgen-" + vm }

func (c *Client) RefreshReplicaDisks(ctx context.Context, ns, vm string, ex Export) (map[string]string, error) {
	if err := c.StoppedReplica(ctx, ns, vm); err != nil {
		return nil, err
	}
	src := replGenName(vm)
	c.deleteReplGenSource(ctx, ns, src)
	cm := &unstructured.Unstructured{Object: obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": src}, "data": obj{"ca.pem": ex.CA}}}
	if _, err := c.dyn.Resource(cmGVR).Namespace(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return nil, err
	}
	if err := c.Secrets(ns).Create(ctx, src, "Opaque", map[string][]byte{"token": []byte("x-kubevirt-export-token: " + ex.Token)}); err != nil {
		return nil, err
	}
	sizes := map[string]any{}
	templates, _, _ := unstructured.NestedSlice(ex.Spec, "dataVolumeTemplates")
	for _, t := range templates {
		n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
		sizes[n], _, _ = unstructured.NestedMap(asObj(t), "spec", "storage")
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mapping := map[string]string{}
	for _, d := range ex.Disks {
		name := genSuffix.ReplaceAllString(d.DataVolume, "") + "-g" + stamp
		storage, _ := sizes[d.DataVolume].(map[string]any)
		dv := &unstructured.Unstructured{Object: obj{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataVolume",
			"metadata":   obj{"name": name},
			"spec": obj{
				"source":  obj{"http": obj{"url": d.URL, "certConfigMap": src, "secretExtraHeaders": []any{src}}},
				"storage": storage,
			},
		}}
		if _, err := c.dyn.Resource(dvGVR).Namespace(ns).Create(ctx, dv, metav1.CreateOptions{}); err != nil {
			cctx, cancel := lifecycle.CleanupContext()
			defer cancel()
			c.AbortReplicaRefresh(cctx, ns, vm, mapping)
			return nil, err
		}
		mapping[d.DataVolume] = name
	}
	return mapping, nil
}

func (c *Client) deleteReplGenSource(ctx context.Context, ns, src string) {
	for _, gvr := range []schema.GroupVersionResource{cmGVR, secGVR} {
		if err := c.dyn.Resource(gvr).Namespace(ns).Delete(ctx, src, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("delete %s %s: %v", gvr.Resource, src, err)
		}
	}
}

func (c *Client) AbortReplicaRefresh(ctx context.Context, ns, vm string, mapping map[string]string) {
	o, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, vm, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		log.Printf("abort replica refresh of %s: %v; keeping its disks", vm, err)
		return
	}
	used := map[string]bool{}
	if err == nil {
		templates, _, _ := unstructured.NestedSlice(o.Object, "spec", "dataVolumeTemplates")
		for _, t := range templates {
			n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
			used[n] = true
		}
	}
	for _, n := range mapping {
		if used[n] {
			continue
		}
		if err := c.DeleteDataVolume(ctx, ns, n); err != nil {
			log.Printf("abort replica refresh of %s: %v", vm, err)
		}
	}
	c.deleteReplGenSource(ctx, ns, replGenName(vm))
}

func (c *Client) SwapReplica(ctx context.Context, ns, vm string, spec obj, mapping map[string]string) error {
	for _, n := range mapping {
		if phase, err := c.DataVolumePhase(ctx, ns, n); err != nil || phase != "Succeeded" {
			return fmt.Errorf("refreshed disk %s is not complete", n)
		}
	}
	var old []string
	err := c.editStoppedVM(ctx, ns, vm, func(o obj) error {
		if err := isReplica(o, vm); err != nil {
			return err
		}
		old = old[:0]
		templates, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
		for _, t := range templates {
			n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
			old = append(old, n)
		}
		next := runtime.DeepCopyJSON(spec)
		tpls, _, _ := unstructured.NestedSlice(next, "dataVolumeTemplates")
		for _, t := range tpls {
			n, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
			nn, ok := mapping[n]
			if !ok {
				return fmt.Errorf("no refreshed copy of disk %s", n)
			}
			dv, err := c.dyn.Resource(dvGVR).Namespace(ns).Get(ctx, nn, metav1.GetOptions{})
			if err != nil {
				return err
			}
			dvSpec, _, _ := unstructured.NestedMap(dv.Object, "spec")
			unstructured.SetNestedField(asObj(t), nn, "metadata", "name")
			unstructured.SetNestedMap(asObj(t), dvSpec, "spec")
		}
		unstructured.SetNestedSlice(next, tpls, "dataVolumeTemplates")
		vols, _, _ := unstructured.NestedSlice(next, "template", "spec", "volumes")
		for _, v := range vols {
			if n, ok, _ := unstructured.NestedString(asObj(v), "dataVolume", "name"); ok && mapping[n] != "" {
				unstructured.SetNestedField(asObj(v), mapping[n], "dataVolume", "name")
			}
		}
		unstructured.SetNestedSlice(next, vols, "template", "spec", "volumes")
		next["runStrategy"] = "Halted"
		delete(next, "running")
		o["spec"] = next
		return nil
	})
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, n := range mapping {
		keep[n] = true
	}
	for _, n := range old {
		if !keep[n] {
			if err := c.DeleteDataVolume(ctx, ns, n); err != nil {
				log.Printf("delete replica disk %s: %v", n, err)
			}
		}
	}
	c.deleteReplGenSource(ctx, ns, replGenName(vm))
	return nil
}
