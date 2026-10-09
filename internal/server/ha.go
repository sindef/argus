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
	"strings"
	"sync"
	"time"
)

const (
	heartbeatFresh = 60 * time.Second
	haInterval     = 10 * time.Second
)

type haState struct {
	mu    sync.Mutex
	last  time.Time
	owned map[string]bool
	peers []string
	vms   []string
}

func (s *Server) writeFence() {
	if s.runDir == "" {
		return
	}
	s.ha.mu.Lock()
	text := fmt.Sprintf("%d\n%s\n%s\n", s.ha.last.Unix(), strings.Join(s.ha.peers, " "), strings.Join(s.ha.vms, " "))
	s.ha.mu.Unlock()
	if err := os.WriteFile(filepath.Join(s.runDir, "ha-fence"), []byte(text), 0o644); err != nil {
		log.Printf("ha: %v", err)
	}
}

func (s *Server) apiHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Owned []string `json:"owned"`
		Peers []string `json:"peers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	owned := map[string]bool{}
	for _, n := range req.Owned {
		owned[n] = true
	}
	s.ha.mu.Lock()
	s.ha.last, s.ha.owned, s.ha.peers = time.Now(), owned, req.Peers
	s.ha.mu.Unlock()
	s.writeFence()
	writeJSON(w, map[string]string{})
}

func (s *Server) apiProbe(w http.ResponseWriter, r *http.Request) {
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
	ex, err := s.kube.ExportVMSnapshot(r.Context(), ns, name, req.BaseURL, string(s.caPEM))
	if err != nil {
		s.kube.DeleteExport(context.Background(), ns, name)
		apiError(w, r, http.StatusConflict, err)
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
		s.ha.mu.Lock()
		last, owned, peers := s.ha.last, s.ha.owned, s.ha.peers
		s.ha.mu.Unlock()
		if last.IsZero() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), haInterval)
		vms, err := s.kube.HAVMs(ctx)
		if err != nil {
			cancel()
			log.Printf("ha: %v", err)
			continue
		}
		names := make([]string, 0, len(vms))
		for _, vm := range vms {
			names = append(names, vm.Name)
		}
		s.ha.mu.Lock()
		s.ha.vms = names
		s.ha.mu.Unlock()
		s.writeFence()
		fresh := time.Since(last) < heartbeatFresh
		isolated := false
		if !fresh {
			isolated = true
			for _, p := range peers {
				if reachable(p) {
					isolated = false
					break
				}
			}
		}
		for _, vm := range vms {
			want := vm.Running
			switch {
			case fresh:
				want = owned[vm.Name] && !vm.UserStopped
			case isolated:
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
			log.Printf("ha: %s %s (fresh=%v isolated=%v)", action, vm.Name, fresh, isolated)
		}
		cancel()
	}
}
