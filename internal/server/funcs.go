package server

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"argus/internal/kube"

	"k8s.io/apimachinery/pkg/api/resource"
)

var funcs = template.FuncMap{
	"bytes": humanBytes,
	"size": func(q string) string {
		v, err := resource.ParseQuantity(q)
		if err != nil {
			return q
		}
		return humanBytes(v.Value())
	},
	"since": func(t time.Time) string {
		d := time.Since(t)
		switch {
		case t.IsZero():
			return ""
		case d < time.Minute:
			return fmt.Sprintf("%ds", int(d.Seconds()))
		case d < time.Hour:
			return fmt.Sprintf("%dm", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
		}
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	},
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"pct": func(a, b any) int {
		if num(b) <= 0 {
			return 0
		}
		return int(num(a) / num(b) * 100)
	},
	"limit":  func(evs []kube.Event, n int) []kube.Event { return evs[:min(n, len(evs))] },
	"min100": func(v int) int { return min(v, 100) },
	"add":    func(a, b int) int { return a + b },
	"sub":    func(a, b int) int { return a - b },
	"vmCtx": func(v vmRow, root map[string]any) map[string]any {
		return map[string]any{"V": v, "Page": fmt.Sprintf("/nodes/%s/vms/%s/%s", v.Host, v.Namespace, v.Name), "CanOperate": root["CanOperate"], "IsAdmin": root["IsAdmin"]}
	},
}

func num(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

func humanBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %s", int64(v), units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
