package hostd

import (
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
