package hostserver

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/ui"
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

func (s *Server) cbtRoutes() []ui.Route {
	return []ui.Route{
		{Pattern: "POST /api/vms/{ns}/{name}/cbt", Priv: auth.Administer, H: s.cbtCreate},
		{Pattern: "GET /api/vms/{ns}/{name}/cbt", Priv: auth.Administer, H: s.cbtList},
		{Pattern: "DELETE /api/vms/{ns}/{name}/cbt/{id}", Priv: auth.Administer, H: s.cbtDelete},
		{Pattern: "GET /api/vms/{ns}/{name}/cbt/{id}/disks/{disk}/changes", Priv: auth.Administer, H: s.cbtChanges},
		{Pattern: "GET /api/vms/{ns}/{name}/cbt/{id}/disks/{disk}/data", Priv: auth.Administer, H: s.cbtData},
	}
}

func (s *Server) cbtCreate(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	spec, err := s.kube.VMSpec(r.Context(), ns, name)
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	disks, ds, err := s.vmDatasets(r)
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	id := "argus-cbt-" + strconv.FormatInt(time.Now().Unix(), 10)
	frozen := s.kube.Freeze(r.Context(), ns, name) == nil
	err = s.hostd.ZFSSnapshot(r.Context(), ds, id)
	if frozen {
		if uerr := s.kube.Unfreeze(r.Context(), ns, name); uerr != nil {
			log.Printf("unfreeze %s/%s: %v", ns, name, uerr)
		}
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	sess := cbtSession{ID: id, Created: cbtCreated(id), Spec: spec}
	for v := range disks {
		sess.Disks = append(sess.Disks, v)
	}
	slices.Sort(sess.Disks)
	ui.WriteJSONCode(w, http.StatusCreated, sess)
}

func (s *Server) cbtList(w http.ResponseWriter, r *http.Request) {
	_, ds, err := s.vmDatasets(r)
	var snaps map[string][]string
	if err == nil {
		snaps, err = s.hostd.ZFSSnapshots(r.Context(), ds, "cbt")
	}
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
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
	ui.WriteJSON(w, out)
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
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSON(w, map[string]string{})
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
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	ch, err := s.hostd.ZFSChanges(r.Context(), ds, r.PathValue("id"), r.URL.Query().Get("base"))
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	ui.WriteJSON(w, ch)
}

func (s *Server) cbtData(w http.ResponseWriter, r *http.Request) {
	ds, err := s.cbtDisk(r)
	if err != nil {
		ui.APIError(w, r, http.StatusNotFound, err)
		return
	}
	resp, err := s.hostd.ZFSRead(r.Context(), ds, r.PathValue("id"), r.Header)
	if err != nil {
		ui.APIError(w, r, http.StatusConflict, err)
		return
	}
	defer resp.Body.Close()
	ui.CopyStream(w, resp)
}
