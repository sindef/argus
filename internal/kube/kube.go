package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	vmGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	secGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

type Client struct {
	cfg  *rest.Config
	dyn  dynamic.Interface
	http *http.Client
	tls  *tls.Config

	Guard func() error
}

func (c *Client) guard() error {
	if c.Guard == nil {
		return nil
	}
	return c.Guard()
}

type VM struct {
	Namespace    string
	Name         string
	DisplayName  string
	Status       string
	Tags         []string
	Template     bool
	Backup       string
	Running      bool
	Cores        int64
	Memory       string
	StorageBytes int64
	Node         string
	OS           string
	Started      time.Time
	Disks        []Disk
	NICs         []NIC
}

func (v VM) Label() string {
	if v.DisplayName != "" {
		return v.DisplayName
	}
	return v.Name
}

func (v VM) IPs() []string {
	var out []string
	for _, n := range v.NICs {
		out = append(out, n.IPs...)
	}
	return out
}

type Disk struct {
	Name         string
	Bus          string
	Source       string
	StorageClass string
}

type NIC struct {
	Name    string
	Network string
	MAC     string
	IPs     []string
}

func New() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	tc, err := rest.TLSConfigFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, dyn: dyn, http: hc, tls: tc}, nil
}

type Secrets struct {
	r dynamic.ResourceInterface
}

func (c *Client) Secrets(ns string) Secrets {
	return Secrets{r: c.dyn.Resource(secGVR).Namespace(ns)}
}

func (s Secrets) Get(ctx context.Context, name string) (map[string][]byte, string, error) {
	u, err := s.r.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, "", err
	}
	raw, _, _ := unstructured.NestedStringMap(u.Object, "data")
	data := make(map[string][]byte, len(raw))
	for k, v := range raw {
		if data[k], err = base64.StdEncoding.DecodeString(v); err != nil {
			return nil, "", err
		}
	}
	return data, u.GetResourceVersion(), nil
}

func (s Secrets) Create(ctx context.Context, name, typ string, data map[string][]byte) error {
	_, err := s.r.Create(ctx, secret(name, typ, "", data), metav1.CreateOptions{})
	return err
}

func (s Secrets) Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error {
	_, err := s.r.Update(ctx, secret(name, "Opaque", resourceVersion, data), metav1.UpdateOptions{})
	return err
}

func secret(name, typ, resourceVersion string, data map[string][]byte) *unstructured.Unstructured {
	enc := make(map[string]interface{}, len(data))
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString(v)
	}
	meta := map[string]interface{}{"name": name}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       typ,
		"data":       enc,
	}}
}

func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	vms, err := c.dyn.Resource(vmGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	vmis, err := c.dyn.Resource(vmiGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	inst := map[string]*unstructured.Unstructured{}
	for i := range vmis.Items {
		inst[vmis.Items[i].GetNamespace()+"/"+vmis.Items[i].GetName()] = &vmis.Items[i]
	}
	out := make([]VM, 0, len(vms.Items))
	for i := range vms.Items {
		v := &vms.Items[i]
		out = append(out, toVM(v, inst[v.GetNamespace()+"/"+v.GetName()]))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Client) GetVM(ctx context.Context, ns, name string) (VM, error) {
	v, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return VM{}, err
	}
	vmi, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		vmi = nil
	}
	return toVM(v, vmi), nil
}

func ValidAction(action string) bool {
	switch action {
	case "start", "stop", "restart", "pause", "unpause", "template", "untemplate":
		return true
	}
	return false
}

const TemplateLabel = "argus.io/template"

func (c *Client) setTemplate(ctx context.Context, ns, name string, on bool) error {
	if on {
		if _, err := c.dyn.Resource(vmiGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("power off %s first", name)
		}
	}
	value := "null"
	if on {
		value = `"true"`
	}
	patch := []byte(`{"metadata":{"labels":{"` + TemplateLabel + `":` + value + `}}}`)
	_, err := c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) Action(ctx context.Context, ns, name, action string) error {
	if action == "template" || action == "untemplate" {
		return c.setTemplate(ctx, ns, name, action == "template")
	}
	if action == "start" || action == "restart" {
		if vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{}); err == nil && vm.GetLabels()[TemplateLabel] == "true" {
			return fmt.Errorf("%s is a template; convert it to a VM first", name)
		}
	}
	if err := c.Power(ctx, ns, name, action); err != nil {
		return err
	}
	if action != "start" && action != "stop" {
		return nil
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || vm.GetLabels()[haLabel] != "true" {
		return err
	}
	value := "null"
	if action == "stop" {
		value = `"true"`
	}
	patch := []byte(`{"metadata":{"annotations":{"` + haStoppedAnnotation + `":` + value + `}}}`)
	_, err = c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) Power(ctx context.Context, ns, name, action string) error {
	if !ValidAction(action) {
		return fmt.Errorf("unknown action %q", action)
	}
	if action == "start" || action == "restart" || action == "unpause" {
		if err := c.guard(); err != nil {
			return err
		}
	}
	resource := "virtualmachines"
	if action == "pause" || action == "unpause" {
		resource = "virtualmachineinstances"
	}
	u := c.cfg.Host + subresourcePath(ns, resource, name, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewBufferString("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var st struct{ Message string }
		json.NewDecoder(resp.Body).Decode(&st)
		if st.Message == "" {
			st.Message = resp.Status
		}
		return fmt.Errorf("%s %s: %s", action, name, st.Message)
	}
	return nil
}

func (c *Client) DialConsole(ctx context.Context, ns, name, kind string) (*websocket.Conn, error) {
	if kind != "vnc" && kind != "console" {
		return nil, fmt.Errorf("unknown console %q", kind)
	}
	u, err := url.Parse(c.cfg.Host + subresourcePath(ns, "virtualmachineinstances", name, kind))
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	h := http.Header{}
	if tok, err := c.token(); err != nil {
		return nil, err
	} else if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	d := websocket.Dialer{TLSClientConfig: c.tls, Subprotocols: []string{"plain.kubevirt.io"}}
	conn, resp, err := d.DialContext(ctx, u.String(), h)
	if err != nil && resp != nil {
		return nil, fmt.Errorf("%s %s/%s: %s", kind, ns, name, resp.Status)
	}
	return conn, err
}

func (c *Client) token() (string, error) {
	if c.cfg.BearerTokenFile != "" {
		b, err := os.ReadFile(c.cfg.BearerTokenFile)
		return strings.TrimSpace(string(b)), err
	}
	return c.cfg.BearerToken, nil
}

func subresourcePath(ns, resource, name, sub string) string {
	return fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/%s/%s/%s", ns, resource, name, sub)
}

func toVM(v, vmi *unstructured.Unstructured) VM {
	o := v.Object
	vm := VM{Namespace: v.GetNamespace(), Name: v.GetName(), DisplayName: v.GetAnnotations()[displayNameAnnotation], Backup: v.GetAnnotations()[BackupAnnotation], Template: v.GetLabels()[TemplateLabel] == "true"}
	if t := v.GetAnnotations()[TagsAnnotation]; t != "" {
		vm.Tags = strings.Split(t, ",")
	}
	vm.Status, _, _ = unstructured.NestedString(o, "status", "printableStatus")
	templates, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
	for _, t := range templates {
		if q, ok, _ := unstructured.NestedString(t.(map[string]interface{}), "spec", "storage", "resources", "requests", "storage"); ok {
			if qty, err := resource.ParseQuantity(q); err == nil {
				vm.StorageBytes += qty.Value()
			}
		}
	}
	vm.Running = vm.Status == "Running"
	vm.Cores, _, _ = unstructured.NestedInt64(o, "spec", "template", "spec", "domain", "cpu", "cores")
	if vm.Memory, _, _ = unstructured.NestedString(o, "spec", "template", "spec", "domain", "memory", "guest"); vm.Memory == "" {
		vm.Memory, _, _ = unstructured.NestedString(o, "spec", "template", "spec", "domain", "resources", "requests", "memory")
	}

	classes := map[string]string{}
	tpls, _, _ := unstructured.NestedSlice(o, "spec", "dataVolumeTemplates")
	for _, t := range tpls {
		n, _, _ := unstructured.NestedString(t.(map[string]interface{}), "metadata", "name")
		classes[n], _, _ = unstructured.NestedString(t.(map[string]interface{}), "spec", "storage", "storageClassName")
	}
	sources, volumeClass := map[string]string{}, map[string]string{}
	vols, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "volumes")
	for _, x := range vols {
		m, _ := x.(map[string]interface{})
		n, _ := m["name"].(string)
		for k, val := range m {
			if k == "name" {
				continue
			}
			src := k
			if mm, ok := val.(map[string]interface{}); ok {
				if s, ok := mm["name"].(string); ok {
					src += ": " + s
				} else if s, ok := mm["claimName"].(string); ok {
					src += ": " + s
				}
			}
			sources[n] = src
		}
		if dv, ok := m["dataVolume"].(map[string]interface{}); ok {
			volumeClass[n] = classes[fmt.Sprint(dv["name"])]
		}
	}
	disks, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "disks")
	for _, x := range disks {
		m, _ := x.(map[string]interface{})
		d := Disk{}
		d.Name, _ = m["name"].(string)
		for _, t := range []string{"disk", "cdrom", "lun"} {
			if mm, ok := m[t].(map[string]interface{}); ok {
				d.Bus, _ = mm["bus"].(string)
			}
		}
		d.Source, d.StorageClass = sources[d.Name], volumeClass[d.Name]
		vm.Disks = append(vm.Disks, d)
	}

	nets := map[string]string{}
	ns, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "networks")
	for _, x := range ns {
		m, _ := x.(map[string]interface{})
		n, _ := m["name"].(string)
		if mm, ok := m["multus"].(map[string]interface{}); ok {
			nets[n], _ = mm["networkName"].(string)
		} else if _, ok := m["pod"]; ok {
			nets[n] = "pod"
		}
	}
	ifaces, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "domain", "devices", "interfaces")
	for _, x := range ifaces {
		m, _ := x.(map[string]interface{})
		n := NIC{}
		n.Name, _ = m["name"].(string)
		n.MAC, _ = m["macAddress"].(string)
		n.Network = nets[n.Name]
		vm.NICs = append(vm.NICs, n)
	}

	if vmi == nil {
		return vm
	}
	vm.Node, _, _ = unstructured.NestedString(vmi.Object, "status", "nodeName")
	vm.OS, _, _ = unstructured.NestedString(vmi.Object, "status", "guestOSInfo", "prettyName")
	vm.Started = vmi.GetCreationTimestamp().Time
	st, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	for _, x := range st {
		m, _ := x.(map[string]interface{})
		name, _ := m["name"].(string)
		mac, _ := m["mac"].(string)
		ips, _, _ := unstructured.NestedStringSlice(m, "ipAddresses")
		for i := range vm.NICs {
			if vm.NICs[i].Name == name {
				vm.NICs[i].MAC = mac
				vm.NICs[i].IPs = ips
			}
		}
	}
	return vm
}

func (c *Client) RunningVMs(ctx context.Context) (int, error) {
	list, err := c.dyn.Resource(vmiGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, i := range list.Items {
		if phase, _, _ := unstructured.NestedString(i.Object, "status", "phase"); phase != "Failed" && phase != "Succeeded" {
			n++
		}
	}
	return n, nil
}
