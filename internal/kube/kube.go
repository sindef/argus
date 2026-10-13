package kube

import (
	"argus/internal/store"
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	cache atomic.Pointer[vmCache]
}

func (c *Client) guard() error {
	if c.Guard == nil {
		return nil
	}
	return c.Guard()
}

type VM struct {
	Namespace      string
	Name           string
	DisplayName    string
	Status         string
	Tags           []string
	Template       bool
	Backup         string
	Schedule       string
	Folder         string
	Attributes     map[string]string
	Running        bool
	Cores          int64
	Memory         string
	StorageBytes   int64
	Node           string
	OS             string
	Started        time.Time
	Disks          []Disk
	NICs           []NIC
	PendingRestart bool
	Agent          bool
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
	SizeBytes    int64
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
		return nil, "", storeError(err)
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
	return storeError(err)
}

func (s Secrets) Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error {
	_, err := s.r.Update(ctx, secret(name, "Opaque", resourceVersion, data), metav1.UpdateOptions{})
	return storeError(err)
}

func storeError(err error) error {
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %w", store.ErrNotFound, err)
	case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	return err
}

func secret(name, typ, resourceVersion string, data map[string][]byte) *unstructured.Unstructured {
	enc := make(map[string]any, len(data))
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString(v)
	}
	meta := map[string]any{"name": name}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   meta,
		"type":       typ,
		"data":       enc,
	}}
}

func (c *Client) listVMObjects(ctx context.Context) ([]*unstructured.Unstructured, map[string]*unstructured.Unstructured, error) {
	if vc := c.cache.Load(); vc != nil {
		return vc.list()
	}
	vms, err := c.dyn.Resource(vmGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	vmis, err := c.dyn.Resource(vmiGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, err
	}
	out := make([]*unstructured.Unstructured, len(vms.Items))
	for i := range vms.Items {
		out[i] = &vms.Items[i]
	}
	inst := map[string]*unstructured.Unstructured{}
	for i := range vmis.Items {
		inst[vmis.Items[i].GetNamespace()+"/"+vmis.Items[i].GetName()] = &vmis.Items[i]
	}
	return out, inst, nil
}

func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	vms, inst, err := c.listVMObjects(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]VM, 0, len(vms))
	for _, v := range vms {
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
	case "start", "stop", "poweroff", "restart", "pause", "unpause", "template", "untemplate":
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
		} else if err == nil && vm.GetAnnotations()[diskMoveAnnotation] != "" {
			return fmt.Errorf("a disk of %s is being moved; start it when the move finishes", name)
		}
	}
	if err := c.Power(ctx, ns, name, action); err != nil {
		return err
	}
	if action != "start" && action != "stop" && action != "poweroff" {
		return nil
	}
	vm, err := c.dyn.Resource(vmGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil || vm.GetLabels()[haLabel] != "true" {
		return err
	}
	value := "null"
	if action != "start" {
		value = `"true"`
	}
	patch := []byte(`{"metadata":{"annotations":{"` + haStoppedAnnotation + `":` + value + `}}}`)
	_, err = c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) ClearHAStop(ctx context.Context, ns, name string) error {
	patch := []byte(`{"metadata":{"annotations":{"` + haStoppedAnnotation + `":null}}}`)
	_, err := c.dyn.Resource(vmGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
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
	if action == "poweroff" {
		return c.subresource(ctx, ns, resource, name, "stop", `{"gracePeriod":0}`)
	}
	return c.subresource(ctx, ns, resource, name, action, "{}")
}

func (c *Client) subresource(ctx context.Context, ns, resource, name, sub, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.cfg.Host+subresourcePath(ns, resource, name, sub), bytes.NewBufferString(body))
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
		if json.NewDecoder(resp.Body).Decode(&st) != nil || st.Message == "" {
			st.Message = resp.Status
		}
		return fmt.Errorf("%s %s: %s", sub, name, st.Message)
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
	if resp != nil {
		resp.Body.Close()
	}
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

type condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

type nameRef struct {
	Name      string `json:"name"`
	ClaimName string `json:"claimName"`
}

type vmObject struct {
	Spec struct {
		DataVolumeTemplates []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Storage struct {
					StorageClassName string `json:"storageClassName"`
					Resources        struct {
						Requests struct {
							Storage string `json:"storage"`
						} `json:"requests"`
					} `json:"resources"`
				} `json:"storage"`
			} `json:"spec"`
		} `json:"dataVolumeTemplates"`
		Template struct {
			Spec struct {
				Domain struct {
					Memory struct {
						Guest string `json:"guest"`
					} `json:"memory"`
					Resources struct {
						Requests struct {
							Memory string `json:"memory"`
						} `json:"requests"`
					} `json:"resources"`
					Devices struct {
						Disks []struct {
							Name  string `json:"name"`
							Disk  *bus   `json:"disk"`
							CDROM *bus   `json:"cdrom"`
							LUN   *bus   `json:"lun"`
						} `json:"disks"`
						Interfaces []struct {
							Name       string `json:"name"`
							MacAddress string `json:"macAddress"`
						} `json:"interfaces"`
					} `json:"devices"`
				} `json:"domain"`
				Volumes  []map[string]any `json:"volumes"`
				Networks []struct {
					Name   string `json:"name"`
					Multus *struct {
						NetworkName string `json:"networkName"`
					} `json:"multus"`
					Pod *struct{} `json:"pod"`
				} `json:"networks"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		PrintableStatus string      `json:"printableStatus"`
		Conditions      []condition `json:"conditions"`
	} `json:"status"`
}

type bus struct {
	Bus string `json:"bus"`
}

type vmiObject struct {
	Status struct {
		NodeName    string `json:"nodeName"`
		GuestOSInfo struct {
			PrettyName string `json:"prettyName"`
		} `json:"guestOSInfo"`
		Conditions []condition `json:"conditions"`
		Interfaces []struct {
			Name        string   `json:"name"`
			MAC         string   `json:"mac"`
			IPAddresses []string `json:"ipAddresses"`
		} `json:"interfaces"`
	} `json:"status"`
}

func conditionTrue(cs []condition, types ...string) bool {
	return slices.ContainsFunc(cs, func(c condition) bool { return c.Status == "True" && slices.Contains(types, c.Type) })
}

func volumeSource(v map[string]any) string {
	for k, val := range v {
		if k == "name" {
			continue
		}
		var ref nameRef
		if m, ok := val.(map[string]any); ok && runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ref) == nil {
			if n := cmp.Or(ref.Name, ref.ClaimName); n != "" {
				return k + ": " + n
			}
		}
		return k
	}
	return ""
}

func toVM(v, vmi *unstructured.Unstructured) VM {
	ann := v.GetAnnotations()
	vm := VM{Namespace: v.GetNamespace(), Name: v.GetName(), DisplayName: ann[displayNameAnnotation], Backup: ann[BackupAnnotation], Schedule: ann[ScheduleAnnotation],
		Folder: ann[FolderAnnotation], Template: v.GetLabels()[TemplateLabel] == "true"}
	if t := ann[TagsAnnotation]; t != "" {
		vm.Tags = strings.Split(t, ",")
	}
	if a := ann[AttributesAnnotation]; a != "" && json.Unmarshal([]byte(a), &vm.Attributes) != nil {
		vm.Attributes = nil
	}
	var o vmObject
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(v.Object, &o); err != nil {
		log.Printf("read VM %s: %v", v.GetName(), err)
	}
	vm.Status = o.Status.PrintableStatus
	vm.Running = vm.Status == "Running"
	vm.PendingRestart = vm.Running && conditionTrue(o.Status.Conditions, "RestartRequired", "MigrationRequired")
	vm.Cores, _ = vcpus(v.Object)
	tpl := o.Spec.Template.Spec
	vm.Memory = cmp.Or(tpl.Domain.Memory.Guest, tpl.Domain.Resources.Requests.Memory)

	classes, sizes := map[string]string{}, map[string]int64{}
	for _, t := range o.Spec.DataVolumeTemplates {
		classes[t.Metadata.Name] = t.Spec.Storage.StorageClassName
		if qty, err := resource.ParseQuantity(t.Spec.Storage.Resources.Requests.Storage); err == nil {
			sizes[t.Metadata.Name] = qty.Value()
			vm.StorageBytes += qty.Value()
		}
	}
	sources, volumeClass, volumeSize := map[string]string{}, map[string]string{}, map[string]int64{}
	for _, vol := range tpl.Volumes {
		n := fmt.Sprint(vol["name"])
		sources[n] = volumeSource(vol)
		if dv, ok := vol["dataVolume"].(map[string]any); ok {
			volumeClass[n], volumeSize[n] = classes[fmt.Sprint(dv["name"])], sizes[fmt.Sprint(dv["name"])]
		}
	}
	for _, d := range tpl.Domain.Devices.Disks {
		b := cmp.Or(d.Disk, d.CDROM, d.LUN, &bus{})
		vm.Disks = append(vm.Disks, Disk{Name: d.Name, Bus: b.Bus, Source: sources[d.Name], StorageClass: volumeClass[d.Name], SizeBytes: volumeSize[d.Name]})
	}
	nets := map[string]string{}
	for _, n := range tpl.Networks {
		switch {
		case n.Multus != nil:
			nets[n.Name] = n.Multus.NetworkName
		case n.Pod != nil:
			nets[n.Name] = "pod"
		}
	}
	for _, i := range tpl.Domain.Devices.Interfaces {
		vm.NICs = append(vm.NICs, NIC{Name: i.Name, MAC: i.MacAddress, Network: nets[i.Name]})
	}
	if vmi == nil {
		return vm
	}
	var io vmiObject
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(vmi.Object, &io); err != nil {
		log.Printf("read VMI %s: %v", vmi.GetName(), err)
	}
	vm.Node, vm.OS = io.Status.NodeName, io.Status.GuestOSInfo.PrettyName
	vm.Agent = conditionTrue(io.Status.Conditions, "AgentConnected")
	vm.Started = vmi.GetCreationTimestamp().Time
	for _, st := range io.Status.Interfaces {
		for i := range vm.NICs {
			if vm.NICs[i].Name == st.Name {
				vm.NICs[i].MAC, vm.NICs[i].IPs = st.MAC, st.IPAddresses
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

func asObj(v any) obj {
	if m, ok := v.(obj); ok {
		return m
	}
	return obj{}
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}
