package hostserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"argus/internal/kube"
	"argus/internal/ui"
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

var drawerTabs = []string{"overview", "hardware", "snapshots", "backups", "logs"}

func filterVMs(vms []kube.VM, q string) []kube.VM {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return vms
	}
	out := vms[:0]
	for _, v := range vms {
		hay := strings.ToLower(v.Name + " " + v.DisplayName + " " + strings.Join(v.IPs(), " ") + " " + strings.Join(v.Tags, " ") + " " + v.Folder)
		for k, val := range v.Attributes {
			hay += " " + strings.ToLower(k+"="+val)
		}
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
	running, cores := 0, int64(0)
	for _, v := range vms {
		if v.Running {
			running++
			cores += v.Cores
		}
	}
	return map[string]any{
		"Total":        len(vms),
		"Running":      running,
		"Cores":        cores,
		"VMs":          filterVMs(vms, r.FormValue("q")),
		"Selected":     sel,
		"PowerActions": powerActions,
		"Error":        ui.ErrText(err),
	}
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	data := s.vmData(r)
	data["Nav"] = "vms"
	if ns, name := r.PathValue("ns"), r.PathValue("name"); name != "" {
		data["Open"] = kube.VM{Namespace: ns, Name: name}
	}
	s.Render(w, r, "vms.html", "layout", data)
}

func (s *Server) vmTable(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.Render(w, r, "vms.html", "table", s.vmData(r))
}

func (s *Server) navVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.kube.ListVMs(r.Context())
	s.Render(w, r, "vms.html", "nav-vms", map[string]any{"VMs": vms})
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
	data := map[string]any{"VM": vm, "Tab": tab, "Tabs": drawerTabs}
	switch tab {
	case "snapshots":
		data["Snapshots"], err = s.kube.ListSnapshots(r.Context(), vm.Namespace, vm.Name)
	case "logs":
		data["Events"], err = s.kube.Events(r.Context(), vm.Namespace, vm.Name, 50)
	case "hardware":
		pools := s.poolChoices(r.Context())
		data["Move"] = map[string]any{"P": "/vms/" + vm.Namespace + "/" + vm.Name, "Pools": pools}
		data["PoolOf"] = poolNames(pools)
	case "backups":
		data["Backups"] = map[string]any{"B": s.vmBackupInfo(r.Context(), vm.Namespace, vm.Name), "P": "/vms/" + vm.Namespace + "/" + vm.Name, "H": "/vms/" + vm.Namespace + "/" + vm.Name, "Name": vm.Name, "Can": s.Can(r)}
	}
	data["Error"] = ui.ErrText(err)
	s.Render(w, r, "vms.html", "drawer", data)
}

func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	kind := "vnc"
	if r.URL.Query().Get("type") == "serial" {
		kind = "console"
	}
	s.Render(w, r, "console.html", "layout", map[string]any{"Namespace": r.PathValue("ns"), "Name": r.PathValue("name"), "Kind": kind, "Page": "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name"), "Media": "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/media", "WS": "/ws/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/"})
}

func (s *Server) addDisk(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var size int
	var err error
	if vol := r.FormValue("volume"); r.FormValue("source") == "volume" {
		err = s.kube.AttachVolume(r.Context(), ns, name, vol)
	} else if err = ui.IntField(r, "size", 1, 4096, &size); err == nil {
		var class string
		if class, err = s.poolClass(r.Context(), r.FormValue("pool")); err == nil {
			err = s.kube.AddDisk(r.Context(), ns, name, size, class)
		}
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: add disk (%s) to %s/%s", ui.Session(r).User, r.FormValue("source"), ns, name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) newDisk(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	vols, _ := s.kube.Volumes(r.Context())
	var free []kube.Volume
	for _, v := range vols {
		if v.Standalone() && v.UsedBy == "" {
			free = append(free, v)
		}
	}
	s.Render(w, r, "vms.html", "disk-modal", map[string]any{"VM": vm, "Volumes": free, "Pools": s.poolChoices(r.Context())})
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	evs, err := s.kube.Events(r.Context(), kube.Namespace, "", 200)
	s.Render(w, r, "events.html", "layout", map[string]any{"Events": evs, "Error": ui.ErrText(err), "Nav": "events"})
}

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$`)

func parseTags(v string) ([]string, error) {
	var tags []string
	for _, t := range strings.Split(v, ",") {
		if t = strings.TrimSpace(t); t == "" || slices.Contains(tags, t) {
			continue
		}
		if !validTag.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q", t)
		}
		tags = append(tags, t)
	}
	return tags, nil
}

var (
	validFolder = regexp.MustCompile(`^[^/\x00-\x1f]{1,64}(/[^/\x00-\x1f]{1,64}){0,7}$`)
	validAttr   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,62}$`)
)

func (s *Server) organize(ctx context.Context, ns, name, tagList, folder, attrs string) error {
	tags, err := parseTags(tagList)
	if err != nil {
		return err
	}
	folder = strings.Trim(strings.TrimSpace(folder), "/")
	if folder != "" && !validFolder.MatchString(folder) {
		return errors.New("folder must be up to 8 levels separated by /")
	}
	m := map[string]string{}
	for _, line := range strings.Split(attrs, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !validAttr.MatchString(k) || len(v) > 256 {
			return fmt.Errorf("attribute %q must be name=value", line)
		}
		m[k] = v
	}
	if len(m) > 32 {
		return errors.New("at most 32 attributes")
	}
	return s.kube.SetTags(ctx, ns, name, tags, folder, m)
}
