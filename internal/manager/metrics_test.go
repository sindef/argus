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
