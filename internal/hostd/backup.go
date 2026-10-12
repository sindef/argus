package hostd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
)

const (
	backupTargetFile = "/usr/local/argus/backup-target"
	backupMount      = "/run/argus-hostd/backup"
)

var (
	validTarget = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+:/[A-Za-z0-9._/-]*$`)
	validRel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[0-9]{8}-[0-9]{6}(/[A-Za-z0-9._-]+)?$`)
)

type Backup struct {
	VM       string   `json:"vm"`
	Stamp    string   `json:"stamp"`
	Size     int64    `json:"size"`
	Files    []string `json:"files"`
	Complete bool     `json:"complete"`
}

type Manifest struct {
	Files map[string]ManifestFile `json:"files"`
}

type ManifestFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const manifestFile = "manifest.json"

func backupTarget() string {
	b, _ := os.ReadFile(backupTargetFile)
	return strings.TrimSpace(string(b))
}

func mounted(dir string) bool {
	b, _ := os.ReadFile("/proc/mounts")
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == dir {
			return true
		}
	}
	return false
}

func mountTarget(ctx context.Context, target, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_, err := run(ctx, "mount", "-t", "nfs", "-o", "soft,timeo=150,retrans=3,nodev,nosuid,noexec", target, dir)
	return err
}

func backupDir(ctx context.Context) (string, error) {
	t := backupTarget()
	if t == "" {
		return "", errors.New("no backup target configured on this host")
	}
	if !mounted(backupMount) {
		if err := mountTarget(ctx, t, backupMount); err != nil {
			return "", err
		}
	}
	return backupMount, nil
}

func setBackupTarget(ctx context.Context, t string) error {
	if t == backupTarget() {
		return nil
	}
	if t != "" {
		if !validTarget.MatchString(t) {
			return errors.New("backup target must look like nfs-server:/export/path")
		}
		probe := backupMount + "-probe"
		if err := mountTarget(ctx, t, probe); err != nil {
			return err
		}
		err := probeWrite(probe)
		run(ctx, "umount", probe)
		if err != nil {
			return fmt.Errorf("backup target is not writable: %w", err)
		}
	}
	if mounted(backupMount) {
		if _, err := run(ctx, "umount", backupMount); err != nil {
			return err
		}
	}
	return writeConf(backupTargetFile, map[bool]string{true: t + "\n", false: ""}[t != ""])
}

func probeWrite(dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(".argus-write-test", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return root.Remove(".argus-write-test")
}

func backupRoot(ctx context.Context) (*os.Root, error) {
	dir, err := backupDir(ctx)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func listBackups(ctx context.Context) ([]Backup, error) {
	root, err := backupRoot(ctx)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	fsys := root.FS()
	var out []Backup
	vms, _ := fs.ReadDir(fsys, ".")
	for _, vm := range vms {
		if !vm.IsDir() {
			continue
		}
		stamps, _ := fs.ReadDir(fsys, vm.Name())
		for _, st := range stamps {
			if !st.IsDir() || !validRel.MatchString(vm.Name()+"/"+st.Name()) {
				continue
			}
			b := Backup{VM: vm.Name(), Stamp: st.Name()}
			files, _ := fs.ReadDir(fsys, vm.Name()+"/"+st.Name())
			for _, f := range files {
				if info, err := f.Info(); err == nil && info.Mode().IsRegular() && !strings.HasPrefix(f.Name(), ".") {
					b.Size += info.Size()
					b.Files = append(b.Files, f.Name())
					b.Complete = b.Complete || f.Name() == manifestFile
				}
			}
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

func (s *Server) backupFile(w http.ResponseWriter, r *http.Request) {
	rel := r.PathValue("rel")
	if !validRel.MatchString(rel) || strings.Count(rel, "/") != 2 {
		reply(w, nil, errors.New("invalid backup path"))
		return
	}
	root, err := backupRoot(r.Context())
	if err != nil {
		reply(w, nil, err)
		return
	}
	defer root.Close()
	switch r.Method {
	case http.MethodGet:
		f, err := root.Open(rel)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.Error(w, "not a regular file", http.StatusNotFound)
			return
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	case http.MethodPut:
		reply(w, nil, writeStream(root, rel, r.Body))
	}
}

func writeStream(root *os.Root, rel string, body io.Reader) error {
	dir := path.Dir(rel)
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := dir + "/." + path.Base(rel) + ".part"
	root.Remove(tmp)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		root.Remove(tmp)
		return err
	}
	return root.Rename(tmp, rel)
}

func deleteBackup(ctx context.Context, rel string) error {
	if !validRel.MatchString(rel) || strings.Count(rel, "/") != 1 {
		return errors.New("invalid backup path")
	}
	root, err := backupRoot(ctx)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(rel)
}
