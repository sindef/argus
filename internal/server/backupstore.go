package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"argus/internal/sealed"
	"argus/internal/ui"
)

const (
	targetsKey = "argus-backup-targets"
	nfsKey     = "argus-backup-nfs"
)

type nfsSettings struct {
	Passphrase string `json:"passphrase"`
}

func (s *Server) nfsPassphrase(ctx context.Context) (string, error) {
	var n nfsSettings
	_, err := s.LoadJSON(ctx, nfsKey, &n)
	return n.Passphrase, err
}

func (s *Server) setNFSPassphrase(ctx context.Context, pass string) error {
	var n nfsSettings
	rv, err := s.LoadJSON(ctx, nfsKey, &n)
	if err != nil {
		return err
	}
	return s.SaveJSON(ctx, nfsKey, rv, nfsSettings{pass})
}

var stampRE = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

type BackupTarget struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Endpoint   string `json:"endpoint"`
	Region     string `json:"region,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	AccessKey  string `json:"accessKey,omitempty"`
	SecretKey  string `json:"secretKey,omitempty"`
	Passphrase string `json:"passphrase,omitempty"`
	LockDays   int    `json:"lockDays,omitempty"`
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
}

func (s *Server) backupTargets(ctx context.Context) ([]BackupTarget, error) {
	var out []BackupTarget
	if s.hostd != nil {
		st, err := s.hostd.Settings(ctx)
		if err != nil {
			return nil, err
		}
		if st.Backup != "" {
			pass, err := s.nfsPassphrase(ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, BackupTarget{Name: "nfs", Kind: "nfs", Endpoint: st.Backup, Passphrase: pass})
		}
	}
	var extra []BackupTarget
	if _, err := s.LoadJSON(ctx, targetsKey, &extra); err != nil {
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
	var st backupStore = hostdStore{h}
	if t.Kind == "s3" {
		prefix := strings.Trim(t.Prefix, "/")
		if prefix != "" {
			prefix += "/"
		}
		st = s3Store{c: &s3.Client{Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket, AccessKey: t.AccessKey, SecretKey: t.SecretKey, LockDays: t.LockDays}, prefix: prefix}
	}
	if t.Passphrase != "" {
		st = encryptedStore{st, t.Passphrase}
	}
	return st
}

type encryptedStore struct {
	backupStore
	passphrase string
}

func (e encryptedStore) Put(ctx context.Context, rel string, r io.Reader) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(sealed.Encrypt(pw, r, e.passphrase)) }()
	err := e.backupStore.Put(ctx, rel, pr)
	pr.CloseWithError(err)
	return err
}

func (e encryptedStore) Get(ctx context.Context, rel string) (io.ReadCloser, int64, error) {
	body, size, err := e.backupStore.Get(ctx, rel)
	if err != nil {
		return nil, 0, err
	}
	dec, encrypted, err := sealed.Decrypt(body, e.passphrase)
	if err != nil {
		body.Close()
		return nil, 0, err
	}
	if encrypted {
		size = sealed.PlainSize(size)
	}
	return struct {
		io.Reader
		io.Closer
	}{dec, body}, size, nil
}

func (s *Server) saveTarget(ctx context.Context, t BackupTarget, remove bool) error {
	var targets []BackupTarget
	rv, err := s.LoadJSON(ctx, targetsKey, &targets)
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
		if !ui.ValidObject.MatchString(t.Name) || t.Name == "nfs" {
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
		if i >= 0 && t.Passphrase == "" {
			t.Passphrase = targets[i].Passphrase
		}
		if t.LockDays < 0 || t.LockDays > 3650 {
			return errors.New("immutability must be 0-3650 days")
		}
		if t.SecretKey == "" {
			return errors.New("secret key is required")
		}
		probe := ".argus-probe-" + ui.NewToken()[:12]
		probeTarget := t
		probeTarget.Passphrase, probeTarget.LockDays = "", 0
		st := probeTarget.open(nil).(s3Store)
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
	return s.SaveJSON(ctx, targetsKey, rv, targets)
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
		if len(parts) != 3 || !ui.ValidObject.MatchString(parts[0]) || !stampRE.MatchString(parts[1]) {
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

func manifestOf(ctx context.Context, st backupStore, rel string) (hostd.Manifest, error) {
	var man hostd.Manifest
	body, _, err := st.Get(ctx, rel+"/manifest.json")
	if err != nil {
		return man, errors.New("backup has no manifest; it did not finish")
	}
	err = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&man)
	body.Close()
	var syntax *json.SyntaxError
	if err != nil && !errors.As(err, &syntax) {
		return man, err
	}
	if err != nil || len(man.Files) == 0 {
		return man, errors.New("backup manifest is unreadable")
	}
	return man, nil
}

func checkBackup(ctx context.Context, st backupStore, rel string) error {
	man, err := manifestOf(ctx, st, rel)
	if err != nil {
		return err
	}
	for name, f := range man.Files {
		body, size, err := st.Get(ctx, rel+"/"+name)
		if err != nil {
			return fmt.Errorf("backup file %s is missing", name)
		}
		body.Close()
		if size != f.Size {
			return fmt.Errorf("backup file %s is the wrong size", name)
		}
	}
	return nil
}

const verifyTail = 1 << 20

var errChecksum = errors.New("does not match its checksum")

func verifiedCopy(w io.Writer, r io.Reader, want hostd.ManifestFile) (int64, error) {
	h := sha256.New()
	buf := make([]byte, 0, 2*verifyTail)
	var n int64
	chunk := make([]byte, 256<<10)
	for {
		k, err := r.Read(chunk)
		h.Write(chunk[:k])
		buf = append(buf, chunk[:k]...)
		if len(buf) > verifyTail {
			out := len(buf) - verifyTail
			if _, werr := w.Write(buf[:out]); werr != nil {
				return n, werr
			}
			n += int64(out)
			buf = append(buf[:0], buf[out:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, err
		}
	}
	if n+int64(len(buf)) != want.Size || hex.EncodeToString(h.Sum(nil)) != want.SHA256 {
		return n, errChecksum
	}
	k, err := w.Write(buf)
	return n + int64(k), err
}
