package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"argus/internal/auth"
)

type ctxKey struct{}

type ReqAuth struct {
	auth.Session
	Token string
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

type baseKey struct{}

func WithBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, baseKey{}, base)
}

func Base(r *http.Request) string {
	b, _ := r.Context().Value(baseKey{}).(string)
	return b
}

func (s *Core) Render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if IsAPI(r) {
		out := map[string]string{}
		if e := fmt.Sprint(data["Error"]); data["Error"] != nil && e != "" {
			out["error"] = e
		}
		if n := fmt.Sprint(data["Notice"]); data["Notice"] != nil && n != "" {
			out["notice"] = n
		}
		w.Header().Set("Content-Type", "application/json")
		if out["error"] != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	data["Base"] = Base(r)
	data["Session"] = Session(r)
	if _, ok := data["Can"]; !ok {
		data["Can"] = s.Can(r)
	}
	data["Alarms"] = s.ActiveAlarms()
	data["OrgAdmin"] = Session(r).Role.Can(auth.Admin) && len(Session(r).Scopes) == 0
	data["Query"] = r.FormValue("q")
	data["Version"] = s.Version
	data["NodeName"] = s.NodeName
	s.Mode.Decorate(r, tmpl, data)
	if _, ok := data["Nav"]; !ok {
		data["Nav"] = ""
	}
	if err := s.pages[page].ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func Session(r *http.Request) ReqAuth {
	a, _ := r.Context().Value(ctxKey{}).(ReqAuth)
	return a
}

func (s *Core) RequireOrg(h http.HandlerFunc) http.HandlerFunc {
	return s.Authenticate(func(w http.ResponseWriter, r *http.Request) {
		if !Session(r).Role.Can(auth.Admin) || len(Session(r).Scopes) > 0 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

var segmentParams = []string{"ns", "name", "op", "node", "id", "action", "disk", "snap", "kind", "file", "volume", "source", "export", "vm", "stamp", "org", "vol"}

func ValidSegments(r *http.Request) bool {
	for _, k := range segmentParams {
		if v := r.PathValue(k); v != "" && !safeSegment.MatchString(v) {
			return false
		}
	}
	return true
}

func (s *Core) Authenticate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ValidSegments(r) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			t, valid := s.Auth.CheckToken(r.Context(), tok)
			if !valid || !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if t.Scope == auth.MetricsScope && r.URL.Path != "/api/prometheus" || t.Scope == auth.ReadScope && r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			sess, who, err := s.Mode.TokenSession(r.Context(), t)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			actor := r.Header.Get(auth.ActorHeader)
			if actor != "" {
				who += " for " + actor
			}
			ctx := auth.WithActor(context.WithValue(r.Context(), ctxKey{}, ReqAuth{Session: sess}), cmp.Or(actor, sess.User))
			r2 := r.WithContext(ctx)
			s.audited(w, r2, who, func(w http.ResponseWriter) { h(w, r2) })
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := r.Cookie(s.Cookie)
		if err != nil {
			Redirect(w, r, "/login")
			return
		}
		sess, ok := s.Auth.Session(c.Value)
		if !ok {
			Redirect(w, r, "/login")
			return
		}
		if sess.MustChange && r.URL.Path != "/password" {
			Redirect(w, r, "/password")
			return
		}
		r2 := r.WithContext(auth.WithActor(context.WithValue(r.Context(), ctxKey{}, ReqAuth{Session: sess, Token: c.Value}), sess.User))
		s.audited(w, r2, s.AuditUser(sess.Org, sess.User), func(w http.ResponseWriter) { h(w, r2) })
	}
}

func Redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func Flash(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("%s: %s %s: %v", Session(r).User, r.Method, r.URL.Path, err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnprocessableEntity)
	w.Write([]byte(template.HTMLEscapeString(err.Error())))
}

func Failed(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
}

func ErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
