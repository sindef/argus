package server

import (
	"net/http"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/web"
)

const (
	public   auth.Privilege = ""
	signedIn auth.Privilege = "signed-in"
	orgAdmin auth.Privilege = "org-admin"
)

type route struct {
	pattern string
	priv    auth.Privilege
	h       http.HandlerFunc
}

func (s *Server) guard(p auth.Privilege, h http.HandlerFunc) http.HandlerFunc {
	switch p {
	case public:
		return h
	case signedIn:
		return s.authenticate(h)
	case orgAdmin:
		return s.requireOrg(h)
	}
	return s.need(p, h)
}

func (s *Server) routes() []route {
	rs := slices.Concat([]route{
		{"GET /static/", public, http.FileServerFS(web.FS).ServeHTTP},
		{"GET /audit", orgAdmin, s.auditPage},
		{"POST /notify", orgAdmin, s.notifyForm},
		{"POST /certificate", orgAdmin, s.setCertificate},
		{"GET /login", public, s.loginPage},
		{"POST /login", public, s.login},
		{"POST /login/totp", public, s.loginTOTP},
		{"POST /account/mfa/{op}", signedIn, s.mfaAction},
		{"POST /users/{name}/mfa-reset", orgAdmin, s.resetMFA},
		{"POST /logout", public, s.logout},
		{"GET /password", signedIn, s.passwordPage},
		{"POST /password", signedIn, s.changePassword},
		{"GET /users", orgAdmin, s.usersPage},
		{"POST /users", orgAdmin, s.addUser},
		{"POST /users/{name}/role", orgAdmin, s.setRole},
		{"POST /users/scopes", orgAdmin, s.setScopes},
		{"POST /users/{name}/update", orgAdmin, s.updateUser},
		{"POST /users/{name}/reset", orgAdmin, s.resetPassword},
		{"POST /users/{name}/delete", orgAdmin, s.deleteUser},
	}, s.hostPageRoutes())
	if s.manager != nil {
		return append(rs, s.managerRoutes()...)
	}
	return append(rs, s.hostModeRoutes()...)
}

func (s *Server) managerRoutes() []route {
	return append([]route{
		{"GET /user-groups", orgAdmin, s.groupsPage},
		{"POST /user-groups/save", orgAdmin, s.saveGroup},
		{"POST /user-groups/delete", orgAdmin, s.deleteGroup},
		{"POST /user-roles/save", orgAdmin, s.saveRole},
		{"POST /user-roles/delete", orgAdmin, s.deleteRole},
		{"POST /sso/start", public, s.ssoStart},
		{"GET /sso/callback", public, s.ssoCallback},
		{"GET /{$}", auth.VMView, s.managerHome},
		{"GET /datacenters/{dc}", auth.VMView, s.managerHome},
		{"GET /datacenters/{dc}/clusters/{cluster}", auth.VMView, s.managerHome},
		{"GET /vms", auth.VMView, s.managerVMs},
		{"GET /nav/tree", auth.VMView, s.managerTree},
		{"GET /nodes/{node}/vms/{ns}/{name}", auth.VMView, s.managerVM},
		{"GET /nodes/{node}/vms/{ns}/{name}/metrics", auth.VMView, s.managerVMMetrics},
		{"GET /metrics", auth.VMView, s.managerFleetMetrics},
		{"GET /nodes/{node}/vms/{ns}/{name}/console", auth.VMConsole, s.managerConsole},
		{"GET /nodes/{node}/vms/{ns}/{name}/ws/{kind}", auth.VMConsole, s.onNode(s.consoleWS)},
		{"POST /nodes/{node}/vms/{ns}/{name}/migrate", auth.VMMigrate, s.managerMigrate},
		{"POST /nodes/{node}/vms/{ns}/{name}/protect", auth.VMMigrate, s.managerProtect},
		{"POST /nodes/{node}/vms/{ns}/{name}/unprotect", auth.VMMigrate, s.managerUnprotect},
		{"POST /nodes/{node}/vms/{ns}/{name}/disks/{disk}/delete", auth.VMConfig, s.onNode(s.removeDisk)},
		{"POST /nodes/{node}/vms/{ns}/{name}/disks/move", auth.VMConfig, s.onNode(s.moveDiskForm)},
		{"POST /nodes/{node}/vms/{ns}/{name}/tags", auth.VMConfig, s.onNode(s.setTags)},
		{"POST /nodes/{node}/vms/{ns}/{name}/snapshots", auth.VMSnapshot, s.onNode(s.snapshotOp)},
		{"POST /nodes/{node}/vms/{ns}/{name}/snapshots/{snap}/{op}", auth.VMSnapshot, s.onNode(s.snapshotOp)},
		{"POST /nodes/{node}/vms/{ns}/{name}/backups/{op}", auth.VMBackup, s.onNode(s.vmBackupForm)},
		{"POST /nodes/{node}/vms/{ns}/{name}/schedule/{op}", auth.VMPower, s.onNode(s.scheduleForm)},
		{"POST /nodes/{node}/vms/{ns}/{name}/{action}", auth.VMView, s.onNode(s.action)},
		{"GET /events", auth.VMView, s.managerEvents},
		{"POST /alarm-rules/{op}", auth.Administer, s.managerAlarmRule},
		{"POST /jobs/{id}/resolve", auth.Administer, s.managerResolve},
		{"GET /storage", auth.VMView, s.managerStorage},
		{"GET /networks", auth.VMView, s.managerNetworks},
		{"POST /networks", auth.NetworkConfig, s.managerAddNetwork},
		{"POST /networks/{name}/{op}", auth.NetworkConfig, s.managerNetworkOp},
		{"GET /volumes", auth.VMView, s.managerVolumes},
		{"POST /volumes", auth.VMConfig, s.managerVolumeOp},
		{"POST /hosts/{node}/volumes/{op}", auth.VMConfig, s.managerVolumeOp},
		{"GET /hosts", auth.HostView, s.managerHosts},
		{"POST /hosts", orgAdmin, s.managerAddNode},
		{"GET /profile", auth.Administer, s.profilePage},
		{"POST /profile", auth.Administer, s.profileSave},
		{"POST /profile/apply", auth.Administer, s.profileApply},
		{"GET /groups/modal/{op}", auth.Administer, s.groupModal},
		{"POST /groups/{op}", auth.Administer, s.groupAction},
		{"POST /hosts/{node}/delete", orgAdmin, s.managerRemoveNode},
		{"POST /hosts/{node}/access", orgAdmin, s.managerNodeAccess},
		{"POST /hosts/{node}/rotate-token", orgAdmin, s.managerRotateToken},
		{"GET /network/hosts", auth.HostConfig, s.managerHostNets},
		{"GET /vm-new", auth.VMProvision, s.managerNewVM},
		{"POST /drs/apply", auth.Administer, s.drsApply},
		{"GET /firewall", auth.NetworkConfig, s.firewallPage},
		{"POST /firewall/{op}", auth.NetworkConfig, s.firewallAction},
		{"POST /drs/rules/{op}", auth.Administer, s.drsRule},
		{"GET /account/tokens", signedIn, s.accountTokens},
		{"POST /account/tokens", signedIn, s.createAccountToken},
		{"POST /account/tokens/{name}/delete", signedIn, s.deleteAccountToken},
		{"GET /images", auth.VMView, s.managerImages},
		{"GET /vm-copy", auth.VMProvision, s.vmCopyModal},
		{"POST /vm-copy", auth.VMProvision, s.vmCopy},
		{"POST /templates/delete", auth.ImageConfig, s.deleteLibraryTemplate},
		{"GET /template-data/{org}/{name}/{file}", public, s.serveTemplateData},
		{"POST /images/{op}", auth.ImageConfig, s.managerImageOp},
		{"POST /network/hosts/bridge", auth.HostConfig, s.managerBridgeRollout},
		{"GET /switches/{name}/ports", auth.VMView, s.managerSwitchPorts},
		{"POST /switches/teaming", auth.HostConfig, s.managerSwitchTeaming},
		{"GET /settings", orgAdmin, s.managerSettings},
		{"POST /settings/sso", orgAdmin, s.managerSSO},
		{"POST /settings/ldap", orgAdmin, s.managerLDAP},
		{"POST /settings/orgs", orgAdmin, s.managerAddOrg},
		{"POST /settings/orgs/{id}/delete", orgAdmin, s.managerDeleteOrg},
		{"POST /settings/orgs/{id}/rename", orgAdmin, s.managerRenameOrg},
		{"POST /settings/backup", orgAdmin, s.managerBackup},
		{"POST /settings/ha", orgAdmin, s.managerHASettings},
		{"POST /ha/failover", auth.Administer, s.managerFailover},
		{"POST /settings/migration", orgAdmin, s.managerMigrationSettings},
	}, s.apiV1Routes()...)
}

func (s *Server) hostModeRoutes() []route {
	return slices.Concat([]route{
		{"GET /events", auth.VMView, s.eventsPage},
		{"GET /volumes", auth.VMView, s.volumesPage},
		{"POST /volumes/{op}", auth.VMConfig, s.volumeAction},
		{"GET /images", auth.VMView, s.images},
		{"POST /images", auth.ImageConfig, s.importImage},
		{"POST /images/upload", auth.ImageConfig, s.uploadImage},
		{"POST /images/{name}/delete", auth.ImageConfig, s.deleteImage},
		{"GET /networks", auth.VMView, s.networks},
		{"POST /networks", auth.NetworkConfig, s.createNetwork},
		{"POST /networks/{name}/delete", auth.NetworkConfig, s.deleteNetwork},
		{"GET /host", auth.HostConfig, s.host},
		{"POST /host/settings", auth.HostConfig, s.hostSettingsForm},
		{"POST /host/backup-targets/{op}", auth.HostConfig, s.backupTargetForm},
		{"POST /host/{action}", auth.HostConfig, s.hostRequest},
		{"POST /tokens", orgAdmin, s.createToken},
		{"POST /tokens/{name}/delete", orgAdmin, s.deleteToken},
		{"GET /ca.pem", public, s.caCert},
		{"GET /export/{ns}/{export}/{volume}", public, s.exportPassthrough},
		{"GET /backups/{vm}/{stamp}/{file}", public, s.serveBackup},
		{"GET /esxi-import/{disk}", public, s.serveESXi},
		{"GET /api/vms/{ns}/{name}/backups", auth.Administer, s.apiVMBackups},
		{"POST /api/vms/{ns}/{name}/backups/{op}", auth.Administer, s.apiVMBackupOp},
		{"POST /api/vms/{ns}/{name}/schedule/{op}", auth.Administer, s.apiScheduleOp},
		{"POST /api/vms/{ns}/{name}/media", auth.Administer, s.apiMedia},
		{"POST /api/vms/{ns}/{name}/media/{op}", auth.Administer, s.apiMedia},
		{"GET /api/host/network", auth.Administer, s.apiHostNetwork},
		{"GET /api/host/status", auth.Administer, s.apiHostStatus},
		{"GET /api/images", auth.Administer, s.apiImages},
		{"GET /api/alarms", auth.Administer, s.apiAlarms},
		{"GET /api/prometheus", auth.Administer, s.apiPrometheus},
		{"POST /api/images/import", auth.Administer, s.apiImportImage},
		{"POST /api/images/{name}/{op}", auth.Administer, s.apiImageOp},
		{"GET /api/host/settings", auth.Administer, s.apiHostSettings},
		{"POST /api/host/settings", auth.Administer, s.apiApplyHostSettings},
		{"GET /api/host/backup-targets", auth.Administer, s.apiBackupTargets},
		{"POST /api/tokens/rotate", auth.Administer, s.apiRotateToken},
		{"POST /api/tokens/retire", auth.Administer, s.apiRetireToken},
		{"POST /api/host/backup-targets/{op}", auth.Administer, s.apiBackupTargetOp},
		{"POST /api/host/power/{action}", auth.Administer, s.apiHostRequest},
		{"GET /api/node", auth.Administer, s.apiNode},
		{"GET /api/vms", auth.Administer, s.apiVMs},
		{"GET /api/metrics", auth.Administer, s.apiMetrics},
		{"GET /api/events", auth.Administer, s.apiEvents},
		{"GET /api/storage", auth.Administer, s.apiStorage},
		{"POST /api/volumes/{op}", auth.Administer, s.apiVolumeOp},
		{"POST /api/vms/{ns}/{name}/disks/{disk}/delete", auth.Administer, s.apiRemoveDisk},
		{"POST /api/vms/{ns}/{name}/disks/move", auth.Administer, s.apiMoveDisk},
		{"POST /api/vms/{ns}/{name}/tags", auth.Administer, s.apiSetTags},
		{"GET /api/networks", auth.Administer, s.apiNetworks},
		{"POST /api/networks/{op}", auth.Administer, s.apiNetworkOp},
		{"GET /api/vms/{ns}/{name}/disks", auth.Administer, s.apiDisks},
		{"POST /api/vms/{ns}/{name}/export", auth.Administer, s.apiExport},
		{"POST /api/vms/{ns}/{name}/unexport", auth.Administer, s.apiUnexport},
		{"POST /api/vms/{ns}/{name}/delete", auth.Administer, s.apiDelete},
		{"POST /api/vms/{ns}/{name}/imported", auth.Administer, s.apiImported},
		{"POST /api/vms/{ns}/{name}/{action}", auth.Administer, s.apiAction},
		{"POST /api/import", auth.Administer, s.apiImport},
		{"POST /api/heartbeat", auth.Administer, s.apiHeartbeat},
		{"GET /api/probe", auth.Administer, s.apiProbe},
		{"POST /api/vms/{ns}/{name}/ha", auth.Administer, s.apiSetHA},
		{"POST /api/vms/{ns}/{name}/replica/{op}", auth.Administer, s.apiReplica},
		{"GET /api/vms/{ns}/{name}/zfs", auth.Administer, s.apiZFSDisks},
		{"POST /api/vms/{ns}/{name}/zfs/snapshot", auth.Administer, s.apiZFSSnapshot},
		{"POST /api/vms/{ns}/{name}/zfs/prune", auth.Administer, s.apiZFSPrune},
		{"GET /api/vms/{ns}/{name}/zfs/send", auth.Administer, s.apiZFSSend},
		{"PUT /api/vms/{ns}/{name}/zfs/recv", auth.Administer, s.apiZFSRecv},
		{"POST /api/vms/{ns}/{name}/snapshot-export", auth.Administer, s.apiSnapshotExport},
		{"GET /api/kubevirt-ca", auth.Administer, s.apiKubeVirtCA},
		{"POST /api/kubevirt-external-ca", auth.Administer, s.apiExternalCA},
		{"POST /api/migration-network", auth.Administer, s.apiMigrationNetwork},
		{"POST /api/migration-network/remove", auth.Administer, s.apiRemoveMigrationNetwork},
		{"POST /api/sync-controller", auth.Administer, s.apiSyncController},
		{"POST /api/receive", auth.Administer, s.apiReceive},
		{"GET /api/vms/{ns}/{name}/spec", auth.Administer, s.apiVMSpec},
		{"GET /api/vms/{ns}/{name}", auth.Administer, s.apiVM},
		{"GET /api/vms/{ns}/{name}/events", auth.Administer, s.apiEvents},
		{"GET /api/vms/{ns}/{name}/snapshots", auth.Administer, s.apiSnapshots},
		{"POST /api/vms/{ns}/{name}/snapshots", auth.Administer, s.apiSnapshotOp},
		{"POST /api/vms/{ns}/{name}/snapshots/{snap}/{op}", auth.Administer, s.apiSnapshotOp},
		{"POST /api/vms/{ns}/{name}/meta/{op}", auth.Administer, s.apiRenameClone},
		{"GET /api/vms/{ns}/{name}/ws/{kind}", auth.Administer, s.consoleWS},
		{"GET /api/vms/{ns}/{name}/migration", auth.Administer, s.apiMigrationPhase},
		{"POST /api/vms/{ns}/{name}/send", auth.Administer, s.apiSend},
		{"POST /api/vms/{ns}/{name}/migration-cleanup", auth.Administer, s.apiMigrationCleanup},
		{"POST /api/vms/{ns}/{name}/abort", auth.Administer, s.apiAbort},
		{"GET /{$}", auth.VMView, s.vms},
		{"GET /vms", auth.VMView, s.vmTable},
		{"GET /vms/{ns}/{name}", auth.VMView, s.vms},
		{"GET /vms/{ns}/{name}/drawer", auth.VMView, s.vmDrawer},
		{"GET /nav/vms", auth.VMView, s.navVMs},
		{"POST /vms/bulk/{action}", auth.VMView, s.bulk},
		{"POST /vms/{ns}/{name}/disks", auth.VMConfig, s.addDisk},
		{"POST /vms/{ns}/{name}/disks/move", auth.VMConfig, s.moveDiskForm},
		{"POST /vms/{ns}/{name}/tags", auth.VMConfig, s.setTags},
		{"GET /vms/{ns}/{name}/disks/new", auth.VMConfig, s.newDisk},
		{"POST /vms/{ns}/{name}/disks/{disk}/delete", auth.VMConfig, s.removeDisk},
		{"POST /vms/{ns}/{name}/snapshots", auth.VMSnapshot, s.snapshotOp},
		{"POST /vms/{ns}/{name}/backups/{op}", auth.VMBackup, s.vmBackupForm},
		{"POST /vms/{ns}/{name}/schedule/{op}", auth.VMPower, s.scheduleForm},
		{"POST /vms/{ns}/{name}/snapshots/{snap}/{op}", auth.VMSnapshot, s.snapshotOp},
		{"POST /vms/{ns}/{name}/{action}", auth.VMView, s.action},
		{"GET /vms/{ns}/{name}/console", auth.VMConsole, s.console},
		{"GET /ws/{ns}/{name}/{kind}", auth.VMConsole, s.consoleWS},
	}, s.hostAPIRoutes(), s.flrAPIRoutes(), s.cbtRoutes())
}

func (s *Server) hostPageRoutes() []route {
	rs := []route{
		{"GET /dashboard", auth.HostView, s.dashboard},
		{"GET /dashboard/stats", auth.HostView, s.dashboardStats},
		{"GET /disks", auth.HostConfig, s.disksPage},
		{"GET /disks/{name}", auth.HostConfig, s.diskModal},
		{"POST /disks/{name}/{action}", auth.HostConfig, s.diskAction},
		{"GET /devices", auth.HostConfig, s.devicesPage},
		{"POST /devices", auth.HostConfig, s.deviceAction},
		{"GET /network/host", auth.HostConfig, s.hostNetworkPage},
		{"GET /network/host/iface/{name}", auth.HostConfig, s.ifaceModal},
		{"POST /network/host/{op}", auth.HostConfig, s.hostNetworkAction},
		{"GET /pools", auth.HostConfig, s.poolsPage},
		{"GET /pools/new", auth.HostConfig, s.poolModal},
		{"GET /pools/{name}/add", auth.HostConfig, s.poolModal},
		{"POST /pools", auth.HostConfig, s.createPool},
		{"POST /pools/{name}/{op}", auth.HostConfig, s.poolAction},
		{"GET /vms/new", auth.VMProvision, s.newVM},
		{"POST /vms", auth.VMProvision, s.createVM},
		{"GET /vms/import", auth.VMProvision, s.importModal},
		{"POST /vms/import/{source}", auth.VMProvision, s.importVM},
		{"POST /vms/import/vcenter-vms", auth.VMProvision, s.vcenterVMsOptions},
		{"GET /vms/{ns}/{name}/hardware", auth.VMConfig, s.hardwareModal},
		{"POST /vms/{ns}/{name}/hardware/{op}", auth.VMConfig, s.hardwareAction},
		{"POST /vms/{ns}/{name}/disks/{disk}/download", auth.VMBackup, s.downloadLink},
		{"GET /vms/{ns}/{name}/disks/{disk}/download", auth.VMBackup, s.downloadDisk},
		{"GET /vms/{ns}/{name}/export.ovf", auth.VMBackup, s.exportOVF},
		{"GET /vms/{ns}/{name}/files", auth.VMBackup, s.filesPage},
		{"POST /vms/{ns}/{name}/files", auth.VMBackup, s.filesStart},
		{"GET /vms/{ns}/{name}/files/download", auth.VMBackup, s.filesDownload},
		{"POST /vms/{ns}/{name}/files/close", auth.VMBackup, s.filesClose},
		{"GET /volumes/{vol}/files", auth.VMBackup, s.filesPage},
		{"POST /volumes/{vol}/files", auth.VMBackup, s.filesStart},
		{"GET /volumes/{vol}/files/download", auth.VMBackup, s.filesDownload},
		{"POST /volumes/{vol}/files/close", auth.VMBackup, s.filesClose},
		{"POST /vms/{ns}/{name}/media", auth.VMConfig, s.mediaMount},
		{"POST /vms/{ns}/{name}/media/eject", auth.VMConfig, s.mediaEject},
	}
	if s.manager == nil {
		return rs
	}
	for i, rt := range rs {
		method, path, _ := strings.Cut(rt.pattern, " ")
		h := rt.h
		rs[i] = route{method + " /hosts/{node}" + path, rt.priv, func(w http.ResponseWriter, r *http.Request) {
			n, err := s.orgNode(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			h(w, onHost(r, n))
		}}
	}
	return rs
}
