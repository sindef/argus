package server

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"

	"github.com/gorilla/websocket"
)

type selection map[string]bool

func (sel selection) Has(ns, name string) bool { return sel[ns+"/"+name] }

var powerActions = [][3]string{
	{"start", "Power on", "play"},
	{"stop", "Power off", "power"},
	{"restart", "Restart", "rotate-cw"},
	{"pause", "Pause", "pause"},
	{"unpause", "Resume", "play"},
}

var drawerTabs = []string{"overview", "hardware", "snapshots", "logs"}

func filterVMs(vms []kube.VM, q string) []kube.VM {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return vms
	}
	out := vms[:0]
	for _, v := range vms {
		hay := strings.ToLower(v.Name + " " + v.DisplayName + " " + strings.Join(v.IPs(), " "))
		if strings.Contains(hay, q) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) vmData(r *http.Request) map[string]any {
	vms, err := s.kube.ListVMs(r.Context())
	sel := selection{}
	for _, v := range r.Form["sel"] {
		sel[v] = true
	}
	return map[string]any{
		"VMs":          filterVMs(vms, r.FormValue("q")),
		"Selected":     sel,
		"PowerActions": powerActions,
		"CanOperate":   session(r).Role.Can(auth.Operator),
		"Error":        errText(err),
	}
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	data := s.vmData(r)
	if ns, name := r.PathValue("ns"), r.PathValue("name"); name != "" {
		data["Open"] = kube.VM{Namespace: ns, Name: name}
	}
	s.render(w, r, "vms.html", "layout", data)
}

func (s *Server) vmTable(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.render(w, r, "vms.html", "table", s.vmData(r))
}

func (s *Server) navVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.kube.ListVMs(r.Context())
	s.render(w, r, "vms.html", "nav-vms", map[string]any{"VMs": vms})
}

func (s *Server) vmDrawer(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	tab := r.URL.Query().Get("tab")
	if !slices.Contains(drawerTabs, tab) {
		tab = "overview"
	}
	data := map[string]any{"VM": vm, "Tab": tab, "Tabs": drawerTabs, "CanOperate": session(r).Role.Can(auth.Operator)}
	switch tab {
	case "snapshots":
		data["Snapshots"], err = s.kube.ListSnapshots(r.Context(), vm.Namespace, vm.Name)
	case "logs":
		data["Events"], err = s.kube.Events(r.Context(), vm.Namespace, vm.Name, 50)
	}
	data["Error"] = errText(err)
	s.render(w, r, "vms.html", "drawer", data)
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	act, sel := r.PathValue("action"), r.Form["sel"]
	if len(sel) == 0 {
		flash(w, r, errors.New("select at least one VM"))
		return
	}
	single := act == "clone" || act == "rename"
	if single && len(sel) != 1 {
		flash(w, r, errors.New("select exactly one VM"))
		return
	}
	if !kube.ValidAction(act) && act != "snapshot" && act != "delete" && !single {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	prompt := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	var errs []string
	for _, v := range sel {
		ns, name, ok := strings.Cut(v, "/")
		if !ok {
			continue
		}
		var err error
		switch act {
		case "snapshot":
			_, err = s.kube.CreateSnapshot(r.Context(), ns, name)
		case "delete":
			err = s.kube.DeleteVM(r.Context(), ns, name)
		case "rename":
			err = s.kube.Rename(r.Context(), ns, name, prompt)
		case "clone":
			if !validObject.MatchString(prompt) {
				err = errors.New("invalid clone name")
			} else {
				err = s.kube.Clone(r.Context(), ns, name, prompt)
			}
		default:
			err = s.kube.Action(r.Context(), ns, name, act)
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		log.Printf("%s: %s %s/%s %s", session(r).User, act, ns, name, prompt)
	}
	if len(errs) > 0 {
		flash(w, r, errors.New(strings.Join(errs, "; ")))
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if !kube.ValidAction(act) {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err := s.kube.Action(r.Context(), ns, name, act); err != nil {
		flash(w, r, err)
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
	s.render(w, r, "console.html", "layout", map[string]any{"Namespace": r.PathValue("ns"), "Name": r.PathValue("name"), "Kind": kind, "Page": "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name"), "WS": "/ws/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/"})
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
	s.waitConsole(r, done)
}

func (s *Server) sessionValid(r *http.Request) bool {
	if tok := session(r).Token; tok != "" {
		_, ok := s.auth.Session(tok)
		return ok
	}
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		_, valid := s.auth.CheckToken(r.Context(), tok)
		return valid
	}
	return false
}

func (s *Server) waitConsole(r *http.Request, done <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if !s.sessionValid(r) {
				log.Printf("%s: console closed, session revoked", session(r).User)
				return
			}
		}
	}
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
	tmpl := "layout"
	if r.Header.Get("HX-Request") != "" {
		tmpl = "modal"
	}
	s.render(w, r, "vmnew.html", tmpl, map[string]any{"Images": imgs, "Networks": nets, "Error": errText(err)})
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
		flash(w, r, err)
		return
	}
	log.Printf("%s: create vm %s", session(r).User, spec.Name)
	redirect(w, r, "/vms/"+kube.Namespace+"/"+spec.Name)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.kube.DeleteVM(r.Context(), ns, name); err != nil {
		flash(w, r, err)
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
		flash(w, r, err)
		return
	}
	log.Printf("%s: add %dGi disk to %s/%s", session(r).User, size, ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) removeDisk(w http.ResponseWriter, r *http.Request) {
	ns, name, disk := r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")
	if err := s.kube.RemoveDisk(r.Context(), ns, name, disk); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: remove disk %s from %s/%s", session(r).User, disk, ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	snap, err := s.kube.CreateSnapshot(r.Context(), ns, name)
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: snapshot %s/%s as %s", session(r).User, ns, name, snap)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	ns, name, snap := r.PathValue("ns"), r.PathValue("name"), r.PathValue("snap")
	if err := s.kube.RestoreSnapshot(r.Context(), ns, name, snap); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: restore %s/%s from %s", session(r).User, ns, name, snap)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	ns, snap := r.PathValue("ns"), r.PathValue("snap")
	if err := s.kube.DeleteSnapshot(r.Context(), ns, snap); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: delete snapshot %s/%s", session(r).User, ns, snap)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	evs, err := s.kube.Events(r.Context(), "default", "", 200)
	s.render(w, r, "events.html", "layout", map[string]any{"Events": evs, "Error": errText(err), "Nav": "events"})
}
