package managerserver

import (
	"net/http"
	"slices"
	"strings"

	"argus/internal/kube"
	"argus/internal/ui"
)

var v1Skip = []string{"/login", "/logout", "/sso/", "/password", "/account/", "/files"}

func v1FormPath(path string) string {
	path = strings.Replace(path, "/vms/import/", "/imports/", 1)
	for _, p := range []string{"/nodes/{node}/vms/{ns}/", "/hosts/{node}/vms/{ns}/"} {
		if rest, ok := strings.CutPrefix(path, p); ok {
			return "/api/v1/hosts/{node}/vms/" + rest
		}
	}
	return "/api/v1" + path
}

func v1FormRoutes(routes, have []ui.Route) []ui.Route {
	mux := http.NewServeMux()
	for _, rt := range have {
		mux.HandleFunc(rt.Pattern, rt.H)
	}
	var out []ui.Route
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		if method != http.MethodPost || slices.ContainsFunc(v1Skip, func(p string) bool { return strings.Contains(path, p) }) {
			continue
		}
		pattern := method + " " + v1FormPath(path)
		if !registers(mux, pattern) {
			continue
		}
		out = append(out, ui.Route{Pattern: pattern, Priv: rt.Priv, H: v1Form(rt.H, path == "/settings/backup")})
	}
	return out
}

func v1Form(h http.HandlerFunc, stream bool) http.HandlerFunc {
	wrapped := ui.FormAPI(h)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "" {
			r.SetPathValue("ns", kube.Namespace)
		}
		if !stream {
			wrapped(w, r)
			return
		}
		if r2, ok := ui.JSONForm(w, r); ok {
			h(w, r2)
		}
	}
}

func registers(mux *http.ServeMux, pattern string) (ok bool) {
	defer func() { ok = recover() == nil }()
	mux.HandleFunc(pattern, http.NotFound)
	return
}
