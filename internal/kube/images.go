package kube

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"argus/internal/lifecycle"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Namespace   = "default"
	imageLabel  = "argus.io/image"
	uploadProxy = "https://cdi-uploadproxy.cdi.svc"
)

var (
	dvGVR  = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "datavolumes"}
	utrGVR = schema.GroupVersionResource{Group: "upload.cdi.kubevirt.io", Version: "v1beta1", Resource: "uploadtokenrequests"}
	cmGVR  = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

type Image struct {
	Name     string
	Size     string
	Phase    string
	Progress string
	Source   string
	Error    string
	Restarts int64
}

func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	list, err := c.dyn.Resource(dvGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{LabelSelector: imageLabel + "=true"})
	if err != nil {
		return nil, err
	}
	out := make([]Image, 0, len(list.Items))
	for _, dv := range list.Items {
		o := dv.Object
		img := Image{Name: dv.GetName()}
		img.Size, _, _ = unstructured.NestedString(o, "spec", "storage", "resources", "requests", "storage")
		img.Phase, _, _ = unstructured.NestedString(o, "status", "phase")
		img.Progress, _, _ = unstructured.NestedString(o, "status", "progress")
		img.Restarts, _, _ = unstructured.NestedInt64(o, "status", "restartCount")
		img.Error = dvError(o)
		if u, ok, _ := unstructured.NestedString(o, "spec", "source", "http", "url"); ok {
			img.Source = u
		} else if u, ok, _ := unstructured.NestedString(o, "spec", "source", "registry", "url"); ok {
			img.Source = u
		} else {
			img.Source = "upload"
		}
		out = append(out, img)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) CreateImage(ctx context.Context, name, size, url string) error {
	source := map[string]any{"upload": map[string]any{}}
	if url != "" {
		source = map[string]any{"http": map[string]any{"url": url}}
	}
	dv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{imageLabel: "true"},
		},
		"spec": map[string]any{
			"source":  source,
			"storage": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}},
		},
	}}
	_, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Create(ctx, dv, metav1.CreateOptions{})
	return err
}

func (c *Client) DeleteImage(ctx context.Context, name string) error {
	dv, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if dv.GetLabels()[imageLabel] != "true" {
		return fmt.Errorf("%s is not an image", name)
	}
	if err := c.imageInUse(ctx, name); err != nil {
		return err
	}
	if err := c.dyn.Resource(dvGVR).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return err
	}
	return c.FinishImageImport(ctx, name)
}

func (c *Client) imageInUse(ctx context.Context, name string) error {
	dvs, err := c.dyn.Resource(dvGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	cloned := map[string]bool{}
	for _, dv := range dvs.Items {
		src, _, _ := unstructured.NestedString(dv.Object, "spec", "source", "pvc", "name")
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
		if src == name && phase != "Succeeded" {
			return fmt.Errorf("%s is still being cloned to %s", name, dv.GetName())
		}
		cloned[dv.GetName()] = phase == "Succeeded"
	}
	vms, err := c.dyn.Resource(vmGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, vm := range vms.Items {
		vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		for _, v := range vols {
			if claim, _, _ := unstructured.NestedString(asObj(v), "persistentVolumeClaim", "claimName"); claim == name {
				return fmt.Errorf("%s is attached as a CD-ROM to %s", name, vm.GetName())
			}
		}
		templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
		for _, t := range templates {
			src, _, _ := unstructured.NestedString(asObj(t), "spec", "source", "pvc", "name")
			dv, _, _ := unstructured.NestedString(asObj(t), "metadata", "name")
			if src == name && !cloned[dv] {
				return fmt.Errorf("%s has not finished cloning %s yet", vm.GetName(), name)
			}
		}
	}
	return nil
}

func (c *Client) Upload(ctx context.Context, name string, body io.Reader) error {
	if err := c.waitUploadReady(ctx, name); err != nil {
		return err
	}
	utr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "upload.cdi.kubevirt.io/v1beta1",
		"kind":       "UploadTokenRequest",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"pvcName": name},
	}}
	res, err := c.dyn.Resource(utrGVR).Namespace(Namespace).Create(ctx, utr, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	token, _, _ := unstructured.NestedString(res.Object, "status", "token")
	hc, err := c.uploadClient(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadProxy+"/v1beta1/upload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload %s: %s %s", name, resp.Status, msg)
	}
	return nil
}

func (c *Client) waitUploadReady(ctx context.Context, name string) error {
	for {
		dv, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		phase := ""
		if err == nil {
			phase, _, _ = unstructured.NestedString(dv.Object, "status", "phase")
		}
		switch phase {
		case "UploadReady":
			return nil
		case "Failed":
			return errors.New("upload target failed to start")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Client) uploadClient(ctx context.Context) (*http.Client, error) {
	cm, err := c.dyn.Resource(cmGVR).Namespace("cdi").Get(ctx, "cdi-uploadproxy-signer-bundle", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	ca, _, _ := unstructured.NestedString(cm.Object, "data", "ca-bundle.crt")
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca)) {
		return nil, errors.New("no CDI upload proxy CA")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}, nil
}

func imageExportName(name string) string { return "argus-img-" + name }

func (c *Client) ExportImage(ctx context.Context, name, baseURL, caPEM string) (Export, error) {
	dv, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Export{}, err
	}
	if dv.GetLabels()[imageLabel] != "true" {
		return Export{}, fmt.Errorf("%s is not an image", name)
	}
	if err := c.DeleteImageExport(ctx, name); err != nil {
		return Export{}, err
	}
	exp := imageExportName(name)
	if err := c.waitGone(ctx, Namespace, exp); err != nil {
		return Export{}, err
	}
	b := make([]byte, 24)
	rand.Read(b)
	token := hex.EncodeToString(b)
	if err := c.Secrets(Namespace).Create(ctx, exp, "Opaque", map[string][]byte{"token": []byte(token)}); err != nil {
		return Export{}, err
	}
	ve := &unstructured.Unstructured{Object: obj{
		"apiVersion": "export.kubevirt.io/v1",
		"kind":       "VirtualMachineExport",
		"metadata":   obj{"name": exp, "labels": obj{exportLabel: "true"}},
		"spec": obj{
			"source":         obj{"apiGroup": "", "kind": "PersistentVolumeClaim", "name": name},
			"tokenSecretRef": exp,
			"ttlDuration":    "13h",
		},
	}}
	if _, err := c.dyn.Resource(vmExportGVR).Namespace(Namespace).Create(ctx, ve, metav1.CreateOptions{}); err != nil {
		return Export{}, err
	}
	if err := c.waitExportReady(ctx, Namespace, exp); err != nil {
		return Export{}, err
	}
	size, _, _ := unstructured.NestedString(dv.Object, "spec", "storage", "resources", "requests", "storage")
	return Export{Spec: obj{"size": size}, Token: token, CA: caPEM, Disks: []ExportDisk{{DataVolume: name, URL: fmt.Sprintf("%s/export/%s/%s/%s", baseURL, Namespace, exp, name)}}}, nil
}

func (c *Client) DeleteImageExport(ctx context.Context, name string) error {
	for _, gvr := range []schema.GroupVersionResource{vmExportGVR, secGVR} {
		if err := c.dyn.Resource(gvr).Namespace(Namespace).Delete(ctx, imageExportName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func imageImportName(name string) string { return "argus-imgimport-" + name }

func (c *Client) ImportImage(ctx context.Context, name, size string, ex Export) error {
	if len(ex.Disks) != 1 {
		return errors.New("an image export has exactly one disk")
	}
	if err := c.FinishImageImport(ctx, name); err != nil {
		return err
	}
	src := imageImportName(name)
	cm := &unstructured.Unstructured{Object: obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": src}, "data": obj{"ca.pem": ex.CA}}}
	if _, err := c.dyn.Resource(cmGVR).Namespace(Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		return err
	}
	if err := c.Secrets(Namespace).Create(ctx, src, "Opaque", map[string][]byte{"token": []byte("x-kubevirt-export-token: " + ex.Token)}); err != nil {
		return err
	}
	dv := &unstructured.Unstructured{Object: obj{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata":   obj{"name": name, "labels": obj{imageLabel: "true"}},
		"spec": obj{
			"source":  obj{"http": obj{"url": ex.Disks[0].URL, "certConfigMap": src, "secretExtraHeaders": []any{src}}},
			"storage": obj{"resources": obj{"requests": obj{"storage": size}}},
		},
	}}
	if _, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Create(ctx, dv, metav1.CreateOptions{}); err != nil {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		return errors.Join(err, c.FinishImageImport(cctx, name))
	}
	return nil
}

type RegistrySource struct {
	Image string `json:"image"`
	CA    string `json:"ca,omitempty"`
	User  string `json:"user,omitempty"`
	Pass  string `json:"pass,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

func (c *Client) ImportRegistryImage(ctx context.Context, name, size string, src RegistrySource) error {
	if err := c.checkImageSpace(ctx, src.Bytes); err != nil {
		return err
	}
	if err := c.FinishImageImport(ctx, name); err != nil {
		return err
	}
	ref := imageImportName(name)
	reg := obj{"url": "docker://" + src.Image}
	if src.CA != "" {
		cm := &unstructured.Unstructured{Object: obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": ref}, "data": obj{"ca.pem": src.CA}}}
		if _, err := c.dyn.Resource(cmGVR).Namespace(Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return err
		}
		reg["certConfigMap"] = ref
	}
	if src.User != "" {
		if err := c.Secrets(Namespace).Create(ctx, ref, "Opaque", map[string][]byte{"accessKeyId": []byte(src.User), "secretKey": []byte(src.Pass)}); err != nil {
			cctx, cancel := lifecycle.CleanupContext()
			defer cancel()
			return errors.Join(err, c.FinishImageImport(cctx, name))
		}
		reg["secretRef"] = ref
	}
	dv := &unstructured.Unstructured{Object: obj{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata":   obj{"name": name, "labels": obj{imageLabel: "true"}},
		"spec": obj{
			"source":  obj{"registry": reg},
			"storage": obj{"resources": obj{"requests": obj{"storage": size}}},
		},
	}}
	if _, err := c.dyn.Resource(dvGVR).Namespace(Namespace).Create(ctx, dv, metav1.CreateOptions{}); err != nil {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		return errors.Join(err, c.FinishImageImport(cctx, name))
	}
	return nil
}

func (c *Client) FinishImageImport(ctx context.Context, name string) error {
	for _, gvr := range []schema.GroupVersionResource{cmGVR, secGVR} {
		if err := c.dyn.Resource(gvr).Namespace(Namespace).Delete(ctx, imageImportName(name), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Client) CreateTokenSource(ctx context.Context, name, ca, token string) error {
	if err := c.Secrets(Namespace).Create(ctx, name, "Opaque", map[string][]byte{"token": []byte("x-kubevirt-export-token: " + token)}); err != nil {
		return err
	}
	cm := &unstructured.Unstructured{Object: obj{"apiVersion": "v1", "kind": "ConfigMap", "metadata": obj{"name": name}, "data": obj{"ca.pem": ca}}}
	if _, err := c.dyn.Resource(cmGVR).Namespace(Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		c.DeleteTokenSource(cctx, name)
		return err
	}
	return nil
}

func (c *Client) DeleteTokenSource(ctx context.Context, name string) {
	for _, gvr := range []schema.GroupVersionResource{secGVR, cmGVR} {
		if err := c.dyn.Resource(gvr).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			log.Printf("delete %s %s: %v", gvr.Resource, name, err)
		}
	}
}

func (c *Client) CleanupInterrupted(ctx context.Context) []string {
	var out []string
	if list, err := c.dyn.Resource(vmExportGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, e := range list.Items {
			if vm, ok := strings.CutPrefix(e.GetName(), "argus-backup-"); ok {
				if err := c.DeleteBackupExport(ctx, Namespace, vm); err != nil {
					log.Printf("cleanup %s: %v", e.GetName(), err)
					continue
				}
				out = append(out, e.GetName())
			}
		}
	}
	if list, err := c.dyn.Resource(dvGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{LabelSelector: importLabel}); err == nil {
		for _, dv := range list.Items {
			if len(dv.GetOwnerReferences()) == 0 && c.DeleteDataVolume(ctx, Namespace, dv.GetName()) == nil {
				out = append(out, dv.GetName())
			}
		}
	}
	if list, err := c.dyn.Resource(cmGVR).Namespace(Namespace).List(ctx, metav1.ListOptions{}); err == nil {
		for _, s := range list.Items {
			if strings.HasPrefix(s.GetName(), "argus-esxi-") {
				c.DeleteTokenSource(ctx, s.GetName())
				out = append(out, s.GetName())
			}
		}
	}
	return out
}

func dvError(o map[string]any) string {
	conds, _, _ := unstructured.NestedSlice(o, "status", "conditions")
	for _, c := range conds {
		msg, _, _ := unstructured.NestedString(c.(map[string]any), "message")
		if i := strings.Index(msg, "Unable to"); i >= 0 {
			return msg[i:]
		}
	}
	return ""
}

func (c *Client) checkImageSpace(ctx context.Context, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	classes, err := c.dyn.Resource(scGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var pool string
	for _, sc := range classes.Items {
		if sc.GetAnnotations()["storageclass.kubernetes.io/is-default-class"] == "true" {
			pool, _, _ = unstructured.NestedString(sc.Object, "parameters", "poolname")
		}
	}
	pools, err := c.Pools(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pools, func(p Pool) bool { return p.Name == pool })
	if need := bytes*2 + bytes/10; i >= 0 && pools[i].Free < need {
		return fmt.Errorf("pool %s has %.1f GiB free; copying this image needs about %.1f GiB for the download and the disk", pool, float64(pools[i].Free)/(1<<30), float64(need)/(1<<30))
	}
	return nil
}
