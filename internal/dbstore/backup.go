package dbstore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"argus/internal/sealed"
)

func Pack(recs map[string]Record, passphrase string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(recs)) {
		if name == "argus-sessions" {
			continue
		}
		b, err := json.Marshal(recs[name])
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + ".json", Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	if err := errors.Join(tw.Close(), gz.Close()); err != nil {
		return nil, err
	}
	return sealed.Seal(passphrase, buf.Bytes())
}

func Unpack(enc []byte, passphrase string) (map[string]Record, error) {
	plain, err := sealed.Open(passphrase, enc)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, err
	}
	recs := map[string]Record{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return nil, err
		}
		name := filepath.Base(h.Name)
		if name != h.Name || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("unexpected entry %q in backup", h.Name)
		}
		var rec Record
		if err := json.NewDecoder(io.LimitReader(tr, 64<<20)).Decode(&rec); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		recs[strings.TrimSuffix(name, ".json")] = rec
	}
}
