package server

import (
	"testing"
	"time"
)

func TestScheduleDue(t *testing.T) {
	ts := parseSchedule("mon,fri 22:00 shutdown;daily 07:30 start;bad entry")
	if len(ts) != 2 || formatSchedule(ts) != "mon,fri 22:00 shutdown;daily 07:30 start" {
		t.Fatalf("parsed %+v", ts)
	}
	mon := time.Date(2026, 10, 12, 21, 59, 40, 0, time.UTC)
	if !ts[0].dueBetween(mon, mon.Add(90*time.Second)) {
		t.Error("Monday 22:00 skipped when a tick lands late")
	}
	if ts[0].dueBetween(mon.Add(time.Minute), mon.Add(2*time.Minute)) {
		t.Error("22:00 ran twice")
	}
	if ts[0].dueBetween(mon.AddDate(0, 0, 1), mon.AddDate(0, 0, 1).Add(time.Minute)) {
		t.Error("ran on Tuesday")
	}
	if !ts[1].dueBetween(time.Date(2026, 10, 18, 7, 29, 59, 0, time.UTC), time.Date(2026, 10, 18, 7, 30, 1, 0, time.UTC)) {
		t.Error("daily task skipped on Sunday")
	}
}
