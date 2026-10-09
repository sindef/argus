package server

import (
	"context"
	"html/template"
	"log"
	"net/http"
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
	manager     *manager.Manager
	ha          haState
	pages       map[string]*template.Template
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
	s := &Server{kube: k, auth: a, banner: o.Banner, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM, nodeName: o.NodeName, manager: o.Manager, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	for _, p := range []string{"login.html", "password.html", "users.html", "vms.html", "vm.html", "console.html", "images.html", "networks.html", "vmnew.html", "host.html", "manager.html", "nodes.html"} {
		t, err := template.ParseFS(web.FS, "templates/layout.html", "templates/"+p)
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
	s.mux.HandleFunc("GET /users", s.require(auth.Admin, s.users))
	s.mux.HandleFunc("POST /users", s.require(auth.Admin, s.addUser))
	s.mux.HandleFunc("POST /users/{name}/role", s.require(auth.Admin, s.setRole))
	s.mux.HandleFunc("POST /users/{name}/reset", s.require(auth.Admin, s.resetPassword))
	s.mux.HandleFunc("POST /users/{name}/delete", s.require(auth.Admin, s.deleteUser))
	if s.manager != nil {
		s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.managerVMs))
		s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.managerTable))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/migrate", s.require(auth.Admin, s.managerMigrate))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/protect", s.require(auth.Admin, s.managerProtect))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/unprotect", s.require(auth.Admin, s.managerUnprotect))
		s.mux.HandleFunc("POST /nodes/{node}/vms/{ns}/{name}/{action}", s.require(auth.Operator, s.managerAction))
		s.mux.HandleFunc("GET /nodes", s.require(auth.ReadOnly, s.managerNodes))
		s.mux.HandleFunc("POST /nodes", s.require(auth.Admin, s.managerAddNode))
		s.mux.HandleFunc("POST /nodes/{node}/delete", s.require(auth.Admin, s.managerRemoveNode))
		s.mux.HandleFunc("POST /settings/migration", s.require(auth.Admin, s.managerMigrationSettings))
		return s, nil
	}
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
	s.mux.HandleFunc("GET /api/networks", s.require(auth.Admin, s.apiNetworks))
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
	s.mux.HandleFunc("POST /api/receive", s.require(auth.Admin, s.apiReceive))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/spec", s.require(auth.Admin, s.apiVMSpec))
	s.mux.HandleFunc("GET /api/vms/{ns}/{name}/migration", s.require(auth.Admin, s.apiMigrationPhase))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/send", s.require(auth.Admin, s.apiSend))
	s.mux.HandleFunc("POST /api/vms/{ns}/{name}/migration-cleanup", s.require(auth.Admin, s.apiMigrationCleanup))
	s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.vmTable))
	s.mux.HandleFunc("GET /vms/new", s.require(auth.Operator, s.newVM))
	s.mux.HandleFunc("POST /vms", s.require(auth.Operator, s.createVM))
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vm))
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
	s.mux.ServeHTTP(w, r)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Session"] = session(r)
	data["Manager"] = s.manager != nil
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
