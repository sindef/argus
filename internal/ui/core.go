package ui

import (
	"context"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"argus/internal/auth"
	"argus/internal/lifecycle"
	"argus/web"
)

type Mode interface {
	TokenSession(ctx context.Context, t auth.Token) (auth.Session, string, error)
	Grants(ctx context.Context, sess auth.Session) ([]auth.Grant, error)
	Decorate(r *http.Request, tmpl string, data map[string]any)
	LoginStore(w http.ResponseWriter, r *http.Request) (store *auth.Store, remember bool, err error)
	Directory(r *http.Request, store *auth.Store) (auth.User, bool, error)
	LoginData(r *http.Request, data map[string]any)
	ScopeNames(r *http.Request) []string
	Super(r *http.Request) bool
}

type Config struct {
	Auth     *auth.Store
	Banner   func(password string)
	Product  string
	Cookie   string
	Version  string
	NodeName string
	MultiOrg bool
}

type Core struct {
	Config
	Mode    Mode
	Mux     *http.ServeMux
	Limiter *LoginLimiter
	pages   map[string]*template.Template
	mfa     mfaState
	audit   auditLog
	alarms  alarmState
}

func New(c Config) (*Core, error) {
	core := &Core{Config: c, Mux: http.NewServeMux(), Limiter: newLoginLimiter(), pages: map[string]*template.Template{}}
	files, err := fs.Glob(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		p := path.Base(f)
		if p == "layout.html" || p == "manager-nav.html" {
			continue
		}
		t, err := template.New(p).Funcs(funcs).ParseFS(web.FS, "templates/layout.html", "templates/manager-nav.html", f)
		if err != nil {
			return nil, err
		}
		core.pages[p] = t
	}
	return core, nil
}

func (c *Core) Handle(rs []Route) {
	for _, rt := range rs {
		c.Mux.HandleFunc(rt.Pattern, c.Guard(rt.Priv, rt.H))
	}
}

func (c *Core) Run(ctx context.Context) {
	var g lifecycle.Group
	g.Go(ctx, c.auditLoop)
	g.Go(ctx, c.Auth.PersistSessions)
	g.Wait()
}

func (c *Core) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); r.Body != nil && !strings.HasPrefix(ct, "multipart/") && !strings.HasSuffix(r.URL.Path, "/zfs/recv") {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	}
	c.Mux.ServeHTTP(w, r)
}
