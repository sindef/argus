package server

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/registry"
	"argus/internal/tlscert"
	"argus/web"
)

func (s *Server) cookieName() string {
	if s.manager != nil {
		return "argus_manager_session"
	}
	return "argus_session"
}

type Server struct {
	kube        *kube.Client
	auth        *auth.Store
	banner      func(password string)
	runDir      string
	hostd       *hostd.Client
	releaseFile string
	caPEM       []byte
	nodeName    string
	version     string
	manager     *manager.Manager
	ha          haState
	sso         ssoFlows
	pages       map[string]*template.Template
	limiter     *loginLimiter
	mfa         mfaState
	mux         *http.ServeMux
	backups     backupState
	certs       *tlscert.Holder
	dataDir     string
	esxi        esxiStreams
	auditLog    auditLog
	alarms      alarmState
	registry    *registry.Registry
	regAuth     registryAuth
}

type Options struct {
	Banner      func(password string)
	RunDir      string
	ReleaseFile string
	CAPEM       []byte
	NodeName    string
	Manager     *manager.Manager
	Certs       *tlscert.Holder
	DataDir     string
}

type ctxKey struct{}

type reqAuth struct {
	auth.Session
	Token string
}

func New(k *kube.Client, a *auth.Store, o Options) (*Server, error) {
	s := &Server{kube: k, auth: a, banner: o.Banner, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM, nodeName: o.NodeName, manager: o.Manager, certs: o.Certs, dataDir: o.DataDir, pages: map[string]*template.Template{}, mux: http.NewServeMux(), limiter: newLoginLimiter()}
	if k != nil {
		k.Guard = s.maintenance
	}
	if o.RunDir != "" {
		s.hostd = hostd.NewClient(filepath.Join(o.RunDir, "hostd.sock"), os.Getenv("HOST_IP"))
	}
	s.version = readEnv(o.ReleaseFile)["KAIROS_VERSION"]
	s.sso.m = map[string]ssoFlow{}
	for _, p := range []string{"login.html", "password.html", "users.html", "vms.html", "console.html", "images.html", "networks.html", "vmnew.html", "host.html", "manager.html", "manager-vms.html", "manager-vm.html", "hosts.html", "settings.html", "events.html", "volumes.html", "storage.html", "disks.html", "pools.html", "hostnet.html", "dashboard.html", "hostnets.html", "profile.html", "manager-images.html", "audit.html", "groups.html", "tokens.html"} {
		t, err := template.New(p).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/manager-nav.html", "templates/"+p)
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}

	go s.auditLoop()
	go s.auth.PersistSessions(context.Background())
	if k != nil || s.manager != nil {
		go s.alarmLoop()
	}
	s.mux.Handle("GET /static/", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /audit", s.requireOrg(auth.Admin, s.auditPage))
	s.mux.HandleFunc("POST /notify", s.requireOrg(auth.Admin, s.notifyForm))
	s.mux.HandleFunc("POST /certificate", s.requireOrg(auth.Admin, s.setCertificate))
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /login/totp", s.loginTOTP)
	s.mux.HandleFunc("POST /account/mfa/{op}", s.require(auth.ReadOnly, s.mfaAction))
	s.mux.HandleFunc("POST /users/{name}/mfa-reset", s.requireOrg(auth.Admin, s.resetMFA))
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /password", s.require(auth.ReadOnly, s.passwordPage))
	s.mux.HandleFunc("POST /password", s.require(auth.ReadOnly, s.changePassword))
	s.mux.HandleFunc("GET /users", s.requireOrg(auth.Admin, s.usersPage))
	s.mux.HandleFunc("POST /users", s.requireOrg(auth.Admin, s.addUser))
	s.mux.HandleFunc("POST /users/{name}/role", s.requireOrg(auth.Admin, s.setRole))
	s.mux.HandleFunc("POST /users/scopes", s.requireOrg(auth.Admin, s.setScopes))
	s.mux.HandleFunc("POST /users/{name}/update", s.requireOrg(auth.Admin, s.updateUser))
	s.mux.HandleFunc("POST /users/{name}/reset", s.requireOrg(auth.Admin, s.resetPassword))
	s.mux.HandleFunc("POST /users/{name}/delete", s.requireOrg(auth.Admin, s.deleteUser))
	if s.manager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.startRegistry(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
		s.mux.HandleFunc("GET /user-groups", s.requireOrg(auth.Admin, s.groupsPage))
		s.mux.HandleFunc("POST /user-groups/save", s.requireOrg(auth.Admin, s.saveGroup))
		s.mux.HandleFunc("POST /user-groups/delete", s.requireOrg(auth.Admin, s.deleteGroup))
		s.mux.HandleFunc("POST /sso/start", s.ssoStart)
		s.mux.HandleFunc("GET /sso/callback", s.ssoCallback)
		s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.managerHome))
		s.mux.HandleFunc("GET /datacenters/{dc}", s.require(auth.ReadOnly, s.managerHome))
		s.mux.HandleFunc("GET /datacenters/{dc}/clusters/{cluster}", s.require(auth.ReadOnly, s.managerHome))
		s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.managerVMs))
		s.mux.HandleFunc("GET /nav/tree", s.require(auth.ReadOnly, s.managerTree))
		s.mux.HandleFunc("GET /nodes/{node}/vms/{ns}/{name}", s.require(auth.ReadOnly, s.managerVM))
		s.mux.HandleFunc("GET /nodes/{node}/vms/{ns}/{name}/metrics", s.require(auth.ReadOnly, s.managerVMMetrics))
		s.mux.HandleFunc("GET /metrics", s.require(auth.ReadOnly, s.managerFleetMetrics))
		s.mux.HandleFunc("GET /nodes/{node}/vms/{ns}/{name}/console", s.require(auth.Operator, s.managerConsole))
		s.mux.HandleFunc("GET /nodes/{node}/vms/{ns}/{name}/ws/{kind}", s.require(auth.Operator, s.managerConsoleWS))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/migrate", s.require(auth.Admin, s.managerMigrate))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/protect", s.require(auth.Admin, s.managerProtect))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/unprotect", s.require(auth.Admin, s.managerUnprotect))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Operator, s.managerRemoveDisk))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/disks/move", s.require(auth.Operator, s.managerMoveDisk))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/tags", s.require(auth.Operator, s.managerSetTags))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/snapshots/{snap}/{op}", s.require(auth.Operator, s.managerSnapshotOp))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/backups/{op}", s.require(auth.Operator, s.managerBackupOp))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/{action}", s.require(auth.Operator, s.managerVMAction))
		s.mux.HandleFunc("GET /events", s.require(auth.ReadOnly, s.managerEvents))
		s.mux.HandleFunc("POST /jobs/{id}/resolve", s.require(auth.Admin, s.managerResolve))
		s.mux.HandleFunc("GET /storage", s.require(auth.ReadOnly, s.managerStorage))
		s.mux.HandleFunc("GET /networks", s.require(auth.ReadOnly, s.managerNetworks))
		s.mux.HandleFunc("POST /networks", s.requireOrg(auth.Admin, s.managerAddNetwork))
		s.mux.HandleFunc("POST /networks/{name}/{op}", s.requireOrg(auth.Admin, s.managerNetworkOp))
		s.mux.HandleFunc("GET /volumes", s.require(auth.ReadOnly, s.managerVolumes))
		s.mux.HandleFunc("POST /volumes", s.require(auth.Operator, s.managerVolumeOp))
		s.mux.HandleFunc("POST /hosts/{node}/volumes/{op}", s.require(auth.Operator, s.managerVolumeOp))
		s.mux.HandleFunc("GET /hosts", s.require(auth.ReadOnly, s.managerHosts))
		s.mux.HandleFunc("POST /hosts", s.requireOrg(auth.Admin, s.managerAddNode))
		s.mux.HandleFunc("GET /profile", s.requireOrg(auth.Admin, s.profilePage))
		s.mux.HandleFunc("POST /profile", s.requireOrg(auth.Admin, s.profileSave))
		s.mux.HandleFunc("POST /profile/apply", s.requireOrg(auth.Admin, s.profileApply))
		s.mux.HandleFunc("GET /groups/modal/{op}", s.requireOrg(auth.Admin, s.groupModal))
		s.mux.HandleFunc("POST /groups/{op}", s.requireOrg(auth.Admin, s.groupAction))
		s.mux.HandleFunc("POST /hosts/{node}/delete", s.requireOrg(auth.Admin, s.managerRemoveNode))
		s.mux.HandleFunc("POST /hosts/{node}/access", s.requireOrg(auth.Admin, s.managerNodeAccess))
		s.mux.HandleFunc("POST /hosts/{node}/rotate-token", s.requireOrg(auth.Admin, s.managerRotateToken))
		s.mux.HandleFunc("GET /network/hosts", s.requireOrg(auth.Admin, s.managerHostNets))
		s.mux.HandleFunc("GET /vm-new", s.require(auth.Operator, s.managerNewVM))
		s.apiV1Routes()
		s.mux.HandleFunc("POST /drs/apply", s.require(auth.Admin, s.drsApply))
		s.mux.HandleFunc("POST /drs/rules/{op}", s.requireOrg(auth.Admin, s.drsRule))
		s.mux.HandleFunc("GET /account/tokens", s.require(auth.ReadOnly, s.accountTokens))
		s.mux.HandleFunc("POST /account/tokens", s.require(auth.ReadOnly, s.createAccountToken))
		s.mux.HandleFunc("POST /account/tokens/{name}/delete", s.require(auth.ReadOnly, s.deleteAccountToken))
		s.mux.HandleFunc("GET /images", s.require(auth.ReadOnly, s.managerImages))
		s.mux.HandleFunc("GET /vm-copy", s.require(auth.Operator, s.vmCopyModal))
		s.mux.HandleFunc("POST /vm-copy", s.requireOrg(auth.Operator, s.vmCopy))
		s.mux.HandleFunc("POST /templates/delete", s.requireOrg(auth.Operator, s.deleteLibraryTemplate))
		s.mux.HandleFunc("GET /template-data/{org}/{name}/{file}", s.serveTemplateData)
		s.mux.HandleFunc("POST /images/{op}", s.requireOrg(auth.Operator, s.managerImageOp))
		s.mux.HandleFunc("POST /network/hosts/bridge", s.requireOrg(auth.Admin, s.managerBridgeRollout))
		s.mux.HandleFunc("GET /settings", s.requireOrg(auth.Admin, s.managerSettings))
		s.mux.HandleFunc("POST /settings/sso", s.requireOrg(auth.Admin, s.managerSSO))
		s.mux.HandleFunc("POST /settings/orgs", s.requireOrg(auth.Admin, s.managerAddOrg))
		s.mux.HandleFunc("POST /settings/orgs/{id}/delete", s.requireOrg(auth.Admin, s.managerDeleteOrg))
		s.mux.HandleFunc("POST /settings/orgs/{id}/rename", s.requireOrg(auth.Admin, s.managerRenameOrg))
		s.mux.HandleFunc("POST /settings/backup", s.requireOrg(auth.Admin, s.managerBackup))
		s.mux.HandleFunc("POST /settings/ha", s.requireOrg(auth.Admin, s.managerHASettings))
		s.mux.HandleFunc("POST /ha/failover", s.require(auth.Admin, s.managerFailover))
		s.mux.HandleFunc("POST /settings/migration", s.requireOrg(auth.Admin, s.managerMigrationSettings))
	}
	for _, hr := range hostRoutes {
		h := hr.h
		s.hostRoute(hr.pattern, hr.role, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	}
	if s.manager != nil {
		return s, nil
	}
	s.mux.HandleFunc("GET /events", s.require(auth.ReadOnly, s.eventsPage))
	s.mux.HandleFunc("GET /volumes", s.require(auth.ReadOnly, s.volumesPage))
	s.mux.HandleFunc("POST /volumes/{op}", s.require(auth.Operator, s.volumeAction))
	s.mux.HandleFunc("GET /images", s.require(auth.ReadOnly, s.images))
	s.mux.HandleFunc("POST /images", s.require(auth.Operator, s.importImage))
	s.mux.HandleFunc("POST /images/upload", s.require(auth.Operator, s.uploadImage))
	s.mux.HandleFunc("POST /images/{name}/delete", s.require(auth.Operator, s.deleteImage))
	s.mux.HandleFunc("GET /networks", s.require(auth.ReadOnly, s.networks))
	s.mux.HandleFunc("POST /networks", s.requireOrg(auth.Admin, s.createNetwork))
	s.mux.HandleFunc("POST /networks/{name}/delete", s.requireOrg(auth.Admin, s.deleteNetwork))
	s.mux.HandleFunc("GET /host", s.require(auth.Admin, s.host))
	s.mux.HandleFunc("POST /host/settings", s.require(auth.Admin, s.hostSettingsForm))
	s.mux.HandleFunc("POST /host/backup-targets/{op}", s.require(auth.Admin, s.backupTargetForm))
	s.mux.HandleFunc("POST /host/{action}", s.require(auth.Admin, s.hostRequest))
	s.mux.HandleFunc("POST /tokens", s.require(auth.Admin, s.createToken))
	s.mux.HandleFunc("POST /tokens/{name}/delete", s.require(auth.Admin, s.deleteToken))
	s.mux.HandleFunc("GET /ca.pem", s.caCert)
	s.mux.HandleFunc("GET /export/{ns}/{export}/{volume}", s.exportPassthrough)
	s.mux.HandleFunc("GET /backups/{vm}/{stamp}/{file}", s.serveBackup)
	s.mux.HandleFunc("GET /esxi-import/{disk}", s.serveESXi)
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/backups", s.require(auth.Admin, s.apiVMBackups))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/backups/{op}", s.require(auth.Admin, s.apiVMBackupOp))
	s.hostAPIRoutes()
	s.cbtRoutes()
	s.mux.HandleFunc("GET /api/host/network", s.require(auth.Admin, s.apiHostNetwork))
	s.mux.HandleFunc("GET /api/host/status", s.require(auth.Admin, s.apiHostStatus))
	s.mux.HandleFunc("GET /api/images", s.require(auth.Admin, s.apiImages))
	s.mux.HandleFunc("GET /api/alarms", s.require(auth.Admin, s.apiAlarms))
	s.mux.HandleFunc("GET /api/prometheus", s.require(auth.Admin, s.apiPrometheus))
	s.mux.HandleFunc("POST /api/images/import", s.require(auth.Admin, s.apiImportImage))
	s.mux.HandleFunc("POST /api/images/{name}/{op}", s.require(auth.Admin, s.apiImageOp))
	s.mux.HandleFunc("GET /api/host/settings", s.require(auth.Admin, s.apiHostSettings))
	s.mux.HandleFunc("POST /api/host/settings", s.require(auth.Admin, s.apiApplyHostSettings))
	s.mux.HandleFunc("GET /api/host/backup-targets", s.require(auth.Admin, s.apiBackupTargets))
	s.mux.HandleFunc("POST /api/tokens/rotate", s.require(auth.Admin, s.apiRotateToken))
	s.mux.HandleFunc("POST /api/tokens/retire", s.require(auth.Admin, s.apiRetireToken))
	s.mux.HandleFunc("POST /api/host/backup-targets/{op}", s.require(auth.Admin, s.apiBackupTargetOp))
	s.mux.HandleFunc("POST /api/host/power/{action}", s.require(auth.Admin, s.apiHostRequest))
	s.mux.HandleFunc("GET /api/node", s.require(auth.Admin, s.apiNode))
	s.mux.HandleFunc("GET /api/vms", s.require(auth.Admin, s.apiVMs))
	s.mux.HandleFunc("GET /api/metrics", s.require(auth.Admin, s.apiMetrics))
	s.mux.HandleFunc("GET /api/events", s.require(auth.Admin, s.apiEvents))
	s.mux.HandleFunc("GET /api/storage", s.require(auth.Admin, s.apiStorage))
	s.mux.HandleFunc("POST /api/volumes/{op}", s.require(auth.Admin, s.apiVolumeOp))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Admin, s.apiRemoveDisk))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/disks/move", s.require(auth.Admin, s.apiMoveDisk))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/tags", s.require(auth.Admin, s.apiSetTags))
	s.mux.HandleFunc("GET /api/networks", s.require(auth.Admin, s.apiNetworks))
	s.mux.HandleFunc("POST /api/networks/{op}", s.require(auth.Admin, s.apiNetworkOp))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/disks", s.require(auth.Admin, s.apiDisks))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/export", s.require(auth.Admin, s.apiExport))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/unexport", s.require(auth.Admin, s.apiUnexport))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/delete", s.require(auth.Admin, s.apiDelete))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/imported", s.require(auth.Admin, s.apiImported))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/{action}", s.require(auth.Admin, s.apiAction))
	s.mux.HandleFunc("POST /api/import", s.require(auth.Admin, s.apiImport))
	s.mux.HandleFunc("POST /api/heartbeat", s.require(auth.Admin, s.apiHeartbeat))
	s.mux.HandleFunc("GET /api/probe", s.require(auth.Admin, s.apiProbe))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/ha", s.require(auth.Admin, s.apiSetHA))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/replica/{op}", s.require(auth.Admin, s.apiReplica))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/zfs", s.require(auth.Admin, s.apiZFSDisks))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/zfs/snapshot", s.require(auth.Admin, s.apiZFSSnapshot))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/zfs/prune", s.require(auth.Admin, s.apiZFSPrune))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/zfs/send", s.require(auth.Admin, s.apiZFSSend))
	s.mux.HandleFunc("PUT /api/vms/{ns}/{name}/zfs/recv", s.require(auth.Admin, s.apiZFSRecv))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/snapshot-export", s.require(auth.Admin, s.apiSnapshotExport))
	s.mux.HandleFunc("GET /api/kubevirt-ca", s.require(auth.Admin, s.apiKubeVirtCA))
	s.mux.HandleFunc("POST /api/kubevirt-external-ca", s.require(auth.Admin, s.apiExternalCA))
	s.mux.HandleFunc("POST /api/migration-network", s.require(auth.Admin, s.apiMigrationNetwork))
	s.mux.HandleFunc("POST /api/migration-network/remove", s.require(auth.Admin, s.apiRemoveMigrationNetwork))
	s.mux.HandleFunc("POST /api/sync-controller", s.require(auth.Admin, s.apiSyncController))
	s.mux.HandleFunc("POST /api/receive", s.require(auth.Admin, s.apiReceive))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/spec", s.require(auth.Admin, s.apiVMSpec))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}", s.require(auth.Admin, s.apiVM))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/events", s.require(auth.Admin, s.apiEvents))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/snapshots", s.require(auth.Admin, s.apiSnapshots))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/snapshots", s.require(auth.Admin, s.apiSnapshotOp))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/snapshots/{snap}/{op}", s.require(auth.Admin, s.apiSnapshotOp))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/meta/{op}", s.require(auth.Admin, s.apiRenameClone))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/ws/{kind}", s.require(auth.Admin, s.consoleWS))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/migration", s.require(auth.Admin, s.apiMigrationPhase))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/send", s.require(auth.Admin, s.apiSend))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/migration-cleanup", s.require(auth.Admin, s.apiMigrationCleanup))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/abort", s.require(auth.Admin, s.apiAbort))
	s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.vmTable))
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/drawer", s.require(auth.ReadOnly, s.vmDrawer))
	s.mux.HandleFunc("GET /nav/vms", s.require(auth.ReadOnly, s.navVMs))
	s.mux.HandleFunc("POST /vms/bulk/{action}", s.require(auth.Operator, s.bulk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/delete", s.require(auth.Operator, s.deleteVM))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks", s.require(auth.Operator, s.addDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks/move", s.require(auth.Operator, s.moveDiskForm))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/tags", s.require(auth.Operator, s.setTags))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/disks/new", s.require(auth.Operator, s.newDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Operator, s.removeDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots", s.require(auth.Operator, s.snapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/backups/{op}", s.require(auth.Operator, s.vmBackupForm))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots/{snap}/restore", s.require(auth.Operator, s.restoreSnapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots/{snap}/delete", s.require(auth.Operator, s.deleteSnapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/{action}", s.require(auth.Operator, s.action))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/console", s.require(auth.Operator, s.console))
	s.mux.HandleFunc("GET /ws/{ns}/{name}/{kind}", s.require(auth.Operator, s.consoleWS))
	go s.haLoop()
	if s.manager == nil && k != nil {
		go s.backupLoop()
	}
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	if s.certs != nil && r.TLS != nil && r.TLS.ServerName != "" && r.TLS.ServerName != tlscert.InternalName && s.certs.Custom() != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if s.registry != nil && strings.HasPrefix(r.URL.Path, "/v2/") {
		s.serveRegistry(w, r)
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); r.Body != nil && !strings.HasPrefix(ct, "multipart/") && !strings.HasSuffix(r.URL.Path, "/zfs/recv") {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	}
	s.mux.ServeHTTP(w, r)
}

func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		ref, err := url.Parse(r.Header.Get("Referer"))
		if err != nil || ref.Host == "" {
			return false
		}
		origin = ref.Scheme + "://" + ref.Host
	}
	return origin == "https://"+r.Host
}

type hostRouteDef struct {
	pattern string
	role    auth.Role
	h       func(*Server, http.ResponseWriter, *http.Request)
}

var hostRoutes = []hostRouteDef{
	{"GET /dashboard", auth.ReadOnly, (*Server).dashboard},
	{"GET /dashboard/stats", auth.ReadOnly, (*Server).dashboardStats},
	{"GET /disks", auth.Admin, (*Server).disksPage},
	{"GET /disks/{name}", auth.Admin, (*Server).diskModal},
	{"POST /disks/{name}/{action}", auth.Admin, (*Server).diskAction},
	{"GET /network/host", auth.Admin, (*Server).hostNetworkPage},
	{"GET /network/host/iface/{name}", auth.Admin, (*Server).ifaceModal},
	{"POST /network/host/{op}", auth.Admin, (*Server).hostNetworkAction},
	{"GET /pools", auth.Admin, (*Server).poolsPage},
	{"GET /pools/new", auth.Admin, (*Server).poolModal},
	{"GET /pools/{name}/add", auth.Admin, (*Server).poolModal},
	{"POST /pools", auth.Admin, (*Server).createPool},
	{"POST /pools/{name}/{op}", auth.Admin, (*Server).poolAction},
	{"GET /vms/new", auth.Operator, (*Server).newVM},
	{"POST /vms", auth.Operator, (*Server).createVM},
	{"GET /vms/import", auth.Operator, (*Server).importModal},
	{"POST /vms/import/{source}", auth.Operator, (*Server).importVM},
	{"POST /vms/import/vcenter-vms", auth.Operator, (*Server).vcenterVMsOptions},
	{"GET /vms/{ns}/{name}/hardware", auth.Operator, (*Server).hardwareModal},
	{"POST /vms/{ns}/{name}/hardware/{op}", auth.Operator, (*Server).hardwareAction},
	{"POST /vms/{ns}/{name}/disks/{disk}/download", auth.Operator, (*Server).downloadLink},
	{"GET /vms/{ns}/{name}/disks/{disk}/download", auth.Operator, (*Server).downloadDisk},
}

func (s *Server) hostRoute(pattern string, min auth.Role, h http.HandlerFunc) {
	if s.manager == nil {
		s.mux.HandleFunc(pattern, s.require(min, h))
		return
	}
	method, path, _ := strings.Cut(pattern, " ")
	s.mux.HandleFunc(method+" /hosts/{node}"+path, s.require(min, func(w http.ResponseWriter, r *http.Request) {
		n, err := s.orgNode(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		h(w, onHost(r, n))
	}))
}

func onHost(r *http.Request, n manager.Node) *http.Request {
	ctx := context.WithValue(r.Context(), baseKey{}, "/hosts/"+n.Name)
	ctx = context.WithValue(ctx, opsKey{}, remoteOps{n})
	return r.WithContext(context.WithValue(ctx, hostNodeKey{}, n))
}

type baseKey struct{}

type hostNodeKey struct{}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Base"], _ = r.Context().Value(baseKey{}).(string)
	if n, ok := r.Context().Value(hostNodeKey{}).(manager.Node); ok && tmpl == "layout" {
		data["HostShell"], data["Host"], data["Node"], data["Tabs"], data["Nav"] = true, n.Name, n, hostTabs, "host:"+n.Name
		data["Tab"] = strings.TrimPrefix(r.URL.Path, "/hosts/"+n.Name+"/")
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	data["Session"] = session(r)
	data["Alarms"] = s.activeAlarms()
	data["OrgAdmin"] = session(r).Role.Can(auth.Admin) && len(session(r).Scopes) == 0
	if s.manager != nil {
		hosts := map[string]bool{}
		for _, st := range s.inventory(r) {
			hosts[st.Node.Name] = true
		}
		data["Alarms"] = slices.DeleteFunc(s.activeAlarms(), func(a Alarm) bool { return !hosts[a.Host] })
	}
	data["Manager"] = s.manager != nil
	data["Query"] = r.FormValue("q")
	data["Version"] = s.version
	data["NodeName"] = s.nodeName
	if s.runDir != "" {
		if b, err := os.ReadFile(filepath.Join(s.runDir, "net-pending.json")); err == nil {
			var p hostd.Pending
			if json.Unmarshal(b, &p) == nil {
				data["NetPending"] = p
			}
		}
	}
	if _, ok := data["Nav"]; !ok {
		data["Nav"] = ""
	}
	if err := s.pages[page].ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func session(r *http.Request) reqAuth {
	a, _ := r.Context().Value(ctxKey{}).(reqAuth)
	return a
}

func (s *Server) requireOrg(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return s.require(min, func(w http.ResponseWriter, r *http.Request) {
		if len(session(r).Scopes) > 0 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

var segmentParams = []string{"ns", "name", "op", "node", "id", "action", "disk", "snap", "kind", "file", "volume", "source", "export", "vm", "stamp", "org"}

func validSegments(r *http.Request) bool {
	for _, k := range segmentParams {
		if v := r.PathValue(k); v != "" && !safeSegment.MatchString(v) {
			return false
		}
	}
	return true
}

func (s *Server) require(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSegments(r) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			t, valid := s.auth.CheckToken(r.Context(), tok)
			if !valid || !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if t.Scope == auth.MetricsScope && r.URL.Path != "/api/prometheus" || t.Scope == auth.ReadScope && r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			sess, who, err := s.tokenSession(r.Context(), t)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !sess.Role.Can(min) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r2 := r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess}))
			s.audited(w, r2, who, func(w http.ResponseWriter) { h(w, r2) })
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := r.Cookie(s.cookieName())
		if err != nil {
			redirect(w, r, "/login")
			return
		}
		sess, ok := s.auth.Session(c.Value)
		if !ok {
			redirect(w, r, "/login")
			return
		}
		if sess.MustChange && r.URL.Path != "/password" {
			redirect(w, r, "/password")
			return
		}
		if !sess.Role.Can(min) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r2 := r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess, Token: c.Value}))
		s.audited(w, r2, s.auditUser(sess.Org, sess.User), func(w http.ResponseWriter) { h(w, r2) })
	}
}

func (s *Server) tokenSession(ctx context.Context, t auth.Token) (auth.Session, string, error) {
	if s.manager == nil {
		return auth.Session{User: "token:" + t.Name, Role: auth.Admin}, "token:" + t.Name, nil
	}
	st := s.auth.ForOrg(t.Org)
	us, err := st.Users(ctx)
	if err != nil {
		return auth.Session{}, "", err
	}
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == t.User })
	if t.User == "" || i < 0 {
		return auth.Session{}, "", errors.New("token owner not found")
	}
	role, scopes := st.Access(ctx, us[i])
	return auth.Session{User: t.User, Org: t.Org, Role: role, Scopes: scopes}, s.auditUser(t.Org, t.User) + " (token " + t.Name + ")", nil
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func flash(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s: %s %s: %v", session(r).User, r.Method, r.URL.Path, err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnprocessableEntity)
	w.Write([]byte(template.HTMLEscapeString(err.Error())))
}

func failed(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
