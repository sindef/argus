package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

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
		flash(w, err)
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

func (s *Server) newVM(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.kube.ListImages(r.Context())
	nets, nerr := s.kube.ListNetworks(r.Context())
	if err == nil {
		err = nerr
	}
	s.render(w, r, "vmnew.html", "layout", map[string]any{"Images": imgs, "Networks": nets, "Error": errText(err)})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	spec := kube.VMSpec{
		Name:     r.FormValue("name"),
		Image:    r.FormValue("image"),
		CDROM:    r.FormValue("cdrom"),
		Network:  r.FormValue("network"),
		UserData: r.FormValue("userdata"),
		Start:    r.FormValue("start") != "",
	}
	err := intField(r, "cores", 1, 64, &spec.Cores)
	if err == nil {
		err = intField(r, "memory", 128, 262144, &spec.MemoryMiB)
	}
	if err == nil {
		err = intField(r, "disk", 1, 4096, &spec.DiskGiB)
	}
	if err == nil && !validObject.MatchString(spec.Name) {
		err = errors.New("invalid name")
	}
	if err == nil && spec.Network == "" {
		err = errors.New("network required")
	}
	if err == nil {
		err = s.kube.CreateVM(r.Context(), spec)
	}
	if err != nil {
		flash(w, err)
		return
	}
	log.Printf("%s: create vm %s", session(r).User, spec.Name)
	redirect(w, r, "/vms/"+kube.Namespace+"/"+spec.Name)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.kube.DeleteVM(r.Context(), ns, name); err != nil {
		flash(w, err)
		return
	}
	log.Printf("%s: delete vm %s/%s", session(r).User, ns, name)
	redirect(w, r, "/")
}

func intField(r *http.Request, field string, min, max int, dst *int) error {
	n, err := strconv.Atoi(r.FormValue(field))
	if err != nil || n < min || n > max {
		return fmt.Errorf("%s must be %d-%d", field, min, max)
	}
	*dst = n
	return nil
}

func (s *Server) addDisk(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var size int
	err := intField(r, "size", 1, 4096, &size)
	if err == nil {
		err = s.kube.AddDisk(r.Context(), ns, name, size)
	}
	if err != nil {
		flash(w, err)
		return
	}
	log.Printf("%s: add %dGi disk to %s/%s", session(r).User, size, ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) removeDisk(w http.ResponseWriter, r *http.Request) {
	ns, name, disk := r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")
	if err := s.kube.RemoveDisk(r.Context(), ns, name, disk); err != nil {
		flash(w, err)
		return
	}
	log.Printf("%s: remove disk %s from %s/%s", session(r).User, disk, ns, name)
	w.Header().Set("HX-Refresh", "true")
}
