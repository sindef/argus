package managerserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"

	"argus/internal/hostd"
	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/vsphere"
	"argus/internal/wire"
)

type remoteOps struct {
	n manager.Node
	s *Server
}

func (o remoteOps) get(ctx context.Context, path string, out any) error {
	return o.n.Call(ctx, http.MethodGet, path, nil, out)
}

func (o remoteOps) post(ctx context.Context, path string, in any) error {
	return o.n.Call(ctx, http.MethodPost, path, in, nil)
}

func (o remoteOps) DashStats(ctx context.Context) (hostui.DashStats, error) {
	var out hostui.DashStats
	return out, o.get(ctx, "/api/host/dashboard", &out)
}

func (o remoteOps) Devices(ctx context.Context) (hostd.Devices, error) {
	var out hostd.Devices
	return out, o.get(ctx, "/api/host/devices", &out)
}

func (o remoteOps) DeviceOp(ctx context.Context, req hostui.DeviceReq) error {
	return o.post(ctx, "/api/host/devices", req)
}

func (o remoteOps) Disks(ctx context.Context) ([]hostd.Disk, error) {
	var out []hostd.Disk
	return out, o.get(ctx, "/api/host/disks", &out)
}

func (o remoteOps) Disk(ctx context.Context, name string) (hostd.DiskDetail, error) {
	var out hostd.DiskDetail
	return out, o.get(ctx, manager.APIPath("/api/host/disks/%s", name), &out)
}

func (o remoteOps) DiskAction(ctx context.Context, name, action, confirm, test string) error {
	return o.post(ctx, manager.APIPath("/api/host/disks/%s/%s", name, action), map[string]string{"confirm": confirm, "test": test})
}

func (o remoteOps) Pools(ctx context.Context) (hostui.PoolsView, error) {
	var out hostui.PoolsView
	return out, o.get(ctx, "/api/host/pools", &out)
}

func (o remoteOps) PoolOp(ctx context.Context, name, op string, req hostui.PoolOpReq) error {
	if name == "" {
		name = req.Spec.Name
	}
	return o.post(ctx, manager.APIPath("/api/host/pools/%s/%s", name, op), req)
}

func (o remoteOps) Network(ctx context.Context) (hostui.HostNetInfo, error) {
	var out hostui.HostNetInfo
	return out, o.get(ctx, "/api/host/network", &out)
}

func (o remoteOps) NetworkOp(ctx context.Context, op string, req hostui.NetOpReq) error {
	if op == "bridge" {
		return o.post(ctx, "/api/host/network/bridge", req.Bridge)
	}
	return o.post(ctx, manager.APIPath("/api/host/network/%s", op), req)
}

func (o remoteOps) VMForm(ctx context.Context) (hostui.VMForm, error) {
	var out hostui.VMForm
	err := o.get(ctx, "/api/host/vm-form", &out)
	o.s.libraryChoices(o.n.Org, &out)
	return out, err
}

func (o remoteOps) CreateVM(ctx context.Context, req hostui.CreateVMReq) error {
	return o.s.createWithPull(ctx, o.n, req)
}

func (o remoteOps) Hardware(ctx context.Context, ns, name string) (kube.Hardware, error) {
	var out kube.Hardware
	return out, o.get(ctx, manager.APIPath("/api/vms/%s/%s/hardware", ns, name), &out)
}

func (o remoteOps) HardwareOp(ctx context.Context, ns, name, op string, req hostui.HWOpReq) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/hardware/%s", ns, name, op), req)
}

func (o remoteOps) Import(ctx context.Context, source, contentType string, body io.Reader) (string, error) {
	resp, err := o.n.Open(ctx, http.MethodPost, manager.APIPath("/api/vms/import/%s", source), http.Header{"Content-Type": {contentType}}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct{ Name string }
	return out.Name, ui.DecodeJSON(resp.Body, &out)
}

func (o remoteOps) VCenterVMs(ctx context.Context, fields map[string]string) ([]vsphere.VM, error) {
	var out []vsphere.VM
	return out, o.n.Call(ctx, http.MethodPost, "/api/vms/import/vcenter-vms", fields, &out)
}

func (o remoteOps) DownloadDisk(ctx context.Context, ns, name, disk, format string) (hostui.Download, error) {
	path := manager.APIPath("/api/vms/%s/%s/disks/%s/download", ns, name, disk)
	if format == "vmdk" {
		path += "?format=vmdk"
	}
	return hostui.RemoteDownload(o.n.Open(ctx, http.MethodGet, path, nil, nil))
}

func (o remoteOps) DialConsole(ctx context.Context, ns, name, kind string) (*websocket.Conn, error) {
	return o.n.DialConsole(ctx, ns, name, kind)
}

func (o remoteOps) Power(ctx context.Context, ns, name, act string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/%s", ns, name, act), nil)
}

func (o remoteOps) DeleteVM(ctx context.Context, ns, name string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/delete", ns, name), nil)
}

func (o remoteOps) SnapshotOp(ctx context.Context, ns, name, snap, op string) error {
	if snap == "" {
		return o.post(ctx, manager.APIPath("/api/vms/%s/%s/snapshots", ns, name), nil)
	}
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/snapshots/%s/%s", ns, name, snap, op), nil)
}

func (o remoteOps) Meta(ctx context.Context, ns, name, op string, req wire.MetaRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/meta/%s", ns, name, op), req)
}

func (o remoteOps) RemoveDisk(ctx context.Context, ns, name, disk string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/disks/%s/delete", ns, name, disk), nil)
}

func (o remoteOps) MoveDisk(ctx context.Context, ns, name string, req wire.MoveDiskRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/disks/move", ns, name), req)
}

func (o remoteOps) SetTags(ctx context.Context, ns, name string, req wire.TagsRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/tags", ns, name), req)
}

func (o remoteOps) BackupOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/backups/%s", ns, name, op), form)
}

func (o remoteOps) ScheduleOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/schedule/%s", ns, name, op), form)
}

func (s *Server) onNode(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := s.orgNode(r)
		change := r.Method != http.MethodGet
		if err == nil && change && s.manager.Busy(n.Name, r.PathValue("name")) {
			err = errors.New(r.PathValue("name") + " has a migration in progress")
		}
		if err != nil {
			ui.Flash(w, r, err)
			return
		}
		ctx := ui.WithObject(hostui.WithOps(r.Context(), remoteOps{n, s}), objectOf(n, ""))
		h(w, r.WithContext(ctx))
		if change {
			go s.manager.Refresh(n.Name)
		}
	}
}

func flrAPI(owner string) string {
	if vol, ok := strings.CutPrefix(owner, "volume:"); ok {
		return manager.APIPath("/api/volumes/%s/flr", vol)
	}
	ns, name, _ := strings.Cut(strings.TrimPrefix(owner, "vm:"), "/")
	return manager.APIPath("/api/vms/%s/%s/flr", ns, name)
}

func (o remoteOps) FLRStart(ctx context.Context, ns, name, store, stamp string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(hostui.VMOwner(ns, name)), map[string]string{"store": store, "stamp": stamp}, &out)
}

func (o remoteOps) FLRVolume(ctx context.Context, name string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(hostui.VolumeOwner(name)), nil, &out)
}

func (o remoteOps) FLR(ctx context.Context, owner, id string) (hostui.FLRSession, error) {
	var out hostui.FLRSession
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id), &out)
}

func (o remoteOps) FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error) {
	var out []hostd.FLREntry
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/ls?"+url.Values{"part": {part}, "path": {dir}}.Encode(), &out)
}

func (o remoteOps) FLRFile(ctx context.Context, owner, id, part, file string) (hostui.Download, error) {
	return hostui.RemoteDownload(o.n.Open(ctx, http.MethodGet, flrAPI(owner)+"/"+url.PathEscape(id)+"/file?"+url.Values{"part": {part}, "path": {file}}.Encode(), nil, nil))
}

func (o remoteOps) FLRClose(ctx context.Context, owner, id string) error {
	return o.post(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/close", nil)
}

func (o remoteOps) MountMedia(ctx context.Context, ns, name, contentType string, size int64, body io.Reader) error {
	resp, err := o.n.Open(ctx, http.MethodPost, manager.APIPath("/api/vms/%s/%s/media", ns, name), http.Header{"Content-Type": {contentType}, "X-Media-Size": {fmt.Sprint(size)}}, body)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (o remoteOps) EjectMedia(ctx context.Context, ns, name string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/media/eject", ns, name), nil)
}

func (o remoteOps) OVF(ctx context.Context, ns, name string) (string, error) {
	resp, err := o.n.Open(ctx, http.MethodGet, manager.APIPath("/api/vms/%s/%s/ovf", ns, name), nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), err
}
