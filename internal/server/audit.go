package server

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
	ok, result := auditResult(aw, w.Header())
	s.record(user, r.Method+" "+r.URL.Path, ok, result)
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

func (s *Server) auditLoop() {
	store := s.auth.Secrets()
	ctx := context.Background()
	var es []AuditEntry
	if _, err := auth.LoadJSON(ctx, store, auditKey, "entries", &es); err == nil {
		s.auditLog.mu.Lock()
		s.auditLog.entries = append(es, s.auditLog.entries...)
		s.auditLog.mu.Unlock()
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
		if err := auth.PutJSON(ctx, store, auditKey, "entries", json.RawMessage(b)); err != nil {
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
