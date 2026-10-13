package hostserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/netguard"
	"argus/internal/ui"
	"argus/internal/wire"
)

func (s *Server) storage(ctx context.Context) (wire.StorageInfo, error) {
	pools, err := s.kube.Pools(ctx)
	if err != nil {
		return wire.StorageInfo{}, err
	}
	vols, err := s.kube.Volumes(ctx)
	if err == nil && s.hostd != nil {
		var datasets []string
		for _, v := range vols {
			if v.Dataset != "" {
				datasets = append(datasets, v.Dataset)
			}
		}
		if zv, zerr := s.hostd.ZVols(ctx, datasets); zerr == nil {
			for i, v := range vols {
				z := zv[v.Dataset]
				vols[i].Allocated, vols[i].SnapUsed, vols[i].Thick = z.Allocated, z.SnapUsed, z.Thick
			}
		}
	}
	return wire.StorageInfo{Pools: pools, Volumes: vols}, err
}

func (s *Server) volumeOp(ctx context.Context, op string, q wire.VolumeRequest) error {
	switch op {
	case "create":
		if !ui.ValidObject.MatchString(q.Name) {
			return errors.New("invalid name")
		}
		if q.Size < 1 || q.Size > 4096 {
			return errors.New("size must be 1-4096 GiB")
		}
		if q.URL != "" {
			if err := netguard.CheckURL(ctx, q.URL); err != nil {
				return err
			}
		}
		class, err := s.poolClass(ctx, q.Pool)
		if err != nil {
			return err
		}
		return s.kube.CreateVolume(ctx, q.Name, q.Size, q.URL, class)
	case "delete":
		return s.kube.DeleteVolume(ctx, q.Name)
	case "resize":
		return s.kube.ResizeVolume(ctx, q.Name, q.Size)
	case "snapshot":
		return s.kube.SnapshotVolume(ctx, q.Name)
	case "attach":
		return s.kube.AttachVolume(ctx, kube.Namespace, q.VM, q.Name)
	case "delete-snapshot":
		return s.kube.DeleteVolumeSnapshot(ctx, q.Name)
	}
	return errors.New("unknown volume operation")
}

func (s *Server) volumesPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.storage(r.Context())
	vms, _ := s.kube.ListVMs(r.Context())
	rows := make([]hostui.HostVolume, len(st.Volumes))
	for i, v := range st.Volumes {
		rows[i] = hostui.HostVolume{Volume: v}
	}
	s.Render(w, r, "volumes.html", "layout", map[string]any{"Nav": "volumes", "Pools": st.Pools, "Volumes": rows, "Attachable": map[string][]string{"": hostui.StoppedVMs(vms)}, "PoolChoices": s.poolChoices(r.Context()), "Error": ui.ErrText(err),
		"PoolNames": hostui.VolumePools(rows), "PoolFilter": r.FormValue("pool")})
}

func (s *Server) volumeAction(w http.ResponseWriter, r *http.Request) {
	q := hostui.FormVolume(r)
	if err := s.volumeOp(r.Context(), r.PathValue("op"), q); err != nil {
		ui.Flash(w, r, err)
		return
	}
	log.Printf("%s: volume %s %s", ui.Session(r).User, r.PathValue("op"), q.Name)
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) apiStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.storage(r.Context())
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, st)
}

func (s *Server) apiVolumeOp(w http.ResponseWriter, r *http.Request) {
	var q wire.VolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
		ui.APIError(w, r, http.StatusBadRequest, err)
		return
	}
	if err := s.volumeOp(r.Context(), r.PathValue("op"), q); err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: volume %s %s", ui.Session(r).User, r.PathValue("op"), q.Name)
	ui.WriteJSON(w, map[string]string{})
}

func (s *Server) moveDisk(ctx context.Context, ns, name, disk, pool string) error {
	class, err := s.poolClass(ctx, pool)
	if err != nil {
		return err
	}
	m, err := s.kube.MoveDisk(ctx, ns, name, disk, class, pool)
	if err != nil {
		return err
	}
	go s.finishMove(m, s.tasks.begin(ctx, "Move disk "+disk+" to "+pool, name, ""))
	return nil
}

func (s *Server) finishMove(m kube.DiskMove, t *wire.Task) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	failed, err := kube.WaitDisks(ctx, 10*time.Second, s.kube.DataVolumes(m.Namespace, m.Moved), func(ds []kube.DiskStatus) { s.tasks.set(t, diskProgress(ds)) })
	if failed != "" {
		err = fmt.Errorf("copy failed, %s kept", m.Old)
	}
	fctx, fcancel := lifecycle.CleanupContext()
	err = errors.Join(err, s.kube.FinishDiskMove(fctx, m, failed == "" && err == nil))
	fcancel()
	log.Printf("move disk %s/%s to %s: %v", m.VM, m.Disk, m.Pool, err)
	s.tasks.end(t, err)
}

func (s *Server) resumeDiskMoves(ctx context.Context) {
	ms, err := s.kube.PendingDiskMoves(ctx)
	if err != nil {
		log.Printf("resume disk moves: %v", err)
	}
	for _, m := range ms {
		go s.finishMove(m, s.tasks.begin(context.Background(), "Move disk "+m.Disk+" to "+m.Pool, m.VM, ""))
	}
}

func poolNames(pools []kube.Pool) map[string]string {
	m := map[string]string{}
	for _, p := range pools {
		m[p.StorageClass] = p.Name
	}
	return m
}
