package hostd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	VM    string   `json:"vm"`
	Stamp string   `json:"stamp"`
	Size  int64    `json:"size"`
	Files []string `json:"files"`
}

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
	_, err := run(ctx, "mount", "-t", "nfs", "-o", "soft,timeo=150,retrans=3", target, dir)
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
		test := filepath.Join(probe, ".argus-write-test")
		err := os.WriteFile(test, []byte("ok"), 0o600)
		os.Remove(test)
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

func backupPath(ctx context.Context, rel string) (string, error) {
	if !validRel.MatchString(rel) {
		return "", errors.New("invalid backup path")
	}
	dir, err := backupDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, rel), nil
}

func listBackups(ctx context.Context) ([]Backup, error) {
	dir, err := backupDir(ctx)
	if err != nil {
		return nil, err
	}
	var out []Backup
	vms, _ := os.ReadDir(dir)
	for _, vm := range vms {
		stamps, _ := os.ReadDir(filepath.Join(dir, vm.Name()))
		for _, st := range stamps {
			if !validRel.MatchString(vm.Name() + "/" + st.Name()) {
				continue
			}
			b := Backup{VM: vm.Name(), Stamp: st.Name()}
			files, _ := os.ReadDir(filepath.Join(dir, vm.Name(), st.Name()))
			for _, f := range files {
				if info, err := f.Info(); err == nil && !strings.HasPrefix(f.Name(), ".") {
					b.Size += info.Size()
					b.Files = append(b.Files, f.Name())
				}
			}
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

func (s *Server) backupFile(w http.ResponseWriter, r *http.Request) {
	path, err := backupPath(r.Context(), r.PathValue("rel"))
	if err != nil {
		reply(w, nil, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		http.ServeFile(w, r, path)
	case http.MethodPut:
		err = writeStream(path, r.Body)
		reply(w, nil, err)
	}
}

func writeStream(path string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".part")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func deleteBackup(ctx context.Context, rel string) error {
	if strings.Count(rel, "/") != 1 {
		return errors.New("invalid backup path")
	}
	path, err := backupPath(ctx, rel)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
