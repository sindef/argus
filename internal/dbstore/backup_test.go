package dbstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"argus/internal/sealed"
)

func TestPackRoundTrip(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "registry", "blobs"), 0o700)
	os.WriteFile(filepath.Join(src, "registry", "blobs", "b1"), []byte("layer"), 0o600)
	recs := map[string]Record{"argus-orgs": {Data: map[string][]byte{"orgs": []byte(`[{"id":"default"}]`)}}, "argus-sessions": {}}
	var buf bytes.Buffer
	if err := Pack(&buf, recs, "correct-horse-battery", map[string]int{"images": 1}, src, []string{"registry", "templates"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Unpack(bytes.NewReader(buf.Bytes()), "wrong-passphrase-123", ""); err == nil {
		t.Error("wrong passphrase accepted")
	}
	dst := t.TempDir()
	got, manifest, err := Unpack(bytes.NewReader(buf.Bytes()), "correct-horse-battery", dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["argus-orgs"].Data["orgs"]) != `[{"id":"default"}]` || string(manifest) != `{"images":1}` {
		t.Errorf("round trip lost data or kept sessions: %+v %s", got, manifest)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "registry", "blobs", "b1")); string(b) != "layer" {
		t.Errorf("library file not restored: %q", b)
	}
}

func TestUnpackLegacy(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	b, _ := json.Marshal(Record{Data: map[string][]byte{"k": []byte("v")}})
	tw.WriteHeader(&tar.Header{Name: "argus-orgs.json", Mode: 0o600, Size: int64(len(b))})
	tw.Write(b)
	tw.Close()
	gz.Close()
	old, err := sealed.Seal("correct-horse-battery", archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := Unpack(bytes.NewReader(old), "correct-horse-battery", "")
	if err != nil || string(got["argus-orgs"].Data["k"]) != "v" {
		t.Errorf("legacy backup: %+v %v", got, err)
	}
}
