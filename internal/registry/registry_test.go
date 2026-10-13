package registry

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func do(t *testing.T, srv *httptest.Server, method, path string, body []byte, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPushPullDelete(t *testing.T) {
	g, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.Authorize = func(r *http.Request, repo string, a Access) bool {
		u, _, _ := r.BasicAuth()
		return u == "ok" && (repo == "" || strings.HasPrefix(repo, "org/"))
	}
	srv := httptest.NewServer(g)
	defer srv.Close()
	basic := []string{"Authorization", "Basic b2s6eA=="}

	if resp := do(t, srv, "GET", "/v2/", nil); resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("ping without auth: %d", resp.StatusCode)
	}
	if resp := do(t, srv, "GET", "/v2/other/x/tags/list", nil, basic...); resp.StatusCode != 401 {
		t.Fatalf("foreign repo: %d", resp.StatusCode)
	}

	layer := tarLayer(t, "disk/a.img", rawDisk())
	ld := digestOf(layer)
	resp := do(t, srv, "POST", "/v2/org/app/blobs/uploads/", nil, basic...)
	if resp.StatusCode != 202 {
		t.Fatalf("start upload: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if resp := do(t, srv, "PATCH", loc, layer[:2000], append(basic, "Content-Range", "0-1999")...); resp.StatusCode != 202 || resp.Header.Get("Range") != "0-1999" {
		t.Fatalf("patch: %d %s", resp.StatusCode, resp.Header.Get("Range"))
	}
	if resp := do(t, srv, "PATCH", loc, layer[2000:], append(basic, "Content-Range", "0-10")...); resp.StatusCode != 416 {
		t.Fatalf("out of order patch: %d", resp.StatusCode)
	}
	if resp := do(t, srv, "PUT", loc+"?digest="+ld, layer[2000:], basic...); resp.StatusCode != 201 {
		t.Fatalf("finish upload: %d", resp.StatusCode)
	}
	cfg := []byte(`{"os":"linux"}`)
	cd := digestOf(cfg)
	if resp := do(t, srv, "POST", "/v2/org/app/blobs/uploads/?digest="+cd, cfg, basic...); resp.StatusCode != 201 {
		t.Fatalf("monolithic upload: %d", resp.StatusCode)
	}

	bad, _ := json.Marshal(manifest{SchemaVersion: 2, Config: &descriptor{Digest: cd}, Layers: []descriptor{{Digest: digestOf([]byte("missing"))}}})
	if resp := do(t, srv, "PUT", "/v2/org/app/manifests/v1", bad, basic...); resp.StatusCode != 400 {
		t.Fatalf("manifest with unknown blob: %d", resp.StatusCode)
	}
	m, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: ociManifest, Config: &descriptor{Digest: cd, Size: int64(len(cfg))}, Layers: []descriptor{{Digest: ld, Size: int64(len(layer))}}})
	resp = do(t, srv, "PUT", "/v2/org/app/manifests/v1", m, append(basic, "Content-Type", ociManifest)...)
	if resp.StatusCode != 201 || resp.Header.Get("Docker-Content-Digest") != digestOf(m) {
		t.Fatalf("put manifest: %d", resp.StatusCode)
	}

	app := tarLayer(t, "bin/sh", []byte("#!"))
	if resp := do(t, srv, "POST", "/v2/org/app/blobs/uploads/?digest="+digestOf(app), app, basic...); resp.StatusCode != 201 {
		t.Fatalf("app layer upload: %d", resp.StatusCode)
	}
	notDisk, _ := json.Marshal(manifest{SchemaVersion: 2, MediaType: ociManifest, Config: &descriptor{Digest: cd}, Layers: []descriptor{{Digest: digestOf(app)}}})
	if resp := do(t, srv, "PUT", "/v2/org/app/manifests/sh", notDisk, basic...); resp.StatusCode != 400 {
		t.Fatalf("non-disk image accepted: %d", resp.StatusCode)
	}
	if exists(g.blob(digestOf(app))) || exists(g.layerLink("org/app", digestOf(app))) {
		t.Fatal("refused layer left on disk")
	}
	resp = do(t, srv, "PUT", "/v2/org/app/manifests/sh", notDisk, basic...)
	if msg, _ := io.ReadAll(resp.Body); resp.StatusCode != 400 || !strings.Contains(string(msg), "not a disk image") {
		t.Fatalf("retry after refusal: %d %s", resp.StatusCode, msg)
	}

	resp = do(t, srv, "GET", "/v2/org/app/manifests/v1", nil, basic...)
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Equal(got, m) || resp.Header.Get("Content-Type") != ociManifest {
		t.Fatalf("get manifest: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp = do(t, srv, "GET", "/v2/org/app/blobs/"+ld, nil, append(basic, "Range", "bytes=0-5")...)
	got, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(got) != "disk/a" {
		t.Fatalf("ranged blob: %d %q", resp.StatusCode, got)
	}
	if resp := do(t, srv, "GET", "/v2/org/other/blobs/"+ld, nil, basic...); resp.StatusCode != 404 {
		t.Fatalf("blob from unlinked repo: %d", resp.StatusCode)
	}
	resp = do(t, srv, "GET", "/v2/_catalog", nil, basic...)
	var cat struct{ Repositories []string }
	json.NewDecoder(resp.Body).Decode(&cat)
	if len(cat.Repositories) != 1 || cat.Repositories[0] != "org/app" {
		t.Fatalf("catalog: %v", cat.Repositories)
	}

	if err := g.Delete("org/app", "v1"); err != nil {
		t.Fatal(err)
	}
	if resp := do(t, srv, "GET", "/v2/org/app/manifests/v1", nil, basic...); resp.StatusCode != 404 {
		t.Fatalf("manifest after delete: %d", resp.StatusCode)
	}
	if len(g.List("org/")) != 0 {
		t.Fatal("image still listed after delete")
	}
	if exists(g.blob(ld)) || exists(g.blob(digestOf(m))) || exists(g.repo("org")) {
		t.Fatal("deleted image left data on disk")
	}
}

func TestPutDisk(t *testing.T) {
	g, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := rawDisk()
	d, err := g.PutDisk("org/cirros", "0.6", "cirros.qcow2", "arm64", int64(len(disk)), bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	imgs := g.List("org/")
	if len(imgs) != 1 || imgs[0].Digest != d || imgs[0].Tag != "0.6" || imgs[0].Arch != "arm64" {
		t.Fatalf("list: %+v", imgs)
	}
	b, _ := io.ReadAll(must(t, g.blob(d)))
	var m manifest
	json.Unmarshal(b, &m)
	tr := tar.NewReader(must(t, g.blob(m.Layers[0].Digest)))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Typeflag == tar.TypeReg {
			if c, _ := io.ReadAll(tr); !bytes.Equal(c, disk) || h.Uid != kubevirtOwner {
				t.Fatal("disk content or owner differs")
			}
		}
	}
	if strings.Join(names, ",") != "disk/,disk/cirros.qcow2" {
		t.Fatalf("layer entries: %v", names)
	}
	if _, err := g.PutDisk("org/text", "x", "a.raw", "amd64", 3, bytes.NewReader([]byte("abc"))); !errors.Is(err, ErrNotDisk) {
		t.Fatalf("non-disk upload: %v", err)
	}
}

func rawDisk() []byte {
	b := make([]byte, 64<<10)
	b[510], b[511] = 0x55, 0xaa
	return b
}

func tarLayer(t *testing.T, name string, content []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o444, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(content)
	tw.Close()
	return buf.Bytes()
}

func must(t *testing.T, p string) io.Reader {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}
