package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/tlscert"
	"argus/internal/ui"
	"argus/internal/wire"
)

func (s *Server) apiNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.kube.RunningVMs(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	mem, _ := memInfo()
	cpu, _ := s.kube.CPUInfo(r.Context())
	ui.WriteJSON(w, kube.NodeInfo{Name: s.NodeName, Version: s.Version, CAFingerprint: tlscert.Fingerprint(s.caPEM), RunningVMs: n, CPUCores: int64(runtime.NumCPU()), Arch: runtime.GOARCH, MemoryBytes: mem, BootTime: bootTime(), CPU: cpu})
}

func bootTime() time.Time {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Time{}
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return time.Time{}
	}
	up, _ := strconv.ParseFloat(f[0], 64)
	return time.Now().Add(-time.Duration(up * float64(time.Second))).Truncate(time.Second)
}

func (s *Server) apiVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, vms)
}

func (s *Server) apiNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.kube.ListNetworks(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, nets)
}

func (s *Server) apiDisks(w http.ResponseWriter, r *http.Request) {
	st, err := s.kube.DiskStatus(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, st)
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req wire.ExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BaseURL == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("baseURL required"))
		return
	}
	ex, err := s.kube.ExportVM(r.Context(), ns, name, req.BaseURL, string(s.caPEM))
	if err != nil {
		cctx, cancel := lifecycle.CleanupContext()
		defer cancel()
		ui.APIError(w, r, http.StatusConflict, errors.Join(err, s.kube.DeleteExport(cctx, ns, name)))
		return
	}
	log.Printf("%s: export %s/%s", ui.Session(r).User, ns, name)
	ui.WriteJSON(w, ex)
}

func (s *Server) apiUnexport(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.DeleteExport(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	var req wire.ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !ui.ValidObject.MatchString(req.Name) || req.Operation == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("invalid import request"))
		return
	}
	class, err := s.poolClass(r.Context(), req.Pool)
	if err == nil {
		err = s.kube.ImportVM(r.Context(), kube.Namespace, req.Name, req.Operation, req.Export, kube.ImportOptions{StorageClass: class, Template: req.Template})
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: import vm %s", ui.Session(r).User, req.Name)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiImported(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.FinishImport(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) caCert(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Write(s.caPEM)
}

func (s *Server) exportPassthrough(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("x-kubevirt-export-token")
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	src, ca, err := s.kube.ExportSource(r.Context(), r.PathValue("ns"), r.PathValue("export"), r.PathValue("volume"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp, err := openExport(r.Context(), src, ca, token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	log.Printf("export %s/%s %s: %d bytes", r.PathValue("ns"), r.PathValue("export"), r.PathValue("volume"), n)
}

func (s *Server) apiKubeVirtCA(w http.ResponseWriter, r *http.Request) {
	ca, err := s.kube.KubeVirtCA(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{"ca": ca})
}

func (s *Server) apiExternalCA(w http.ResponseWriter, r *http.Request) {
	var req wire.ExternalCARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.kube.SetExternalCA(r.Context(), req.Bundle); err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiMigrationNetwork(w http.ResponseWriter, r *http.Request) {
	var m kube.MigrationNetwork
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Subnet == "" || m.RangeStart == "" || m.RangeEnd == "" || m.VLAN < 0 || m.VLAN > 4094 {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("invalid migration network"))
		return
	}
	ip, err := s.kube.SetMigrationNetwork(r.Context(), m)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	log.Printf("%s: migration network vlan %d %s-%s, handler at %s", ui.Session(r).User, m.VLAN, m.RangeStart, m.RangeEnd, ip)
	ui.WriteJSON(w, map[string]string{"address": ip})
}

func (s *Server) apiRemoveMigrationNetwork(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.RemoveMigrationNetwork(r.Context()); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: removed migration network", ui.Session(r).User)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiVMSpec(w http.ResponseWriter, r *http.Request) {
	spec, err := s.kube.VMSpec(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSON(w, spec)
}

func (s *Server) apiReceive(w http.ResponseWriter, r *http.Request) {
	var req wire.ReceiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !ui.ValidObject.MatchString(req.Name) || req.MigrationID == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("invalid receive request"))
		return
	}
	addr, err := s.kube.Receive(r.Context(), kube.Namespace, req.Name, req.MigrationID, req.Spec)
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: receiving %s at %s", ui.Session(r).User, req.Name, addr)
	ui.WriteJSON(w, wire.ReceiveResponse{SyncAddress: addr})
}

func (s *Server) apiSend(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req wire.SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MigrationID == "" || req.ConnectURL == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("invalid send request"))
		return
	}
	if err := s.kube.Send(r.Context(), ns, name, req.MigrationID, req.ConnectURL); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: sending %s/%s to %s", ui.Session(r).User, ns, name, req.ConnectURL)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiMigrationPhase(w http.ResponseWriter, r *http.Request) {
	op := r.URL.Query().Get("operation")
	if op == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("operation required"))
		return
	}
	phase, err := s.kube.MigrationPhase(r.Context(), r.PathValue("ns"), op)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{"phase": phase})
}

func (s *Server) apiMigrationCleanup(w http.ResponseWriter, r *http.Request) {
	var req wire.OperationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("operation required"))
		return
	}
	if err := s.kube.CleanupMigration(r.Context(), r.PathValue("ns"), req.Operation); err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiAbort(w http.ResponseWriter, r *http.Request) {
	var req wire.OperationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation == "" {
		ui.APIError(w, r, http.StatusBadRequest, errors.New("operation required"))
		return
	}
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.kube.DeleteOwnedVM(r.Context(), ns, name, req.Operation); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: abort %s for %s/%s", ui.Session(r).User, req.Operation, ns, name)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) apiVM(w http.ResponseWriter, r *http.Request) {
	vm, err := s.kube.GetVM(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	ui.WriteJSON(w, vm)
}

func (s *Server) apiEvents(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if ns == "" {
		ns = kube.Namespace
	}
	evs, err := s.kube.Events(r.Context(), ns, r.PathValue("name"), 50)
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, evs)
}

func (s *Server) apiSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps, err := s.kube.ListSnapshots(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, snaps)
}

func (s *Server) apiMetrics(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.VMMetrics(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	m := kube.MetricsSample{Time: time.Now(), VMs: vms}
	m.CPUTotal, m.CPUIdle = cpuTimes()
	m.MemTotal, m.MemUsed = memInfo()
	if pools, err := s.kube.Pools(r.Context()); err == nil {
		for _, p := range pools {
			m.PoolUsed += p.Used
			m.PoolFree += p.Free
		}
	}
	ui.WriteJSON(w, m)
}

func cpuTimes() (total, idle float64) {
	b, _ := os.ReadFile("/proc/stat")
	line, _, _ := strings.Cut(string(b), "\n")
	for i, f := range strings.Fields(line)[1:] {
		v, _ := strconv.ParseFloat(f, 64)
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total / 100, idle / 100
}

func memInfo() (total, used int64) {
	b, _ := os.ReadFile("/proc/meminfo")
	var avail int64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	return total, total - avail
}

func (s *Server) apiSyncController(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.EnsureSyncController(r.Context()); err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
}
