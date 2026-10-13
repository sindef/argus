package server

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/lifecycle"
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
	schedSeen   time.Time
	flr         flrState
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
	pages, err := fs.Glob(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range pages {
		p := path.Base(f)
		if p == "layout.html" || p == "manager-nav.html" {
			continue
		}
		t, err := template.New(p).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/manager-nav.html", f)
		if err != nil {
			return nil, err
		}
		s.pages[p] = t
	}

	if s.manager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.startRegistry(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	for _, rt := range s.routes() {
		s.mux.HandleFunc(rt.pattern, s.guard(rt.priv, rt.h))
	}
	return s, nil
}

func (s *Server) Run(ctx context.Context) {
	var g lifecycle.Group
	g.Go(ctx, s.auditLoop)
	g.Go(ctx, s.auth.PersistSessions)
	g.Go(ctx, s.haLoop)
	if s.kube != nil || s.manager != nil {
		g.Go(ctx, s.alarmLoop)
	}
	if s.manager == nil && s.kube != nil {
		g.Go(ctx, s.backupLoop)
		g.Go(ctx, s.storageLoop)
		g.Go(ctx, s.kube.WatchVMs)
	}
	if s.registry != nil {
		g.Go(ctx, func(ctx context.Context) {
			lifecycle.Every(ctx, 6*time.Hour, true, "registry gc", func(context.Context) {
				if err := s.registry.GC(); err != nil {
					log.Printf("registry: gc: %v", err)
				}
			})
		})
	}
	g.Wait()
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

func (s *Server) requireOrg(h http.HandlerFunc) http.HandlerFunc {
	return s.authenticate(func(w http.ResponseWriter, r *http.Request) {
		if !session(r).Role.Can(auth.Admin) || len(session(r).Scopes) > 0 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

var segmentParams = []string{"ns", "name", "op", "node", "id", "action", "disk", "snap", "kind", "file", "volume", "source", "export", "vm", "stamp", "org", "vol"}

func validSegments(r *http.Request) bool {
	for _, k := range segmentParams {
		if v := r.PathValue(k); v != "" && !safeSegment.MatchString(v) {
			return false
		}
	}
	return true
}

func (s *Server) authenticate(h http.HandlerFunc) http.HandlerFunc {
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
