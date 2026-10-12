package auth

import (
	"context"
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func LoadJSON(ctx context.Context, s Secrets, name, key string, v any) (string, error) {
	data, rv, err := s.Get(ctx, name)
	if apierrors.IsNotFound(err) {
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
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return SaveJSON(ctx, s, name, key, rv, v)
}
