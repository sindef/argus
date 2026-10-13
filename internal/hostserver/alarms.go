package hostserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"argus/internal/lifecycle"
	"argus/internal/ui"
)

func (s *Server) alarmLoop(ctx context.Context) {
	lifecycle.Every(ctx, time.Minute, true, "alarms", func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		s.ApplyAlarms(s.nodeAlarms(ctx))
	})
}

func (s *Server) nodeAlarms(ctx context.Context) []ui.Alarm {
	var out []ui.Alarm
	add := func(key, sev, object, msg string) {
		out = append(out, ui.Alarm{Key: key, Severity: sev, Object: object, Message: msg})
	}
	if s.hostd != nil {
		if ps, err := s.hostd.Pools(ctx); err == nil {
			for _, p := range ps.Pools {
				if p.Health != "ONLINE" {
					add("pool-health:"+p.Name, "critical", "pool "+p.Name, "health is "+p.Health)
				}
				if p.Size > 0 {
					switch pct := p.Alloc * 100 / p.Size; {
					case pct >= 90:
						add("pool-full:"+p.Name, "critical", "pool "+p.Name, fmt.Sprintf("%d%% used", pct))
					case pct >= 80:
						add("pool-full:"+p.Name, "warning", "pool "+p.Name, fmt.Sprintf("%d%% used", pct))
					}
				}
			}
		}
		if st, err := s.hostd.Stats(ctx); err == nil && st.MemTotal > 0 && st.MemAvail*10 < st.MemTotal {
			add("memory", "warning", "host", fmt.Sprintf("%d%% memory available", st.MemAvail*100/st.MemTotal))
		}
		out = append(out, s.smartAlarms(ctx)...)
	}
	vms, err := s.kube.ListVMs(ctx)
	if err == nil {
		exists := map[string]bool{}
		for _, v := range vms {
			exists[v.Namespace+"/"+v.Name] = true
		}
		s.backups.mu.Lock()
		for vm, last := range s.backups.last {
			switch {
			case !exists[vm]:
				delete(s.backups.last, vm)
			case strings.HasPrefix(last, "Failed"):
				add("backup:"+vm, "warning", "VM "+vm, "last "+strings.ToLower(last))
			}
		}
		s.backups.mu.Unlock()
		for _, v := range vms {
			switch v.Status {
			case "ErrorUnschedulable", "CrashLoopBackOff", "DataVolumeError", "ErrImagePull", "ErrorPvcNotFound":
				add("vm:"+v.Namespace+"/"+v.Name, "warning", "VM "+v.Name, v.Status)
			}
		}
	}
	return out
}

func (s *Server) smartAlarms(ctx context.Context) []ui.Alarm {
	s.smart.mu.Lock()
	if time.Since(s.smart.at) < time.Hour {
		cached := s.smart.found
		s.smart.mu.Unlock()
		out := make([]ui.Alarm, 0, len(cached))
		for _, a := range cached {
			out = append(out, a)
		}
		return out
	}
	s.smart.mu.Unlock()
	found := map[string]ui.Alarm{}
	if disks, err := s.hostd.Disks(ctx); err == nil {
		for _, d := range disks {
			dd, err := s.hostd.Disk(ctx, d.Name)
			if err == nil && dd.Smart.Supported && !dd.Smart.Passed {
				found[d.Name] = ui.Alarm{Key: "smart:" + d.Name, Severity: "critical", Object: "disk " + d.Name, Message: "SMART health check failed"}
			}
		}
	}
	s.smart.mu.Lock()
	s.smart.found, s.smart.at = found, time.Now()
	s.smart.mu.Unlock()
	out := make([]ui.Alarm, 0, len(found))
	for _, a := range found {
		out = append(out, a)
	}
	return out
}

func (s *Server) apiAlarms(w http.ResponseWriter, r *http.Request) {
	ui.WriteJSON(w, s.ActiveAlarms())
}

type smartCache struct {
	mu    sync.Mutex
	found map[string]ui.Alarm
	at    time.Time
}
