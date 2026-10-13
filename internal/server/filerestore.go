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
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/hostd"
	"argus/internal/kube"
	"argus/internal/manager"
)

const flrMaxAge = 2 * time.Hour

type flrSession struct {
	ID      string          `json:"id"`
	Owner   string          `json:"owner"`
	Stamp   string          `json:"stamp,omitempty"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"`
	Parts   []hostd.FLRPart `json:"parts"`
	Started time.Time       `json:"started"`
}

type flrState struct {
	mu sync.Mutex
	m  map[string]*flrSession
}

func vmOwner(ns, name string) string { return "vm:" + ns + "/" + name }

func volumeOwner(name string) string { return "volume:" + name }

func ownerOf(r *http.Request) string {
	if v := r.PathValue("vol"); v != "" {
		return volumeOwner(v)
	}
	return vmOwner(r.PathValue("ns"), r.PathValue("name"))
}

func (s *Server) flrBegin(owner, stamp string, load func(ctx context.Context, id string) error) string {
	sess := &flrSession{ID: newToken()[:16], Owner: owner, Stamp: stamp, State: "Unpacking", Started: time.Now()}
	s.flr.mu.Lock()
	if s.flr.m == nil {
		s.flr.m = map[string]*flrSession{}
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
	return s.flrBegin(vmOwner(ns, name), stamp, func(ctx context.Context, id string) error {
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
	return s.flrBegin(volumeOwner(name), "", func(ctx context.Context, id string) error {
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

func (s *Server) flrSession(owner, id string) (flrSession, error) {
	s.flr.mu.Lock()
	defer s.flr.mu.Unlock()
	sess, ok := s.flr.m[id]
	if !ok || sess.Owner != owner {
		return flrSession{}, errors.New("file browsing session not found or expired")
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

func (l localOps) FLR(ctx context.Context, owner, id string) (flrSession, error) {
	return l.s.flrSession(owner, id)
}

func (l localOps) FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error) {
	if _, err := l.s.flrSession(owner, id); err != nil {
		return nil, err
	}
	return l.s.hostd.FLRList(ctx, id, part, dir)
}

func (l localOps) FLRFile(w http.ResponseWriter, r *http.Request, owner, id, part, file string) {
	if _, err := l.s.flrSession(owner, id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	resp, err := l.s.hostd.FLRFile(r.Context(), id, part, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", resp.Header.Get("Content-Length"))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(file)}))
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("stream %s: %v", r.URL.Path, err)
	}
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
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(vmOwner(ns, name)), map[string]string{"store": store, "stamp": stamp}, &out)
}

func (o remoteOps) FLRVolume(ctx context.Context, name string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, flrAPI(volumeOwner(name)), nil, &out)
}

func (o remoteOps) FLR(ctx context.Context, owner, id string) (flrSession, error) {
	var out flrSession
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id), &out)
}

func (o remoteOps) FLRList(ctx context.Context, owner, id, part, dir string) ([]hostd.FLREntry, error) {
	var out []hostd.FLREntry
	return out, o.get(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/ls?"+url.Values{"part": {part}, "path": {dir}}.Encode(), &out)
}

func (o remoteOps) FLRFile(w http.ResponseWriter, r *http.Request, owner, id, part, file string) {
	resp, err := o.n.Open(r.Context(), http.MethodGet, flrAPI(owner)+"/"+url.PathEscape(id)+"/file?"+url.Values{"part": {part}, "path": {file}}.Encode(), nil, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		w.Header().Set(k, resp.Header.Get(k))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("stream %s: %v", r.URL.Path, err)
	}
}

func (o remoteOps) FLRClose(ctx context.Context, owner, id string) error {
	return o.post(ctx, flrAPI(owner)+"/"+url.PathEscape(id)+"/close", nil)
}

func filesBase(r *http.Request) string {
	base, _ := r.Context().Value(baseKey{}).(string)
	if vol := r.PathValue("vol"); vol != "" {
		return base + "/volumes/" + vol + "/files"
	}
	return base + "/vms/" + r.PathValue("ns") + "/" + r.PathValue("name") + "/files"
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request) {
	q, owner := r.URL.Query(), ownerOf(r)
	data := map[string]any{"Nav": "volumes", "Files": filesBase(r), "Name": cmp.Or(r.PathValue("vol"), r.PathValue("name")), "Part": q.Get("part"), "Path": strings.Trim(q.Get("path"), "/")}
	sess, err := s.ops(r).FLR(r.Context(), owner, q.Get("id"))
	if r.Header.Get("HX-Request") != "" {
		if err != nil || sess.State != "Unpacking" {
			w.Header().Set("HX-Refresh", "true")
		}
		return
	}
	data["S"] = sess
	if err == nil && sess.State == "Ready" && q.Get("part") != "" {
		data["Entries"], err = s.ops(r).FLRList(r.Context(), owner, sess.ID, q.Get("part"), q.Get("path"))
	}
	data["Error"] = errText(err)
	s.render(w, r, "files.html", "layout", data)
}

func (s *Server) filesStart(w http.ResponseWriter, r *http.Request) {
	var id string
	var err error
	if vol := r.PathValue("vol"); vol != "" {
		id, err = s.ops(r).FLRVolume(r.Context(), vol)
	} else {
		id, err = s.ops(r).FLRStart(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.FormValue("store"), r.FormValue("stamp"))
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: browse files of %s %s", session(r).User, ownerOf(r), r.FormValue("stamp"))
	w.Header().Set("HX-Redirect", filesBase(r)+"?id="+id)
}

func (s *Server) filesDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Printf("%s: download %s from %s", session(r).User, q.Get("path"), ownerOf(r))
	s.ops(r).FLRFile(w, r, ownerOf(r), q.Get("id"), q.Get("part"), q.Get("path"))
}

func (s *Server) filesClose(w http.ResponseWriter, r *http.Request) {
	if err := s.ops(r).FLRClose(r.Context(), ownerOf(r), r.FormValue("id")); err != nil {
		flash(w, r, err)
		return
	}
	base, _ := r.Context().Value(baseKey{}).(string)
	host, onManager := strings.CutPrefix(base, "/hosts/")
	switch {
	case r.PathValue("vol") != "" && onManager:
		w.Header().Set("HX-Redirect", "/storage#s-volumes")
	case r.PathValue("vol") != "":
		w.Header().Set("HX-Redirect", "/volumes")
	case onManager:
		w.Header().Set("HX-Redirect", "/nodes/"+host+"/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"?tab=backups")
	default:
		w.Header().Set("HX-Redirect", "/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"?tab=backups")
	}
}

func (s *Server) flrAPIRoutes() {
	l := localOps{s}
	start := func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Store, Stamp string }
		var in any = &req
		if r.PathValue("vol") != "" {
			in = nil
		}
		s.apiOp(w, r, in, func() (any, error) {
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
		s.apiOp(w, r, nil, func() (any, error) { return l.FLR(r.Context(), ownerOf(r), r.PathValue("id")) })
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		s.apiOp(w, r, nil, func() (any, error) {
			return l.FLRList(r.Context(), ownerOf(r), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
		})
	}
	file := func(w http.ResponseWriter, r *http.Request) {
		l.FLRFile(w, r, ownerOf(r), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
	}
	closeSession := func(w http.ResponseWriter, r *http.Request) {
		s.apiOp(w, r, nil, func() (any, error) { return nil, l.FLRClose(r.Context(), ownerOf(r), r.PathValue("id")) })
	}
	for _, prefix := range []string{"/api/vms/{ns}/{name}/flr", "/api/volumes/{vol}/flr"} {
		s.mux.HandleFunc("POST "+prefix, s.require(auth.Admin, start))
		s.mux.HandleFunc("GET "+prefix+"/{id}", s.require(auth.Admin, status))
		s.mux.HandleFunc("GET "+prefix+"/{id}/ls", s.require(auth.Admin, list))
		s.mux.HandleFunc("GET "+prefix+"/{id}/file", s.require(auth.Admin, file))
		s.mux.HandleFunc("POST "+prefix+"/{id}/close", s.require(auth.Admin, closeSession))
	}
}
