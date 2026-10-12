package registry

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Access int

const (
	Pull Access = iota
	Push
)

const (
	ociManifest   = "application/vnd.oci.image.manifest.v1+json"
	ociConfig     = "application/vnd.oci.image.config.v1+json"
	ociLayer      = "application/vnd.oci.image.layer.v1.tar"
	maxManifest   = 4 << 20
	staleAfter    = time.Hour
	staleUploads  = 24 * time.Hour
	kubevirtOwner = 107
)

var (
	repoRE   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uploadRE = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type Registry struct {
	root      string
	Authorize func(r *http.Request, repo string, a Access) bool
	mu        sync.Mutex
}

type Image struct {
	Repo    string
	Tag     string
	Digest  string
	Size    int64
	Created time.Time
}

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        *descriptor  `json:"config,omitempty"`
	Layers        []descriptor `json:"layers,omitempty"`
	Manifests     []descriptor `json:"manifests,omitempty"`
}

func New(root string) (*Registry, error) {
	for _, d := range []string{"blobs/sha256", "repos", "uploads"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Registry{root: root}, nil
}

func (g *Registry) blob(d string) string {
	return filepath.Join(g.root, "blobs", "sha256", strings.TrimPrefix(d, "sha256:"))
}
func (g *Registry) repo(r string) string {
	return filepath.Join(g.root, "repos", filepath.FromSlash(r))
}
func (g *Registry) layerLink(r, d string) string {
	return filepath.Join(g.repo(r), "_layers", strings.TrimPrefix(d, "sha256:"))
}
func (g *Registry) manifestLink(r, d string) string {
	return filepath.Join(g.repo(r), "_manifests", strings.TrimPrefix(d, "sha256:"))
}
func (g *Registry) tag(r, t string) string  { return filepath.Join(g.repo(r), "_tags", t) }
func (g *Registry) upload(id string) string { return filepath.Join(g.root, "uploads", id) }
func exists(p string) bool                  { _, err := os.Stat(p); return err == nil }
func digestOf(b []byte) string              { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

func touch(p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, nil, 0o600)
}

func writeAtomic(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func (g *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := strings.TrimPrefix(r.URL.Path, "/v2/")
	access := Pull
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		access = Push
	}
	var repo, kind, ref string
	switch {
	case p == "" || p == "_catalog":
		kind = p
	case strings.HasSuffix(p, "/tags/list"):
		repo, kind = strings.TrimSuffix(p, "/tags/list"), "tags"
	case strings.Contains(p, "/blobs/uploads/"):
		i := strings.LastIndex(p, "/blobs/uploads/")
		repo, kind, ref = p[:i], "upload", p[i+len("/blobs/uploads/"):]
	case strings.Contains(p, "/manifests/"):
		i := strings.LastIndex(p, "/manifests/")
		repo, kind, ref = p[:i], "manifest", p[i+len("/manifests/"):]
	case strings.Contains(p, "/blobs/"):
		i := strings.LastIndex(p, "/blobs/")
		repo, kind, ref = p[:i], "blob", p[i+len("/blobs/"):]
	default:
		fail(w, http.StatusNotFound, "NOT_FOUND", "unknown path")
		return
	}
	if repo != "" && !repoRE.MatchString(repo) {
		fail(w, http.StatusBadRequest, "NAME_INVALID", "invalid repository name")
		return
	}
	if kind != "_catalog" && (g.Authorize == nil || !g.Authorize(r, repo, access)) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Argus Manager"`)
		fail(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	switch kind {
	case "":
		w.WriteHeader(http.StatusOK)
	case "_catalog":
		g.serveCatalog(w, r)
	case "tags":
		g.serveTags(w, repo)
	case "manifest":
		g.serveManifest(w, r, repo, ref)
	case "blob":
		g.serveBlob(w, r, repo, ref)
	case "upload":
		g.serveUpload(w, r, repo, ref)
	}
}

func (g *Registry) repos(withLayers bool) []string {
	var out []string
	base := filepath.Join(g.root, "repos")
	filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), "_") {
			rel, _ := filepath.Rel(base, filepath.Dir(p))
			if (d.Name() == "_manifests" || withLayers && d.Name() == "_layers") && !slices.Contains(out, filepath.ToSlash(rel)) {
				out = append(out, filepath.ToSlash(rel))
			}
			return filepath.SkipDir
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func (g *Registry) serveCatalog(w http.ResponseWriter, r *http.Request) {
	repos := []string{}
	for _, repo := range g.repos(false) {
		if g.Authorize != nil && g.Authorize(r, repo, Pull) {
			repos = append(repos, repo)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
}

func (g *Registry) tags(repo string) []string {
	ents, _ := os.ReadDir(filepath.Join(g.repo(repo), "_tags"))
	out := []string{}
	for _, e := range ents {
		if tagRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func (g *Registry) serveTags(w http.ResponseWriter, repo string) {
	if !exists(filepath.Join(g.repo(repo), "_manifests")) {
		fail(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": g.tags(repo)})
}

func (g *Registry) resolve(repo, ref string) (string, string, error) {
	d := ref
	if !digestRE.MatchString(ref) {
		if !tagRE.MatchString(ref) {
			return "", "", errors.New("invalid reference")
		}
		b, err := os.ReadFile(g.tag(repo, ref))
		if err != nil {
			return "", "", err
		}
		d = string(b)
	}
	ct, err := os.ReadFile(g.manifestLink(repo, d))
	if err != nil {
		return "", "", err
	}
	return d, string(ct), nil
}

func (g *Registry) serveManifest(w http.ResponseWriter, r *http.Request, repo, ref string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		d, ct, err := g.resolve(repo, ref)
		if err != nil {
			fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
			return
		}
		b, err := os.ReadFile(g.blob(d))
		if err != nil {
			fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not found")
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		if r.Method == http.MethodGet {
			w.Write(b)
		}
	case http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, maxManifest+1))
		if err != nil || len(b) > maxManifest {
			fail(w, http.StatusBadRequest, "MANIFEST_INVALID", "manifest too large or unreadable")
			return
		}
		d, err := g.putManifest(repo, ref, r.Header.Get("Content-Type"), b)
		if err != nil {
			fail(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		w.Header().Set("Location", "/v2/"+repo+"/manifests/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if err := g.deleteRef(repo, ref); err != nil {
			fail(w, http.StatusNotFound, "MANIFEST_UNKNOWN", err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

var ErrNotDisk = errors.New("not a disk image: expected qcow2, raw, ISO, VMDK, VHDX or VDI")

const sniffLen = 0x8006

func isDisk(b []byte) bool {
	at := func(off int, magic string) bool {
		return len(b) >= off+len(magic) && string(b[off:off+len(magic)]) == magic
	}
	return at(0, "QFI\xfb") || at(0, "KDMV") || at(0, "vhdxfile") || at(0x40, "\x7f\x10\xda\xbe") || at(0x8001, "CD001") || at(510, "\x55\xaa") || at(512, "EFI PART")
}

func sniff(r io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(r, sniffLen)
	b, err := br.Peek(sniffLen)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if !isDisk(b) {
		return nil, ErrNotDisk
	}
	return br, nil
}

func (g *Registry) checkDisk(m manifest) error {
	for _, l := range m.Layers {
		f, err := os.Open(g.blob(l.Digest))
		if err != nil {
			return err
		}
		defer f.Close()
		br := bufio.NewReader(f)
		var r io.Reader = br
		switch magic, _ := br.Peek(4); {
		case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
			zr, err := gzip.NewReader(br)
			if err != nil {
				return err
			}
			r = zr
		case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
			return errors.New("zstd-compressed layers are not supported; push with gzip or no compression")
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg && strings.HasPrefix(path.Clean(h.Name), "disk/") {
				_, err := sniff(tr)
				return err
			}
		}
	}
	if len(m.Layers) > 0 {
		return errors.New("not a disk image: no file under /disk")
	}
	return nil
}

func (g *Registry) putManifest(repo, ref, ct string, b []byte) (string, error) {
	return g.storeManifest(repo, ref, ct, b, true)
}

func (g *Registry) storeManifest(repo, ref, ct string, b []byte, check bool) (string, error) {
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil || m.SchemaVersion != 2 {
		return "", errors.New("manifest must be schema version 2 JSON")
	}
	if ct == "" || strings.HasPrefix(ct, "application/json") {
		ct = m.MediaType
	}
	if ct == "" {
		ct = ociManifest
	}
	d := digestOf(b)
	isTag := !digestRE.MatchString(ref)
	if isTag && !tagRE.MatchString(ref) || !isTag && ref != d {
		return "", errors.New("reference does not match the manifest")
	}
	for _, c := range m.refs() {
		if !exists(g.layerLink(repo, c.Digest)) && !exists(g.manifestLink(repo, c.Digest)) {
			return "", fmt.Errorf("blob %s is not in %s", c.Digest, repo)
		}
	}
	if check {
		if err := g.checkDisk(m); err != nil {
			return "", err
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range m.refs() {
		if !exists(g.layerLink(repo, c.Digest)) && !exists(g.manifestLink(repo, c.Digest)) {
			return "", fmt.Errorf("blob %s is not in %s", c.Digest, repo)
		}
	}
	if !exists(g.blob(d)) {
		if err := writeAtomic(g.blob(d), b); err != nil {
			return "", err
		}
	}
	if err := writeAtomic(g.manifestLink(repo, d), []byte(ct)); err != nil {
		return "", err
	}
	if isTag {
		if err := writeAtomic(g.tag(repo, ref), []byte(d)); err != nil {
			return "", err
		}
	}
	return d, nil
}

func (m manifest) refs() []descriptor {
	out := slices.Clone(m.Layers)
	if m.Config != nil {
		out = append(out, *m.Config)
	}
	return append(out, m.Manifests...)
}

func (g *Registry) deleteRef(repo, ref string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !digestRE.MatchString(ref) {
		if !tagRE.MatchString(ref) || !exists(g.tag(repo, ref)) {
			return errors.New("tag not found")
		}
		return os.Remove(g.tag(repo, ref))
	}
	if !exists(g.manifestLink(repo, ref)) {
		return errors.New("manifest not found")
	}
	for _, t := range g.tags(repo) {
		if b, _ := os.ReadFile(g.tag(repo, t)); string(b) == ref {
			os.Remove(g.tag(repo, t))
		}
	}
	return os.Remove(g.manifestLink(repo, ref))
}

func (g *Registry) serveBlob(w http.ResponseWriter, r *http.Request, repo, d string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	if !digestRE.MatchString(d) || !exists(g.layerLink(repo, d)) && !exists(g.manifestLink(repo, d)) {
		fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}
	f, err := os.Open(g.blob(d))
	if err != nil {
		fail(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob not found")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d)
	w.Header().Set("ETag", `"`+d+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (g *Registry) serveUpload(w http.ResponseWriter, r *http.Request, repo, id string) {
	if id == "" {
		if r.Method != http.MethodPost {
			fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
			return
		}
		g.startUpload(w, r, repo)
		return
	}
	owner, err := os.ReadFile(g.upload(id) + ".repo")
	if !uploadRE.MatchString(id) || err != nil || string(owner) != repo {
		fail(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}
	loc := "/v2/" + repo + "/blobs/uploads/" + id
	switch r.Method {
	case http.MethodGet:
		g.uploadStatus(w, id, loc, http.StatusNoContent)
	case http.MethodPatch:
		if err := g.appendUpload(id, r); err != nil {
			fail(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		g.uploadStatus(w, id, loc, http.StatusAccepted)
	case http.MethodPut:
		d := r.URL.Query().Get("digest")
		if err := g.appendUpload(id, r); err != nil {
			fail(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		if err := g.commit(repo, g.upload(id), d); err != nil {
			fail(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
			return
		}
		os.Remove(g.upload(id) + ".repo")
		w.Header().Set("Location", "/v2/"+repo+"/blobs/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		os.Remove(g.upload(id))
		os.Remove(g.upload(id) + ".repo")
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (g *Registry) startUpload(w http.ResponseWriter, r *http.Request, repo string) {
	q := r.URL.Query()
	if d, from := q.Get("mount"), q.Get("from"); digestRE.MatchString(d) && repoRE.MatchString(from) && exists(g.layerLink(from, d)) && g.Authorize(r, from, Pull) {
		g.mu.Lock()
		err := touch(g.layerLink(repo, d))
		g.mu.Unlock()
		if err == nil {
			w.Header().Set("Location", "/v2/"+repo+"/blobs/"+d)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}
	id := newID()
	if err := os.WriteFile(g.upload(id)+".repo", []byte(repo), 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	if err := os.WriteFile(g.upload(id), nil, 0o600); err != nil {
		fail(w, http.StatusInternalServerError, "UNKNOWN", err.Error())
		return
	}
	loc := "/v2/" + repo + "/blobs/uploads/" + id
	if d := q.Get("digest"); d != "" {
		if err := g.appendUpload(id, r); err != nil {
			fail(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", err.Error())
			return
		}
		if err := g.commit(repo, g.upload(id), d); err != nil {
			fail(w, http.StatusBadRequest, "DIGEST_INVALID", err.Error())
			return
		}
		os.Remove(g.upload(id) + ".repo")
		w.Header().Set("Location", "/v2/"+repo+"/blobs/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
		return
	}
	g.uploadStatus(w, id, loc, http.StatusAccepted)
}

func (g *Registry) uploadStatus(w http.ResponseWriter, id, loc string, status int) {
	st, err := os.Stat(g.upload(id))
	if err != nil {
		fail(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload not found")
		return
	}
	w.Header().Set("Location", loc)
	w.Header().Set("Docker-Upload-UUID", id)
	w.Header().Set("Range", fmt.Sprintf("0-%d", max(st.Size()-1, 0)))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

func (g *Registry) appendUpload(id string, r *http.Request) error {
	f, err := os.OpenFile(g.upload(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if cr := r.Header.Get("Content-Range"); cr != "" {
		st, _ := f.Stat()
		start, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes="), "-")
		if n, err := strconv.ParseInt(start, 10, 64); err != nil || n != st.Size() {
			return fmt.Errorf("upload is at offset %d", st.Size())
		}
	}
	if _, err := io.Copy(f, r.Body); err != nil {
		return err
	}
	return f.Sync()
}

func (g *Registry) commit(repo, path, d string) error {
	if !digestRE.MatchString(d) {
		return errors.New("digest must be sha256")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != d {
		os.Remove(path)
		return fmt.Errorf("content is %s, not %s", got, d)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if exists(g.blob(d)) {
		os.Remove(path)
	} else if err := os.Rename(path, g.blob(d)); err != nil {
		return err
	}
	now := time.Now()
	os.Chtimes(g.blob(d), now, now)
	return touch(g.layerLink(repo, d))
}

func (g *Registry) putBlob(repo string, b []byte) (descriptor, error) {
	d := digestOf(b)
	p := g.upload(newID())
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return descriptor{}, err
	}
	return descriptor{Digest: d, Size: int64(len(b))}, g.commit(repo, p, d)
}

func (g *Registry) PutDisk(repo, tag, file string, size int64, src io.Reader) (string, error) {
	if !repoRE.MatchString(repo) || !tagRE.MatchString(tag) {
		return "", errors.New("invalid image name or tag")
	}
	src, err := sniff(src)
	if err != nil {
		return "", err
	}
	tmp := g.upload(newID())
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(f, h))
	now := time.Now()
	err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "disk/", Mode: 0o555, Uid: kubevirtOwner, Gid: kubevirtOwner, ModTime: now})
	if err == nil {
		err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "disk/" + filepath.Base(file), Mode: 0o444, Size: size, Uid: kubevirtOwner, Gid: kubevirtOwner, ModTime: now})
	}
	if err == nil {
		var n int64
		if n, err = io.Copy(tw, src); err == nil && n != size {
			err = fmt.Errorf("expected %d bytes, got %d", size, n)
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	st, serr := f.Stat()
	f.Close()
	if err = errors.Join(err, serr); err != nil {
		return "", err
	}
	layer := descriptor{MediaType: ociLayer, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: st.Size()}
	if err := g.commit(repo, tmp, layer.Digest); err != nil {
		return "", err
	}
	cfg, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "created": now.UTC().Format(time.RFC3339), "rootfs": map[string]any{"type": "layers", "diff_ids": []string{layer.Digest}}})
	config, err := g.putBlob(repo, cfg)
	if err != nil {
		return "", err
	}
	config.MediaType = ociConfig
	m, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: ociManifest, Config: &config, Layers: []descriptor{layer}})
	return g.storeManifest(repo, tag, ociManifest, m, false)
}

func (g *Registry) List(prefix string) []Image {
	var out []Image
	for _, repo := range g.repos(false) {
		if !strings.HasPrefix(repo, prefix) {
			continue
		}
		for _, t := range g.tags(repo) {
			d, _, err := g.resolve(repo, t)
			if err != nil {
				continue
			}
			img := Image{Repo: repo, Tag: t, Digest: d}
			if st, err := os.Stat(g.tag(repo, t)); err == nil {
				img.Created = st.ModTime()
			}
			var m manifest
			if b, err := os.ReadFile(g.blob(d)); err == nil && json.Unmarshal(b, &m) == nil {
				for _, l := range m.Layers {
					img.Size += l.Size
				}
			}
			out = append(out, img)
		}
	}
	return out
}

func (g *Registry) Delete(repo, tag string) error {
	d, _, err := g.resolve(repo, tag)
	if err != nil {
		return fmt.Errorf("%s:%s not found", repo, tag)
	}
	if err := g.deleteRef(repo, tag); err != nil {
		return err
	}
	for _, t := range g.tags(repo) {
		if b, _ := os.ReadFile(g.tag(repo, t)); string(b) == d {
			return g.GC()
		}
	}
	if err := g.deleteRef(repo, d); err != nil {
		return err
	}
	return g.GC()
}

func (g *Registry) GC() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	old := func(p string, age time.Duration) bool {
		st, err := os.Stat(p)
		return err == nil && time.Since(st.ModTime()) > age
	}
	used := map[string]bool{}
	for _, repo := range g.repos(true) {
		keep := map[string]bool{}
		ents, _ := os.ReadDir(filepath.Join(g.repo(repo), "_manifests"))
		for _, e := range ents {
			d := "sha256:" + e.Name()
			keep[d] = true
			var m manifest
			if b, err := os.ReadFile(g.blob(d)); err == nil && json.Unmarshal(b, &m) == nil {
				for _, c := range m.refs() {
					keep[c.Digest] = true
				}
			}
		}
		links, _ := os.ReadDir(filepath.Join(g.repo(repo), "_layers"))
		live := 0
		for _, l := range links {
			d, p := "sha256:"+l.Name(), filepath.Join(g.repo(repo), "_layers", l.Name())
			if !keep[d] && old(p, staleAfter) {
				os.Remove(p)
				continue
			}
			keep[d] = true
			live++
		}
		for d := range keep {
			used[d] = true
		}
		if len(ents) == 0 && live == 0 && len(g.tags(repo)) == 0 {
			os.RemoveAll(g.repo(repo))
		}
	}
	blobs, err := os.ReadDir(filepath.Join(g.root, "blobs", "sha256"))
	if err != nil {
		return err
	}
	for _, b := range blobs {
		p := filepath.Join(g.root, "blobs", "sha256", b.Name())
		if !used["sha256:"+b.Name()] && old(p, staleAfter) {
			os.Remove(p)
		}
	}
	ups, _ := os.ReadDir(filepath.Join(g.root, "uploads"))
	for _, u := range ups {
		if p := filepath.Join(g.root, "uploads", u.Name()); old(p, staleUploads) {
			os.Remove(p)
		}
	}
	return nil
}
