package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"argus/internal/auth"
	"argus/internal/lifecycle"
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

func (s *Core) Record(user, action string, ok bool, result string) {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	s.audit.entries = append(s.audit.entries, AuditEntry{Time: time.Now(), User: user, Action: action, OK: ok, Result: result})
	log.Printf("audit: user=%q action=%q ok=%v result=%q", user, action, ok, result)
	if n := len(s.audit.entries); n > auditKeep {
		s.audit.entries = s.audit.entries[n-auditKeep:]
	}
	s.audit.dirty = true
}

func (s *Core) audited(w http.ResponseWriter, r *http.Request, user string, h func(http.ResponseWriter)) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.URL.Path == "/api/heartbeat" {
		h(w)
		return
	}
	aw := &auditWriter{ResponseWriter: w, status: http.StatusOK}
	h(aw)
	ok, result := auditResult(aw, w.Header())
	s.Record(user, r.Method+" "+r.URL.Path, ok, result)
}

func auditResult(aw *auditWriter, h http.Header) (bool, string) {
	if aw.status < 400 {
		return true, ""
	}
	body := strings.TrimSpace(aw.body.String())
	if body == "" || !utf8.ValidString(body) || strings.HasPrefix(body, "<") || h.Get("Content-Disposition") != "" {
		return false, fmt.Sprintf("HTTP %d", aw.status)
	}
	if r := []rune(body); len(r) > 200 {
		body = string(r[:200]) + "…"
	}
	return false, body
}

func (s *Core) auditLoop(ctx context.Context) {
	store := s.Auth.Secrets()
	var es []AuditEntry
	if _, err := auth.LoadJSON(ctx, store, auditKey, "entries", &es); err == nil {
		s.audit.mu.Lock()
		s.audit.entries = append(es, s.audit.entries...)
		s.audit.mu.Unlock()
	}
	lifecycle.Every(ctx, 10*time.Second, false, "audit", s.flushAudit)
	cctx, cancel := lifecycle.CleanupContext()
	defer cancel()
	s.flushAudit(cctx)
}

func (s *Core) flushAudit(ctx context.Context) {
	s.audit.mu.Lock()
	if !s.audit.dirty {
		s.audit.mu.Unlock()
		return
	}
	b, err := json.Marshal(s.audit.entries)
	s.audit.dirty = false
	s.audit.mu.Unlock()
	if err != nil {
		return
	}
	if err := auth.PutJSON(ctx, s.Auth.Secrets(), auditKey, "entries", json.RawMessage(b)); err != nil {
		log.Printf("audit: save: %v", err)
		s.audit.mu.Lock()
		s.audit.dirty = true
		s.audit.mu.Unlock()
	}
}

func (s *Core) auditPage(w http.ResponseWriter, r *http.Request) {
	s.audit.mu.Lock()
	var es []AuditEntry
	for i := len(s.audit.entries) - 1; i >= 0; i-- {
		e := s.audit.entries[i]
		if !s.MultiOrg || s.Mode.Super(r) || strings.HasPrefix(e.User, Session(r).Org+"/") {
			es = append(es, e)
		}
	}
	s.audit.mu.Unlock()
	if q := strings.ToLower(strings.TrimSpace(r.FormValue("q"))); q != "" {
		out := es[:0]
		for _, e := range es {
			if strings.Contains(strings.ToLower(e.User+" "+e.Action+" "+e.Result), q) {
				out = append(out, e)
			}
		}
		es = out
	}
	s.Render(w, r, "audit.html", "layout", map[string]any{"Nav": "audit", "Entries": es[:min(len(es), 500)]})
}

func (s *Core) AuditUser(org, user string) string {
	if !s.MultiOrg {
		return user
	}
	return org + "/" + user
}

func RecordStatus(w http.ResponseWriter, h func(http.ResponseWriter)) int {
	aw := &auditWriter{ResponseWriter: w, status: http.StatusOK}
	h(aw)
	return aw.status
}
