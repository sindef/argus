package manager

import (
	"testing"
	"time"
)

func TestRollupHourly(t *testing.T) {
	var fine, hourly []Point
	for m := int64(0); m <= 125; m += 5 {
		p := Point{T: 7200 + m*60, V: [6]float32{float32(m / 60)}}
		hourly = rollup(fine, hourly, p, time.Hour*48)
		fine = appendPoint(fine, p, 7*24*time.Hour)
	}
	if len(hourly) != 2 || hourly[0].T != 7200 || hourly[0].V[0] != 0 || hourly[1].T != 10800 || hourly[1].V[0] != 1 {
		t.Fatalf("hourly %+v", hourly)
	}
}

func TestSeriesResolution(t *testing.T) {
	now := time.Now().Unix()
	m := &Manager{tel: &telemetry{
		VMs:         map[string][]Point{"n/default/vm": {{T: now - 120}, {T: now - 60}}},
		VMsHourly:   map[string][]Point{"n/default/vm": {{T: now - 3600}}},
		Hosts:       map[string][]Point{"n": {{T: now - 600, V: [6]float32{0, 0, 0, 1}}, {T: now - 300, V: [6]float32{0, 0, 0, 1}}}},
		HostsHourly: map[string][]Point{"n": {{T: now - 7200, V: [6]float32{0, 0, 0, 1}}}},
	}}
	if got := len(m.VMSeries("n", "default", "vm", 24*time.Hour)); got != 2 {
		t.Fatalf("24h VM series used %d points, want minute data", got)
	}
	if got := len(m.VMSeries("n", "default", "vm", 7*24*time.Hour)); got != 1 {
		t.Fatalf("7d VM series used %d points, want hourly data", got)
	}
	if got := len(m.HostSeries([]string{"n"}, 7*24*time.Hour)); got != 2 {
		t.Fatalf("7d host series used %d points, want 5-minute data", got)
	}
	if got := len(m.HostSeries([]string{"n"}, 30*24*time.Hour)); got != 1 {
		t.Fatalf("30d host series used %d points, want hourly data", got)
	}
}
