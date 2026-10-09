package server

import (
	"html/template"
	"log"
	"net/http"

	"argus/internal/auth"
	"argus/internal/kube"

	"github.com/gorilla/websocket"
)

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	s.render(w, r, "vms.html", "layout", map[string]any{"VMs": vms, "Error": errText(err)})
}

func (s *Server) vmTable(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	s.render(w, r, "vms.html", "table", map[string]any{"VMs": vms, "Error": errText(err)})
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
	s.render(w, r, "vm.html", tmpl, map[string]any{"VM": vm, "CanOperate": session(r).Role.Can(auth.Operator)})
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
	s.render(w, r, "console.html", "layout", map[string]any{"Namespace": r.PathValue("ns"), "Name": r.PathValue("name"), "Kind": kind})
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
