package server

import (
	"context"
	"errors"
	"html/template"
	"log"
	"net/http"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/web"

	"github.com/gorilla/websocket"
)

const cookieName = "argus_session"

type Server struct {
	kube  *kube.Client
	auth  *auth.Store
	pages map[string]*template.Template
	mux   *http.ServeMux
}

type ctxKey struct{}

func New(k *kube.Client, a *auth.Store) (*Server, error) {
	s := &Server{kube: k, auth: a, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	for _, p := range []string{"login.html", "setup.html", "vms.html", "vm.html", "console.html"} {
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
	s.mux.HandleFunc("GET /setup", s.setupPage)
	s.mux.HandleFunc("POST /setup", s.setup)
	s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.vmTable))
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vm))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/{action}", s.require(auth.Operator, s.action))
	s.mux.HandleFunc("GET /vms/{ns}/{name}/console", s.require(auth.Operator, s.console))
	s.mux.HandleFunc("GET /ws/{ns}/{name}/{kind}", s.require(auth.Operator, s.consoleWS))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) render(w http.ResponseWriter, page, tmpl string, data map[string]any) {
	if err := s.pages[page].ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func session(r *http.Request) auth.Session {
	sess, _ := r.Context().Value(ctxKey{}).(auth.Session)
	return sess
}

func (s *Server) require(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sess auth.Session
		ok := false
		if c, err := r.Cookie(cookieName); err == nil {
			sess, ok = s.auth.Session(c.Value)
		}
		if !ok {
			redirect(w, r, "/login")
			return
		}
		if !sess.Role.Can(min) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if ok, err := s.auth.Configured(r.Context()); err == nil && !ok {
		redirect(w, r, "/setup")
		return
	}
	s.render(w, "login.html", "layout", nil)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	tok, err := s.auth.Login(r.Context(), r.FormValue("username"), r.FormValue("password"))
	if err != nil {
		log.Printf("login: %v", err)
	}
	if tok == "" {
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", "layout", map[string]any{"Error": "Invalid username or password"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.auth.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1})
	redirect(w, r, "/login")
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if ok, _ := s.auth.Configured(r.Context()); ok {
		redirect(w, r, "/login")
		return
	}
	s.render(w, "setup.html", "layout", nil)
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	name, pw := r.FormValue("username"), r.FormValue("password")
	fail := func(msg string) {
		w.WriteHeader(http.StatusBadRequest)
		s.render(w, "setup.html", "layout", map[string]any{"Error": msg})
	}
	switch {
	case name == "":
		fail("Username required")
		return
	case len(pw) < 12:
		fail("Password must be at least 12 characters")
		return
	case pw != r.FormValue("confirm"):
		fail("Passwords do not match")
		return
	}
	err := s.auth.CreateAdmin(r.Context(), name, pw)
	if errors.Is(err, auth.ErrExists) {
		redirect(w, r, "/login")
		return
	}
	if err != nil {
		log.Printf("setup: %v", err)
		fail("Could not save user")
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	s.render(w, "vms.html", "layout", map[string]any{"Session": session(r), "VMs": vms, "Error": errText(err)})
}

func (s *Server) vmTable(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	s.render(w, "vms.html", "table", map[string]any{"Session": session(r), "VMs": vms, "Error": errText(err)})
}

func (s *Server) vm(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "detail"
	}
	s.render(w, "vm.html", tmpl, map[string]any{"Session": session(r), "VM": vm, "CanOperate": session(r).Role.Can(auth.Operator)})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if !kube.ValidAction(act) {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err := s.kube.Action(r.Context(), ns, name, act); err != nil {
		log.Printf("%s: %v", session(r).User, err)
		w.Write([]byte(template.HTMLEscapeString(err.Error())))
		return
	}
	log.Printf("%s: %s %s/%s", session(r).User, act, ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	kind := "vnc"
	if r.URL.Query().Get("type") == "serial" {
		kind = "console"
	}
	s.render(w, "console.html", "layout", map[string]any{
		"Session": session(r), "Namespace": r.PathValue("ns"), "Name": r.PathValue("name"), "Kind": kind,
	})
}

func (s *Server) consoleWS(w http.ResponseWriter, r *http.Request) {
	ns, name, kind := r.PathValue("ns"), r.PathValue("name"), r.PathValue("kind")
	backend, err := s.kube.DialConsole(r.Context(), ns, name, kind)
	if err != nil {
		log.Printf("%s: %v", session(r).User, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer backend.Close()
	up := websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}
	client, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	log.Printf("%s: %s %s/%s", session(r).User, kind, ns, name)
	done := make(chan struct{}, 2)
	go pipe(client, backend, done)
	go pipe(backend, client, done)
	<-done
}

func pipe(dst, src *websocket.Conn, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		_, msg, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
