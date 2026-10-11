package hostd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupWritesStayInsideRoot(t *testing.T) {
	share, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(share, "web"), 0o700)
	if err := os.Symlink(outside, filepath.Join(share, "web", "20261011-000000")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(share)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeStream(root, "web/20261011-000000/vm.json", strings.NewReader("{}")); err == nil {
		t.Error("write through a symlinked backup directory succeeded")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("files written outside the backup root: %v", entries)
	}

	victim := filepath.Join(outside, "victim")
	os.Symlink(victim, filepath.Join(share, ".argus-write-test"))
	if err := probeWrite(share); err == nil {
		t.Error("probe followed a pre-existing symlink")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Error("probe created a file outside the backup root")
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	share := t.TempDir()
	root, _ := os.OpenRoot(share)
	defer root.Close()
	writeStream(root, "web/20261011-000000/root.img.gz", strings.NewReader("disk"))
	h := sha256.Sum256([]byte("disk"))
	m, _ := json.Marshal(Manifest{Files: map[string]ManifestFile{"root.img.gz": {SHA256: hex.EncodeToString(h[:]), Size: 4}}})
	writeStream(root, "web/20261011-000000/manifest.json", bytes.NewReader(m))
	if err := verifyIn(root, "web/20261011-000000"); err != nil {
		t.Fatalf("intact backup failed verification: %v", err)
	}
	writeStream(root, "web/20261011-000000/root.img.gz", strings.NewReader("dusk"))
	if err := verifyIn(root, "web/20261011-000000"); err == nil {
		t.Fatal("corrupted backup passed verification")
	}
	root.Remove("web/20261011-000000/manifest.json")
	if err := verifyIn(root, "web/20261011-000000"); err == nil {
		t.Fatal("backup without manifest passed verification")
	}
}
