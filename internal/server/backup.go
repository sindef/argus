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

	"argus/internal/hostd"
	"argus/internal/kube"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type BackupPolicy struct {
	Every   int  `json:"every"`
	Keep    int  `json:"keep"`
	Offsite bool `json:"offsite"`
}

func parsePolicy(s string) (BackupPolicy, bool) {
	var p BackupPolicy
	for _, f := range strings.Fields(s) {
		k, v, _ := strings.Cut(f, "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "every":
			p.Every = n
		case "keep":
			p.Keep = n
		case "offsite":
			p.Offsite = n == 1
		}
	}
	return p, p.Every > 0 && p.Keep > 0
}

func (p BackupPolicy) String() string {
	off := 0
	if p.Offsite {
		off = 1
	}
	return fmt.Sprintf("every=%d keep=%d offsite=%d", p.Every, p.Keep, off)
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
	restores map[string]restoreJob
}

type restoreJob struct {
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
		ctx := context.Background()
		vms, err := s.kube.ListVMs(ctx)
		if err != nil {
			continue
		}
		for _, vm := range vms {
			p, ok := parsePolicy(vm.Backup)
			if !ok {
				continue
			}
			snaps, err := s.kube.ListSnapshots(ctx, vm.Namespace, vm.Name)
			if err != nil {
				continue
			}
			last := time.Time{}
			for _, sn := range snaps {
				if sn.Auto && sn.Created.After(last) {
					last = sn.Created
				}
			}
			if time.Since(last) >= time.Duration(p.Every)*time.Hour {
				s.startBackup(vm.Namespace, vm.Name, p)
			}
		}
	}
}

var transfers = make(chan struct{}, 2)

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
		s.backups.finish(key, err)
	}()
	return true
}

func (s *Server) backupVM(ctx context.Context, ns, name string, p BackupPolicy) error {
	if _, err := s.kube.CreateAutoSnapshot(ctx, ns, name); err != nil {
		return err
	}
	snaps, err := s.kube.ListSnapshots(ctx, ns, name)
	if err != nil {
		return err
	}
	kept := 0
	for _, sn := range snaps {
		if !sn.Auto {
			continue
		}
		if kept++; kept > p.Keep {
			s.kube.DeleteSnapshot(ctx, ns, sn.Name)
		}
	}
	if !p.Offsite {
		return nil
	}
	if err := s.offsite(ctx, ns, name); err != nil {
		return err
	}
	bs, err := s.vmBackups(ctx, name)
	if err != nil {
		return err
	}
	for i, b := range bs {
		if i >= p.Keep {
			s.hostd.DeleteBackup(ctx, b.VM+"/"+b.Stamp)
		}
	}
	return nil
}

func (s *Server) vmBackups(ctx context.Context, name string) ([]hostd.Backup, error) {
	if s.hostd == nil {
		return nil, errors.New("host helper not available")
	}
	all, err := s.hostd.Backups(ctx)
	return slices.DeleteFunc(all, func(b hostd.Backup) bool { return b.VM != name }), err
}

func (s *Server) offsite(ctx context.Context, ns, name string) error {
	if s.hostd == nil {
		return errors.New("host helper not available")
	}
	st, err := s.hostd.Settings(ctx)
	if err != nil {
		return err
	}
	if st.Backup == "" {
		return errors.New("no backup target configured on this host")
	}
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
		s.hostd.DeleteBackup(context.Background(), rel)
		return err
	}
	templates, _, _ := unstructured.NestedSlice(ex.Spec, "dataVolumeTemplates")
	for _, t := range templates {
		dv, _, _ := unstructured.NestedString(t.(map[string]any), "metadata", "name")
		mf, err := s.copyDisk(ctx, ns, name, dv, ex.Token, rel+"/"+dv+".img.gz")
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
	if err := s.hostd.PutBackupFile(ctx, rel+"/vm.json", bytes.NewReader(b)); err != nil {
		return fail(err)
	}
	mb, _ := json.Marshal(man)
	if err := s.hostd.PutBackupFile(ctx, rel+"/manifest.json", bytes.NewReader(mb)); err != nil {
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

func (s *Server) copyDisk(ctx context.Context, ns, name, dv, token, rel string) (hostd.ManifestFile, error) {
	var mf hostd.ManifestFile
	src, cert, err := s.kube.BackupSource(ctx, ns, name, dv)
	if err != nil {
		return mf, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(cert))
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ResponseHeaderTimeout: time.Minute}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return mf, err
	}
	req.Header.Set("x-kubevirt-export-token", token)
	resp, err := hc.Do(req)
	if err != nil {
		return mf, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mf, fmt.Errorf("export returned %s", resp.Status)
	}
	hr := &hashReader{r: resp.Body, h: sha256.New()}
	if err := s.hostd.PutBackupFile(ctx, rel, hr); err != nil {
		return mf, err
	}
	return hostd.ManifestFile{SHA256: hex.EncodeToString(hr.h.Sum(nil)), Size: hr.n}, nil
}

func (s *Server) restoreBackup(ctx context.Context, vm, stamp, target string) error {
	rel := vm + "/" + stamp
	if err := s.hostd.VerifyBackup(ctx, rel); err != nil {
		return err
	}
	resp, err := s.hostd.GetBackupFile(ctx, rel+"/vm.json")
	if err != nil {
		return err
	}
	var meta backupMeta
	err = json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
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
	b := make([]byte, 24)
	rand.Read(b)
	token := hex.EncodeToString(b)
	ex := kube.Export{Spec: meta.Spec, Token: token, CA: string(s.caPEM)}
	for _, d := range meta.Disks {
		ex.Disks = append(ex.Disks, kube.ExportDisk{DataVolume: names[d], URL: fmt.Sprintf("https://%s/backups/%s/%s.img.gz", os.Getenv("HOST_IP"), rel, d)})
	}
	job := restoreJob{Rel: rel, Namespace: meta.Namespace, Target: target, Key: meta.Namespace + "/" + vm}
	s.trackRestore(token, job)
	if err := s.kube.ImportVM(ctx, meta.Namespace, target, "restore-"+stamp, ex); err != nil {
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
	store := s.auth.Secrets()
	data := map[string][]byte{"restores": b}
	_, rv, err := store.Get(ctx, restoresKey)
	if err != nil {
		err = store.Create(ctx, restoresKey, "Opaque", data)
	} else {
		err = store.Update(ctx, restoresKey, rv, data)
	}
	if err != nil {
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
	data, _, err := s.auth.Secrets().Get(context.Background(), restoresKey)
	if err != nil {
		return
	}
	var m map[string]restoreJob
	if json.Unmarshal(data["restores"], &m) != nil {
		return
	}
	s.backups.mu.Lock()
	s.backups.restores = m
	s.backups.mu.Unlock()
	for token, job := range m {
		go s.watchRestore(token, job)
	}
}

func (s *Server) watchRestore(token string, job restoreJob) {
	defer s.untrackRestore(token)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	failed := ""
	for ctx.Err() == nil {
		disks, err := s.kube.DiskStatus(ctx, job.Namespace, job.Target)
		done := err == nil && len(disks) > 0
		for _, d := range disks {
			if d.Phase == "Failed" {
				failed = d.Name
			}
			done = done && (d.Phase == "Succeeded" || d.Phase == "Failed")
		}
		if done {
			break
		}
		time.Sleep(10 * time.Second)
	}
	s.kube.FinishImport(context.Background(), job.Namespace, job.Target)
	switch {
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
	ok := s.backups.restores[strings.TrimSpace(r.Header.Get("x-kubevirt-export-token"))].Rel == rel
	s.backups.mu.Unlock()
	if !ok || s.hostd == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	resp, err := s.hostd.GetBackupFile(r.Context(), rel+"/"+r.PathValue("file"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Length", "Content-Type"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	n, _ := io.Copy(w, resp.Body)
	log.Printf("backup %s/%s served: %d bytes", rel, r.PathValue("file"), n)
}

type VMBackups struct {
	Policy  BackupPolicy   `json:"policy"`
	Enabled bool           `json:"enabled"`
	Target  string         `json:"target"`
	Status  string         `json:"status"`
	Backups []hostd.Backup `json:"backups"`
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
	info.Status = s.backups.status(ns + "/" + name)
	if s.hostd != nil {
		if st, err := s.hostd.Settings(ctx); err == nil {
			info.Target = st.Backup
		}
	}
	if info.Target != "" {
		bs, err := s.vmBackups(ctx, name)
		info.Backups, info.Error = bs, errText(err)
	}
	return info
}

func (s *Server) backupOp(ctx context.Context, ns, name, op string, f func(string) string) error {
	switch op {
	case "policy":
		every, _ := strconv.Atoi(f("every"))
		keep, _ := strconv.Atoi(f("keep"))
		p := BackupPolicy{Every: every, Keep: keep, Offsite: f("offsite") != ""}
		if every == 0 {
			return s.kube.SetBackupPolicy(ctx, ns, name, "")
		}
		if p.Offsite {
			if err := s.kube.CheckBackupable(ctx, ns, name); err != nil {
				return err
			}
		}
		if every < 1 || every > 24*30 || keep < 1 || keep > 365 {
			return errors.New("interval must be 1-720 hours and keep 1-365")
		}
		return s.kube.SetBackupPolicy(ctx, ns, name, p.String())
	case "now":
		vm, err := s.kube.GetVM(ctx, ns, name)
		if err != nil {
			return err
		}
		p, ok := parsePolicy(vm.Backup)
		if !ok {
			p = BackupPolicy{Every: 24, Keep: 7, Offsite: true}
		}
		if p.Offsite {
			if err := s.kube.CheckBackupable(ctx, ns, name); err != nil {
				return err
			}
		}
		if !s.startBackup(ns, name, p) {
			return errors.New("a backup of this VM is already running")
		}
		return nil
	case "restore":
		return s.restoreBackup(ctx, name, f("stamp"), strings.TrimSpace(f("target")))
	case "delete":
		return s.hostd.DeleteBackup(ctx, name+"/"+f("stamp"))
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

func (s *Server) downloadDisk(w http.ResponseWriter, r *http.Request) {
	ns, name, disk := r.PathValue("ns"), r.PathValue("name"), r.PathValue("disk")
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
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(cert))
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, ResponseHeaderTimeout: time.Minute}}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, src, nil)
	req.Header.Set("x-kubevirt-export-token", ex.Token)
	resp, err := hc.Do(req)
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
