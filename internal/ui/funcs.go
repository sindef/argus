package ui

import (
	"encoding/json"
	"fmt"
	"html/template"
	"slices"
	"sort"
	"strings"
	"time"

	"argus/internal/kube"

	"k8s.io/apimachinery/pkg/api/resource"
)

var funcs = template.FuncMap{
	"bytes": HumanBytes,
	"size": func(q string) string {
		v, err := resource.ParseQuantity(q)
		if err != nil {
			return q
		}
		return HumanBytes(v.Value())
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
	"picks":  func(disks any, field string) map[string]any { return map[string]any{"Disks": disks, "Field": field} },
	"min100": func(v int) int { return min(v, 100) },
	"list":   func(v ...string) []string { return v },
	"split":  strings.Split,
	"jsonStr": func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	},
	"attrText": func(m map[string]string) string {
		lines := make([]string, 0, len(m))
		for k, v := range m {
			lines = append(lines, k+"="+v)
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	},
	"rangeSeconds": func(r string) int64 { return int64(RangeDuration(r).Seconds()) },
	"join":         strings.Join,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"stamp": func(s string) string {
		t, err := time.Parse("20060102-150405", s)
		if err != nil {
			return s
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"has":     slices.Contains[[]string],
	"unacked": Unacked,
	"add":     func(a, b int) int { return a + b },
	"sub":     func(a, b int) int { return a - b },
	"add64":   func(a, b int64) int64 { return a + b },
	"vmCtx": func(v interface{ Page() string }, root map[string]any) map[string]any {
		can := root["Can"]
		if c, ok := v.(interface{ Capabilities() (Can, bool) }); ok {
			if own, set := c.Capabilities(); set {
				can = own
			}
		}
		return map[string]any{"V": v, "Page": v.Page(), "Can": can}
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
	case float32:
		return float64(n)
	}
	return 0
}

func HumanBytes(n int64) string {
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

func QuantityBytes(q string) int64 {
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return 0
	}
	return v.Value()
}

func RangeDuration(r string) time.Duration {
	switch r {
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	case "1y":
		return 365 * 24 * time.Hour
	}
	return 24 * time.Hour
}
