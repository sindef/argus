package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"argus/internal/lifecycle"
	"argus/internal/manager"
	"argus/internal/ui"
)

func (s *Server) alarmLoop(ctx context.Context) {
	lifecycle.Every(ctx, time.Minute, true, "alarms", func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if s.manager != nil {
			s.ApplyAlarms(s.fleetAlarms(ctx))
		} else {
			s.ApplyAlarms(s.nodeAlarms(ctx))
		}
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
	s.backups.mu.Lock()
	for vm, last := range s.backups.last {
		if strings.HasPrefix(last, "Failed") {
			add("backup:"+vm, "warning", "VM "+vm, "last "+strings.ToLower(last))
		}
	}
	s.backups.mu.Unlock()
	if vms, err := s.kube.ListVMs(ctx); err == nil {
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

func (s *Server) fleetAlarms(ctx context.Context) []ui.Alarm {
	var (
		mu  sync.Mutex
		out []ui.Alarm
	)
	nodes, _ := s.manager.Nodes(ctx)
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n manager.Node) {
			defer wg.Done()
			var as []ui.Alarm
			err := n.Call(ctx, http.MethodGet, "/api/alarms", nil, &as)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !n.Maintenance {
					out = append(out, ui.Alarm{Key: "host:" + n.Name, Severity: "critical", Host: n.Name, Object: "host", Message: "unreachable: " + err.Error()})
				}
				return
			}
			for _, a := range as {
				a.Key, a.Host = n.Name+":"+a.Key, n.Name
				out = append(out, a)
			}
		}(n)
	}
	wg.Wait()
	for _, h := range s.manager.EvaluateAlarmRules(ctx) {
		out = append(out, ui.Alarm{Key: h.Key, Severity: h.Severity, Host: h.Host, Object: h.Object, Message: h.Message})
	}
	auto := s.manager.HASettings(ctx).Automatic
	if ps, err := s.manager.Protected(ctx); err == nil {
		for _, p := range ps {
			if !auto && s.manager.Down(p.Owner) && !p.LastSync.IsZero() {
				out = append(out, ui.Alarm{Key: "ha-down:" + p.VM, Severity: "critical", Host: p.Owner, Object: "VM " + p.VM, Message: "host down; fail over manually to " + p.Replica + " once it is confirmed off"})
			}
			if !p.LastSync.IsZero() && time.Since(p.LastSync) > 3*time.Duration(p.Interval)*time.Minute {
				out = append(out, ui.Alarm{Key: "ha-stale:" + p.VM, Severity: "warning", Host: p.Owner, Object: "VM " + p.VM, Message: fmt.Sprintf("replica is %s old", time.Since(p.LastSync).Round(time.Minute))})
			}
			if p.LastError != "" {
				out = append(out, ui.Alarm{Key: "ha:" + p.VM, Severity: "warning", Host: p.Owner, Object: "VM " + p.VM, Message: "replication: " + p.LastError})
			}
		}
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
