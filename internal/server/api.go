package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"argus/internal/kube"
	"argus/internal/tlscert"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, r *http.Request, status int, err error) {
	log.Printf("%s: %s %s: %v", session(r).User, r.Method, r.URL.Path, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

type NodeInfo struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	CAFingerprint string `json:"caFingerprint"`
	RunningVMs    int    `json:"runningVMs"`
}

func (s *Server) apiNode(w http.ResponseWriter, r *http.Request) {
	n, err := s.kube.RunningVMs(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, NodeInfo{Name: s.nodeName, Version: readEnv(s.releaseFile)["KAIROS_VERSION"], CAFingerprint: tlscert.Fingerprint(s.caPEM), RunningVMs: n})
}

func (s *Server) apiVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.kube.ListVMs(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, vms)
}

func (s *Server) apiNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := s.kube.ListNetworks(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, nets)
}

func (s *Server) apiDisks(w http.ResponseWriter, r *http.Request) {
	st, err := s.kube.DiskStatus(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if !kube.ValidAction(act) {
		apiError(w, r, http.StatusBadRequest, errors.New("unknown action"))
		return
	}
	if err := s.kube.Action(r.Context(), ns, name, act); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: %s %s/%s", session(r).User, act, ns, name)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.kube.DeleteVM(r.Context(), ns, name); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: delete vm %s/%s", session(r).User, ns, name)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct {
		BaseURL string `json:"baseURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BaseURL == "" {
		apiError(w, r, http.StatusBadRequest, errors.New("baseURL required"))
		return
	}
	ex, err := s.kube.ExportVM(r.Context(), ns, name, req.BaseURL, string(s.caPEM))
	if err != nil {
		s.kube.DeleteExport(context.Background(), ns, name)
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: export %s/%s", session(r).User, ns, name)
	writeJSON(w, ex)
}

func (s *Server) apiUnexport(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.DeleteExport(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) apiImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string      `json:"name"`
		Export kube.Export `json:"export"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validObject.MatchString(req.Name) {
		apiError(w, r, http.StatusBadRequest, errors.New("invalid import request"))
		return
	}
	if err := s.kube.ImportVM(r.Context(), kube.Namespace, req.Name, req.Export); err != nil {
		s.kube.FinishImport(context.Background(), kube.Namespace, req.Name)
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: import vm %s", session(r).User, req.Name)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiImported(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.FinishImport(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{})
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
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca))
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ResponseHeaderTimeout: time.Minute}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, src, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("x-kubevirt-export-token", token)
	resp, err := hc.Do(req)
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
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{"ca": ca})
}

func (s *Server) apiExternalCA(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bundle string `json:"bundle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.kube.SetExternalCA(r.Context(), req.Bundle); err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) apiMigrationNetwork(w http.ResponseWriter, r *http.Request) {
	var m kube.MigrationNetwork
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil || m.Subnet == "" || m.RangeStart == "" || m.RangeEnd == "" || m.VLAN < 0 || m.VLAN > 4094 {
		apiError(w, r, http.StatusBadRequest, errors.New("invalid migration network"))
		return
	}
	if err := s.kube.SetMigrationNetwork(r.Context(), m); err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	log.Printf("%s: migration network vlan %d %s-%s", session(r).User, m.VLAN, m.RangeStart, m.RangeEnd)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiVMSpec(w http.ResponseWriter, r *http.Request) {
	spec, err := s.kube.VMSpec(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, spec)
}

func (s *Server) apiReceive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string         `json:"name"`
		MigrationID string         `json:"migrationID"`
		Spec        map[string]any `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validObject.MatchString(req.Name) || req.MigrationID == "" {
		apiError(w, r, http.StatusBadRequest, errors.New("invalid receive request"))
		return
	}
	addr, err := s.kube.Receive(r.Context(), kube.Namespace, req.Name, req.MigrationID, req.Spec)
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: receiving %s at %s", session(r).User, req.Name, addr)
	writeJSON(w, map[string]string{"syncAddress": addr})
}

func (s *Server) apiSend(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct {
		MigrationID string `json:"migrationID"`
		ConnectURL  string `json:"connectURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.MigrationID == "" || req.ConnectURL == "" {
		apiError(w, r, http.StatusBadRequest, errors.New("invalid send request"))
		return
	}
	if err := s.kube.Send(r.Context(), ns, name, req.MigrationID, req.ConnectURL); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: sending %s/%s to %s", session(r).User, ns, name, req.ConnectURL)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiMigrationPhase(w http.ResponseWriter, r *http.Request) {
	phase, err := s.kube.MigrationPhase(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	writeJSON(w, map[string]string{"phase": phase})
}

func (s *Server) apiMigrationCleanup(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.CleanupMigration(r.Context(), r.PathValue("ns"), r.PathValue("name")); err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{})
}
