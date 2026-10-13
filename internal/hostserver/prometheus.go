package hostserver

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

type promWriter struct {
	b     strings.Builder
	typed map[string]bool
}

func (p *promWriter) metric(name, typ, help string, value float64, labels ...string) {
	if p.typed == nil {
		p.typed = map[string]bool{}
	}
	if !p.typed[name] {
		p.typed[name] = true
		fmt.Fprintf(&p.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	var ls []string
	for i := 0; i+1 < len(labels); i += 2 {
		ls = append(ls, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	if len(ls) > 0 {
		fmt.Fprintf(&p.b, "%s{%s} %g\n", name, strings.Join(ls, ","), value)
	} else {
		fmt.Fprintf(&p.b, "%s %g\n", name, value)
	}
}

func (s *Server) apiPrometheus(w http.ResponseWriter, r *http.Request) {
	var p promWriter
	ctx := r.Context()
	if s.hostd != nil {
		if st, err := s.hostd.Stats(ctx); err == nil {
			p.metric("argus_host_cpu_seconds_total", "counter", "Host CPU time.", float64(st.CPUTotal-st.CPUIdle)/100, "mode", "busy")
			p.metric("argus_host_cpu_seconds_total", "counter", "Host CPU time.", float64(st.CPUIdle)/100, "mode", "idle")
			p.metric("argus_host_cpu_cores", "gauge", "Host CPU cores.", float64(st.Cores))
			p.metric("argus_host_memory_bytes", "gauge", "Host memory.", float64(st.MemTotal), "type", "total")
			p.metric("argus_host_memory_bytes", "gauge", "Host memory.", float64(st.MemAvail), "type", "available")
			p.metric("argus_host_load1", "gauge", "One minute load average.", st.Load[0])
			p.metric("argus_host_uptime_seconds", "gauge", "Host uptime.", st.Uptime)
			for _, d := range sortedKeys(st.Disks) {
				p.metric("argus_disk_read_bytes_total", "counter", "Bytes read from a physical disk.", float64(st.Disks[d].A), "disk", d)
				p.metric("argus_disk_written_bytes_total", "counter", "Bytes written to a physical disk.", float64(st.Disks[d].B), "disk", d)
			}
			for _, n := range sortedKeys(st.Net) {
				p.metric("argus_network_receive_bytes_total", "counter", "Bytes received on a physical port.", float64(st.Net[n].A), "interface", n)
				p.metric("argus_network_transmit_bytes_total", "counter", "Bytes sent on a physical port.", float64(st.Net[n].B), "interface", n)
			}
			for _, pl := range st.Pools {
				p.metric("argus_pool_bytes", "gauge", "Storage pool capacity.", float64(pl.Size), "pool", pl.Name, "type", "size")
				p.metric("argus_pool_bytes", "gauge", "Storage pool capacity.", float64(pl.Alloc), "pool", pl.Name, "type", "allocated")
				p.metric("argus_pool_healthy", "gauge", "1 when the pool is ONLINE.", b2f(pl.Health == "ONLINE"), "pool", pl.Name)
			}
		}
	}
	if vms, err := s.kube.ListVMs(ctx); err == nil {
		for _, v := range vms {
			p.metric("argus_vm_running", "gauge", "1 when the VM is running.", b2f(v.Running), "vm", v.Name, "namespace", v.Namespace)
			p.metric("argus_vm_cpu_cores", "gauge", "Configured vCPUs.", float64(v.Cores), "vm", v.Name, "namespace", v.Namespace)
			if q, err := resource.ParseQuantity(v.Memory); err == nil {
				p.metric("argus_vm_memory_bytes", "gauge", "Configured memory.", float64(q.Value()), "vm", v.Name, "namespace", v.Namespace)
			}
		}
	}
	counts := map[string]int{"warning": 0, "critical": 0}
	for _, a := range s.ActiveAlarms() {
		counts[a.Severity]++
	}
	for sev, n := range counts {
		p.metric("argus_alarms", "gauge", "Active alarms.", float64(n), "severity", sev)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(p.b.String()))
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
