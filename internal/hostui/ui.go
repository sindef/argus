package hostui

import (
	"context"
	"sync"

	"argus/internal/auth"
	"argus/internal/ui"
)

type UI struct {
	*ui.Core
	Local Ops
	links downloads
}

type downloads struct {
	mu sync.Mutex
	m  map[string]downloadGrant
}

func WithOps(ctx context.Context, o Ops) context.Context {
	return context.WithValue(ctx, opsKey{}, o)
}

func (s *UI) PageRoutes() []ui.Route {
	return []ui.Route{
		{Pattern: "GET /dashboard", Priv: auth.HostView, H: s.dashboard},
		{Pattern: "GET /dashboard/stats", Priv: auth.HostView, H: s.dashboardStats},
		{Pattern: "GET /backups", Priv: auth.VMBackup, H: s.backupsPage},
		{Pattern: "POST /backups/restore", Priv: auth.VMBackup, H: s.restoreFromCatalog},
		{Pattern: "GET /disks", Priv: auth.HostConfig, H: s.disksPage},
		{Pattern: "GET /disks/{name}", Priv: auth.HostConfig, H: s.diskModal},
		{Pattern: "POST /disks/{name}/{action}", Priv: auth.HostConfig, H: s.diskAction},
		{Pattern: "GET /devices", Priv: auth.HostConfig, H: s.devicesPage},
		{Pattern: "POST /devices", Priv: auth.HostConfig, H: s.deviceAction},
		{Pattern: "GET /network/host", Priv: auth.HostConfig, H: s.hostNetworkPage},
		{Pattern: "GET /network/host/iface/{name}", Priv: auth.HostConfig, H: s.ifaceModal},
		{Pattern: "POST /network/host/{op}", Priv: auth.HostConfig, H: s.hostNetworkAction},
		{Pattern: "GET /pools", Priv: auth.HostConfig, H: s.poolsPage},
		{Pattern: "GET /pools/new", Priv: auth.HostConfig, H: s.poolModal},
		{Pattern: "GET /pools/{name}/add", Priv: auth.HostConfig, H: s.poolModal},
		{Pattern: "POST /pools", Priv: auth.HostConfig, H: s.createPool},
		{Pattern: "POST /pools/{name}/{op}", Priv: auth.HostConfig, H: s.poolAction},
		{Pattern: "GET /vms/new", Priv: auth.VMProvision, H: s.NewVM},
		{Pattern: "POST /vms", Priv: auth.VMProvision, H: s.createVM},
		{Pattern: "GET /vms/import", Priv: auth.VMProvision, H: s.ImportModal},
		{Pattern: "POST /vms/import/{source}", Priv: auth.VMProvision, H: s.importVM},
		{Pattern: "POST /vms/import/vcenter-vms", Priv: auth.VMProvision, H: s.vcenterVMsOptions},
		{Pattern: "GET /vms/{ns}/{name}/hardware", Priv: auth.VMConfig, H: s.hardwareModal},
		{Pattern: "POST /vms/{ns}/{name}/hardware/{op}", Priv: auth.VMConfig, H: s.hardwareAction},
		{Pattern: "POST /vms/{ns}/{name}/disks/{disk}/download", Priv: auth.VMBackup, H: s.downloadLink},
		{Pattern: "GET /vms/{ns}/{name}/disks/{disk}/download", Priv: auth.VMBackup, H: s.downloadDisk},
		{Pattern: "GET /vms/{ns}/{name}/export.ovf", Priv: auth.VMBackup, H: s.exportOVF},
		{Pattern: "GET /vms/{ns}/{name}/files", Priv: auth.VMBackup, H: s.filesPage},
		{Pattern: "POST /vms/{ns}/{name}/files", Priv: auth.VMBackup, H: s.filesStart},
		{Pattern: "GET /vms/{ns}/{name}/files/download", Priv: auth.VMBackup, H: s.filesDownload},
		{Pattern: "POST /vms/{ns}/{name}/files/close", Priv: auth.VMBackup, H: s.filesClose},
		{Pattern: "GET /volumes/{vol}/files", Priv: auth.VMBackup, H: s.filesPage},
		{Pattern: "POST /volumes/{vol}/files", Priv: auth.VMBackup, H: s.filesStart},
		{Pattern: "GET /volumes/{vol}/files/download", Priv: auth.VMBackup, H: s.filesDownload},
		{Pattern: "POST /volumes/{vol}/files/close", Priv: auth.VMBackup, H: s.filesClose},
		{Pattern: "POST /vms/{ns}/{name}/media", Priv: auth.VMConfig, H: s.mediaMount},
		{Pattern: "POST /vms/{ns}/{name}/media/eject", Priv: auth.VMConfig, H: s.mediaEject},
	}
}
