package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

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
		if u, ok, _ := unstructured.NestedString(o, "spec", "source", "http", "url"); ok {
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
	source := map[string]interface{}{"upload": map[string]interface{}{}}
	if url != "" {
		source = map[string]interface{}{"http": map[string]interface{}{"url": url}}
	}
	dv := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cdi.kubevirt.io/v1beta1",
		"kind":       "DataVolume",
		"metadata": map[string]interface{}{
			"name":   name,
			"labels": map[string]interface{}{imageLabel: "true"},
		},
		"spec": map[string]interface{}{
			"source":  source,
			"storage": map[string]interface{}{"resources": map[string]interface{}{"requests": map[string]interface{}{"storage": size}}},
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
	return c.dyn.Resource(dvGVR).Namespace(Namespace).Delete(ctx, name, metav1.DeleteOptions{})
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
			if claim, _, _ := unstructured.NestedString(v.(obj), "persistentVolumeClaim", "claimName"); claim == name {
				return fmt.Errorf("%s is attached as a CD-ROM to %s", name, vm.GetName())
			}
		}
		templates, _, _ := unstructured.NestedSlice(vm.Object, "spec", "dataVolumeTemplates")
		for _, t := range templates {
			src, _, _ := unstructured.NestedString(t.(obj), "spec", "source", "pvc", "name")
			dv, _, _ := unstructured.NestedString(t.(obj), "metadata", "name")
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
	utr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "upload.cdi.kubevirt.io/v1beta1",
		"kind":       "UploadTokenRequest",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"pvcName": name},
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
		if err != nil {
			return err
		}
		phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
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
