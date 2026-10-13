package hostserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/websocket"

	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/ui"
	"argus/internal/wire"
)

func (l localOps) Power(ctx context.Context, ns, name, act string) error {
	if !kube.ValidAction(act) {
		return ui.ErrUnknownAction
	}
	return l.s.kube.Action(ctx, ns, name, act)
}

func (l localOps) DeleteVM(ctx context.Context, ns, name string) error {
	return l.s.kube.DeleteVM(ctx, ns, name)
}

func (l localOps) SnapshotOp(ctx context.Context, ns, name, snap, op string) error {
	switch {
	case snap == "":
		_, err := l.s.kube.CreateSnapshot(ctx, ns, name)
		return err
	case op == "restore":
		return l.s.kube.RestoreSnapshot(ctx, ns, name, snap)
	case op == "delete":
		return l.s.kube.DeleteSnapshot(ctx, ns, name, snap)
	}
	return errors.New("unknown snapshot operation")
}

func (l localOps) Meta(ctx context.Context, ns, name, op string, req wire.MetaRequest) error {
	switch op {
	case "clone":
		if !ui.ValidObject.MatchString(req.Value) {
			return errors.New("invalid clone name")
		}
		return l.s.kube.Clone(ctx, ns, name, req.Value, req.Template)
	case "rename":
		return l.s.kube.Rename(ctx, ns, name, req.Value)
	}
	return ui.ErrUnknownAction
}

func (l localOps) RemoveDisk(ctx context.Context, ns, name, disk string) error {
	return l.s.kube.RemoveDisk(ctx, ns, name, disk)
}

func (l localOps) MoveDisk(ctx context.Context, ns, name string, req wire.MoveDiskRequest) error {
	return l.s.moveDisk(ctx, ns, name, req.Disk, req.Pool)
}

func (l localOps) SetTags(ctx context.Context, ns, name string, req wire.TagsRequest) error {
	return l.s.organize(ctx, ns, name, req.Tags, req.Folder, req.Attributes)
}

func (l localOps) BackupOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return l.s.backupOp(ctx, ns, name, op, func(k string) string { return form[k] })
}

func (l localOps) BackupCatalog(ctx context.Context) (hostui.BackupCatalog, error) {
	return l.s.backupCatalog(ctx), nil
}

func (l localOps) ScheduleOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return l.s.scheduleOp(ctx, ns, name, op, func(k string) string { return form[k] })
}

func (l localOps) DialConsole(ctx context.Context, ns, name, kind string) (*websocket.Conn, error) {
	return l.s.kube.DialConsole(ctx, ns, name, kind)
}

func (s *Server) apiVMOp(w http.ResponseWriter, r *http.Request, in any, fn func(ctx context.Context, l localOps, ns, name string) error) {
	s.APIOp(w, r, in, func() (any, error) {
		return nil, fn(r.Context(), localOps{s}, r.PathValue("ns"), r.PathValue("name"))
	})
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	s.apiVMOp(w, r, nil, func(ctx context.Context, l localOps, ns, name string) error {
		return l.Power(ctx, ns, name, r.PathValue("action"))
	})
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	s.apiVMOp(w, r, nil, func(ctx context.Context, l localOps, ns, name string) error { return l.DeleteVM(ctx, ns, name) })
}

func (s *Server) apiSnapshotOp(w http.ResponseWriter, r *http.Request) {
	s.apiVMOp(w, r, nil, func(ctx context.Context, l localOps, ns, name string) error {
		return l.SnapshotOp(ctx, ns, name, r.PathValue("snap"), r.PathValue("op"))
	})
}

func (s *Server) apiRenameClone(w http.ResponseWriter, r *http.Request) {
	var req wire.MetaRequest
	s.apiVMOp(w, r, &req, func(ctx context.Context, l localOps, ns, name string) error {
		return l.Meta(ctx, ns, name, r.PathValue("op"), req)
	})
}

func (s *Server) apiRemoveDisk(w http.ResponseWriter, r *http.Request) {
	s.apiVMOp(w, r, nil, func(ctx context.Context, l localOps, ns, name string) error {
		return l.RemoveDisk(ctx, ns, name, r.PathValue("disk"))
	})
}

func (s *Server) apiMoveDisk(w http.ResponseWriter, r *http.Request) {
	var req wire.MoveDiskRequest
	s.apiVMOp(w, r, &req, func(ctx context.Context, l localOps, ns, name string) error { return l.MoveDisk(ctx, ns, name, req) })
}

func (s *Server) apiSetTags(w http.ResponseWriter, r *http.Request) {
	var req wire.TagsRequest
	s.apiVMOp(w, r, &req, func(ctx context.Context, l localOps, ns, name string) error { return l.SetTags(ctx, ns, name, req) })
}

func (s *Server) apiVMBackupOp(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	s.apiVMOp(w, r, &req, func(ctx context.Context, l localOps, ns, name string) error {
		return l.BackupOp(ctx, ns, name, r.PathValue("op"), req)
	})
}

func (s *Server) apiScheduleOp(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	s.apiVMOp(w, r, &req, func(ctx context.Context, l localOps, ns, name string) error {
		return l.ScheduleOp(ctx, ns, name, r.PathValue("op"), req)
	})
}
