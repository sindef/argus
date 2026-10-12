package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"argus/internal/hostd"
	"argus/internal/s3"
)

const targetsKey = "argus-backup-targets"

var stampRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

type BackupTarget struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
}

func (t BackupTarget) Where() string {
	if t.Kind == "s3" {
		return strings.TrimRight(t.Endpoint, "/") + "/" + t.Bucket + "/" + t.Prefix
	}
	return t.Endpoint
}

type backupStore interface {
	Put(ctx context.Context, rel string, r io.Reader) error
	Get(ctx context.Context, rel string) (io.ReadCloser, int64, error)
	List(ctx context.Context) ([]hostd.Backup, error)
	Delete(ctx context.Context, rel string) error
	Verify(ctx context.Context, rel string) error
}

func (s *Server) backupTargets(ctx context.Context) ([]BackupTarget, error) {
	var out []BackupTarget
	if s.hostd != nil {
		st, err := s.hostd.Settings(ctx)
		if err != nil {
			return nil, err
		}
		if st.Backup != "" {
			out = append(out, BackupTarget{Name: "nfs", Kind: "nfs", Endpoint: st.Backup})
		}
	}
	var extra []BackupTarget
	if _, err := s.loadJSON(ctx, targetsKey, &extra); err != nil {
		return nil, err
	}
	return append(out, extra...), nil
}

func (s *Server) store(ctx context.Context, name string) (backupStore, error) {
	targets, err := s.backupTargets(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(targets, func(t BackupTarget) bool { return t.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("backup target %q is not configured on this host", name)
	}
	return targets[i].open(s.hostd), nil
}

func (t BackupTarget) open(h *hostd.Client) backupStore {
	if t.Kind == "s3" {
		prefix := strings.Trim(t.Prefix, "/")
		if prefix != "" {
			prefix += "/"
		}
		return s3Store{c: &s3.Client{Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket, AccessKey: t.AccessKey, SecretKey: t.SecretKey}, prefix: prefix}
	}
	return hostdStore{h}
}

func (s *Server) saveTarget(ctx context.Context, t BackupTarget, remove bool) error {
	var targets []BackupTarget
	rv, err := s.loadJSON(ctx, targetsKey, &targets)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(targets, func(x BackupTarget) bool { return x.Name == t.Name })
	switch {
	case remove && i < 0:
		return fmt.Errorf("backup target %s not found", t.Name)
	case remove:
		targets = slices.Delete(targets, i, i+1)
	default:
		if !validObject.MatchString(t.Name) || t.Name == "nfs" {
			return errors.New("name must be lowercase letters, digits and dashes, and not nfs")
		}
		if u, err := url.Parse(t.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New("endpoint must be an http or https URL")
		}
		if t.Bucket == "" || t.AccessKey == "" {
			return errors.New("bucket and access key are required")
		}
		if i >= 0 && t.SecretKey == "" {
			t.SecretKey = targets[i].SecretKey
		}
		if t.SecretKey == "" {
			return errors.New("secret key is required")
		}
		probe := ".argus-probe-" + newToken()[:12]
		st := t.open(nil).(s3Store)
		if err := st.c.Put(ctx, st.prefix+probe, bytes.NewReader([]byte("ok"))); err != nil {
			return fmt.Errorf("cannot write to the bucket: %w", err)
		}
		if err := st.c.Delete(ctx, st.prefix+probe); err != nil {
			return fmt.Errorf("cannot delete from the bucket: %w", err)
		}
		if i >= 0 {
			targets[i] = t
		} else {
			targets = append(targets, t)
		}
	}
	return s.saveJSON(ctx, targetsKey, rv, targets)
}

type hostdStore struct{ c *hostd.Client }

func (h hostdStore) Put(ctx context.Context, rel string, r io.Reader) error {
	return h.c.PutBackupFile(ctx, rel, r)
}

func (h hostdStore) Get(ctx context.Context, rel string) (io.ReadCloser, int64, error) {
	resp, err := h.c.GetBackupFile(ctx, rel)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (h hostdStore) List(ctx context.Context) ([]hostd.Backup, error) { return h.c.Backups(ctx) }
func (h hostdStore) Delete(ctx context.Context, rel string) error     { return h.c.DeleteBackup(ctx, rel) }
func (h hostdStore) Verify(ctx context.Context, rel string) error     { return h.c.VerifyBackup(ctx, rel) }

type s3Store struct {
	c      *s3.Client
	prefix string
}

func (s s3Store) Put(ctx context.Context, rel string, r io.Reader) error {
	return s.c.Put(ctx, s.prefix+rel, r)
}

func (s s3Store) Get(ctx context.Context, rel string) (io.ReadCloser, int64, error) {
	resp, err := s.c.Get(ctx, s.prefix+rel)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

func (s s3Store) List(ctx context.Context) ([]hostd.Backup, error) {
	objs, err := s.c.List(ctx, s.prefix)
	if err != nil {
		return nil, err
	}
	byRel := map[string]*hostd.Backup{}
	for _, o := range objs {
		parts := strings.Split(strings.TrimPrefix(o.Key, s.prefix), "/")
		if len(parts) != 3 || !validObject.MatchString(parts[0]) || !stampRE.MatchString(parts[1]) {
			continue
		}
		rel := parts[0] + "/" + parts[1]
		b := byRel[rel]
		if b == nil {
			b = &hostd.Backup{VM: parts[0], Stamp: parts[1]}
			byRel[rel] = b
		}
		b.Size += o.Size
		b.Files = append(b.Files, parts[2])
		b.Complete = b.Complete || parts[2] == "manifest.json"
	}
	var out []hostd.Backup
	for _, b := range byRel {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stamp > out[j].Stamp })
	return out, nil
}

func (s s3Store) Delete(ctx context.Context, rel string) error {
	objs, err := s.c.List(ctx, s.prefix+rel+"/")
	if err != nil {
		return err
	}
	for _, o := range objs {
		if err := s.c.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

func (s s3Store) Verify(ctx context.Context, rel string) error {
	resp, err := s.c.Get(ctx, s.prefix+rel+"/manifest.json")
	if err != nil {
		return fmt.Errorf("backup has no manifest; it did not finish: %w", err)
	}
	var man hostd.Manifest
	err = json.NewDecoder(resp.Body).Decode(&man)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("backup manifest: %w", err)
	}
	objs, err := s.c.List(ctx, s.prefix+rel+"/")
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	for _, o := range objs {
		sizes[strings.TrimPrefix(o.Key, s.prefix+rel+"/")] = o.Size
	}
	for name, f := range man.Files {
		if n, ok := sizes[name]; !ok || n != f.Size {
			return fmt.Errorf("backup file %s is missing or the wrong size", name)
		}
	}
	return nil
}
