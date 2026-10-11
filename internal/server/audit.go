package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	auditKey  = "argus-audit"
	auditKeep = 1000
)

type AuditEntry struct {
	Time   time.Time `json:"time"`
	User   string    `json:"user"`
	Action string    `json:"action"`
	OK     bool      `json:"ok"`
	Result string    `json:"result,omitempty"`
}

type auditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	dirty   bool
}

type auditWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (a *auditWriter) WriteHeader(code int) {
	a.status = code
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditWriter) Write(b []byte) (int, error) {
	if a.body.Len() < 512 {
		a.body.Write(b[:min(len(b), 512-a.body.Len())])
	}
	return a.ResponseWriter.Write(b)
}

func (a *auditWriter) Flush() {
	if f, ok := a.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) record(user, action string, ok bool, result string) {
	s.auditLog.mu.Lock()
	defer s.auditLog.mu.Unlock()
	s.auditLog.entries = append(s.auditLog.entries, AuditEntry{Time: time.Now(), User: user, Action: action, OK: ok, Result: result})
	log.Printf("audit: user=%q action=%q ok=%v result=%q", user, action, ok, result)
	if n := len(s.auditLog.entries); n > auditKeep {
		s.auditLog.entries = s.auditLog.entries[n-auditKeep:]
	}
	s.auditLog.dirty = true
}

func (s *Server) audited(w http.ResponseWriter, r *http.Request, user string, h func(http.ResponseWriter)) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.URL.Path == "/api/heartbeat" {
		h(w)
		return
	}
	aw := &auditWriter{ResponseWriter: w, status: http.StatusOK}
	h(aw)
	body := strings.TrimSpace(aw.body.String())
	hx := w.Header().Get("HX-Refresh") != "" || w.Header().Get("HX-Redirect") != ""
	ok := aw.status < 400 && (hx || body == "" || strings.HasPrefix(body, "{") || aw.status >= 300 || strings.HasPrefix(body, "<"))
	result := ""
	if !ok {
		result = body
	}
	s.record(user, r.Method+" "+r.URL.Path, ok, result)
}

func (s *Server) auditLoop() {
	store := s.auth.Secrets()
	ctx := context.Background()
	if data, _, err := store.Get(ctx, auditKey); err == nil {
		var es []AuditEntry
		if json.Unmarshal(data["entries"], &es) == nil {
			s.auditLog.mu.Lock()
			s.auditLog.entries = append(es, s.auditLog.entries...)
			s.auditLog.mu.Unlock()
		}
	}
	for range time.Tick(10 * time.Second) {
		s.auditLog.mu.Lock()
		if !s.auditLog.dirty {
			s.auditLog.mu.Unlock()
			continue
		}
		b, err := json.Marshal(s.auditLog.entries)
		s.auditLog.dirty = false
		s.auditLog.mu.Unlock()
		if err != nil {
			continue
		}
		data := map[string][]byte{"entries": b}
		_, rv, err := store.Get(ctx, auditKey)
		switch {
		case apierrors.IsNotFound(err):
			err = store.Create(ctx, auditKey, "Opaque", data)
		case err == nil:
			err = store.Update(ctx, auditKey, rv, data)
		}
		if err != nil {
			log.Printf("audit: save: %v", err)
			s.auditLog.mu.Lock()
			s.auditLog.dirty = true
			s.auditLog.mu.Unlock()
		}
	}
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	s.auditLog.mu.Lock()
	var es []AuditEntry
	for i := len(s.auditLog.entries) - 1; i >= 0; i-- {
		e := s.auditLog.entries[i]
		if s.manager == nil || superAdmin(r) || strings.HasPrefix(e.User, session(r).Org+"/") {
			es = append(es, e)
		}
	}
	s.auditLog.mu.Unlock()
	if q := strings.ToLower(strings.TrimSpace(r.FormValue("q"))); q != "" {
		out := es[:0]
		for _, e := range es {
			if strings.Contains(strings.ToLower(e.User+" "+e.Action+" "+e.Result), q) {
				out = append(out, e)
			}
		}
		es = out
	}
	s.render(w, r, "audit.html", "layout", map[string]any{"Nav": "audit", "Entries": es[:min(len(es), 500)]})
}

func (s *Server) auditUser(org, user string) string {
	if s.manager == nil {
		return user
	}
	return org + "/" + user
}
