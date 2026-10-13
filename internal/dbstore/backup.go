package dbstore

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"argus/internal/sealed"
)

const (
	ManifestEntry = "backup-manifest.json"
	filesPrefix   = "files/"
)

func Pack(w io.Writer, recs map[string]Record, passphrase string, manifest any, root string, dirs []string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeArchive(pw, recs, manifest, root, dirs)) }()
	err := sealed.Encrypt(w, pr, passphrase)
	pr.CloseWithError(err)
	return err
}

func writeArchive(w io.Writer, recs map[string]Record, manifest any, root string, dirs []string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	add := func(name string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(recs)) {
		if name == "argus-sessions" {
			continue
		}
		b, err := json.Marshal(recs[name])
		if err != nil {
			return err
		}
		if err := add(name+".json", b); err != nil {
			return err
		}
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := add(ManifestEntry, b); err != nil {
		return err
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Name: filesPrefix + filepath.ToSlash(rel), Mode: 0o600, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			return err
		})
		if err != nil {
			return err
		}
	}
	return errors.Join(tw.Close(), gz.Close())
}

func Unpack(r io.Reader, passphrase, filesRoot string) (map[string]Record, []byte, error) {
	br := bufio.NewReader(r)
	plain, stream, err := sealed.Decrypt(br, passphrase)
	if err != nil {
		return nil, nil, err
	}
	if !stream {
		all, err := io.ReadAll(plain)
		if err != nil {
			return nil, nil, err
		}
		b, err := sealed.Open(passphrase, all)
		if err != nil {
			return nil, nil, err
		}
		plain = bytes.NewReader(b)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return nil, nil, err
	}
	recs := map[string]Record{}
	var manifest []byte
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return recs, manifest, nil
		}
		if err != nil {
			return nil, nil, err
		}
		switch rel, isFile := strings.CutPrefix(h.Name, filesPrefix); {
		case isFile:
			if err := extract(tr, h, filesRoot, rel); err != nil {
				return nil, nil, err
			}
		case h.Name == ManifestEntry:
			if manifest, err = io.ReadAll(io.LimitReader(tr, 64<<20)); err != nil {
				return nil, nil, err
			}
		case path.Base(h.Name) == h.Name && strings.HasSuffix(h.Name, ".json"):
			var rec Record
			if err := json.NewDecoder(io.LimitReader(tr, 64<<20)).Decode(&rec); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", h.Name, err)
			}
			recs[strings.TrimSuffix(h.Name, ".json")] = rec
		default:
			return nil, nil, fmt.Errorf("unexpected entry %q in backup", h.Name)
		}
	}
}

func extract(r io.Reader, h *tar.Header, root, rel string) error {
	if root == "" {
		return nil
	}
	clean := path.Clean(rel)
	if h.Typeflag != tar.TypeReg || clean != rel || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return fmt.Errorf("unexpected file %q in backup", h.Name)
	}
	dst := filepath.Join(root, filepath.FromSlash(clean))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	return errors.Join(err, f.Close())
}
