package server

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/manager"
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
	mux         *http.ServeMux
	ui          *http.ServeMux
	backups     backupState
	certs       *tlscert.Holder
	esxi        esxiStreams
	auditLog    auditLog
	alarms      alarmState
}

type Options struct {
	Banner      func(password string)
	RunDir      string
	ReleaseFile string
	CAPEM       []byte
	NodeName    string
	Manager     *manager.Manager
	Certs       *tlscert.Holder
}

type ctxKey struct{}

type reqAuth struct {
	auth.Session
	Token string
}

func New(k *kube.Client, a *auth.Store, o Options) (*Server, error) {
	s := &Server{kube: k, auth: a, banner: o.Banner, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM, nodeName: o.NodeName, manager: o.Manager, certs: o.Certs, pages: map[string]*template.Template{}, mux: http.NewServeMux(), ui: http.NewServeMux(), limiter: newLoginLimiter()}
	if k != nil {
		k.Guard = s.maintenance
	}
	if o.RunDir != "" {
		s.hostd = hostd.NewClient(filepath.Join(o.RunDir, "hostd.sock"), os.Getenv("HOST_IP"))
	}
	s.version = readEnv(o.ReleaseFile)["KAIROS_VERSION"]
	s.sso.m = map[string]ssoFlow{}
	for _, p := range []string{"login.html", "password.html", "users.html", "vms.html", "console.html", "images.html", "networks.html", "vmnew.html", "host.html", "manager.html", "manager-vms.html", "manager-vm.html", "hosts.html", "settings.html", "events.html", "volumes.html", "storage.html", "disks.html", "pools.html", "hostnet.html", "dashboard.html", "remote.html", "hostnets.html", "profile.html", "manager-images.html", "audit.html"} {
		t, err := template.New(p).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/manager-nav.html", "templates/"+p)
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}

	go s.auditLoop()
	if k != nil || s.manager != nil {
		go s.alarmLoop()
	}
	s.mux.Handle("GET /static/", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /audit", s.requireOrg(auth.Admin, s.auditPage))
	s.mux.HandleFunc("POST /notify", s.requireOrg(auth.Admin, s.notifyForm))
	s.mux.HandleFunc("POST /certificate", s.requireOrg(auth.Admin, s.setCertificate))
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /password", s.require(auth.ReadOnly, s.passwordPage))
	s.mux.HandleFunc("POST /password", s.require(auth.ReadOnly, s.changePassword))
	s.mux.HandleFunc("GET /users", s.requireOrg(auth.Admin, s.usersPage))
	s.mux.HandleFunc("POST /users", s.requireOrg(auth.Admin, s.addUser))
	s.mux.HandleFunc("POST /users/{name}/role", s.requireOrg(auth.Admin, s.setRole))
	s.mux.HandleFunc("POST /users/scopes", s.requireOrg(auth.Admin, s.setScopes))
	s.mux.HandleFunc("POST /users/{name}/reset", s.requireOrg(auth.Admin, s.resetPassword))
	s.mux.HandleFunc("POST /users/{name}/delete", s.requireOrg(auth.Admin, s.deleteUser))
	if s.manager != nil {
		s.mux.HandleFunc("POST /sso/start", s.ssoStart)
		s.mux.HandleFunc("GET /sso/callback", s.ssoCallback)
		s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.managerHome))
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
		s.mux.HandleFunc("GET /hosts/{node}/{path...}", s.require(auth.ReadOnly, s.managerHostUI))
		s.mux.HandleFunc("POST /hosts/{node}/{path...}", s.require(auth.Admin, s.managerHostUI))
		s.mux.HandleFunc("POST /hosts/{node}/vms/{ns}/{name}/hardware/{op}", s.require(auth.Operator, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("path", "vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"/hardware/"+r.PathValue("op"))
			s.managerHostUI(w, r)
		}))
		s.mux.HandleFunc("POST /hosts/{node}/vms/import/{source}", s.require(auth.Operator, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("path", "vms/import/"+r.PathValue("source"))
			s.managerHostUI(w, r)
		}))
		s.mux.HandleFunc("POST /hosts/{node}/vms", s.require(auth.Operator, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("path", "vms")
			s.managerHostUI(w, r)
		}))
		s.mux.HandleFunc("GET /network/hosts", s.requireOrg(auth.Admin, s.managerHostNets))
		s.mux.HandleFunc("GET /vm-new", s.require(auth.Operator, s.managerNewVM))
		s.mux.HandleFunc("GET /images", s.require(auth.ReadOnly, s.managerImages))
		s.mux.HandleFunc("POST /images/{op}", s.requireOrg(auth.Operator, s.managerImageOp))
		s.mux.HandleFunc("POST /network/hosts/bridge", s.requireOrg(auth.Admin, s.managerBridgeRollout))
		s.mux.HandleFunc("GET /settings", s.requireOrg(auth.Admin, s.managerSettings))
		s.mux.HandleFunc("POST /settings/sso", s.requireOrg(auth.Admin, s.managerSSO))
		s.mux.HandleFunc("POST /settings/orgs", s.requireOrg(auth.Admin, s.managerAddOrg))
		s.mux.HandleFunc("POST /settings/orgs/{id}/delete", s.requireOrg(auth.Admin, s.managerDeleteOrg))
		s.mux.HandleFunc("POST /settings/migration", s.requireOrg(auth.Admin, s.managerMigrationSettings))
		return s, nil
	}
	s.mux.HandleFunc("GET /events", s.require(auth.ReadOnly, s.eventsPage))
	s.mux.HandleFunc("GET /volumes", s.require(auth.ReadOnly, s.volumesPage))
	s.mux.HandleFunc("POST /volumes/{op}", s.require(auth.Operator, s.volumeAction))
	s.hostRoute("GET /dashboard", auth.ReadOnly, s.dashboard)
	s.hostRoute("GET /dashboard/stats", auth.ReadOnly, s.dashboardStats)
	s.hostRoute("GET /disks", auth.Admin, s.disksPage)
	s.hostRoute("GET /disks/{name}", auth.Admin, s.diskModal)
	s.hostRoute("POST /disks/{name}/{action}", auth.Admin, s.diskAction)
	s.hostRoute("GET /network/host", auth.Admin, s.hostNetworkPage)
	s.hostRoute("GET /network/host/iface/{name}", auth.Admin, s.ifaceModal)
	s.hostRoute("POST /network/host/{op}", auth.Admin, s.hostNetworkAction)
	s.hostRoute("GET /pools", auth.Admin, s.poolsPage)
	s.hostRoute("GET /pools/new", auth.Admin, s.poolModal)
	s.hostRoute("GET /pools/{name}/add", auth.Admin, s.poolModal)
	s.hostRoute("POST /pools", auth.Admin, func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("op", "create")
		s.poolAction(w, r)
	})
	s.hostRoute("POST /pools/{name}/{op}", auth.Admin, s.poolAction)
	s.mux.HandleFunc("GET /images", s.require(auth.ReadOnly, s.images))
	s.mux.HandleFunc("POST /images", s.require(auth.Operator, s.importImage))
	s.mux.HandleFunc("POST /images/upload", s.require(auth.Operator, s.uploadImage))
	s.mux.HandleFunc("POST /images/{name}/delete", s.require(auth.Operator, s.deleteImage))
	s.mux.HandleFunc("GET /networks", s.require(auth.ReadOnly, s.networks))
	s.mux.HandleFunc("POST /networks", s.requireOrg(auth.Admin, s.createNetwork))
	s.mux.HandleFunc("POST /networks/{name}/delete", s.requireOrg(auth.Admin, s.deleteNetwork))
	s.mux.HandleFunc("GET /host", s.require(auth.Admin, s.host))
	s.mux.HandleFunc("POST /host/settings", s.require(auth.Admin, s.hostSettingsForm))
	s.mux.HandleFunc("POST /host/{action}", s.require(auth.Admin, s.hostRequest))
	s.mux.HandleFunc("POST /tokens", s.require(auth.Admin, s.createToken))
	s.mux.HandleFunc("POST /tokens/{name}/delete", s.require(auth.Admin, s.deleteToken))
	s.mux.HandleFunc("GET /ca.pem", s.caCert)
	s.mux.HandleFunc("GET /export/{ns}/{export}/{volume}", s.exportPassthrough)
	s.mux.HandleFunc("GET /backups/{vm}/{stamp}/{file}", s.serveBackup)
	s.mux.HandleFunc("GET /esxi-import/{disk}", s.serveESXi)
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/backups", s.require(auth.Admin, s.apiVMBackups))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/backups/{op}", s.require(auth.Admin, s.apiVMBackupOp))
	s.mux.HandleFunc("/api/ui/", s.require(auth.Admin, s.serveUI))
	s.mux.HandleFunc("GET /api/host/network", s.require(auth.Admin, s.apiHostNetwork))
	s.mux.HandleFunc("GET /api/host/status", s.require(auth.Admin, s.apiHostStatus))
	s.mux.HandleFunc("GET /api/images", s.require(auth.Admin, s.apiImages))
	s.mux.HandleFunc("GET /api/alarms", s.require(auth.Admin, s.apiAlarms))
	s.mux.HandleFunc("GET /api/prometheus", s.require(auth.Admin, s.apiPrometheus))
	s.mux.HandleFunc("POST /api/images/import", s.require(auth.Admin, s.apiImportImage))
	s.mux.HandleFunc("POST /api/images/{name}/{op}", s.require(auth.Admin, s.apiImageOp))
	s.mux.HandleFunc("GET /api/host/settings", s.require(auth.Admin, s.apiHostSettings))
	s.mux.HandleFunc("POST /api/host/settings", s.require(auth.Admin, s.apiApplyHostSettings))
	s.mux.HandleFunc("POST /api/host/power/{action}", s.require(auth.Admin, s.apiHostRequest))
	s.mux.HandleFunc("POST /api/host/network/{op}", s.require(auth.Admin, s.apiHostNetworkOp))
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
	s.hostRoute("GET /vms/new", auth.Operator, s.newVM)
	s.hostRoute("POST /vms", auth.Operator, s.createVM)
	s.hostRoute("GET /vms/import", auth.Operator, s.importModal)
	s.hostRoute("POST /vms/import/{source}", auth.Operator, s.importVM)
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/drawer", s.require(auth.ReadOnly, s.vmDrawer))
	s.mux.HandleFunc("GET /nav/vms", s.require(auth.ReadOnly, s.navVMs))
	s.mux.HandleFunc("POST /vms/bulk/{action}", s.require(auth.Operator, s.bulk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/delete", s.require(auth.Operator, s.deleteVM))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks", s.require(auth.Operator, s.addDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks/move", s.require(auth.Operator, s.moveDiskForm))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/tags", s.require(auth.Operator, s.setTags))
	s.hostRoute("GET /vms/{ns}/{name}/hardware", auth.Operator, s.hardwareModal)
	s.hostRoute("GET /vms/{ns}/{name}/disks/{disk}/download", auth.Operator, s.downloadDisk)
	s.hostRoute("POST /vms/{ns}/{name}/hardware/{op}", auth.Operator, s.hardwareAction)
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
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
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

func (s *Server) hostRoute(pattern string, min auth.Role, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, s.require(min, h))
	s.ui.HandleFunc(pattern, h)
}

const BaseHeader = "X-Argus-Base"

var validBase = regexp.MustCompile(`^/hosts/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type baseKey struct{}

func (s *Server) serveUI(w http.ResponseWriter, r *http.Request) {
	base := r.Header.Get(BaseHeader)
	if !validBase.MatchString(base) {
		http.Error(w, "invalid base", http.StatusBadRequest)
		return
	}
	r2 := r.Clone(context.WithValue(r.Context(), baseKey{}, base))
	r2.URL.Path, r2.URL.RawPath = strings.TrimPrefix(r.URL.Path, "/api/ui"), ""
	s.ui.ServeHTTP(w, r2)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	base, remote := r.Context().Value(baseKey{}).(string)
	data["Base"] = base
	if remote && tmpl == "layout" {
		tmpl = "remote"
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	data["Session"] = session(r)
	data["Alarms"] = s.activeAlarms()
	data["OrgAdmin"] = session(r).Role.Can(auth.Admin) && len(session(r).Scopes) == 0
	if s.manager != nil && len(session(r).Scopes) > 0 {
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

func (s *Server) require(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			name, valid := s.auth.CheckToken(r.Context(), tok)
			if !valid || !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			sess := auth.Session{User: "token:" + name, Role: auth.Admin}
			r2 := r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess}))
			s.audited(w, r2, sess.User, func(w http.ResponseWriter) { h(w, r2) })
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

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func flash(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s: %s %s: %v", session(r).User, r.Method, r.URL.Path, err)
	w.Write([]byte(template.HTMLEscapeString(err.Error())))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
