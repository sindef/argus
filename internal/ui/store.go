package ui

import (
	"context"

	"argus/internal/auth"
)

func (s *Core) LoadJSON(ctx context.Context, key string, v any) (string, error) {
	return auth.LoadJSON(ctx, s.Auth.Secrets(), key, "config", v)
}

func (s *Core) SaveJSON(ctx context.Context, key, rv string, v any) error {
	return auth.SaveJSON(ctx, s.Auth.Secrets(), key, "config", rv, v)
}
