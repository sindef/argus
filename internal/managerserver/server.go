package managerserver

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/hostui"
	"argus/internal/lifecycle"
	"argus/internal/manager"
	"argus/internal/registry"
	"argus/internal/tlscert"
	"argus/internal/ui"
)

type Server struct {
	*hostui.UI
	manager  *manager.Manager
	sso      ssoFlows
	dataDir  string
	registry *registry.Registry
	regAuth  registryAuth
	caPEM    []byte
	port     string
}

type Options struct {
	Banner  func(password string)
	Manager *manager.Manager
	Certs   *tlscert.Holder
	DataDir string
	Addr    string
}

func New(a *auth.Store, o Options) (*Server, error) {
	core, err := ui.New(ui.Config{Auth: a, Banner: o.Banner, Product: "Argus Manager", Cookie: "argus_manager_session", MultiOrg: true, Certs: o.Certs})
	if err != nil {
		return nil, err
	}
	s := &Server{UI: &hostui.UI{Core: core}, manager: o.Manager, dataDir: o.DataDir, port: "443"}
	if _, port, err := net.SplitHostPort(o.Addr); err == nil {
		s.port = port
	}
	core.Mode = s
	s.sso.m = map[string]ssoFlow{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = s.startRegistry(ctx)
	cancel()
	if err != nil {
		return nil, err
	}
	s.Handle(s.routes())
	return s, nil
}

func (s *Server) Run(ctx context.Context) {
	var g lifecycle.Group
	g.Go(ctx, s.Core.Run)
	g.Go(ctx, s.alarmLoop)
	g.Go(ctx, func(ctx context.Context) {
		lifecycle.Every(ctx, 6*time.Hour, true, "registry gc", func(context.Context) {
			if err := s.registry.GC(); err != nil {
				log.Printf("registry: gc: %v", err)
			}
		})
	})
	g.Wait()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v2/") {
		s.serveRegistry(w, r)
		return
	}
	s.Core.ServeHTTP(w, r)
}

func (s *Server) onHost(r *http.Request, n manager.Node) *http.Request {
	ctx := ui.WithBase(r.Context(), "/hosts/"+n.Name)
	ctx = hostui.WithOps(ctx, remoteOps{n, s})
	return r.WithContext(context.WithValue(ctx, hostNodeKey{}, n))
}

type hostNodeKey struct{}

func (s *Server) TokenSession(ctx context.Context, t auth.Token) (auth.Session, string, error) {
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
	return s.Auth.ForOrg(sess.Org).Grants(ctx, sess.User)
}

func (s *Server) Decorate(r *http.Request, tmpl string, data map[string]any) {
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
	org := r.FormValue("org")
	if _, err := s.manager.Org(r.Context(), org); err != nil {
		return nil, false, err
	}
	http.SetCookie(w, &http.Cookie{Name: "argus_org", Value: org, Path: "/", MaxAge: 365 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return s.Auth.ForOrg(org), r.FormValue("remember") != "", nil
}

func (s *Server) Directory(r *http.Request, store *auth.Store) (auth.User, bool, error) {
	return s.directoryLogin(r, store)
}

func (s *Server) LoginData(r *http.Request, data map[string]any) {
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
	return superAdmin(r)
}

func (s *Server) AlarmVisible(r *http.Request, a ui.Alarm) bool {
	if a.Host == "" {
		return superAdmin(r)
	}
	_, err := s.nodeIn(r, a.Host)
	return err == nil
}
