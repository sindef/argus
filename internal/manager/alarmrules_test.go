package manager

import (
	"testing"
	"time"
)

func TestAlarmRuleSustained(t *testing.T) {
	now := time.Unix(10000, 0)
	pts := func(vs ...float32) []Point {
		var ps []Point
		for i, v := range vs {
			ps = append(ps, Point{T: now.Unix() - int64(len(vs)-1-i)*60, V: [6]float32{v}})
		}
		return ps
	}
	cpu := func(p Point) (float64, bool) { return float64(p.V[0]), true }
	r := AlarmRule{Warn: 80, Critical: 95, Minutes: 3}
	cases := []struct {
		ps   []Point
		want string
	}{
		{pts(10, 99, 99, 99, 99), "critical"},
		{pts(99, 99, 85, 99), "warning"},
		{pts(99, 99, 50, 99), ""},
		{pts(), ""},
	}
	for i, c := range cases {
		if got, _ := r.judge(window(c.ps, r.Minutes, now), cpu); got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
	if got := window(pts(99), 3, now.Add(20*time.Minute)); got != nil {
		t.Error("stale samples raised an alarm")
	}
	if got := window(pts(99, 99), 3, now); got != nil {
		t.Error("history shorter than the duration raised an alarm")
	}
	gap := pts(99, 99, 99, 99)
	gap[0].T -= 1200
	if got := window(gap[1:], 2, now); got == nil {
		t.Error("full history did not qualify")
	}
	if got := window(gap, 15, now); got != nil {
		t.Error("history with a sampling gap qualified")
	}
}
