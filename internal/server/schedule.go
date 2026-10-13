package server

import (
	"context"
	"errors"
	"log"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"argus/internal/kube"
)

type ScheduledTask struct {
	Days   []string `json:"days"`
	At     string   `json:"at"`
	Action string   `json:"action"`
}

var (
	weekdays        = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}
	scheduleActions = []string{"start", "shutdown", "poweroff", "restart", "snapshot"}
	validAt         = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func parseSchedule(s string) []ScheduledTask {
	var out []ScheduledTask
	for _, entry := range strings.Split(s, ";") {
		f := strings.Fields(entry)
		if len(f) != 3 || !validAt.MatchString(f[1]) || !slices.Contains(scheduleActions, f[2]) {
			continue
		}
		t := ScheduledTask{At: f[1], Action: f[2]}
		if f[0] != "daily" {
			t.Days = strings.Split(f[0], ",")
		}
		out = append(out, t)
	}
	return out
}

func formatSchedule(ts []ScheduledTask) string {
	var parts []string
	for _, t := range ts {
		days := strings.Join(t.Days, ",")
		if days == "" {
			days = "daily"
		}
		parts = append(parts, days+" "+t.At+" "+t.Action)
	}
	return strings.Join(parts, ";")
}

func (t ScheduledTask) matches(tm time.Time) bool {
	return tm.Format("15:04") == t.At && (len(t.Days) == 0 || slices.Contains(t.Days, weekdays[tm.Weekday()]))
}

func (t ScheduledTask) dueBetween(from, to time.Time) bool {
	for m := from.Truncate(time.Minute).Add(time.Minute); !m.After(to); m = m.Add(time.Minute) {
		if t.matches(m) {
			return true
		}
	}
	return false
}

func (s *Server) scheduleOp(ctx context.Context, ns, name, op string, f func(string) string) error {
	vm, err := s.kube.GetVM(ctx, ns, name)
	if err != nil {
		return err
	}
	tasks := parseSchedule(vm.Schedule)
	switch op {
	case "add":
		t := ScheduledTask{At: f("at"), Action: f("action")}
		for _, d := range weekdays {
			if f("day_"+d) != "" {
				t.Days = append(t.Days, d)
			}
		}
		switch {
		case !validAt.MatchString(t.At):
			return errors.New("time must be HH:MM")
		case !slices.Contains(scheduleActions, t.Action):
			return errors.New("unknown action")
		case len(t.Days) == 0:
			return errors.New("pick at least one day")
		case len(t.Days) == len(weekdays):
			t.Days = nil
		}
		tasks = append(tasks, t)
	case "delete":
		i, err := strconv.Atoi(f("index"))
		if err != nil || i < 0 || i >= len(tasks) {
			return errors.New("task not found")
		}
		tasks = slices.Delete(tasks, i, i+1)
	default:
		return errors.New("unknown operation")
	}
	return s.kube.SetSchedule(ctx, ns, name, formatSchedule(tasks))
}

func (s *Server) runSchedules(ctx context.Context, vms []kube.VM, from, to time.Time) {
	for _, vm := range vms {
		for _, t := range parseSchedule(vm.Schedule) {
			if !t.dueBetween(from, to) {
				continue
			}
			var err error
			switch t.Action {
			case "snapshot":
				_, err = s.kube.CreateSnapshot(ctx, vm.Namespace, vm.Name)
			case "shutdown":
				err = s.kube.Action(ctx, vm.Namespace, vm.Name, "stop")
			default:
				err = s.kube.Action(ctx, vm.Namespace, vm.Name, t.Action)
			}
			log.Printf("scheduled %s of %s/%s: %v", t.Action, vm.Namespace, vm.Name, err)
			s.record("scheduler", "scheduled "+t.Action+" of "+vm.Name, err == nil, errText(err))
		}
	}
}
