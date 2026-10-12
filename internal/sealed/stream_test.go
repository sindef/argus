package sealed

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestStream(t *testing.T) {
	for _, size := range []int{0, 10, chunk, 3*chunk + 7} {
		plain := make([]byte, size)
		rand.Read(plain)
		var enc bytes.Buffer
		if err := Encrypt(&enc, bytes.NewReader(plain), "pw"); err != nil {
			t.Fatal(err)
		}
		if PlainSize(int64(enc.Len())) != int64(size) {
			t.Fatalf("size %d: PlainSize %d", size, PlainSize(int64(enc.Len())))
		}
		r, _, _ := Decrypt(bytes.NewReader(enc.Bytes()), "pw")
		if got, err := io.ReadAll(r); err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("size %d: round trip failed: %v", size, err)
		}
		if size > chunk {
			r, _, _ = Decrypt(bytes.NewReader(enc.Bytes()[:enc.Len()-chunk/2]), "pw")
			if _, err := io.ReadAll(r); err == nil {
				t.Fatalf("size %d: truncated stream accepted", size)
			}
		}
		r, _, _ = Decrypt(bytes.NewReader(enc.Bytes()), "wrong")
		if _, err := io.ReadAll(r); err == nil {
			t.Fatalf("size %d: wrong passphrase accepted", size)
		}
	}
	r, _, _ := Decrypt(bytes.NewReader([]byte("plain backup")), "pw")
	if got, _ := io.ReadAll(r); string(got) != "plain backup" {
		t.Fatal("unencrypted data not passed through")
	}
}
