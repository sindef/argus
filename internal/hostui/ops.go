package hostui

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/vsphere"
	"argus/internal/wire"
)

type DashStats struct {
	Host    hostd.Stats `json:"host"`
	VMs     int         `json:"vms"`
	Running int         `json:"running"`
	VCPUs   int         `json:"vcpus"`
	VMMem   int64       `json:"vmMem"`
}

type PoolsView struct {
	Pools      []PoolRow          `json:"pools"`
	Importable []hostd.Importable `json:"importable"`
}

type PoolOpReq struct {
	Spec    hostd.PoolSpec `json:"spec"`
	Confirm string         `json:"confirm"`
}

type NetOpReq struct {
	Bridge     hostd.BridgeSpec  `json:"bridge"`
	Delete     string            `json:"delete"`
	Address    hostd.AddressSpec `json:"address"`
	Iface      string            `json:"iface"`
	Migration  bool              `json:"migration"`
	Management bool              `json:"management"`
	Teaming    hostd.Teaming     `json:"teaming"`
}

type VMForm struct {
	Images    []kube.Image    `json:"images"`
	Templates []kube.Template `json:"templates"`
	Networks  []kube.Network  `json:"networks"`
	Pools     []kube.Pool     `json:"pools"`
	Arch      string          `json:"arch"`
}

type CreateVMReq struct {
	Spec kube.VMSpec `json:"spec"`
	Pool string      `json:"pool"`
}

type HWOpReq struct {
	Hardware kube.Hardware `json:"hardware"`
	Network  string        `json:"network"`
	NIC      string        `json:"nic"`
	Disk     string        `json:"disk"`
	SizeGiB  int64         `json:"sizeGiB"`
	Device   string        `json:"device"`
	Thick    bool          `json:"thick"`
}

type Ops interface {
	DashStats(ctx context.Context) (DashStats, error)
	Disks(ctx context.Context) ([]hostd.Disk, error)
	Disk(ctx context.Context, name string) (hostd.DiskDetail, error)
	DiskAction(ctx context.Context, name, action, confirm, test string) error
	Pools(ctx context.Context) (PoolsView, error)
	PoolOp(ctx context.Context, name, op string, req PoolOpReq) error
	Network(ctx context.Context) (HostNetInfo, error)
	NetworkOp(ctx context.Context, op string, req NetOpReq) error
	VMForm(ctx context.Context) (VMForm, error)
	CreateVM(ctx context.Context, req CreateVMReq) error
	Hardware(ctx context.Context, ns, name string) (kube.Hardware, error)
	HardwareOp(ctx context.Context, ns, name, op string, req HWOpReq) error
	Import(ctx context.Context, source, contentType string, body io.Reader) (string, error)
	VCenterVMs(ctx context.Context, fields map[string]string) ([]vsphere.VM, error)
	DownloadDisk(ctx context.Context, ns, name, disk, format string) (Download, error)
	OVF(ctx context.Context, ns, name string) (string, error)
	Devices(ctx context.Context) (hostd.Devices, error)
	FLRStart(ctx context.Context, ns, name, store, stamp string) (string, error)
	FLRVolume(ctx context.Context, name string) (string, error)
	FLR(ctx context.Context, owner, id string) (FLRSession, error)
	FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error)
	FLRFile(ctx context.Context, owner, id, part, file string) (Download, error)
	FLRClose(ctx context.Context, owner, id string) error
	MountMedia(ctx context.Context, ns, name, contentType string, size int64, body io.Reader) error
	EjectMedia(ctx context.Context, ns, name string) error
	DeviceOp(ctx context.Context, req DeviceReq) error
	Power(ctx context.Context, ns, name, act string) error
	DeleteVM(ctx context.Context, ns, name string) error
	SnapshotOp(ctx context.Context, ns, name, snap, op string) error
	Meta(ctx context.Context, ns, name, op string, req wire.MetaRequest) error
	RemoveDisk(ctx context.Context, ns, name, disk string) error
	MoveDisk(ctx context.Context, ns, name string, req wire.MoveDiskRequest) error
	SetTags(ctx context.Context, ns, name string, req wire.TagsRequest) error
	BackupOp(ctx context.Context, ns, name, op string, form map[string]string) error
	ScheduleOp(ctx context.Context, ns, name, op string, form map[string]string) error
	DialConsole(ctx context.Context, ns, name, kind string) (*websocket.Conn, error)
}

type DeviceReq struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	On   bool   `json:"on"`
}

type opsKey struct{}

func (s *UI) Ops(r *http.Request) Ops {
	if o, ok := r.Context().Value(opsKey{}).(Ops); ok {
		return o
	}
	return s.Local
}

type Download struct {
	io.ReadCloser
	Type, Length, Name string
}

type ReadCloser struct {
	io.Reader
	OnClose func() error
}

func (c ReadCloser) Close() error { return c.OnClose() }

type StatusError struct {
	Code int
	Err  error
}

func (e StatusError) Error() string { return e.Err.Error() }

func (e StatusError) Unwrap() error { return e.Err }

func RemoteDownload(resp *http.Response, err error) (Download, error) {
	if err != nil {
		return Download{}, err
	}
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	return Download{resp.Body, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Length"), params["filename"]}, nil
}

func ServeDownload(w http.ResponseWriter, r *http.Request, d Download, err error) {
	if err != nil {
		code := http.StatusBadGateway
		var se StatusError
		if errors.As(err, &se) {
			code = se.Code
		}
		log.Printf("%s: %s: %v", ui.Session(r).User, r.URL.Path, err)
		http.Error(w, err.Error(), code)
		return
	}
	defer d.Close()
	w.Header().Set("Content-Type", d.Type)
	if d.Length != "" {
		w.Header().Set("Content-Length", d.Length)
	}
	if d.Name != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.Name}))
	}
	n, err := io.Copy(w, d)
	if err != nil {
		log.Printf("%s: %s: %v after %d bytes", ui.Session(r).User, r.URL.Path, err, n)
		panic(http.ErrAbortHandler)
	}
	log.Printf("%s: %s: %d bytes", ui.Session(r).User, r.URL.Path, n)
}

func AccessCtx(r *http.Request) context.Context {
	return hostd.WithAccess(r.Context(), r.Host)
}

type HostNetInfo = manager.HostNetwork

type FLRSession struct {
	ID      string          `json:"id"`
	Owner   string          `json:"owner"`
	Stamp   string          `json:"stamp,omitempty"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"`
	Parts   []hostd.FLRPart `json:"parts"`
	Started time.Time       `json:"started"`
}

type PoolRow struct {
	hostd.Pool
	Class       string
	Provisioned int64
	Volumes     int
}
