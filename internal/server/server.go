package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	"argus/internal/ui"
)

type Server struct {
	*ui.Core
	kube        *kube.Client
	runDir      string
	hostd       *hostd.Client
	releaseFile string
	caPEM       []byte
	manager     *manager.Manager
	ha          haState
	sso         ssoFlows
	backups     backupState
	schedSeen   time.Time
	flr         flrState
	certs       *tlscert.Holder
	dataDir     string
	esxi        esxiStreams
	registry    *registry.Registry
	regAuth     registryAuth
	smart       smartCache
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

func New(k *kube.Client, a *auth.Store, o Options) (*Server, error) {
	cfg := ui.Config{Auth: a, Banner: o.Banner, Product: "Argus", Cookie: "argus_session", Version: readEnv(o.ReleaseFile)["KAIROS_VERSION"], NodeName: o.NodeName}
	if o.Manager != nil {
		cfg.Product, cfg.Cookie, cfg.MultiOrg = "Argus Manager", "argus_manager_session", true
	}
	core, err := ui.New(cfg)
	if err != nil {
		return nil, err
	}
	s := &Server{Core: core, kube: k, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM, manager: o.Manager, certs: o.Certs, dataDir: o.DataDir}
	core.Mode = s
	if k != nil {
		k.Guard = s.maintenance
	}
	if o.RunDir != "" {
		s.hostd = hostd.NewClient(filepath.Join(o.RunDir, "hostd.sock"), os.Getenv("HOST_IP"))
	}
	s.sso.m = map[string]ssoFlow{}
	if s.manager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := s.startRegistry(ctx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	s.Handle(s.routes())
	return s, nil
}

func (s *Server) Run(ctx context.Context) {
	var g lifecycle.Group
	g.Go(ctx, s.Core.Run)
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
	if s.certs != nil && r.TLS != nil && r.TLS.ServerName != "" && r.TLS.ServerName != tlscert.InternalName && s.certs.Custom() != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	if s.registry != nil && strings.HasPrefix(r.URL.Path, "/v2/") {
		s.serveRegistry(w, r)
		return
	}
	s.Core.ServeHTTP(w, r)
}

func onHost(r *http.Request, n manager.Node) *http.Request {
	ctx := ui.WithBase(r.Context(), "/hosts/"+n.Name)
	ctx = context.WithValue(ctx, opsKey{}, remoteOps{n})
	return r.WithContext(context.WithValue(ctx, hostNodeKey{}, n))
}

type hostNodeKey struct{}

func (s *Server) TokenSession(ctx context.Context, t auth.Token) (auth.Session, string, error) {
	if s.manager == nil {
		return auth.Session{User: "token:" + t.Name, Role: auth.Admin}, "token:" + t.Name, nil
	}
	st := s.Auth.ForOrg(t.Org)
	us, err := st.Users(ctx)
	if err != nil {
		return auth.Session{}, "", err
	}
	i := slices.IndexFunc(us, func(u auth.User) bool { return u.Name == t.User })
	if t.User == "" || i < 0 {
		return auth.Session{}, "", errors.New("token owner not found")
	}
	role, scopes := st.Access(ctx, us[i])
	return auth.Session{User: t.User, Org: t.Org, Role: role, Scopes: scopes}, s.AuditUser(t.Org, t.User) + " (token " + t.Name + ")", nil
}

func (s *Server) Grants(ctx context.Context, sess auth.Session) ([]auth.Grant, error) {
	if s.manager == nil {
		return auth.RoleGrants(sess.Role), nil
	}
	return s.Auth.ForOrg(sess.Org).Grants(ctx, sess.User)
}

func (s *Server) Decorate(r *http.Request, tmpl string, data map[string]any) {
	if s.manager == nil {
		if b, err := os.ReadFile(filepath.Join(s.runDir, "net-pending.json")); s.runDir != "" && err == nil {
			var p hostd.Pending
			if json.Unmarshal(b, &p) == nil {
				data["NetPending"] = p
			}
		}
		return
	}
	data["Manager"] = true
	hosts := map[string]bool{}
	for _, st := range s.inventory(r) {
		hosts[st.Node.Name] = true
	}
	data["Alarms"] = slices.DeleteFunc(s.ActiveAlarms(), func(a ui.Alarm) bool { return !hosts[a.Host] })
	if n, ok := r.Context().Value(hostNodeKey{}).(manager.Node); ok && tmpl == "layout" {
		data["HostShell"], data["Host"], data["Node"], data["Tabs"], data["Nav"] = true, n.Name, n, hostTabs, "host:"+n.Name
		data["Tab"] = strings.TrimPrefix(r.URL.Path, "/hosts/"+n.Name+"/")
	}
}

func (s *Server) LoginStore(w http.ResponseWriter, r *http.Request) (*auth.Store, bool, error) {
	if s.manager == nil {
		return s.Auth, false, nil
	}
	org := r.FormValue("org")
	if _, err := s.manager.Org(r.Context(), org); err != nil {
		return nil, false, err
	}
	http.SetCookie(w, &http.Cookie{Name: "argus_org", Value: org, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return s.Auth.ForOrg(org), r.FormValue("remember") != "", nil
}

func (s *Server) Directory(r *http.Request, store *auth.Store) (auth.User, bool, error) {
	if s.manager == nil {
		return auth.User{}, false, nil
	}
	return s.directoryLogin(r, store)
}

func (s *Server) LoginData(r *http.Request, data map[string]any) {
	if s.manager == nil {
		return
	}
	orgs, _ := s.manager.Orgs(r.Context())
	sso := false
	for _, o := range orgs {
		sso = sso || o.SSO()
	}
	org := r.FormValue("org")
	if c, err := r.Cookie("argus_org"); err == nil && org == "" {
		org = c.Value
	}
	data["Orgs"], data["Org"], data["SSO"] = orgs, org, sso
}

func (s *Server) Super(r *http.Request) bool {
	return s.manager == nil || superAdmin(r)
}
