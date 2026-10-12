package server

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"argus/internal/auth"
)

var validCBT = regexp.MustCompile(`^argus-cbt-[0-9]+$`)

type cbtSession struct {
	ID      string         `json:"id"`
	Created time.Time      `json:"created"`
	Disks   []string       `json:"disks,omitempty"`
	Spec    map[string]any `json:"spec,omitempty"`
}

func cbtCreated(id string) time.Time {
	n, _ := strconv.ParseInt(strings.TrimPrefix(id, "argus-cbt-"), 10, 64)
	return time.Unix(n, 0).UTC()
}

func (s *Server) cbtRoutes() {
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /api/vms/{ns}/{name}/cbt":                          s.cbtCreate,
		"GET /api/vms/{ns}/{name}/cbt":                           s.cbtList,
		"DELETE /api/vms/{ns}/{name}/cbt/{id}":                   s.cbtDelete,
		"GET /api/vms/{ns}/{name}/cbt/{id}/disks/{disk}/changes": s.cbtChanges,
		"GET /api/vms/{ns}/{name}/cbt/{id}/disks/{disk}/data":    s.cbtData,
	} {
		s.mux.HandleFunc(pattern, s.require(auth.Admin, h))
	}
}

func (s *Server) cbtCreate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	spec, err := s.kube.VMSpec(r.Context(), ns, name)
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	disks, ds, err := s.vmDatasets(r)
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	id := "argus-cbt-" + strconv.FormatInt(time.Now().Unix(), 10)
	frozen := s.kube.Freeze(r.Context(), ns, name) == nil
	err = s.hostd.ZFSSnapshot(r.Context(), ds, id)
	if frozen {
		s.kube.Unfreeze(r.Context(), ns, name)
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	sess := cbtSession{ID: id, Created: cbtCreated(id), Spec: spec}
	for v := range disks {
		sess.Disks = append(sess.Disks, v)
	}
	slices.Sort(sess.Disks)
	writeJSONCode(w, http.StatusCreated, sess)
}

func (s *Server) cbtList(w http.ResponseWriter, r *http.Request) {
	_, ds, err := s.vmDatasets(r)
	var snaps map[string][]string
	if err == nil {
		snaps, err = s.hostd.ZFSSnapshots(r.Context(), ds, "cbt")
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	out := []cbtSession{}
	if len(ds) > 0 {
		for _, id := range snaps[ds[0]] {
			if !slices.ContainsFunc(ds, func(d string) bool { return !slices.Contains(snaps[d], id) }) {
				out = append(out, cbtSession{ID: id, Created: cbtCreated(id)})
			}
		}
	}
	writeJSON(w, out)
}

func (s *Server) cbtDelete(w http.ResponseWriter, r *http.Request) {
	_, ds, err := s.vmDatasets(r)
	if err == nil && !validCBT.MatchString(r.PathValue("id")) {
		err = errors.New("invalid backup id")
	}
	if err == nil {
		err = s.hostd.ZFSDestroy(r.Context(), ds, r.PathValue("id"))
	}
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, map[string]string{})
}

func (s *Server) cbtDisk(r *http.Request) (string, error) {
	if !validCBT.MatchString(r.PathValue("id")) || (r.URL.Query().Get("base") != "" && !validCBT.MatchString(r.URL.Query().Get("base"))) {
		return "", errors.New("invalid backup id")
	}
	disks, _, err := s.vmDatasets(r)
	if err != nil {
		return "", err
	}
	d, ok := disks[r.PathValue("disk")]
	if !ok {
		return "", errors.New("unknown disk")
	}
	return d.Dataset, nil
}

func (s *Server) cbtChanges(w http.ResponseWriter, r *http.Request) {
	ds, err := s.cbtDisk(r)
	if err != nil {
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	ch, err := s.hostd.ZFSChanges(r.Context(), ds, r.PathValue("id"), r.URL.Query().Get("base"))
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, ch)
}

func (s *Server) cbtData(w http.ResponseWriter, r *http.Request) {
	ds, err := s.cbtDisk(r)
	if err != nil {
		apiError(w, r, http.StatusNotFound, err)
		return
	}
	resp, err := s.hostd.ZFSRead(r.Context(), ds, r.PathValue("id"), r.Header)
	if err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	defer resp.Body.Close()
	copyStream(w, resp)
}

func copyStream(w http.ResponseWriter, resp *http.Response) {
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
