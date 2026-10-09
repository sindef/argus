package server

import (
	"context"
	"html/template"
	"log"
	"net/http"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/web"
)

const cookieName = "argus_session"

type Server struct {
	kube   *kube.Client
	auth   *auth.Store
	banner func(password string)
	pages  map[string]*template.Template
	mux    *http.ServeMux
}

type ctxKey struct{}

type reqAuth struct {
	auth.Session
	Token string
}

func New(k *kube.Client, a *auth.Store, banner func(string)) (*Server, error) {
	s := &Server{kube: k, auth: a, banner: banner, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	for _, p := range []string{"login.html", "password.html", "users.html", "vms.html", "vm.html", "console.html", "images.html", "networks.html", "vmnew.html"} {
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
	s.mux.HandleFunc("GET /password", s.require(auth.ReadOnly, s.passwordPage))
	s.mux.HandleFunc("POST /password", s.require(auth.ReadOnly, s.changePassword))
	s.mux.HandleFunc("GET /users", s.require(auth.Admin, s.users))
	s.mux.HandleFunc("POST /users", s.require(auth.Admin, s.addUser))
	s.mux.HandleFunc("POST /users/{name}/role", s.require(auth.Admin, s.setRole))
	s.mux.HandleFunc("POST /users/{name}/reset", s.require(auth.Admin, s.resetPassword))
	s.mux.HandleFunc("POST /users/{name}/delete", s.require(auth.Admin, s.deleteUser))
	s.mux.HandleFunc("GET /images", s.require(auth.ReadOnly, s.images))
	s.mux.HandleFunc("POST /images", s.require(auth.Operator, s.importImage))
	s.mux.HandleFunc("POST /images/upload", s.require(auth.Operator, s.uploadImage))
	s.mux.HandleFunc("POST /images/{name}/delete", s.require(auth.Operator, s.deleteImage))
	s.mux.HandleFunc("GET /networks", s.require(auth.ReadOnly, s.networks))
	s.mux.HandleFunc("POST /networks", s.require(auth.Admin, s.createNetwork))
	s.mux.HandleFunc("POST /networks/{name}/delete", s.require(auth.Admin, s.deleteNetwork))
	s.mux.HandleFunc("GET /{$}", s.require(auth.ReadOnly, s.vms))
	s.mux.HandleFunc("GET /vms", s.require(auth.ReadOnly, s.vmTable))
	s.mux.HandleFunc("GET /vms/new", s.require(auth.Operator, s.newVM))
	s.mux.HandleFunc("POST /vms", s.require(auth.Operator, s.createVM))
	s.mux.HandleFunc("GET /vms/{ns}/{name}", s.require(auth.ReadOnly, s.vm))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/delete", s.require(auth.Operator, s.deleteVM))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks", s.require(auth.Operator, s.addDisk))
	s.mux.HandleFunc("POST /vms/{ns}/{name}/disks/{disk}/delete", s.require(auth.Operator, s.removeDisk))
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

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, tmpl string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Session"] = session(r)
	if err := s.pages[page].ExecuteTemplate(w, tmpl, data); err != nil {
		log.Printf("render %s: %v", page, err)
	}
}

func session(r *http.Request) reqAuth {
	a, _ := r.Context().Value(ctxKey{}).(reqAuth)
	return a
}

func (s *Server) require(min auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
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
		if !sess.Role.Can(min) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, reqAuth{Session: sess, Token: c.Value})))
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", to)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func flash(w http.ResponseWriter, err error) {
	w.Write([]byte(template.HTMLEscapeString(err.Error())))
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
