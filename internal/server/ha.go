package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/kube"
	"argus/internal/netguard"
)

const (
	heartbeatFresh = 60 * time.Second
	haInterval     = 10 * time.Second
)

type haState struct {
	mu       sync.Mutex
	last     time.Time
	owned    []string
	replicas map[string]string
}

func (s *Server) writeFence() {
	if s.runDir == "" {
		return
	}
	s.ha.mu.Lock()
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", s.ha.last.Unix())
	for vm, replica := range s.ha.replicas {
		fmt.Fprintf(&b, "%s %s\n", vm, replica)
	}
	s.ha.mu.Unlock()
	if err := os.WriteFile(filepath.Join(s.runDir, "ha-fence"), []byte(b.String()), 0o644); err != nil {
		log.Printf("ha: %v", err)
	}
}

func (s *Server) apiHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owned    []string          `json:"owned"`
		Replicas map[string]string `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	s.ha.mu.Lock()
	s.ha.last, s.ha.owned, s.ha.replicas = time.Now(), req.Owned, req.Replicas
	s.ha.mu.Unlock()
	s.writeFence()
	writeJSON(w, map[string]string{})
}

func (s *Server) apiProbe(w http.ResponseWriter, r *http.Request) {
	if err := netguard.CheckURL(r.Context(), r.URL.Query().Get("url")); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"reachable": reachable(r.URL.Query().Get("url"))})
}

func (s *Server) apiSetHA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.kube.SetHA(r.Context(), r.PathValue("ns"), r.PathValue("name"), req.Enabled); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: ha %v for %s/%s", session(r).User, req.Enabled, r.PathValue("ns"), r.PathValue("name"))
	writeJSON(w, map[string]string{})
}

func (s *Server) apiSnapshotExport(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct {
		BaseURL string `json:"baseURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BaseURL == "" {
		apiError(w, r, http.StatusBadRequest, errors.New("baseURL required"))
		return
	}
	if req.BaseURL == "self" {
		req.BaseURL = "https://" + os.Getenv("HOST_IP")
	}
	ex, err := s.kube.ExportVMSnapshot(r.Context(), ns, name, req.BaseURL, string(s.caPEM))
	if err != nil {
		cctx, cancel := cleanupCtx()
		defer cancel()
		apiError(w, r, http.StatusConflict, errors.Join(err, s.kube.DeleteExport(cctx, ns, name)))
		return
	}
	writeJSON(w, ex)
}

func reachable(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	c, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (s *Server) haLoop() {
	for range time.Tick(haInterval) {
		recovering("ha", s.haTick)
	}
}

func (s *Server) haTick() {
	s.ha.mu.Lock()
	last, owned, replicas := s.ha.last, s.ha.owned, s.ha.replicas
	s.ha.mu.Unlock()
	if last.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), haInterval)
	defer cancel()
	vms, err := s.kube.HAVMs(ctx)
	if err != nil {
		log.Printf("ha: %v", err)
		return
	}
	fresh := time.Since(last) < heartbeatFresh
	up := map[string]bool{}
	lost := func(url string) bool {
		if _, ok := up[url]; !ok {
			up[url] = reachable(url)
		}
		return !up[url]
	}
	for _, vm := range vms {
		replica := replicas[vm.Name]
		want := vm.Running
		switch {
		case fresh:
			want = slices.Contains(owned, vm.Name) && !vm.UserStopped
		case replica != "" && lost(replica):
			want = false
		}
		if want == vm.Running {
			continue
		}
		action := "stop"
		if want {
			action = "start"
		}
		if err := s.kube.Power(ctx, vm.Namespace, vm.Name, action); err != nil {
			log.Printf("ha: %s %s: %v", action, vm.Name, err)
			continue
		}
		log.Printf("ha: %s %s (fresh=%v)", action, vm.Name, fresh)
	}
}

type replicaReq struct {
	Export  kube.Export       `json:"export"`
	Mapping map[string]string `json:"mapping"`
}

func (s *Server) apiReplica(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req replicaReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	var out any = map[string]string{}
	var err error
	switch r.PathValue("op") {
	case "refresh":
		out, err = s.kube.RefreshReplicaDisks(r.Context(), ns, name, req.Export)
	case "status":
		var st []kube.DiskStatus
		for _, dv := range req.Mapping {
			phase, perr := s.kube.DataVolumePhase(r.Context(), ns, dv)
			if perr != nil {
				phase = "Failed"
			}
			st = append(st, kube.DiskStatus{Name: dv, Phase: phase})
		}
		out = st
	case "swap":
		err = s.kube.SwapReplica(r.Context(), ns, name, req.Export.Spec, req.Mapping)
	case "abort":
		s.kube.AbortReplicaRefresh(r.Context(), ns, name, req.Mapping)
	default:
		err = errors.New("unknown replica operation")
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, out)
}
