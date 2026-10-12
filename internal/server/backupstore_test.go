package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"argus/internal/hostd"
)

func TestVerifiedCopy(t *testing.T) {
	data := make([]byte, 3<<20+123)
	rand.Read(data)
	sum := sha256.Sum256(data)
	want := hostd.ManifestFile{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	var out bytes.Buffer
	if n, err := verifiedCopy(&out, bytes.NewReader(data), want); err != nil || n != want.Size || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("intact copy: n=%d err=%v", n, err)
	}
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 1
	out.Reset()
	if _, err := verifiedCopy(&out, bytes.NewReader(bad), want); err == nil {
		t.Fatal("corrupted copy passed")
	}
	if int64(out.Len()) >= want.Size {
		t.Fatal("corrupted copy delivered the whole file")
	}
}
