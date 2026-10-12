package s3

import (
	"bytes"
	"context"
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
