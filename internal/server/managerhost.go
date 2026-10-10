package server

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
)

var hostTabs = []struct{ Path, Label string }{{"dashboard", "Summary"}, {"pools", "Pools"}, {"disks", "Disks"}, {"network/host", "Network"}}

func (s *Server) managerHostUI(w http.ResponseWriter, r *http.Request) {
	n, err := s.orgNode(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	path := "/api/ui/" + r.PathValue("path")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	h := http.Header{}
	for _, k := range []string{"Content-Type", "HX-Request", "HX-Prompt", "HX-Target"} {
		if v := r.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set(BaseHeader, "/hosts/"+n.Name)
	resp, err := n.Proxy(r.Context(), r.Method, path, r.Body, h)
	if err != nil {
		flash(w, r, fmt.Errorf("%s: %w", n.Name, err))
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		flash(w, r, err)
		return
	}
	page := r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" && resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html")
	if !page {
		for k, v := range resp.Header {
			if strings.HasPrefix(k, "Hx-") || k == "Content-Type" {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
		return
	}
	s.render(w, r, "remote.html", "layout", map[string]any{"Nav": "host:" + n.Name, "Host": n.Name, "Tabs": hostTabs, "Tab": r.PathValue("path"), "Body": template.HTML(body)})
}
