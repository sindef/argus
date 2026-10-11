package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"sync"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
)

type imageRow struct {
	Name  string
	Size  string
	Hosts map[string]kube.Image
}

func (s *Server) managerImages(w http.ResponseWriter, r *http.Request) {
	states := s.inventory(r)
	var mu sync.Mutex
	rows := map[string]*imageRow{}
	var hosts []string
	errs := fanOut(r.Context(), states, func(ctx context.Context, n manager.Node) error {
		var imgs []kube.Image
		if err := n.Call(ctx, http.MethodGet, "/api/images", nil, &imgs); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		hosts = append(hosts, n.Name)
		for _, img := range imgs {
			if rows[img.Name] == nil {
				rows[img.Name] = &imageRow{Name: img.Name, Size: img.Size, Hosts: map[string]kube.Image{}}
			}
			rows[img.Name].Hosts[n.Name] = img
		}
		return nil
	})
	var out []*imageRow
	for _, row := range rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	sort.Strings(hosts)
	s.render(w, r, "manager-images.html", "layout", map[string]any{"Nav": "images", "Rows": out, "HostNames": hosts, "CanOperate": session(r).Role.Can(auth.Operator), "Error": errs})
}

func (s *Server) managerImageOp(w http.ResponseWriter, r *http.Request) {
	org := session(r).Org
	r.ParseForm()
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
			err = s.manager.CopyImage(org, r.FormValue("image"), src, targets)
		}
	case "import":
		size, serr := gibibytes(r.FormValue("size"))
		name, url := r.FormValue("name"), r.FormValue("url")
		switch {
		case serr != nil:
			err = serr
		case !validObject.MatchString(name):
			err = errors.New("invalid name")
		case len(r.Form["hosts"]) == 0:
			err = errors.New("pick at least one host")
		}
		var members []manager.NodeState
		for _, st := range s.inventory(r) {
			for _, h := range r.Form["hosts"] {
				if st.Node.Name == h {
					members = append(members, st)
				}
			}
		}
		if err == nil {
			if e := fanOut(r.Context(), members, func(ctx context.Context, n manager.Node) error {
				return n.Call(ctx, http.MethodPost, "/api/images/import", imageImportReq{Name: name, Size: size, URL: url}, nil)
			}); e != "" {
				err = errors.New(e)
			}
		}
	case "delete":
		name := r.FormValue("image")
		if e := fanOut(r.Context(), s.inventory(r), func(ctx context.Context, n manager.Node) error {
			var imgs []kube.Image
			if err := n.Call(ctx, http.MethodGet, "/api/images", nil, &imgs); err != nil {
				return err
			}
			for _, i := range imgs {
				if i.Name == name {
					return n.Call(ctx, http.MethodPost, "/api/images/"+name+"/delete", nil, nil)
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
		flash(w, r, err)
		return
	}
	log.Printf("%s: image %s %s%s", session(r).User, r.PathValue("op"), r.FormValue("image"), r.FormValue("name"))
	w.Header().Set("HX-Refresh", "true")
}
