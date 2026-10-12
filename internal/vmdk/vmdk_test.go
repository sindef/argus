package vmdk

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRoundTripQemuImg(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not installed")
	}
	size := int64(70<<20 + 4096)
	raw := make([]byte, size)
	for _, off := range []int64{0, 5 << 20, 33<<20 + 100, size - 3000} {
		rand.Read(raw[off : off+3000])
	}
	dir := t.TempDir()
	f, _ := os.Create(filepath.Join(dir, "d.vmdk"))
	if err := Write(f, bytes.NewReader(raw), size, "d.vmdk"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if out, err := exec.Command("qemu-img", "check", "-f", "vmdk", filepath.Join(dir, "d.vmdk")).CombinedOutput(); err != nil {
		t.Fatalf("check: %v %s", err, out)
	}
	if out, err := exec.Command("qemu-img", "convert", "-f", "vmdk", "-O", "raw", filepath.Join(dir, "d.vmdk"), filepath.Join(dir, "d.raw")).CombinedOutput(); err != nil {
		t.Fatalf("convert: %v %s", err, out)
	}
	back, _ := os.ReadFile(filepath.Join(dir, "d.raw"))
	if !bytes.Equal(back, raw) {
		t.Fatalf("round trip differs: %d vs %d bytes", len(back), len(raw))
	}
}
