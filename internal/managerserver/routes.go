package managerserver

import (
	"net/http"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/ui"
)

func (s *Server) routes() []ui.Route {
	return slices.Concat(s.Routes(), s.hostPageRoutes(), []ui.Route{
		{Pattern: "GET /user-groups", Priv: ui.OrgAdmin, H: s.groupsPage},
		{Pattern: "POST /user-groups/save", Priv: ui.OrgAdmin, H: s.saveGroup},
		{Pattern: "POST /user-groups/delete", Priv: ui.OrgAdmin, H: s.deleteGroup},
		{Pattern: "POST /user-roles/save", Priv: ui.OrgAdmin, H: s.saveRole},
		{Pattern: "POST /user-roles/delete", Priv: ui.OrgAdmin, H: s.deleteRole},
		{Pattern: "POST /sso/start", Priv: ui.Public, H: s.ssoStart},
		{Pattern: "GET /sso/callback", Priv: ui.Public, H: s.ssoCallback},
		{Pattern: "GET /{$}", Priv: auth.VMView, H: s.managerHome},
		{Pattern: "GET /datacenters/{dc}", Priv: auth.VMView, H: s.managerHome},
		{Pattern: "GET /datacenters/{dc}/clusters/{cluster}", Priv: auth.VMView, H: s.managerHome},
		{Pattern: "GET /vms", Priv: auth.VMView, H: s.managerVMs},
		{Pattern: "GET /nav/tree", Priv: auth.VMView, H: s.managerTree},
		{Pattern: "GET /nodes/{node}/vms/{ns}/{name}", Priv: auth.VMView, H: s.managerVM},
		{Pattern: "GET /nodes/{node}/vms/{ns}/{name}/metrics", Priv: auth.VMView, H: s.managerVMMetrics},
		{Pattern: "GET /metrics", Priv: auth.VMView, H: s.managerFleetMetrics},
		{Pattern: "GET /nodes/{node}/vms/{ns}/{name}/console", Priv: auth.VMConsole, H: s.managerConsole},
		{Pattern: "GET /nodes/{node}/vms/{ns}/{name}/ws/{kind}", Priv: auth.VMConsole, H: s.onNode(s.ConsoleWS)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/migrate", Priv: auth.VMMigrate, H: s.managerMigrate},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/protect", Priv: auth.VMMigrate, H: s.managerProtect},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/unprotect", Priv: auth.VMMigrate, H: s.managerUnprotect},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/disks/{disk}/delete", Priv: auth.VMConfig, H: s.onNode(s.RemoveDisk)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/disks/move", Priv: auth.VMConfig, H: s.onNode(s.MoveDiskForm)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/tags", Priv: auth.VMConfig, H: s.onNode(s.SetTags)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/snapshots", Priv: auth.VMSnapshot, H: s.onNode(s.SnapshotOp)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/snapshots/{snap}/{op}", Priv: auth.VMSnapshot, H: s.onNode(s.SnapshotOp)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/backups/{op}", Priv: auth.VMBackup, H: s.onNode(s.VMBackupForm)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/schedule/{op}", Priv: auth.VMPower, H: s.onNode(s.ScheduleForm)},
		{Pattern: "POST /nodes/{node}/vms/{ns}/{name}/{action}", Priv: auth.VMView, H: s.onNode(s.Action)},
		{Pattern: "GET /events", Priv: auth.VMView, H: s.managerEvents},
		{Pattern: "POST /alarm-rules/{op}", Priv: auth.Administer, H: s.managerAlarmRule},
		{Pattern: "POST /jobs/{id}/resolve", Priv: auth.Administer, H: s.managerResolve},
		{Pattern: "GET /storage", Priv: auth.VMView, H: s.managerStorage},
		{Pattern: "GET /networks", Priv: auth.VMView, H: s.managerNetworks},
		{Pattern: "POST /networks", Priv: auth.NetworkConfig, H: s.managerAddNetwork},
		{Pattern: "POST /networks/{name}/{op}", Priv: auth.NetworkConfig, H: s.managerNetworkOp},
		{Pattern: "GET /volumes", Priv: auth.VMView, H: s.managerVolumes},
		{Pattern: "POST /volumes", Priv: auth.VMConfig, H: s.managerVolumeOp},
		{Pattern: "POST /hosts/{node}/volumes/{op}", Priv: auth.VMConfig, H: s.managerVolumeOp},
		{Pattern: "GET /hosts", Priv: auth.HostView, H: s.managerHosts},
		{Pattern: "POST /hosts", Priv: ui.OrgAdmin, H: s.managerAddNode},
		{Pattern: "GET /profile", Priv: auth.Administer, H: s.profilePage},
		{Pattern: "POST /profile", Priv: auth.Administer, H: s.profileSave},
		{Pattern: "POST /profile/apply", Priv: auth.Administer, H: s.profileApply},
		{Pattern: "GET /groups/modal/{op}", Priv: auth.Administer, H: s.groupModal},
		{Pattern: "POST /groups/{op}", Priv: auth.Administer, H: s.groupAction},
		{Pattern: "POST /hosts/{node}/delete", Priv: ui.OrgAdmin, H: s.managerRemoveNode},
		{Pattern: "POST /hosts/{node}/access", Priv: ui.OrgAdmin, H: s.managerNodeAccess},
		{Pattern: "POST /hosts/{node}/rotate-token", Priv: ui.OrgAdmin, H: s.managerRotateToken},
		{Pattern: "GET /network/hosts", Priv: auth.HostConfig, H: s.managerHostNets},
		{Pattern: "GET /vm-new", Priv: auth.VMProvision, H: s.managerNewVM},
		{Pattern: "POST /drs/apply", Priv: auth.Administer, H: s.drsApply},
		{Pattern: "GET /firewall", Priv: auth.NetworkConfig, H: s.firewallPage},
		{Pattern: "POST /firewall/{op}", Priv: auth.NetworkConfig, H: s.firewallAction},
		{Pattern: "POST /drs/rules/{op}", Priv: auth.Administer, H: s.drsRule},
		{Pattern: "GET /account/tokens", Priv: ui.SignedIn, H: s.accountTokens},
		{Pattern: "POST /account/tokens", Priv: ui.SignedIn, H: s.createAccountToken},
		{Pattern: "POST /account/tokens/{name}/delete", Priv: ui.SignedIn, H: s.deleteAccountToken},
		{Pattern: "GET /images", Priv: auth.VMView, H: s.managerImages},
		{Pattern: "GET /vm-copy", Priv: auth.VMProvision, H: s.vmCopyModal},
		{Pattern: "POST /vm-copy", Priv: auth.VMProvision, H: s.vmCopy},
		{Pattern: "POST /templates/delete", Priv: auth.ImageConfig, H: s.deleteLibraryTemplate},
		{Pattern: "GET /template-data/{org}/{name}/{file}", Priv: ui.Public, H: s.serveTemplateData},
		{Pattern: "POST /images/{op}", Priv: auth.ImageConfig, H: s.managerImageOp},
		{Pattern: "POST /network/hosts/bridge", Priv: auth.HostConfig, H: s.managerBridgeRollout},
		{Pattern: "GET /switches/{name}/ports", Priv: auth.VMView, H: s.managerSwitchPorts},
		{Pattern: "POST /switches/teaming", Priv: auth.HostConfig, H: s.managerSwitchTeaming},
		{Pattern: "GET /settings", Priv: ui.OrgAdmin, H: s.managerSettings},
		{Pattern: "POST /settings/sso", Priv: ui.OrgAdmin, H: s.managerSSO},
		{Pattern: "POST /settings/ldap", Priv: ui.OrgAdmin, H: s.managerLDAP},
		{Pattern: "POST /settings/orgs", Priv: ui.OrgAdmin, H: s.managerAddOrg},
		{Pattern: "POST /settings/orgs/{id}/delete", Priv: ui.OrgAdmin, H: s.managerDeleteOrg},
		{Pattern: "POST /settings/orgs/{id}/rename", Priv: ui.OrgAdmin, H: s.managerRenameOrg},
		{Pattern: "POST /settings/backup", Priv: ui.OrgAdmin, H: s.managerBackup},
		{Pattern: "POST /settings/ha", Priv: ui.OrgAdmin, H: s.managerHASettings},
		{Pattern: "POST /ha/failover", Priv: auth.Administer, H: s.managerFailover},
		{Pattern: "POST /settings/migration", Priv: ui.OrgAdmin, H: s.managerMigrationSettings},
	}, s.apiV1Routes())
}

func (s *Server) hostPageRoutes() []ui.Route {
	rs := s.PageRoutes()
	for i, rt := range rs {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		h := rt.H
		rs[i] = ui.Route{Pattern: method + " /hosts/{node}" + path, Priv: rt.Priv, H: func(w http.ResponseWriter, r *http.Request) {
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
