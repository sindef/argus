package store

import (
	"context"
	"encoding/json"
	"errors"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("changed concurrently")
)

type Secrets interface {
	Get(ctx context.Context, name string) (map[string][]byte, string, error)
	Create(ctx context.Context, name, typ string, data map[string][]byte) error
	Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error
}

func LoadJSON(ctx context.Context, s Secrets, name, key string, v any) (string, error) {
	data, rv, err := s.Get(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return rv, json.Unmarshal(data[key], v)
}

func SaveJSON(ctx context.Context, s Secrets, name, key, rv string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data := map[string][]byte{key: b}
	if rv == "" {
		return s.Create(ctx, name, "Opaque", data)
	}
	return s.Update(ctx, name, rv, data)
}

func PutJSON(ctx context.Context, s Secrets, name, key string, v any) error {
	_, rv, err := s.Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	return SaveJSON(ctx, s, name, key, rv, v)
}
