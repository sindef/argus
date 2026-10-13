package ui

import (
	"argus/internal/store"
	"context"
)

func (s *Core) LoadJSON(ctx context.Context, key string, v any) (string, error) {
	return store.LoadJSON(ctx, s.Auth.Secrets(), key, "config", v)
}

func (s *Core) SaveJSON(ctx context.Context, key, rv string, v any) error {
	return store.SaveJSON(ctx, s.Auth.Secrets(), key, "config", rv, v)
}
