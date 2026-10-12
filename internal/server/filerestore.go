package server

import (
	"compress/gzip"
	"context"
	"errors"
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
	"argus/internal/manager"
)

const flrMaxAge = 2 * time.Hour

type flrSession struct {
	ID      string          `json:"id"`
	NS      string          `json:"ns"`
	VM      string          `json:"vm"`
	Stamp   string          `json:"stamp"`
	State   string          `json:"state"`
	Error   string          `json:"error,omitempty"`
	Parts   []hostd.FLRPart `json:"parts"`
	Started time.Time       `json:"started"`
}

type flrState struct {
	mu sync.Mutex
	m  map[string]*flrSession
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
	sess := &flrSession{ID: newToken()[:16], NS: ns, VM: name, Stamp: stamp, State: "Unpacking", Started: time.Now()}
	s.flr.mu.Lock()
	if s.flr.m == nil {
		s.flr.m = map[string]*flrSession{}
	}
	s.flr.m[sess.ID] = sess
	s.flr.mu.Unlock()
	go func() {
		ctx := context.Background()
		var err error
		for _, file := range slices.Sorted(maps.Keys(man.Files)) {
			disk, ok := strings.CutSuffix(file, ".img.gz")
			if !ok {
				continue
			}
			if err = s.flrLoad(ctx, st, rel+"/"+file, man.Files[file], sess.ID, disk); err != nil {
				break
			}
		}
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
		log.Printf("file restore %s of %s/%s: %v", sess.ID, name, stamp, err)
	}()
	return sess.ID, nil
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

func (s *Server) flrSession(ns, name, id string) (flrSession, error) {
	s.flr.mu.Lock()
	defer s.flr.mu.Unlock()
	sess, ok := s.flr.m[id]
	if !ok || sess.NS != ns || sess.VM != name {
		return flrSession{}, errors.New("file browsing session not found or expired")
	}
	return *sess, nil
}

func (s *Server) flrClose(ctx context.Context, ns, name, id string) error {
	if _, err := s.flrSession(ns, name, id); err != nil {
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

func (l localOps) FLR(ctx context.Context, ns, name, id string) (flrSession, error) {
	return l.s.flrSession(ns, name, id)
}

func (l localOps) FLRList(ctx context.Context, ns, name, id, part, dir string) ([]hostd.FLREntry, error) {
	if _, err := l.s.flrSession(ns, name, id); err != nil {
		return nil, err
	}
	return l.s.hostd.FLRList(ctx, id, part, dir)
}

func (l localOps) FLRFile(w http.ResponseWriter, r *http.Request, ns, name, id, part, file string) {
	if _, err := l.s.flrSession(ns, name, id); err != nil {
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
	io.Copy(w, resp.Body)
}

func (l localOps) FLRClose(ctx context.Context, ns, name, id string) error {
	return l.s.flrClose(ctx, ns, name, id)
}

func flrPath(ns, name, id, rest string) string {
	return manager.APIPath("/api/vms/%s/%s/flr/%s", ns, name, id) + rest
}

func (o remoteOps) FLRStart(ctx context.Context, ns, name, store, stamp string) (string, error) {
	var out struct{ ID string }
	return out.ID, o.n.Call(ctx, http.MethodPost, manager.APIPath("/api/vms/%s/%s/flr", ns, name), map[string]string{"store": store, "stamp": stamp}, &out)
}

func (o remoteOps) FLR(ctx context.Context, ns, name, id string) (flrSession, error) {
	var out flrSession
	return out, o.get(ctx, flrPath(ns, name, id, ""), &out)
}

func (o remoteOps) FLRList(ctx context.Context, ns, name, id, part, dir string) ([]hostd.FLREntry, error) {
	var out []hostd.FLREntry
	return out, o.get(ctx, flrPath(ns, name, id, "/ls?"+url.Values{"part": {part}, "path": {dir}}.Encode()), &out)
}

func (o remoteOps) FLRFile(w http.ResponseWriter, r *http.Request, ns, name, id, part, file string) {
	resp, err := o.n.Open(r.Context(), http.MethodGet, flrPath(ns, name, id, "/file?"+url.Values{"part": {part}, "path": {file}}.Encode()), nil, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		w.Header().Set(k, resp.Header.Get(k))
	}
	io.Copy(w, resp.Body)
}

func (o remoteOps) FLRClose(ctx context.Context, ns, name, id string) error {
	return o.post(ctx, flrPath(ns, name, id, "/close"), nil)
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request) {
	ns, name, q := r.PathValue("ns"), r.PathValue("name"), r.URL.Query()
	data := map[string]any{"Nav": "vms", "NS": ns, "Name": name, "Part": q.Get("part"), "Path": strings.Trim(q.Get("path"), "/")}
	sess, err := s.ops(r).FLR(r.Context(), ns, name, q.Get("id"))
	if r.Header.Get("HX-Request") != "" {
		if err != nil || sess.State != "Unpacking" {
			w.Header().Set("HX-Refresh", "true")
		}
		return
	}
	data["S"] = sess
	if err == nil && sess.State == "Ready" && q.Get("part") != "" {
		data["Entries"], err = s.ops(r).FLRList(r.Context(), ns, name, sess.ID, q.Get("part"), q.Get("path"))
	}
	data["Error"] = errText(err)
	s.render(w, r, "files.html", "layout", data)
}

func (s *Server) filesStart(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	id, err := s.ops(r).FLRStart(r.Context(), ns, name, r.FormValue("store"), r.FormValue("stamp"))
	if err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: browse backup %s of %s/%s", session(r).User, r.FormValue("stamp"), ns, name)
	base, _ := r.Context().Value(baseKey{}).(string)
	w.Header().Set("HX-Redirect", base+"/vms/"+ns+"/"+name+"/files?id="+id)
}

func (s *Server) filesDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	log.Printf("%s: download %s from backup browse %s of %s", session(r).User, q.Get("path"), q.Get("id"), r.PathValue("name"))
	s.ops(r).FLRFile(w, r, r.PathValue("ns"), r.PathValue("name"), q.Get("id"), q.Get("part"), q.Get("path"))
}

func (s *Server) filesClose(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	if err := s.ops(r).FLRClose(r.Context(), ns, name, r.FormValue("id")); err != nil {
		flash(w, r, err)
		return
	}
	base, _ := r.Context().Value(baseKey{}).(string)
	if host, ok := strings.CutPrefix(base, "/hosts/"); ok {
		base = "/nodes/" + host
	}
	w.Header().Set("HX-Redirect", base+"/vms/"+ns+"/"+name+"?tab=backups")
}

func (s *Server) flrAPIRoutes() {
	l := localOps{s}
	routes := map[string]http.HandlerFunc{
		"POST /api/vms/{ns}/{name}/flr": func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Store, Stamp string }
			s.apiOp(w, r, &req, func() (any, error) {
				id, err := l.FLRStart(r.Context(), r.PathValue("ns"), r.PathValue("name"), req.Store, req.Stamp)
				return map[string]string{"id": id}, err
			})
		},
		"GET /api/vms/{ns}/{name}/flr/{id}": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) {
				return l.FLR(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("id"))
			})
		},
		"GET /api/vms/{ns}/{name}/flr/{id}/ls": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) {
				return l.FLRList(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
			})
		},
		"GET /api/vms/{ns}/{name}/flr/{id}/file": func(w http.ResponseWriter, r *http.Request) {
			l.FLRFile(w, r, r.PathValue("ns"), r.PathValue("name"), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
		},
		"POST /api/vms/{ns}/{name}/flr/{id}/close": func(w http.ResponseWriter, r *http.Request) {
			s.apiOp(w, r, nil, func() (any, error) {
				return nil, l.FLRClose(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("id"))
			})
		},
	}
	for pattern, h := range routes {
		s.mux.HandleFunc(pattern, s.require(auth.Admin, h))
	}
}
