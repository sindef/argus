package hostserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/tlscert"
	"argus/internal/ui"
)

type Server struct {
	*hostui.UI
	kube        *kube.Client
	runDir      string
	hostd       *hostd.Client
	releaseFile string
	caPEM       []byte
	ha          haState
	backups     backupState
	schedSeen   time.Time
	flr         flrState
	esxi        esxiStreams
	tasks       taskList
	smart       smartCache
}

type Options struct {
	Banner      func(password string)
	RunDir      string
	ReleaseFile string
	CAPEM       []byte
	NodeName    string
	Certs       *tlscert.Holder
}

func New(k *kube.Client, a *auth.Store, o Options) (*Server, error) {
	core, err := ui.New(ui.Config{Auth: a, Banner: o.Banner, Product: "Argus", Cookie: "argus_session", Version: readEnv(o.ReleaseFile)["KAIROS_VERSION"], NodeName: o.NodeName, Certs: o.Certs})
	if err != nil {
		return nil, err
	}
	s := &Server{UI: &hostui.UI{Core: core}, kube: k, runDir: o.RunDir, releaseFile: o.ReleaseFile, caPEM: o.CAPEM}
	core.Mode = s
	s.Local = localOps{s}
	if k != nil {
		k.Guard = s.maintenance
	}
	if o.RunDir != "" {
		s.hostd = hostd.NewClient(filepath.Join(o.RunDir, "hostd.sock"), os.Getenv("HOST_IP"))
	}
	s.Handle(s.routes())
	return s, nil
}

func (s *Server) Run(ctx context.Context) {
	var g lifecycle.Group
	g.Go(ctx, s.Core.Run)
	g.Go(ctx, s.haLoop)
	if s.kube != nil {
		g.Go(ctx, s.alarmLoop)
		g.Go(ctx, s.backupLoop)
		g.Go(ctx, s.storageLoop)
		g.Go(ctx, s.kube.WatchVMs)
	}
	g.Wait()
}

func (s *Server) TokenSession(_ context.Context, t auth.Token) (auth.Session, string, error) {
	return auth.Session{User: "token:" + t.Name, Role: auth.Admin}, "token:" + t.Name, nil
}

func (s *Server) Grants(_ context.Context, sess auth.Session) ([]auth.Grant, error) {
	return auth.RoleGrants(sess.Role), nil
}

func (s *Server) Decorate(r *http.Request, tmpl string, data map[string]any) {
	if s.runDir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(s.runDir, "net-pending.json")); err == nil {
		var p hostd.Pending
		if json.Unmarshal(b, &p) == nil {
			data["NetPending"] = p
		}
	}
}

func (s *Server) LoginStore(http.ResponseWriter, *http.Request) (*auth.Store, bool, error) {
	return s.Auth, false, nil
}

func (s *Server) Directory(*http.Request, *auth.Store) (auth.User, bool, error) {
	return auth.User{}, false, nil
}

func (s *Server) LoginData(*http.Request, map[string]any) {}

func (s *Server) ScopeNames(*http.Request) []string { return nil }

func (s *Server) Super(*http.Request) bool { return true }

func (s *Server) AlarmVisible(*http.Request, ui.Alarm) bool { return true }
