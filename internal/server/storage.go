package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"
	"net/http"
	"net/url"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
)

type volumeReq struct {
	Name string `json:"name"`
	Size int    `json:"size"`
	URL  string `json:"url"`
	VM   string `json:"vm"`
	Pool string `json:"pool"`
}

type storageInfo struct {
	Pools   []kube.Pool   `json:"pools"`
	Volumes []kube.Volume `json:"volumes"`
}

func (s *Server) storage(ctx context.Context) (storageInfo, error) {
	pools, err := s.kube.Pools(ctx)
	if err != nil {
		return storageInfo{}, err
	}
	vols, err := s.kube.Volumes(ctx)
	return storageInfo{Pools: pools, Volumes: vols}, err
}

func (s *Server) volumeOp(ctx context.Context, op string, q volumeReq) error {
	switch op {
	case "create":
		if !validObject.MatchString(q.Name) {
			return errors.New("invalid name")
		}
		if q.Size < 1 || q.Size > 4096 {
			return errors.New("size must be 1-4096 GiB")
		}
		if u, err := url.Parse(q.URL); q.URL != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https")) {
			return errors.New("URL must be http or https")
		}
		class, err := s.poolClass(ctx, q.Pool)
		if err != nil {
			return err
		}
		return s.kube.CreateVolume(ctx, q.Name, q.Size, q.URL, class)
	case "delete":
		return s.kube.DeleteVolume(ctx, q.Name)
	case "snapshot":
		return s.kube.SnapshotVolume(ctx, q.Name)
	case "attach":
		return s.kube.AttachVolume(ctx, kube.Namespace, q.VM, q.Name)
	case "delete-snapshot":
		return s.kube.DeleteVolumeSnapshot(ctx, q.Name)
	}
	return errors.New("unknown volume operation")
}

func formVolume(r *http.Request) volumeReq {
	var size int
	intField(r, "size", 1, 4096, &size)
	return volumeReq{Name: r.FormValue("name"), Size: size, URL: r.FormValue("url"), VM: r.FormValue("vm"), Pool: r.FormValue("pool")}
}

type hostVolume struct {
	kube.Volume
	Host string
}

func stoppedVMs(vms []kube.VM) []string {
	var out []string
	for _, v := range vms {
		if !v.Running {
			out = append(out, v.Name)
		}
	}
	return out
}

func (s *Server) volumesPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.storage(r.Context())
	vms, _ := s.kube.ListVMs(r.Context())
	rows := make([]hostVolume, len(st.Volumes))
	for i, v := range st.Volumes {
		rows[i] = hostVolume{Volume: v}
	}
	s.render(w, r, "volumes.html", "layout", map[string]any{"Nav": "volumes", "Pools": st.Pools, "Volumes": rows, "Attachable": map[string][]string{"": stoppedVMs(vms)}, "PoolChoices": s.poolChoices(r.Context()), "CanOperate": session(r).Role.Can(auth.Operator), "Error": errText(err)})
}

func (s *Server) volumeAction(w http.ResponseWriter, r *http.Request) {
	q := formVolume(r)
	if err := s.volumeOp(r.Context(), r.PathValue("op"), q); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: volume %s %s", session(r).User, r.PathValue("op"), q.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.storage(r.Context())
	if err != nil {
		apiError(w, r, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) apiVolumeOp(w http.ResponseWriter, r *http.Request) {
	var q volumeReq
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		apiError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.volumeOp(r.Context(), r.PathValue("op"), q); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: volume %s %s", session(r).User, r.PathValue("op"), q.Name)
	writeJSON(w, map[string]string{})
}

func (s *Server) apiRemoveDisk(w http.ResponseWriter, r *http.Request) {
	if err := s.kube.RemoveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: remove disk %s from %s/%s", session(r).User, r.PathValue("disk"), r.PathValue("ns"), r.PathValue("name"))
	writeJSON(w, map[string]string{})
}

func (s *Server) disksPage(w http.ResponseWriter, r *http.Request) {
	var ds []hostd.Disk
	err := errors.New("host helper not available")
	if s.hostd != nil {
		ds, err = s.hostd.Disks(r.Context())
	}
	s.render(w, r, "disks.html", "layout", map[string]any{"Nav": "disks", "Disks": ds, "Error": errText(err)})
}

func (s *Server) diskModal(w http.ResponseWriter, r *http.Request) {
	if s.hostd == nil {
		flash(w, r, errors.New("host helper not available"))
		return
	}
	dd, err := s.hostd.Disk(r.Context(), r.PathValue("name"))
	if err != nil {
		flash(w, r, err)
		return
	}
	s.render(w, r, "disks.html", "disk-modal", map[string]any{"D": dd})
}

func (s *Server) diskAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	confirm := r.FormValue("confirm")
	if confirm == "" {
		confirm = r.Header.Get("HX-Prompt")
	}
	if err := s.hostd.DiskAction(r.Context(), name, action, confirm, r.FormValue("test")); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: disk %s %s %s", session(r).User, name, action, r.FormValue("test"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "dashboard.html", "layout", map[string]any{"Nav": "dashboard"})
}

func (s *Server) dashboardStats(w http.ResponseWriter, r *http.Request) {
	if s.hostd == nil {
		http.Error(w, "host helper not available", http.StatusServiceUnavailable)
		return
	}
	st, err := s.hostd.Stats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	vms, _ := s.kube.ListVMs(r.Context())
	var running, cores int
	var mem int64
	for _, v := range vms {
		if v.Running {
			running++
			cores += int(v.Cores)
			mem += quantityBytes(v.Memory)
		}
	}
	writeJSON(w, map[string]any{"host": st, "vms": len(vms), "running": running, "vcpus": cores, "vmMem": mem})
}

func (s *Server) moveDisk(ctx context.Context, ns, name, disk, pool string) error {
	class, err := s.poolClass(ctx, pool)
	if err != nil {
		return err
	}
	old, moved, err := s.kube.MoveDisk(ctx, ns, name, disk, class)
	if err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		for ctx.Err() == nil {
			phase, err := s.kube.DataVolumePhase(ctx, ns, moved)
			switch {
			case err == nil && phase == "Succeeded":
				if err := s.kube.DeleteDataVolume(ctx, ns, old); err != nil {
					log.Printf("move disk %s/%s: delete %s: %v", name, disk, old, err)
				}
				log.Printf("move disk %s/%s to %s: done", name, disk, pool)
				return
			case err == nil && phase == "Failed":
				log.Printf("move disk %s/%s to %s: copy failed, %s kept", name, disk, pool, old)
				return
			}
			time.Sleep(10 * time.Second)
		}
	}()
	return nil
}

func poolNames(pools []kube.Pool) map[string]string {
	m := map[string]string{}
	for _, p := range pools {
		m[p.StorageClass] = p.Name
	}
	return m
}

func (s *Server) moveDiskForm(w http.ResponseWriter, r *http.Request) {
	if err := s.moveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.FormValue("disk"), r.FormValue("pool")); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: move disk %s of %s to %s", session(r).User, r.FormValue("disk"), r.PathValue("name"), r.FormValue("pool"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiMoveDisk(w http.ResponseWriter, r *http.Request) {
	var req struct{ Disk, Pool string }
	json.NewDecoder(r.Body).Decode(&req)
	if err := s.moveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), req.Disk, req.Pool); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]string{})
}
