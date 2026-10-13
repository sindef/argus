package hostui

import (
	"cmp"
	"log"
	"net/http"
	"strings"

	"argus/internal/ui"
)

func VMOwner(ns, name string) string { return "vm:" + ns + "/" + name }

func VolumeOwner(name string) string { return "volume:" + name }

func OwnerOf(r *http.Request) string {
	if v := r.PathValue("vol"); v != "" {
		return VolumeOwner(v)
	}
	return VMOwner(r.PathValue("ns"), r.PathValue("name"))
}

func filesBase(r *http.Request) string {
	base := ui.Base(r)
	if vol := r.PathValue("vol"); vol != "" {
		return base + "/volumes/" + vol + "/files"
	}
	return base + "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/files"
}

func (s *UI) filesPage(w http.ResponseWriter, r *http.Request) {
	q, owner := r.URL.Query(), OwnerOf(r)
	data := map[string]any{"Nav": "volumes", "Files": filesBase(r), "Name": cmp.Or(r.PathValue("vol"), r.PathValue("name")), "Part": q.Get("part"), "Path": strings.Trim(q.Get("path"), "/")}
	sess, err := s.Ops(r).FLR(r.Context(), owner, q.Get("id"))
	if r.Header.Get("HX-Request") != "" {
		if err != nil || sess.State != "Unpacking" {
			w.Header().Set("HX-Refresh", "true")
		}
		return
	}
	data["S"] = sess
	if err == nil && sess.State == "Ready" && q.Get("part") != "" {
		data["Entries"], err = s.Ops(r).FLRList(r.Context(), owner, sess.ID, q.Get("part"), q.Get("path"))
	}
	data["Error"] = ui.ErrText(err)
	s.Render(w, r, "files.html", "layout", data)
}

func (s *UI) filesStart(w http.ResponseWriter, r *http.Request) {
	var id string
	var err error
	if vol := r.PathValue("vol"); vol != "" {
		id, err = s.Ops(r).FLRVolume(r.Context(), vol)
	} else {
		id, err = s.Ops(r).FLRStart(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.FormValue("store"), r.FormValue("stamp"))
	}
	if err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: browse files of %s %s", ui.Session(r).User, OwnerOf(r), r.FormValue("stamp"))
	w.Header().Set("HX-Redirect", filesBase(r)+"?id="+id)
}

func (s *UI) filesDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Printf("%s: download %s from %s", ui.Session(r).User, q.Get("path"), OwnerOf(r))
	d, err := s.Ops(r).FLRFile(r.Context(), OwnerOf(r), q.Get("id"), q.Get("part"), q.Get("path"))
	ServeDownload(w, r, d, err)
}

func (s *UI) filesClose(w http.ResponseWriter, r *http.Request) {
	if err := s.Ops(r).FLRClose(r.Context(), OwnerOf(r), r.FormValue("id")); err != nil {
		ui.Flash(w, r, err)
		return
	}
	base := ui.Base(r)
	host, onManager := strings.CutPrefix(base, "/hosts/")
	switch {
	case r.PathValue("vol") != "" && onManager:
		w.Header().Set("HX-Redirect", "/storage#s-volumes")
	case r.PathValue("vol") != "":
		w.Header().Set("HX-Redirect", "/volumes")
	case onManager:
		w.Header().Set("HX-Redirect", "/nodes/"+host+"/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"?tab=backups")
	default:
		w.Header().Set("HX-Redirect", "/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"?tab=backups")
	}
}
