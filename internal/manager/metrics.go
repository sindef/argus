package manager

import (
	"encoding/gob"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/kube"
)

const (
	vmWindow   = 24 * time.Hour
	vmStep     = time.Minute
	hostWindow = 7 * 24 * time.Hour
	hostStep   = 5 * time.Minute
	saveEvery  = 5 * time.Minute
	vmLong     = 30 * 24 * time.Hour
	hostLong   = 365 * 24 * time.Hour
)

type Point struct {
	T int64
	V [6]float32
}

type mark struct {
	T time.Time
	C kube.VMCounters
}

type telemetry struct {
	mu          sync.Mutex
	path        string
	saved       time.Time
	VMs         map[string][]Point
	Hosts       map[string][]Point
	VMsHourly   map[string][]Point
	HostsHourly map[string][]Point
	vmMark      map[string]mark
	hostMark    map[string]mark
}

func newTelemetry(path string) *telemetry {
	t := &telemetry{path: path, VMs: map[string][]Point{}, Hosts: map[string][]Point{}, vmMark: map[string]mark{}, hostMark: map[string]mark{}, saved: time.Now()}
	if f, err := os.Open(path); err == nil {
		if err := gob.NewDecoder(f).Decode(t); err != nil {
			log.Printf("telemetry: %v", err)
		}
		f.Close()
	}
	if t.VMsHourly == nil {
		t.VMsHourly = map[string][]Point{}
	}
	if t.HostsHourly == nil {
		t.HostsHourly = map[string][]Point{}
	}
	return t
}

func (t *telemetry) save(force bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path == "" || !force && time.Since(t.saved) < saveEvery {
		return
	}
	t.saved = time.Now()
	tmp := t.path + ".tmp"
	f, err := os.Create(tmp)
	if err == nil {
		err = gob.NewEncoder(f).Encode(t)
		f.Close()
	}
	if err == nil {
		err = os.Rename(tmp, t.path)
	}
	if err != nil {
		log.Printf("telemetry: save: %v", err)
	}
}

func rate(now, prev float64, dt float64) float32 {
	if now < prev || dt <= 0 {
		return 0
	}
	return float32((now - prev) / dt)
}

func appendPoint(ps []Point, p Point, window time.Duration) []Point {
	ps = append(ps, p)
	cut := p.T - int64(window/time.Second)
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= cut })
	return ps[i:]
}

func rollup(fine, hourly []Point, next Point, window time.Duration) []Point {
	if len(fine) == 0 || fine[len(fine)-1].T/3600 == next.T/3600 {
		return hourly
	}
	hour := fine[len(fine)-1].T / 3600 * 3600
	avg := Point{T: hour}
	n := float32(0)
	for i := len(fine) - 1; i >= 0 && fine[i].T >= hour; i-- {
		for j := range avg.V {
			avg.V[j] += fine[i].V[j]
		}
		n++
	}
	for j := range avg.V {
		avg.V[j] /= n
	}
	return appendPoint(hourly, avg, window)
}

func (t *telemetry) record(node string, cores int64, s kube.MetricsSample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	host := kube.VMCounters{s.CPUTotal, s.CPUIdle}
	if m, ok := t.hostMark[node]; !ok {
		t.hostMark[node] = mark{s.Time, host}
	} else if s.Time.Sub(m.T) >= hostStep {
		cpu := float32(0)
		if d := host[0] - m.C[0]; d > 0 {
			cpu = float32(100 * (1 - (host[1]-m.C[1])/d))
		}
		p := Point{T: s.Time.Unix(), V: [6]float32{cpu, float32(s.MemUsed), float32(s.MemTotal), float32(cores), float32(s.PoolUsed), float32(s.PoolFree)}}
		t.HostsHourly[node] = rollup(t.Hosts[node], t.HostsHourly[node], p, hostLong)
		t.Hosts[node] = appendPoint(t.Hosts[node], p, hostWindow)
		t.hostMark[node] = mark{s.Time, host}
	}
	for vm, c := range s.VMs {
		k := node + "/" + vm
		m, ok := t.vmMark[k]
		if !ok {
			t.vmMark[k] = mark{s.Time, c}
			continue
		}
		dt := s.Time.Sub(m.T).Seconds()
		if dt < vmStep.Seconds() {
			continue
		}
		p := Point{T: s.Time.Unix(), V: [6]float32{rate(c[0], m.C[0], dt), float32(c[1])}}
		for i := 2; i < 6; i++ {
			p.V[i] = rate(c[i], m.C[i], dt)
		}
		t.VMsHourly[k] = rollup(t.VMs[k], t.VMsHourly[k], p, vmLong)
		t.VMs[k] = appendPoint(t.VMs[k], p, vmWindow)
		t.vmMark[k] = mark{s.Time, c}
	}
	cut, longCut := s.Time.Add(-vmWindow).Unix(), s.Time.Add(-vmLong).Unix()
	for k, ps := range t.VMs {
		if _, ok := s.VMs[strings.TrimPrefix(k, node+"/")]; strings.HasPrefix(k, node+"/") && !ok {
			delete(t.vmMark, k)
			if len(ps) == 0 || ps[len(ps)-1].T < cut {
				delete(t.VMs, k)
			}
		}
	}
	for k, ps := range t.VMsHourly {
		if strings.HasPrefix(k, node+"/") && (len(ps) == 0 || ps[len(ps)-1].T < longCut) {
			delete(t.VMsHourly, k)
		}
	}
}

func (c *Manager) VMSeries(node, ns, vm string, span time.Duration) []Point {
	since := time.Now().Add(-span)
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	src := c.tel.VMs
	if span > vmWindow {
		src = c.tel.VMsHourly
	}
	ps := src[node+"/"+ns+"/"+vm]
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= since.Unix() })
	return append([]Point(nil), ps[i:]...)
}

func (c *Manager) HostSeries(nodes []string, span time.Duration) []Point {
	since := time.Now().Add(-span)
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	step, src := int64(hostStep/time.Second), c.tel.Hosts
	if span > hostWindow {
		step, src = 3600, c.tel.HostsHourly
	}
	buckets := map[int64]*Point{}
	for _, n := range nodes {
		for _, p := range src[n] {
			if p.T < since.Unix() {
				continue
			}
			b := buckets[p.T/step*step]
			if b == nil {
				b = &Point{T: p.T / step * step}
				buckets[b.T] = b
			}
			b.V[0] += p.V[0] * p.V[3]
			b.V[1] += p.V[1]
			b.V[2] += p.V[2]
			b.V[3] += p.V[3]
			b.V[4] += p.V[4]
			b.V[5] += p.V[5]
		}
	}
	out := make([]Point, 0, len(buckets))
	for _, b := range buckets {
		if b.V[3] > 0 {
			b.V[0] /= b.V[3]
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func (c *Manager) HostLatest(node string) (Point, bool) {
	c.tel.mu.Lock()
	defer c.tel.mu.Unlock()
	ps := c.tel.Hosts[node]
	if len(ps) == 0 {
		return Point{}, false
	}
	return ps[len(ps)-1], true
}
