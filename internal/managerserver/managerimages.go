package managerserver

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
)

type imageRow struct {
	Name  string
	Size  string
	Hosts map[string]kube.Image
}

func (s *Server) managerImages(w http.ResponseWriter, r *http.Request) {
	states := s.inventory(r)
	rows := map[string]*imageRow{}
	var hosts []string
	arch := map[string]string{}
	for _, st := range states {
		arch[st.Node.Name] = st.Arch()
		if !st.Reachable() {
			continue
		}
		hosts = append(hosts, st.Node.Name)
		for _, img := range st.Images {
			if rows[img.Name] == nil {
				rows[img.Name] = &imageRow{Name: img.Name, Size: img.Size, Hosts: map[string]kube.Image{}}
			}
			rows[img.Name].Hosts[st.Node.Name] = img
		}
	}
	errs := stateErrors(states)
	var out []*imageRow
	for _, row := range rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(hosts)
	auto := ""
	if i := slices.IndexFunc(states, manager.NodeState.Reachable); i >= 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		auto, _ = s.hostRegistry(ctx, states[i].Node, false)
		cancel()
	}
	sources, _, err := s.imageSources(r.Context(), ui.Session(r).Org)
	if err != nil {
		errs = strings.TrimSpace(errs + " " + err.Error())
	}
	s.Render(w, r, "manager-images.html", "layout", map[string]any{"Nav": "images", "Rows": out, "HostNames": hosts, "CanOperate": ui.Session(r).Role.Can(auth.Operator), "Error": errs,
		"Library": s.library(ui.Session(r).Org), "HostArch": arch, "Templates": s.libraryTemplates(ui.Session(r).Org), "HostTemplates": s.hostTemplates(r), "Sources": sources, "Address": s.registryAddress(r.Context()), "AutoAddress": auto, "Org": ui.Session(r).Org, "Super": superAdmin(r)})
}

func (s *Server) managerImageOp(w http.ResponseWriter, r *http.Request) {
	org := ui.Session(r).Org
	if r.PathValue("op") != "library-add" {
		r.ParseForm()
	}
	if op := r.PathValue("op"); strings.HasPrefix(op, "library-") || strings.HasPrefix(op, "source-") {
		if err := s.permit(r, auth.ImageConfig, auth.Object{}); err != nil {
			ui.Flash(w, r, err)
			return
		}
	}
	var err error
	switch r.PathValue("op") {
	case "copy":
		var src manager.Node
		src, err = s.nodeIn(r, r.FormValue("from"))
		var targets []manager.Node
		for _, h := range r.Form["hosts"] {
			if t, terr := s.nodeIn(r, h); terr == nil && h != src.Name {
				targets = append(targets, t)
			}
		}
		if err == nil && len(targets) == 0 {
			err = errors.New("pick at least one other host")
		}
		if err == nil {
			err = s.manager.CopyImage(r.Context(), org, r.FormValue("image"), src, targets)
		}
	case "import":
		err = s.importFromSource(r)
	case "library-add":
		err = s.libraryAdd(r)
	case "send":
		err = s.librarySend(r)
	case "library-delete":
		name, tag, _ := strings.Cut(r.FormValue("image"), ":")
		err = s.registry.Delete(org+"/"+name, tag)
	case "source-add", "source-delete":
		err = s.changeSources(r, r.PathValue("op") == "source-add")
	case "address":
		err = s.setRegistryAddress(r)
	case "delete":
		name := r.FormValue("image")
		var targets []manager.NodeState
		for _, st := range s.inventory(r) {
			if s.permit(r, auth.ImageConfig, objectOf(st.Node, "")) == nil {
				targets = append(targets, st)
			}
		}
		if e := fanOut(r.Context(), targets, func(ctx context.Context, n manager.Node) error {
			var imgs []kube.Image
			if err := n.Call(ctx, http.MethodGet, "/api/images", nil, &imgs); err != nil {
				return err
			}
			for _, i := range imgs {
				if i.Name == name {
					defer s.manager.Refresh(n.Name)
					return n.Call(ctx, http.MethodPost, manager.APIPath("/api/images/%s/delete", name), nil, nil)
				}
			}
			return nil
		}); e != "" {
			err = errors.New(e)
		}
	default:
		err = errors.New("unknown operation")
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: image %s %s%s", ui.Session(r).User, r.PathValue("op"), r.FormValue("image"), r.FormValue("name"))
	w.Header().Set("HX-Refresh", "true")
}
