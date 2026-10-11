package server

import (
	"strings"
	"testing"
)

func TestSanitizeRemoteHTML(t *testing.T) {
	in := `<div hx-get="/hosts/a/pools" hx-post="/hosts/b/pools/x/destroy" onclick="steal()" hx-on::load="steal()" hx-trigger="load[steal()]" hx-vals="js:{a:steal()}">ok</div>` +
		`<script>steal()</script><script src="/static/dashboard.js"></script><script src="https://evil/x.js"></script>` +
		`<a href="javascript:steal()">x</a><a href="/hosts/a/disks">d</a><img src="/static/logo.svg"><iframe src="/hosts/a"></iframe>` +
		`<button data-fill='{"action":"/users/admin/delete"}'>b</button>`
	out := string(sanitizeHTML([]byte(in), "/hosts/a"))
	for _, bad := range []string{"/hosts/b", "onclick", "hx-on", "steal()", "evil", "javascript:", "iframe", "/users/admin"} {
		if strings.Contains(out, bad) {
			t.Errorf("sanitized output still contains %q: %s", bad, out)
		}
	}
	for _, good := range []string{`hx-get="/hosts/a/pools"`, `src="/static/dashboard.js"`, `href="/hosts/a/disks"`, `src="/static/logo.svg"`} {
		if !strings.Contains(out, good) {
			t.Errorf("sanitized output lost %q: %s", good, out)
		}
	}
}
