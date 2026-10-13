package managerserver

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/manager"
	"argus/internal/ui"
	"argus/internal/wire"
)

func (s *Server) jobTask(j manager.Job) wire.Task {
	kind := j.Kind
	if kind == "" {
		kind = "Migrate"
		if j.Live {
			kind = "Live migrate"
		}
	}
	host := j.From
	if _, ok := s.manager.NodeState(host); !ok {
		host = ""
	}
	if j.To != "" && host != "" {
		host += " → "
	}
	return wire.Task{ID: j.ID, Kind: kind, VM: j.VM, Host: host + j.To, User: j.User, Status: j.Status, Started: j.Started, Ended: j.Ended, Done: j.Done, Failed: j.Failed, Hold: j.Attention}
}

func (s *Server) Tasks(r *http.Request) []wire.Task {
	grants, err := s.Grants(r.Context(), ui.Session(r).Session)
	if err != nil {
		return nil
	}
	seeVM := func(host, vm string) bool {
		st, ok := s.manager.NodeState(host)
		return ok && auth.Allows(grants, auth.VMView, objectOf(st.Node, vm))
	}
	var out []wire.Task
	for _, j := range s.jobs(r) {
		if !j.Dismissed && (j.VM == "" || seeVM(cmp.Or(j.From, j.To), j.VM)) {
			out = append(out, s.jobTask(j))
		}
	}
	var nodes []manager.Node
	for _, st := range s.inventory(r) {
		nodes = append(nodes, st.Node)
	}
	for host, ts := range hostTasks(r.Context(), nodes) {
		for _, t := range ts {
			if strings.HasPrefix(t.User, "token:") || t.VM != "" && !seeVM(host, t.VM) {
				continue
			}
			t.Host, t.Node = host, host
			out = append(out, t)
		}
	}
	return out
}

func hostTasks(ctx context.Context, nodes []manager.Node) map[string][]wire.Task {
	out := map[string][]wire.Task{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var ts []wire.Task
			if n.Call(ctx, http.MethodGet, "/api/tasks", nil, &ts) == nil {
				mu.Lock()
				out[n.Name] = ts
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return out
}

func (s *Server) DismissTask(r *http.Request, host, id string) error {
	if !slices.ContainsFunc(s.Tasks(r), func(t wire.Task) bool { return t.ID == id && t.Node == host }) {
		return errors.New("task not found")
	}
	if host == "" {
		return s.manager.Dismiss(ui.Session(r).Org, id)
	}
	n, err := s.nodeIn(r, host)
	if err != nil {
		return err
	}
	return n.Call(r.Context(), http.MethodPost, manager.APIPath("/api/tasks/%s/dismiss", id), nil, nil)
}
