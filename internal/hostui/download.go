package hostui

import (
	"net/http"
	"strings"
	"time"

	"argus/internal/ui"
)

type downloadGrant struct {
	path    string
	expires time.Time
}

func (s *UI) downloadLink(w http.ResponseWriter, r *http.Request) {
	tok := ui.NewToken()
	path := strings.TrimSuffix(r.URL.Path, "/download")
	s.links.mu.Lock()
	if s.links.m == nil {
		s.links.m = map[string]downloadGrant{}
	}
	for k, g := range s.links.m {
		if time.Now().After(g.expires) {
			delete(s.links.m, k)
		}
	}
	s.links.m[tok] = downloadGrant{path: path, expires: time.Now().Add(5 * time.Minute)}
	s.links.mu.Unlock()
	base := ui.Base(r)
	w.Header().Set("HX-Redirect", base+"/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"/disks/"+r.PathValue("disk")+"/download?t="+tok+map[bool]string{true: "&format=vmdk"}[r.FormValue("format") == "vmdk"])
}

func (s *UI) downloadDisk(w http.ResponseWriter, r *http.Request) {
	ns, name, disk := r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")
	s.links.mu.Lock()
	g, ok := s.links.m[r.URL.Query().Get("t")]
	delete(s.links.m, r.URL.Query().Get("t"))
	s.links.mu.Unlock()
	if !ok || time.Now().After(g.expires) || g.path != strings.TrimSuffix(r.URL.Path, "/download") {
		http.Error(w, "this download link has expired; start the download again", http.StatusForbidden)
		return
	}
	s.Record(s.AuditUser(ui.Session(r).Org, ui.Session(r).User), "download disk "+disk+" of "+ns+"/"+name, true, "")
	d, err := s.Ops(r).DownloadDisk(r.Context(), ns, name, disk, r.URL.Query().Get("format"))
	ServeDownload(w, r, d, err)
}
