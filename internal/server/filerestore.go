package server

import (
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/hostui"
	"argus/internal/kube"
	"argus/internal/manager"
	"argus/internal/ui"
)

const flrMaxAge = 2 * time.Hour

type flrState struct {
	mu sync.Mutex
	m  map[string]*hostui.FLRSession
}

func (s *Server) flrBegin(owner, stamp string, load func(ctx context.Context, id string) error) string {
	sess := &hostui.FLRSession{ID: ui.NewToken()[:16], Owner: owner, Stamp: stamp, State: "Unpacking", Started: time.Now()}
	s.flr.mu.Lock()
	if s.flr.m == nil {
		s.flr.m = map[string]*hostui.FLRSession{}
	}
	s.flr.m[sess.ID] = sess
	s.flr.mu.Unlock()
	go func() {
		ctx := context.Background()
		err := load(ctx, sess.ID)
		parts, perr := s.hostd.FLRParts(ctx, sess.ID)
		if err == nil {
			err = perr
		}
		if err == nil && len(parts) == 0 {
			err = errors.New("no readable file systems; supported are ext2/3/4, XFS, Btrfs, NTFS, FAT and exFAT")
		}
		s.flr.mu.Lock()
		sess.State, sess.Parts = "Ready", parts
		if err != nil {
			sess.State, sess.Error = "Failed", err.Error()
		}
		s.flr.mu.Unlock()
		log.Printf("file browse %s of %s: %v", sess.ID, owner, err)
	}()
	return sess.ID
}

func (s *Server) flrStart(ctx context.Context, ns, name, store, stamp string) (string, error) {
	if s.hostd == nil {
		return "", errNoHostd
	}
	if !stampRE.MatchString(stamp) {
		return "", errors.New("invalid backup")
	}
	st, err := s.store(ctx, store)
	if err != nil {
		return "", err
	}
	rel := name + "/" + stamp
	man, err := manifestOf(ctx, st, rel)
	if err != nil {
		return "", err
	}
	return s.flrBegin(hostui.VMOwner(ns, name), stamp, func(ctx context.Context, id string) error {
		for _, file := range slices.Sorted(maps.Keys(man.Files)) {
			if disk, ok := strings.CutSuffix(file, ".img.gz"); ok {
				if err := s.flrLoad(ctx, st, rel+"/"+file, man.Files[file], id, disk); err != nil {
					return err
				}
			}
		}
		return nil
	}), nil
}

func (s *Server) flrVolume(ctx context.Context, name string) (string, error) {
	if s.hostd == nil {
		return "", errNoHostd
	}
	vols, err := s.kube.Volumes(ctx)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(vols, func(v kube.Volume) bool { return v.Name == name })
	if i < 0 || vols[i].Dataset == "" {
		return "", fmt.Errorf("volume %s not found on a ZFS pool", name)
	}
	if vm := cmp.Or(vols[i].UsedBy, vols[i].Owner); vm != "" {
		if v, err := s.kube.GetVM(ctx, kube.Namespace, vm); err == nil && v.Running {
			return "", fmt.Errorf("power off %s first", vm)
		}
	}
	return s.flrBegin(hostui.VolumeOwner(name), "", func(ctx context.Context, id string) error {
		return s.hostd.FLRAttachVolume(ctx, id, "disk", vols[i].Dataset)
	}), nil
}

func (s *Server) flrLoad(ctx context.Context, st backupStore, rel string, want hostd.ManifestFile, id, disk string) error {
	body, _, err := st.Get(ctx, rel)
	if err != nil {
		return err
	}
	defer body.Close()
	pr, pw := io.Pipe()
	go func() {
		_, err := verifiedCopy(pw, body, want)
		pw.CloseWithError(err)
	}()
	gz, err := gzip.NewReader(pr)
	if err == nil {
		err = s.hostd.FLRLoad(ctx, id, disk, gz)
	}
	pr.CloseWithError(err)
	return err
}

func (s *Server) flrSession(owner, id string) (hostui.FLRSession, error) {
	s.flr.mu.Lock()
	defer s.flr.mu.Unlock()
	sess, ok := s.flr.m[id]
	if !ok || sess.Owner != owner {
		return hostui.FLRSession{}, errors.New("file browsing session not found or expired")
	}
	return *sess, nil
}

func (s *Server) flrClose(ctx context.Context, owner, id string) error {
	if _, err := s.flrSession(owner, id); err != nil {
		return err
	}
	if err := s.hostd.FLRClose(ctx, id); err != nil {
		return err
	}
	s.flr.mu.Lock()
	delete(s.flr.m, id)
	s.flr.mu.Unlock()
	return nil
}

func (s *Server) flrSweep(ctx context.Context) {
	s.flr.mu.Lock()
	for id, sess := range s.flr.m {
		if time.Since(sess.Started) > flrMaxAge {
			delete(s.flr.m, id)
		}
	}
	s.flr.mu.Unlock()
	if err := s.hostd.FLRSweep(ctx); err != nil {
		log.Printf("file restore sweep: %v", err)
	}
}

func (l localOps) FLRStart(ctx context.Context, ns, name, store, stamp string) (string, error) {
	return l.s.flrStart(ctx, ns, name, store, stamp)
}

func (l localOps) FLRVolume(ctx context.Context, name string) (string, error) {
	return l.s.flrVolume(ctx, name)
}

func (l localOps) FLR(ctx context.Context, owner, id string) (hostui.FLRSession, error) {
	return l.s.flrSession(owner, id)
}

func (l localOps) FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error) {
	if _, err := l.s.flrSession(owner, id); err != nil {
		return nil, err
	}
	return l.s.hostd.FLRList(ctx, id, part, dir)
}

func (l localOps) FLRFile(ctx context.Context, owner, id, part, file string) (hostui.Download, error) {
	if _, err := l.s.flrSession(owner, id); err != nil {
		return hostui.Download{}, hostui.StatusError{Code: http.StatusNotFound, Err: err}
	}
	d, err := hostui.RemoteDownload(l.s.hostd.FLRFile(ctx, id, part, file))
	if err != nil {
		return hostui.Download{}, hostui.StatusError{Code: http.StatusNotFound, Err: err}
	}
	d.Type, d.Name = "application/octet-stream", path.Base(file)
	return d, nil
}

func (l localOps) FLRClose(ctx context.Context, owner, id string) error {
	return l.s.flrClose(ctx, owner, id)
}

func flrAPI(owner string) string {
	if vol, ok := strings.CutPrefix(owner, "volume:"); ok {
		return manager.APIPath("/api/volumes/%s/flr", vol)
	}
	ns, name, _ := strings.Cut(strings.TrimPrefix(owner, "vm:"), "/")
	return manager.APIPath("/api/vms/%s/%s/flr", ns, name)
}

func (o remoteOps) FLRStart(ctx context.Context, ns, name, store, stamp string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(hostui.VMOwner(ns, name)), map[string]string{"store": store, "stamp": stamp}, &out)
}

func (o remoteOps) FLRVolume(ctx context.Context, name string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(hostui.VolumeOwner(name)), nil, &out)
}

func (o remoteOps) FLR(ctx context.Context, owner, id string) (hostui.FLRSession, error) {
	var out hostui.FLRSession
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id), &out)
}

func (o remoteOps) FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error) {
	var out []hostd.FLREntry
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/ls?"+url.Values{"part": {part}, "path": {dir}}.Encode(), &out)
}

func (o remoteOps) FLRFile(ctx context.Context, owner, id, part, file string) (hostui.Download, error) {
	return hostui.RemoteDownload(o.n.Open(ctx, http.MethodGet, flrAPI(owner)+"/"+url.PathEscape(id)+"/file?"+url.Values{"part": {part}, "path": {file}}.Encode(), nil, nil))
}

func (o remoteOps) FLRClose(ctx context.Context, owner, id string) error {
	return o.post(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/close", nil)
}

func (s *Server) flrAPIRoutes() []ui.Route {
	l := localOps{s}
	start := func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Store, Stamp string }
		var in any = &req
		if r.PathValue("vol") != "" {
			in = nil
		}
		s.APIOp(w, r, in, func() (any, error) {
			var id string
			var err error
			if vol := r.PathValue("vol"); vol != "" {
				id, err = l.FLRVolume(r.Context(), vol)
			} else {
				id, err = l.FLRStart(r.Context(), r.PathValue("ns"), r.PathValue("name"), req.Store, req.Stamp)
			}
			return map[string]string{"id": id}, err
		})
	}
	status := func(w http.ResponseWriter, r *http.Request) {
		s.APIOp(w, r, nil, func() (any, error) { return l.FLR(r.Context(), hostui.OwnerOf(r), r.PathValue("id")) })
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		s.APIOp(w, r, nil, func() (any, error) {
			return l.FLRList(r.Context(), hostui.OwnerOf(r), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
		})
	}
	file := func(w http.ResponseWriter, r *http.Request) {
		d, err := l.FLRFile(r.Context(), hostui.OwnerOf(r), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
		hostui.ServeDownload(w, r, d, err)
	}
	closeSession := func(w http.ResponseWriter, r *http.Request) {
		s.APIOp(w, r, nil, func() (any, error) { return nil, l.FLRClose(r.Context(), hostui.OwnerOf(r), r.PathValue("id")) })
	}
	var rs []ui.Route
	for _, prefix := range []string{"/api/vms/{ns}/{name}/flr", "/api/volumes/{vol}/flr"} {
		rs = append(rs,
			ui.Route{Pattern: "POST " + prefix, Priv: auth.Administer, H: start},
			ui.Route{Pattern: "GET " + prefix + "/{id}", Priv: auth.Administer, H: status},
			ui.Route{Pattern: "GET " + prefix + "/{id}/ls", Priv: auth.Administer, H: list},
			ui.Route{Pattern: "GET " + prefix + "/{id}/file", Priv: auth.Administer, H: file},
			ui.Route{Pattern: "POST " + prefix + "/{id}/close", Priv: auth.Administer, H: closeSession},
		)
	}
	return rs
}
