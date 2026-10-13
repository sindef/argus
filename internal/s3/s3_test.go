package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPutLockAndChecksum(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", LockDays: 7}
	if err := c.Put(context.Background(), "k", bytes.NewReader([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	if got.Get("x-amz-object-lock-mode") != "COMPLIANCE" || got.Get("x-amz-object-lock-retain-until-date") == "" {
		t.Errorf("object lock headers missing: %v", got)
	}
	if got.Get("Content-Md5") != "XUFAKrxLKna5cZ2REBfFkg==" {
		t.Errorf("Content-MD5 = %q", got.Get("Content-Md5"))
	}
	auth := got.Get("Authorization")
	for _, h := range []string{"content-md5", "x-amz-object-lock-mode", "x-amz-object-lock-retain-until-date", "x-amz-date"} {
		if !strings.Contains(auth, h) {
			t.Errorf("%s not signed: %s", h, auth)
		}
	}
}

func TestSpooledMultipart(t *testing.T) {
	spooledPart = 1 << 10
	defer func() { spooledPart = 512 << 20 }()
	var parts [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		if r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) || int64(len(body)) != r.ContentLength {
			t.Errorf("%s %s: payload hash or length mismatch", r.Method, r.URL)
		}
		switch {
		case r.URL.Query().Has("uploads"):
			w.Write([]byte("<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>"))
		case r.URL.Query().Get("partNumber") != "":
			parts = append(parts, body)
			w.Header().Set("ETag", `"e"`)
		}
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", Scratch: t.TempDir()}
	data := bytes.Repeat([]byte("0123456789"), 300)
	if err := c.Put(context.Background(), "k", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || !bytes.Equal(bytes.Join(parts, nil), data) {
		t.Errorf("got %d parts, reassembled correctly: %v", len(parts), bytes.Equal(bytes.Join(parts, nil), data))
	}
}
