package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"argus/internal/kube"
)

func (s *Server) vmDatasets(r *http.Request) (map[string]kube.ZDisk, []string, error) {
	disks, err := s.kube.VMDatasets(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	if err != nil {
		return nil, nil, err
	}
	var ds []string
	for _, d := range disks {
		ds = append(ds, d.Dataset)
	}
	return disks, ds, nil
}

func (s *Server) apiZFSDisks(w http.ResponseWriter, r *http.Request) {
	disks, ds, err := s.vmDatasets(r)
	var snaps map[string][]string
	if err == nil {
		snaps, err = s.hostd.ZFSSnapshots(r.Context(), ds)
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	for v, d := range disks {
		d.Snapshots = snaps[d.Dataset]
		disks[v] = d
	}
	writeJSON(w, disks)
}

func (s *Server) apiZFSSnapshot(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var req struct{ Keep []string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	spec, err := s.kube.VMSpec(r.Context(), ns, name)
	var ds []string
	if err == nil {
		_, ds, err = s.vmDatasets(r)
	}
	if err == nil {
		err = s.hostd.ZFSPrune(r.Context(), ds, req.Keep)
	}
	snap := "argus-zrepl-" + strconv.FormatInt(time.Now().Unix(), 10)
	if err == nil {
		frozen := s.kube.Freeze(r.Context(), ns, name) == nil
		err = s.hostd.ZFSSnapshot(r.Context(), ds, snap)
		if frozen {
			s.kube.Unfreeze(r.Context(), ns, name)
		}
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]any{"snap": snap, "spec": spec})
}

func (s *Server) apiZFSPrune(w http.ResponseWriter, r *http.Request) {
	var req struct{ Keep []string }
	err := json.NewDecoder(r.Body).Decode(&req)
	var ds []string
	if err == nil {
		_, ds, err = s.vmDatasets(r)
	}
	if err == nil {
		err = s.hostd.ZFSPrune(r.Context(), ds, req.Keep)
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) zfsDataset(r *http.Request) (string, error) {
	disks, _, err := s.vmDatasets(r)
	if err != nil {
		return "", err
	}
	d, ok := disks[r.URL.Query().Get("volume")]
	if !ok {
		return "", errors.New("unknown volume")
	}
	return d.Dataset, nil
}

func (s *Server) apiZFSSend(w http.ResponseWriter, r *http.Request) {
	ds, err := s.zfsDataset(r)
	var resp *http.Response
	if err == nil {
		resp, err = s.hostd.ZFSSend(r.Context(), ds, r.URL.Query().Get("snap"), r.URL.Query().Get("from"))
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, resp.Body)
}

func (s *Server) apiZFSRecv(w http.ResponseWriter, r *http.Request) {
	err := s.kube.Stopped(r.Context(), r.PathValue("ns"), r.PathValue("name"))
	var ds string
	if err == nil {
		ds, err = s.zfsDataset(r)
	}
	if err == nil {
		err = s.hostd.ZFSRecv(r.Context(), ds, r.URL.Query().Get("full") == "1", r.Body)
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]string{})
}
