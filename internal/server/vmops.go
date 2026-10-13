package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/wire"
)

var errUnknownAction = errors.New("unknown action")

func (l localOps) Power(ctx context.Context, ns, name, act string) error {
	if !kube.ValidAction(act) {
		return errUnknownAction
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
		return l.s.kube.DeleteSnapshot(ctx, ns, snap)
	}
	return errors.New("unknown snapshot operation")
}

func (l localOps) Meta(ctx context.Context, ns, name, op string, req wire.MetaRequest) error {
	switch op {
	case "clone":
		if !validObject.MatchString(req.Value) {
			return errors.New("invalid clone name")
		}
		return l.s.kube.Clone(ctx, ns, name, req.Value, req.Template)
	case "rename":
		return l.s.kube.Rename(ctx, ns, name, req.Value)
	}
	return errUnknownAction
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

func (l localOps) ScheduleOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return l.s.scheduleOp(ctx, ns, name, op, func(k string) string { return form[k] })
}

func (o remoteOps) Power(ctx context.Context, ns, name, act string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/%s", ns, name, act), nil)
}

func (o remoteOps) DeleteVM(ctx context.Context, ns, name string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/delete", ns, name), nil)
}

func (o remoteOps) SnapshotOp(ctx context.Context, ns, name, snap, op string) error {
	if snap == "" {
		return o.post(ctx, manager.APIPath("/api/vms/%s/%s/snapshots", ns, name), nil)
	}
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/snapshots/%s/%s", ns, name, snap, op), nil)
}

func (o remoteOps) Meta(ctx context.Context, ns, name, op string, req wire.MetaRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/meta/%s", ns, name, op), req)
}

func (o remoteOps) RemoveDisk(ctx context.Context, ns, name, disk string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/disks/%s/delete", ns, name, disk), nil)
}

func (o remoteOps) MoveDisk(ctx context.Context, ns, name string, req wire.MoveDiskRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/disks/move", ns, name), req)
}

func (o remoteOps) SetTags(ctx context.Context, ns, name string, req wire.TagsRequest) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/tags", ns, name), req)
}

func (o remoteOps) BackupOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/backups/%s", ns, name, op), form)
}

func (o remoteOps) ScheduleOp(ctx context.Context, ns, name, op string, form map[string]string) error {
	return o.post(ctx, manager.APIPath("/api/vms/%s/%s/schedule/%s", ns, name, op), form)
}

func vmAction(ctx context.Context, o hostOps, ns, name, act, prompt string) error {
	switch {
	case kube.ValidAction(act):
		return o.Power(ctx, ns, name, act)
	case act == "delete":
		return o.DeleteVM(ctx, ns, name)
	case act == "snapshot":
		return o.SnapshotOp(ctx, ns, name, "", "")
	case act == "rename":
		return o.Meta(ctx, ns, name, act, wire.MetaRequest{Value: prompt})
	case act == "clone", act == "template-copy":
		return o.Meta(ctx, ns, name, "clone", wire.MetaRequest{Value: prompt, Template: act == "template-copy"})
	}
	return errUnknownAction
}

func (s *Server) onNode(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := s.orgNode(r)
		if err == nil && s.manager.Busy(n.Name, r.PathValue("name")) {
			err = errors.New(r.PathValue("name") + " has a migration in progress")
		}
		if err != nil {
			flash(w, r, err)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), opsKey{}, remoteOps{n})))
		go s.manager.Refresh(n.Name)
	}
}

func (s *Server) permits(r *http.Request, p auth.Privilege) bool {
	if o, ok := s.ops(r).(remoteOps); ok {
		return s.can(r, p, o.n, r.PathValue("name"))
	}
	return granted(r, p)
}

func formMap(r *http.Request) map[string]string {
	r.ParseForm()
	m := map[string]string{}
	for k := range r.Form {
		m[k] = r.FormValue(k)
	}
	return m
}

func (s *Server) vmDone(w http.ResponseWriter, r *http.Request, err error, what string) {
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: %s %s/%s", session(r).User, what, r.PathValue("ns"), r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	ns, name, act := r.PathValue("ns"), r.PathValue("name"), r.PathValue("action")
	if !s.permits(r, actionPrivilege(act)) {
		http.Error(w, "you do not have permission for this action", http.StatusForbidden)
		return
	}
	prompt := strings.TrimSpace(r.Header.Get("HX-Prompt"))
	err := vmAction(r.Context(), s.ops(r), ns, name, act, prompt)
	if err == nil && act == "delete" {
		log.Printf("%s: delete %s/%s", session(r).User, ns, name)
		redirect(w, r, "/")
		return
	}
	s.vmDone(w, r, err, act+" "+prompt)
}

func (s *Server) snapshotOp(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.ops(r).SnapshotOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("snap"), r.PathValue("op")), "snapshot "+r.PathValue("op")+" "+r.PathValue("snap"))
}

func (s *Server) removeDisk(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.ops(r).RemoveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")), "remove disk "+r.PathValue("disk"))
}

func (s *Server) moveDiskForm(w http.ResponseWriter, r *http.Request) {
	req := wire.MoveDiskRequest{Disk: r.FormValue("disk"), Pool: r.FormValue("pool")}
	s.vmDone(w, r, s.ops(r).MoveDisk(r.Context(), r.PathValue("ns"), r.PathValue("name"), req), "move disk "+req.Disk+" to "+req.Pool)
}

func (s *Server) setTags(w http.ResponseWriter, r *http.Request) {
	req := wire.TagsRequest{Tags: r.FormValue("tags"), Folder: r.FormValue("folder"), Attributes: r.FormValue("attributes")}
	s.vmDone(w, r, s.ops(r).SetTags(r.Context(), r.PathValue("ns"), r.PathValue("name"), req), "organize")
}

func (s *Server) vmBackupForm(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.ops(r).BackupOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), formMap(r)), "backup "+r.PathValue("op"))
}

func (s *Server) scheduleForm(w http.ResponseWriter, r *http.Request) {
	s.vmDone(w, r, s.ops(r).ScheduleOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), formMap(r)), "schedule "+r.PathValue("op"))
}

func (s *Server) apiVMOp(w http.ResponseWriter, r *http.Request, in any, fn func(ctx context.Context, l localOps, ns, name string) error) {
	s.apiOp(w, r, in, func() (any, error) {
		err := fn(r.Context(), localOps{s}, r.PathValue("ns"), r.PathValue("name"))
		if err == nil {
			log.Printf("%s: %s %s", session(r).User, r.Method, r.URL.Path)
		}
		return nil, err
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
