package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"argus/internal/auth"
	"argus/internal/config"
	"argus/internal/hostd"
	"argus/internal/kube"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type BackupPolicy struct {
	SnapEvery   int    `json:"snapEvery"`
	SnapKeep    int    `json:"snapKeep"`
	BackupEvery int    `json:"backupEvery"`
	BackupKeep  int    `json:"backupKeep"`
	Target      string `json:"target"`
}

func parsePolicy(s string) (BackupPolicy, bool) {
	var p BackupPolicy
	for _, f := range strings.Fields(s) {
		k, v, _ := strings.Cut(f, "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "snap":
			p.SnapEvery = n
		case "snapkeep":
			p.SnapKeep = n
		case "backup":
			p.BackupEvery = n
		case "backupkeep":
			p.BackupKeep = n
		case "target":
			p.Target = v
		}
	}
	return p, p.SnapEvery > 0 || p.BackupEvery > 0
}

func (p BackupPolicy) String() string {
	return fmt.Sprintf("snap=%d snapkeep=%d backup=%d backupkeep=%d target=%s", p.SnapEvery, p.SnapKeep, p.BackupEvery, p.BackupKeep, p.Target)
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type backupMeta struct {
	Namespace string         `json:"namespace"`
	Spec      map[string]any `json:"spec"`
	Disks     []string       `json:"disks"`
	Created   time.Time      `json:"created"`
}

type backupState struct {
	mu       sync.Mutex
	running  map[string]bool
	last     map[string]string
	lastCopy map[string]time.Time
	restores map[string]restoreJob
	links    map[string]downloadGrant
}

type downloadGrant struct {
	path    string
	expires time.Time
}

type restoreJob struct {
	Store     string `json:"store"`
	Rel       string `json:"rel"`
	Namespace string `json:"namespace"`
	Target    string `json:"target"`
	Key       string `json:"key"`
}

const restoresKey = "argus-restores"

func (b *backupState) start(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == nil {
		b.running, b.last = map[string]bool{}, map[string]string{}
	}
	if b.running[key] {
		return false
	}
	b.running[key] = true
	return true
}

func (b *backupState) finish(key string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, key)
	msg := "Succeeded " + time.Now().Format("2006-01-02 15:04")
	if err != nil {
		msg = "Failed " + time.Now().Format("2006-01-02 15:04") + ": " + err.Error()
	}
	b.last[key] = msg
}

func (b *backupState) note(key, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == nil {
		b.last = map[string]string{}
	}
	b.last[key] = msg + " " + time.Now().Format("2006-01-02 15:04")
}

func (b *backupState) status(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running[key] {
		return "Running"
	}
	return b.last[key]
}

func (s *Server) backupLoop() {
	s.resumeRestores()
	for range time.Tick(time.Minute) {
		recovering("backups", s.backupTick)
	}
}

func (s *Server) backupTick() {
	ctx := context.Background()
	vms, err := s.kube.ListVMs(ctx)
	if err != nil {
		return
	}
	for _, vm := range vms {
		p, ok := parsePolicy(vm.Backup)
		if !ok {
			continue
		}
		if p.SnapEvery > 0 {
			s.snapshotDue(ctx, vm.Namespace, vm.Name, p)
		}
		if p.BackupEvery > 0 && time.Since(s.lastCopy(ctx, vm.Namespace, vm.Name, p.Target)) >= time.Duration(p.BackupEvery)*time.Hour {
			s.startBackup(vm.Namespace, vm.Name, p)
		}
	}
}

func recovering(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered from panic: %v", name, r)
		}
	}()
	fn()
}

func (s *Server) snapshotDue(ctx context.Context, ns, name string, p BackupPolicy) {
	snaps, err := s.kube.ListSnapshots(ctx, ns, name)
	if err != nil {
		return
	}
	var auto []kube.Snapshot
	for _, sn := range snaps {
		if sn.Auto {
			auto = append(auto, sn)
		}
	}
	slices.SortFunc(auto, func(a, b kube.Snapshot) int { return b.Created.Compare(a.Created) })
	if len(auto) == 0 || time.Since(auto[0].Created) >= time.Duration(p.SnapEvery)*time.Hour {
		if _, err := s.kube.CreateAutoSnapshot(ctx, ns, name); err != nil {
			log.Printf("snapshot %s/%s: %v", ns, name, err)
			return
		}
		auto = append([]kube.Snapshot{{}}, auto...)
	}
	for i, sn := range auto {
		if i >= max(p.SnapKeep, 1) && sn.Name != "" {
			s.kube.DeleteSnapshot(ctx, ns, sn.Name)
		}
	}
}

func (s *Server) lastCopy(ctx context.Context, ns, name, target string) time.Time {
	key := ns + "/" + name + "@" + target
	s.backups.mu.Lock()
	t, ok := s.backups.lastCopy[key]
	s.backups.mu.Unlock()
	if ok {
		return t
	}
	if st, err := s.store(ctx, target); err == nil {
		if bs, err := vmBackups(ctx, st, name); err == nil {
			for _, b := range bs {
				if c, err := time.Parse("20060102-150405", b.Stamp); err == nil && b.Complete && c.After(t) {
					t = c
				}
			}
		}
	}
	s.setLastCopy(key, t)
	return t
}

func (s *Server) setLastCopy(key string, t time.Time) {
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()
	if s.backups.lastCopy == nil {
		s.backups.lastCopy = map[string]time.Time{}
	}
	s.backups.lastCopy[key] = t
}

var transfers = make(chan struct{}, config.Int("ARGUS_TRANSFERS", 2))

func (s *Server) startBackup(ns, name string, p BackupPolicy) bool {
	key := ns + "/" + name
	if !s.backups.start(key) {
		return false
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
		defer cancel()
		transfers <- struct{}{}
		defer func() { <-transfers }()
		err := s.backupVM(ctx, ns, name, p)
		if err != nil {
			log.Printf("backup %s: %v", key, err)
		}
		done := time.Now()
		if err != nil {
			done = done.Add(15*time.Minute - time.Duration(p.BackupEvery)*time.Hour)
		}
		s.setLastCopy(key+"@"+p.Target, done)
		s.backups.finish(key, err)
	}()
	return true
}

func (s *Server) backupVM(ctx context.Context, ns, name string, p BackupPolicy) error {
	st, err := s.store(ctx, p.Target)
	if err != nil {
		return err
	}
	if err := s.offsite(ctx, st, ns, name); err != nil {
		return err
	}
	bs, err := vmBackups(ctx, st, name)
	if err != nil {
		return err
	}
	for i, b := range bs {
		if i >= max(p.BackupKeep, 1) {
			st.Delete(ctx, b.VM+"/"+b.Stamp)
		}
	}
	return nil
}

func vmBackups(ctx context.Context, st backupStore, name string) ([]hostd.Backup, error) {
	all, err := st.List(ctx)
	return slices.DeleteFunc(all, func(b hostd.Backup) bool { return b.VM != name }), err
}

func (s *Server) offsite(ctx context.Context, st backupStore, ns, name string) error {
	if err := s.kube.CheckBackupable(ctx, ns, name); err != nil {
		return err
	}
	ex, err := s.kube.BackupExport(ctx, ns, name)
	defer s.kube.DeleteBackupExport(context.Background(), ns, name)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	rel := name + "/" + stamp
	meta := backupMeta{Namespace: ns, Spec: ex.Spec, Created: time.Now()}
	man := hostd.Manifest{Files: map[string]hostd.ManifestFile{}}
	fail := func(err error) error {
		st.Delete(context.Background(), rel)
		return err
	}
	templates, _, _ := unstructured.NestedSlice(ex.Spec, "dataVolumeTemplates")
	for _, t := range templates {
		dv, _, _ := unstructured.NestedString(t.(map[string]any), "metadata", "name")
		mf, err := s.copyDisk(ctx, st, ns, name, dv, ex.Token, rel+"/"+dv+".img.gz")
		if err != nil {
			return fail(fmt.Errorf("disk %s: %w", dv, err))
		}
		man.Files[dv+".img.gz"] = mf
		meta.Disks = append(meta.Disks, dv)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(b)
	man.Files["vm.json"] = hostd.ManifestFile{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}
	if err := st.Put(ctx, rel+"/vm.json", bytes.NewReader(b)); err != nil {
		return fail(err)
	}
	mb, _ := json.Marshal(man)
	if err := st.Put(ctx, rel+"/manifest.json", bytes.NewReader(mb)); err != nil {
		return fail(err)
	}
	return nil
}

type hashReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (h *hashReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

func (s *Server) copyDisk(ctx context.Context, st backupStore, ns, name, dv, token, rel string) (hostd.ManifestFile, error) {
	var mf hostd.ManifestFile
	src, cert, err := s.kube.BackupSource(ctx, ns, name, dv)
	if err != nil {
		return mf, err
	}
	resp, err := openExport(ctx, src, cert, token)
	if err != nil {
		return mf, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mf, fmt.Errorf("export returned %s", resp.Status)
	}
	hr := &hashReader{r: resp.Body, h: sha256.New()}
	if err := st.Put(ctx, rel, hr); err != nil {
		return mf, err
	}
	return hostd.ManifestFile{SHA256: hex.EncodeToString(hr.h.Sum(nil)), Size: hr.n}, nil
}

func (s *Server) restoreBackup(ctx context.Context, store, vm, stamp, target string) error {
	rel := vm + "/" + stamp
	st, err := s.store(ctx, store)
	if err != nil {
		return err
	}
	if err := checkBackup(ctx, st, rel); err != nil {
		return err
	}
	body, _, err := st.Get(ctx, rel+"/vm.json")
	if err != nil {
		return err
	}
	var meta backupMeta
	err = json.NewDecoder(body).Decode(&meta)
	body.Close()
	if err != nil {
		return fmt.Errorf("backup metadata: %w", err)
	}
	if target == "" {
		target = vm
	}
	if !validObject.MatchString(target) {
		return errors.New("invalid VM name")
	}
	if meta.Namespace != kube.Namespace {
		return fmt.Errorf("backup is for namespace %q, which this host does not use", meta.Namespace)
	}
	if err := kube.ValidateRestoreSpec(meta.Spec); err != nil {
		return err
	}
	names := map[string]string{}
	for _, d := range meta.Disks {
		names[d] = d
	}
	if target != vm {
		names = kube.RenameSpec(meta.Spec, vm, target)
	}
	token := newToken()
	ex := kube.Export{Spec: meta.Spec, Token: token, CA: string(s.caPEM)}
	for _, d := range meta.Disks {
		ex.Disks = append(ex.Disks, kube.ExportDisk{DataVolume: names[d], URL: fmt.Sprintf("https://%s/backups/%s/%s.img.gz", os.Getenv("HOST_IP"), rel, d)})
	}
	job := restoreJob{Store: store, Rel: rel, Namespace: meta.Namespace, Target: target, Key: meta.Namespace + "/" + vm}
	s.trackRestore(token, job)
	if err := s.kube.ImportVM(ctx, meta.Namespace, target, "restore-"+stamp, ex, kube.ImportOptions{}); err != nil {
		s.untrackRestore(token)
		return err
	}
	s.backups.note(job.Key, "Restoring "+stamp+" as "+target)
	go s.watchRestore(token, job)
	return nil
}

func (s *Server) saveRestores() {
	s.backups.mu.Lock()
	b, _ := json.Marshal(s.backups.restores)
	s.backups.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := auth.PutJSON(ctx, s.auth.Secrets(), restoresKey, "restores", json.RawMessage(b)); err != nil {
		log.Printf("restores: save: %v", err)
	}
}

func (s *Server) trackRestore(token string, job restoreJob) {
	s.backups.mu.Lock()
	if s.backups.restores == nil {
		s.backups.restores = map[string]restoreJob{}
	}
	s.backups.restores[token] = job
	s.backups.mu.Unlock()
	s.saveRestores()
}

func (s *Server) untrackRestore(token string) {
	s.backups.mu.Lock()
	delete(s.backups.restores, token)
	s.backups.mu.Unlock()
	s.saveRestores()
}

func (s *Server) resumeRestores() {
	var m map[string]restoreJob
	if _, err := auth.LoadJSON(context.Background(), s.auth.Secrets(), restoresKey, "restores", &m); err != nil {
		return
	}
	s.backups.mu.Lock()
	s.backups.restores = m
	s.backups.mu.Unlock()
	for token, job := range m {
		go s.watchRestore(token, job)
	}
}

var errRestoreAborted = errors.New("restore aborted")

func (s *Server) watchRestore(token string, job restoreJob) {
	defer s.untrackRestore(token)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	failed, err := kube.WaitDisks(ctx, 10*time.Second, func(ctx context.Context) ([]kube.DiskStatus, error) {
		s.backups.mu.Lock()
		_, tracked := s.backups.restores[token]
		s.backups.mu.Unlock()
		if !tracked {
			return nil, errRestoreAborted
		}
		disks, _ := s.kube.DiskStatus(ctx, job.Namespace, job.Target)
		return disks, ctx.Err()
	}, nil)
	s.kube.FinishImport(context.Background(), job.Namespace, job.Target)
	switch {
	case errors.Is(err, errRestoreAborted):
		if err := s.kube.DeleteVM(context.Background(), job.Namespace, job.Target); err != nil {
			log.Printf("restore of %s: remove %s: %v", job.Rel, job.Target, err)
		}
	case failed != "":
		s.backups.note(job.Key, "Failed restore as "+job.Target+": disk "+failed+" did not copy")
	case ctx.Err() != nil:
		s.backups.note(job.Key, "Failed restore as "+job.Target+": timed out")
	default:
		s.backups.note(job.Key, "Restored as "+job.Target)
	}
	log.Printf("restore of %s as %s finished (failed disk: %q)", job.Rel, job.Target, failed)
}

func (s *Server) serveBackup(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("vm") + "/" + r.PathValue("stamp")
	s.backups.mu.Lock()
	token := strings.TrimSpace(r.Header.Get("x-kubevirt-export-token"))
	job, ok := s.backups.restores[token]
	s.backups.mu.Unlock()
	if !ok || job.Rel != rel {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if job.Store == "" {
		job.Store = "nfs"
	}
	st, err := s.store(r.Context(), job.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	man, err := manifestOf(r.Context(), st, rel)
	want, listed := man.Files[r.PathValue("file")]
	if err != nil || !listed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, _, err := st.Get(r.Context(), rel+"/"+r.PathValue("file"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(want.Size, 10))
	n, err := verifiedCopy(w, body, want)
	if err != nil {
		log.Printf("backup %s/%s: %v after %d bytes; aborting the restore stream", rel, r.PathValue("file"), err, n)
		if !errors.Is(err, errChecksum) {
			panic(http.ErrAbortHandler)
		}
		s.backups.note(job.Key, "Failed restore as "+job.Target+": "+r.PathValue("file")+" "+err.Error())
		s.untrackRestore(token)
		panic(http.ErrAbortHandler)
	}
	log.Printf("backup %s/%s served: %d bytes", rel, r.PathValue("file"), n)
}

type VMBackup struct {
	hostd.Backup
	Target string `json:"target"`
}

type VMBackups struct {
	Policy  BackupPolicy   `json:"policy"`
	Enabled bool           `json:"enabled"`
	Targets []BackupTarget `json:"targets"`
	Status  string         `json:"status"`
	Backups []VMBackup     `json:"backups"`
	Error   string         `json:"error"`
}

func (s *Server) vmBackupInfo(ctx context.Context, ns, name string) VMBackups {
	var info VMBackups
	vm, err := s.kube.GetVM(ctx, ns, name)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Policy, info.Enabled = parsePolicy(vm.Backup)
	if info.Policy.SnapKeep == 0 {
		info.Policy.SnapKeep = 7
	}
	if info.Policy.BackupKeep == 0 {
		info.Policy.BackupKeep = 7
	}
	info.Status = s.backups.status(ns + "/" + name)
	targets, err := s.backupTargets(ctx)
	if err != nil {
		info.Error = err.Error()
	}
	var errs []string
	for _, t := range targets {
		info.Targets = append(info.Targets, BackupTarget{Name: t.Name, Kind: t.Kind, Endpoint: t.Where()})
		bs, err := vmBackups(ctx, t.open(s.hostd), name)
		if err != nil {
			errs = append(errs, t.Name+": "+err.Error())
		}
		for _, b := range bs {
			info.Backups = append(info.Backups, VMBackup{b, t.Name})
		}
	}
	slices.SortFunc(info.Backups, func(a, b VMBackup) int { return strings.Compare(b.Stamp, a.Stamp) })
	if len(errs) > 0 {
		info.Error = strings.Join(errs, "; ")
	}
	return info
}

func (s *Server) backupOp(ctx context.Context, ns, name, op string, f func(string) string) error {
	num := func(k string) int { n, _ := strconv.Atoi(f(k)); return n }
	switch op {
	case "policy":
		p := BackupPolicy{SnapEvery: num("snap"), SnapKeep: num("snapkeep"), BackupEvery: num("backup"), BackupKeep: num("backupkeep"), Target: f("target")}
		for _, v := range [][3]int{{p.SnapEvery, 0, 720}, {p.BackupEvery, 0, 720}} {
			if v[0] < v[1] || v[0] > v[2] {
				return errors.New("intervals must be 0-720 hours")
			}
		}
		if p.SnapEvery > 0 && (p.SnapKeep < 1 || p.SnapKeep > 365) || p.BackupEvery > 0 && (p.BackupKeep < 1 || p.BackupKeep > 365) {
			return errors.New("keep must be 1-365")
		}
		if p.BackupEvery > 0 {
			if _, err := s.store(ctx, p.Target); err != nil {
				return err
			}
			if err := s.kube.CheckBackupable(ctx, ns, name); err != nil {
				return err
			}
		}
		if p.SnapEvery == 0 && p.BackupEvery == 0 {
			return s.kube.SetBackupPolicy(ctx, ns, name, "")
		}
		return s.kube.SetBackupPolicy(ctx, ns, name, p.String())
	case "now":
		target := f("target")
		if target == "" {
			vm, err := s.kube.GetVM(ctx, ns, name)
			if err != nil {
				return err
			}
			p, _ := parsePolicy(vm.Backup)
			target = p.Target
		}
		if target == "" {
			targets, err := s.backupTargets(ctx)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				return errors.New("add a backup target in host settings first")
			}
			target = targets[0].Name
		}
		if _, err := s.store(ctx, target); err != nil {
			return err
		}
		if err := s.kube.CheckBackupable(ctx, ns, name); err != nil {
			return err
		}
		keep := 7
		if vm, err := s.kube.GetVM(ctx, ns, name); err == nil {
			if p, _ := parsePolicy(vm.Backup); p.Target == target && p.BackupKeep > 0 {
				keep = p.BackupKeep
			}
		}
		if !s.startBackup(ns, name, BackupPolicy{BackupKeep: keep, Target: target}) {
			return errors.New("a backup of this VM is already running")
		}
		return nil
	case "restore":
		return s.restoreBackup(ctx, f("store"), name, f("stamp"), strings.TrimSpace(f("target")))
	case "delete":
		st, err := s.store(ctx, f("store"))
		if err != nil {
			return err
		}
		return st.Delete(ctx, name+"/"+f("stamp"))
	}
	return errors.New("unknown backup operation")
}

func (s *Server) apiVMBackups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.vmBackupInfo(r.Context(), r.PathValue("ns"), r.PathValue("name")))
}

func (s *Server) apiVMBackupOp(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)
	if err := s.backupOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), func(k string) string { return req[k] }); err != nil {
		apiError(w, r, http.StatusConflict, err)
		return
	}
	log.Printf("%s: backup %s %s/%s", session(r).User, r.PathValue("op"), r.PathValue("ns"), r.PathValue("name"))
	writeJSON(w, map[string]string{})
}

func (s *Server) vmBackupForm(w http.ResponseWriter, r *http.Request) {
	if err := s.backupOp(r.Context(), r.PathValue("ns"), r.PathValue("name"), r.PathValue("op"), r.FormValue); err != nil {
		flash(w, r, err)
		return
	}
	log.Printf("%s: backup %s %s/%s", session(r).User, r.PathValue("op"), r.PathValue("ns"), r.PathValue("name"))
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) downloadLink(w http.ResponseWriter, r *http.Request) {
	tok := newToken()
	path := strings.TrimSuffix(r.URL.Path, "/download")
	s.backups.mu.Lock()
	if s.backups.links == nil {
		s.backups.links = map[string]downloadGrant{}
	}
	for k, g := range s.backups.links {
		if time.Now().After(g.expires) {
			delete(s.backups.links, k)
		}
	}
	s.backups.links[tok] = downloadGrant{path: path, expires: time.Now().Add(5 * time.Minute)}
	s.backups.mu.Unlock()
	base, _ := r.Context().Value(baseKey{}).(string)
	w.Header().Set("HX-Redirect", base+"/vms/"+r.PathValue("ns")+"/"+r.PathValue("name")+"/disks/"+r.PathValue("disk")+"/download?t="+tok)
}

func (s *Server) downloadDisk(w http.ResponseWriter, r *http.Request) {
	ns, name, disk := r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")
	s.backups.mu.Lock()
	g, ok := s.backups.links[r.URL.Query().Get("t")]
	delete(s.backups.links, r.URL.Query().Get("t"))
	s.backups.mu.Unlock()
	if !ok || time.Now().After(g.expires) || g.path != strings.TrimSuffix(r.URL.Path, "/download") {
		http.Error(w, "this download link has expired; start the download again", http.StatusForbidden)
		return
	}
	s.record(s.auditUser(session(r).Org, session(r).User), "download disk "+disk+" of "+ns+"/"+name, true, "")
	s.ops(r).DownloadDisk(w, r, ns, name, disk)
}

func (s *Server) streamDisk(w http.ResponseWriter, r *http.Request, ns, name, disk string) {
	hw, err := s.kube.Hardware(r.Context(), ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	i := slices.IndexFunc(hw.Disks, func(d kube.HardwareDisk) bool { return d.Name == disk })
	if i < 0 {
		http.Error(w, "disk not found", http.StatusNotFound)
		return
	}
	key := ns + "/" + name
	if !s.backups.start(key) {
		http.Error(w, "a backup or download of this VM is already running", http.StatusConflict)
		return
	}
	var copied int64
	defer func() {
		s.backups.mu.Lock()
		delete(s.backups.running, key)
		s.backups.mu.Unlock()
	}()
	ex, err := s.kube.BackupExport(r.Context(), ns, name)
	defer s.kube.DeleteBackupExport(context.Background(), ns, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	src, cert, err := s.kube.BackupSource(r.Context(), ns, name, hw.Disks[i].PVC)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := openExport(r.Context(), src, cert, ex.Token)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "export returned "+resp.Status, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.img.gz"`, name, disk))
	if v := resp.Header.Get("Content-Length"); v != "" {
		w.Header().Set("Content-Length", v)
	}
	copied, _ = io.Copy(w, resp.Body)
	log.Printf("%s: downloaded %s/%s disk %s (%d bytes)", session(r).User, ns, name, disk, copied)
}

func openExport(ctx context.Context, src, ca, token string) (*http.Response, error) {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(ca))
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ResponseHeaderTimeout: time.Minute}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-kubevirt-export-token", token)
	return hc.Do(req)
}
