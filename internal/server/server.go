package server

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/web"
)

const cookieName = "argus_session"

type Server struct {
	kube        *kube.Client
	auth        *auth.Store
	banner      func(password string)
	runDir      string
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
}

type Options struct {
	Banner      func(password string)
	RunDir      string
	ReleaseFile string
	CAPEM       []byte
	NodeName    string
	Manager     *manager.Manager
}

type ctxKey struct{}

type reqAuth struct {
	auth.Session
	Token string
}

func New(k *kube.Client, a *auth.Store, o Options) (*Server, error) {
	s := &Server{kube: k, auth: a, banner: o.Banner, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM, nodeName: o.NodeName, manager: o.Manager, pages: map[string]*template.Template{}, mux: http.NewServeMux(), limiter: newLoginLimiter()}
	if k != nil {
		k.Guard = s.maintenance
	}
	s.version = readEnv(o.ReleaseFile)["KAIROS_VERSION"]
	s.sso.m = map[string]ssoFlow{}
	for _, p := range []string{"login.html", "password.html", "users.html", "vms.html", "console.html", "images.html", "networks.html", "vmnew.html", "host.html", "manager.html", "manager-vms.html", "manager-vm.html", "hosts.html", "settings.html", "events.html", "volumes.html", "storage.html"} {
		t, err := template.New(p).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/manager-nav.html", "templates/"+p)
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}

	s.mux.Handle("GET /static/", http.FileServerFS(web.FS))
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /password", s.require(auth.ReadOnly, s.passwordPage))
	s.mux.HandleFunc("POST /password", s.require(auth.ReadOnly, s.changePassword))
	s.mux.HandleFunc("GET /users", s.require(auth.Admin, s.usersPage))
	s.mux.HandleFunc("POST /users", s.require(auth.Admin, s.addUser))
	s.mux.HandleFunc("POST /users/{name}/role", s.require(auth.Admin, s.setRole))
	s.mux.HandleFunc("POST /users/{name}/reset", s.require(auth.Admin, s.resetPassword))
	s.mux.HandleFunc("POST /users/{name}/delete", s.require(auth.Admin, s.deleteUser))
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
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/snapshots/{snap}/{op}", s.require(auth.Operator, s.managerSnapshotOp))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/{action}", s.require(auth.Operator, s.managerVMAction))
		s.mux.HandleFunc("GET /events", s.require(auth.ReadOnly, s.managerEvents))
		s.mux.HandleFunc("POST /jobs/{id}/resolve", s.require(auth.Admin, s.managerResolve))
		s.mux.HandleFunc("GET /storage", s.require(auth.ReadOnly, s.managerStorage))
		s.mux.HandleFunc("GET /networks", s.require(auth.ReadOnly, s.managerNetworks))
		s.mux.HandleFunc("POST /networks", s.require(auth.Admin, s.managerAddNetwork))
		s.mux.HandleFunc("POST /networks/{name}/{op}", s.require(auth.Admin, s.managerNetworkOp))
		s.mux.HandleFunc("GET /volumes", s.require(auth.ReadOnly, s.managerVolumes))
		s.mux.HandleFunc("POST /volumes", s.require(auth.Operator, s.managerVolumeOp))
		s.mux.HandleFunc("POST /hosts/{node}/volumes/{op}", s.require(auth.Operator, s.managerVolumeOp))
		s.mux.HandleFunc("GET /hosts", s.require(auth.ReadOnly, s.managerHosts))
		s.mux.HandleFunc("POST /hosts", s.require(auth.Admin, s.managerAddNode))
		s.mux.HandleFunc("POST /hosts/{node}/group", s.require(auth.Admin, s.managerGroupNode))
		s.mux.HandleFunc("POST /hosts/{node}/delete", s.require(auth.Admin, s.managerRemoveNode))
		s.mux.HandleFunc("GET /settings", s.require(auth.Admin, s.managerSettings))
		s.mux.HandleFunc("POST /settings/sso", s.require(auth.Admin, s.managerSSO))
		s.mux.HandleFunc("POST /settings/orgs", s.require(auth.Admin, s.managerAddOrg))
		s.mux.HandleFunc("POST /settings/orgs/{id}/delete", s.require(auth.Admin, s.managerDeleteOrg))
		s.mux.HandleFunc("POST /settings/migration", s.require(auth.Admin, s.managerMigrationSettings))
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
	s.mux.HandleFunc("POST /networks", s.require(auth.Admin, s.createNetwork))
	s.mux.HandleFunc("POST /networks/{name}/delete", s.require(auth.Admin, s.deleteNetwork))
	s.mux.HandleFunc("GET /host", s.require(auth.Admin, s.host))
	s.mux.HandleFunc("POST /host/{action}", s.require(auth.Admin, s.hostRequest))
	s.mux.HandleFunc("POST /tokens", s.require(auth.Admin, s.createToken))
	s.mux.HandleFunc("POST /tokens/{name}/delete", s.require(auth.Admin, s.deleteToken))
	s.mux.HandleFunc("GET /ca.pem", s.caCert)
	s.mux.HandleFunc("GET /export/{ns}/{export}/{volume}", s.exportPassthrough)
	s.mux.HandleFunc("GET /api/node", s.require(auth.Admin, s.apiNode))
	s.mux.HandleFunc("GET /api/vms", s.require(auth.Admin, s.apiVMs))
	s.mux.HandleFunc("GET /api/metrics", s.require(auth.Admin, s.apiMetrics))
	s.mux.HandleFunc("GET /api/events", s.require(auth.Admin, s.apiEvents))
	s.mux.HandleFunc("GET /api/storage", s.require(auth.Admin, s.apiStorage))
	s.mux.HandleFunc("POST /api/volumes/{op}", s.require(auth.Admin, s.apiVolumeOp))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Admin, s.apiRemoveDisk))
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
	s.mux.HandleFunc("GET /vms/new", s.require(auth.Operator, s.newVM))
	s.mux.HandleFunc("POST /vms", s.require(auth.Operator, s.createVM))
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/drawer", s.require(auth.ReadOnly, s.vmDrawer))
	s.mux.HandleFunc("GET /nav/vms", s.require(auth.ReadOnly, s.navVMs))
	s.mux.HandleFunc("POST /vms/bulk/{action}", s.require(auth.Operator, s.bulk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/delete", s.require(auth.Operator, s.deleteVM))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks", s.require(auth.Operator, s.addDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Operator, s.removeDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots", s.require(auth.Operator, s.snapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots/{snap}/restore", s.require(auth.Operator, s.restoreSnapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/snapshots/{snap}/delete", s.require(auth.Operator, s.deleteSnapshot))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/{action}", s.require(auth.Operator, s.action))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/console", s.require(auth.Operator, s.console))
	s.mux.HandleFunc("GET /ws/{ns}/{name}/{kind}", s.require(auth.Operator, s.consoleWS))
	go s.haLoop()
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

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Session"] = session(r)
	data["Manager"] = s.manager != nil
	data["Query"] = r.FormValue("q")
	data["Version"] = s.version
	data["NodeName"] = s.nodeName
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

func (s *Server) require(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			name, valid := s.auth.CheckToken(r.Context(), tok)
			if !valid || !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			sess := auth.Session{User: "token:" + name, Role: auth.Admin}
			h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess})))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := r.Cookie(cookieName)
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
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess, Token: c.Value})))
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
