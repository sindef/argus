package hostd

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Counters struct {
	A uint64 `json:"a"`
	B uint64 `json:"b"`
}

type Stats struct {
	Time     int64               `json:"t"`
	CPUTotal uint64              `json:"cpuTotal"`
	CPUIdle  uint64              `json:"cpuIdle"`
	Cores    int                 `json:"cores"`
	MemTotal uint64              `json:"memTotal"`
	MemAvail uint64              `json:"memAvail"`
	Load     [3]float64          `json:"load"`
	Uptime   float64             `json:"uptime"`
	Disks    map[string]Counters `json:"disks"`
	Net      map[string]Counters `json:"net"`
	Pools    []Pool              `json:"pools"`
}

func fields(path string) [][]string {
	b, _ := os.ReadFile(path)
	var out [][]string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			out = append(out, f)
		}
	}
	return out
}

func u64(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func hostStats(ctx context.Context) Stats {
	st := Stats{Time: time.Now().UnixMilli(), Disks: map[string]Counters{}, Net: map[string]Counters{}}
	for _, f := range fields("/proc/stat") {
		switch {
		case f[0] == "cpu":
			for i, v := range f[1:] {
				st.CPUTotal += u64(v)
				if i == 3 || i == 4 {
					st.CPUIdle += u64(v)
				}
			}
		case strings.HasPrefix(f[0], "cpu"):
			st.Cores++
		}
	}
	for _, f := range fields("/proc/meminfo") {
		switch f[0] {
		case "MemTotal:":
			st.MemTotal = u64(f[1]) * 1024
		case "MemAvailable:":
			st.MemAvail = u64(f[1]) * 1024
		}
	}
	if l := fields("/proc/loadavg"); len(l) > 0 && len(l[0]) >= 3 {
		for i := range 3 {
			st.Load[i], _ = strconv.ParseFloat(l[0][i], 64)
		}
	}
	if u := fields("/proc/uptime"); len(u) > 0 {
		st.Uptime, _ = strconv.ParseFloat(u[0][0], 64)
	}
	for _, f := range fields("/proc/diskstats") {
		if len(f) < 10 || virtualDisk(f[2]) {
			continue
		}
		if _, err := os.Stat(filepath.Join("/sys/block", f[2], "device")); err != nil {
			continue
		}
		st.Disks[f[2]] = Counters{A: u64(f[5]) * 512, B: u64(f[9]) * 512}
	}
	for _, f := range fields("/proc/net/dev") {
		name := strings.TrimSuffix(f[0], ":")
		if len(f) < 10 || !physical(name) {
			continue
		}
		st.Net[name] = Counters{A: u64(f[1]), B: u64(f[9])}
	}
	if ps, err := listPoolSizes(ctx); err == nil {
		st.Pools = ps
	}
	return st
}

func listPoolSizes(ctx context.Context) ([]Pool, error) {
	out, err := run(ctx, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,health")
	if err != nil {
		return nil, err
	}
	var ps []Pool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		p := Pool{Name: f[0], Health: f[4]}
		p.Size, _ = strconv.ParseInt(f[1], 10, 64)
		p.Alloc, _ = strconv.ParseInt(f[2], 10, 64)
		p.Free, _ = strconv.ParseInt(f[3], 10, 64)
		ps = append(ps, p)
	}
	return ps, nil
}
