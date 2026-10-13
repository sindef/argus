package managerserver

import (
	"context"
	"fmt"
	"net/http"
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
		s.ApplyAlarms(s.fleetAlarms(ctx))
	})
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
