package ui

import (
	"log"
	"net/http"
	"slices"
	"time"

	"argus/internal/wire"
)

const recentTasks = 15 * time.Minute

func RecentTasks(ts []wire.Task) []wire.Task {
	ts = slices.DeleteFunc(ts, func(t wire.Task) bool {
		return t.Done && !t.Failed && !t.Hold && time.Since(t.Ended) > recentTasks
	})
	slices.SortStableFunc(ts, func(a, b wire.Task) int { return b.Started.Compare(a.Started) })
	return ts[:min(len(ts), 50)]
}

func (s *Core) tasksBar(w http.ResponseWriter, r *http.Request) {
	ts := RecentTasks(s.Mode.Tasks(r))
	running, failed := 0, 0
	for _, t := range ts {
		switch {
		case !t.Done:
			running++
		case t.Failed || t.Hold:
			failed++
		}
	}
	s.Render(w, r, "events.html", "tasks", map[string]any{"Tasks": ts, "Running": running, "Failed": failed})
}

func (s *Core) dismissTask(w http.ResponseWriter, r *http.Request) {
	if err := s.Mode.DismissTask(r, r.FormValue("host"), r.FormValue("id")); err != nil {
		Flash(w, r, err)
		return
	}
	log.Printf("%s: dismissed task %s", Session(r).User, r.FormValue("id"))
	s.tasksBar(w, r)
}
