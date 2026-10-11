package filestore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var resource = schema.GroupResource{Resource: "secrets"}

type Store struct {
	dir string
	mu  sync.Mutex
}

type record struct {
	Version int               `json:"version"`
	Type    string            `json:"type"`
	Data    map[string][]byte `json:"data"`
}

func New(dir string) (*Store, error) {
	return &Store{dir: dir}, os.MkdirAll(dir, 0o700)
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name+".json")
}

func (s *Store) read(name string) (record, error) {
	var rec record
	b, err := os.ReadFile(s.path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return rec, apierrors.NewNotFound(resource, name)
	}
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

func (s *Store) write(name string, rec record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := s.path(name) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(name)); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(s.path(name)))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) Get(ctx context.Context, name string) (map[string][]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.read(name)
	if err != nil {
		return nil, "", err
	}
	return rec.Data, strconv.Itoa(rec.Version), nil
}

func (s *Store) Create(ctx context.Context, name, typ string, data map[string][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.read(name); err == nil {
		return apierrors.NewAlreadyExists(resource, name)
	}
	return s.write(name, record{Version: 1, Type: typ, Data: data})
}

func (s *Store) Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.read(name)
	if err != nil {
		return err
	}
	if strconv.Itoa(rec.Version) != resourceVersion {
		return apierrors.NewConflict(resource, name, errors.New("modified"))
	}
	rec.Version++
	rec.Data = data
	return s.write(name, rec)
}
