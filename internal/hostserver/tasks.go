package hostserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/ui"
	"argus/internal/wire"
)

const keepTasks = 100

type taskList struct {
	mu   sync.Mutex
	list []*wire.Task
}

func (l *taskList) begin(ctx context.Context, kind, vm, object string) *wire.Task {
	t := &wire.Task{ID: ui.NewToken()[:16], Kind: kind, VM: vm, Object: object, User: auth.Actor(ctx), Status: "Running", Started: time.Now()}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, t)
	if len(l.list) > keepTasks {
		l.list = l.list[len(l.list)-keepTasks:]
	}
	return t
}

func (l *taskList) set(t *wire.Task, status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Status, t.Percent = status, wire.Percent(status)
}

func (l *taskList) end(t *wire.Task, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Done, t.Ended, t.Status = true, time.Now(), "Completed"
	if err != nil {
		t.Failed, t.Status = true, err.Error()
	}
}

func (l *taskList) all() []wire.Task {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]wire.Task, 0, len(l.list))
	for i := len(l.list) - 1; i >= 0; i-- {
		out = append(out, *l.list[i])
	}
	return out
}

func (l *taskList) dismiss(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.list)
	l.list = slices.DeleteFunc(l.list, func(t *wire.Task) bool { return t.ID == id && t.Done })
	if len(l.list) == n {
		return errors.New("no finished task with that ID")
	}
	return nil
}

func (s *Server) Tasks(*http.Request) []wire.Task { return s.tasks.all() }

func (s *Server) DismissTask(_ *http.Request, host, id string) error {
	if host != "" {
		return errors.New("no such host")
	}
	return s.tasks.dismiss(id)
}

func (s *Server) apiTasks(w http.ResponseWriter, r *http.Request) {
	ui.WriteJSON(w, s.tasks.all())
}

func (s *Server) apiDismissTask(w http.ResponseWriter, r *http.Request) {
	if err := s.tasks.dismiss(r.PathValue("id")); err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) watchDisks(ctx context.Context, t *wire.Task, list func(context.Context) ([]kube.DiskStatus, error)) error {
	failed, err := kube.WaitDisks(ctx, 10*time.Second, list, func(ds []kube.DiskStatus) { s.tasks.set(t, diskProgress(ds)) })
	if failed != "" {
		return fmt.Errorf("disk %s did not copy", failed)
	}
	return err
}

func diskProgress(ds []kube.DiskStatus) string {
	var parts []string
	for _, d := range ds {
		if d.Phase == "Succeeded" {
			continue
		}
		label := "Copying"
		if d.Phase == "" || d.Phase == "Pending" || strings.HasSuffix(d.Phase, "Scheduled") {
			label = "Waiting"
		}
		parts = append(parts, strings.TrimSpace(label+" "+strings.TrimSuffix(d.Progress, "N/A")))
	}
	if len(parts) == 0 {
		return "Running"
	}
	return strings.Join(parts, ", ")
}

func (s *Server) trackImage(ctx context.Context, kind, name string) {
	t := s.tasks.begin(ctx, kind, "", name)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		s.tasks.end(t, s.watchDisks(ctx, t, func(ctx context.Context) ([]kube.DiskStatus, error) {
			imgs, err := s.kube.ListImages(ctx)
			if err != nil {
				return nil, ctx.Err()
			}
			i := slices.IndexFunc(imgs, func(x kube.Image) bool { return x.Name == name })
			if i < 0 {
				return nil, errors.New("image was deleted")
			}
			return []kube.DiskStatus{{Name: name, Phase: imgs[i].Phase, Progress: imgs[i].Progress}}, nil
		}))
	}()
}

func (s *Server) trackCreate(ctx context.Context, name string) {
	t := s.tasks.begin(ctx, "Create VM", name, "")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		s.tasks.end(t, s.watchDisks(ctx, t, func(ctx context.Context) ([]kube.DiskStatus, error) {
			ds, err := s.kube.DiskStatus(ctx, kube.Namespace, name)
			if err != nil {
				return nil, ctx.Err()
			}
			for i, d := range ds {
				if d.Phase == "WaitForFirstConsumer" || d.Phase == "PendingPopulation" {
					ds[i].Phase = "Succeeded"
				}
			}
			return ds, nil
		}))
	}()
}

func (s *Server) record(ctx context.Context, kind, vm string, err error) error {
	s.tasks.end(s.tasks.begin(ctx, kind, vm, ""), err)
	return err
}

func (s *Server) waitState(t *wire.Task, ns, name, want string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		if vm, err := s.kube.GetVM(ctx, ns, name); err == nil && vm.Status == want {
			s.tasks.end(t, nil)
			return
		}
		select {
		case <-ctx.Done():
			s.tasks.end(t, fmt.Errorf("not %s after 5 minutes", strings.ToLower(want)))
			return
		case <-time.After(2 * time.Second):
		}
	}
}
