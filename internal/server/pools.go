package server

import (
	"context"
	"fmt"
	"slices"

	"argus/internal/kube"
)

func (s *Server) poolClass(ctx context.Context, pool string) (string, error) {
	if pool == "" {
		return "", nil
	}
	classes, err := s.kube.PoolClasses(ctx)
	if err != nil {
		return "", err
	}
	c, ok := classes[pool]
	if !ok {
		return "", fmt.Errorf("pool %s is not available for volumes", pool)
	}
	return c, nil
}

func (s *Server) poolChoices(ctx context.Context) []kube.Pool {
	pools, _ := s.kube.Pools(ctx)
	return slices.DeleteFunc(pools, func(p kube.Pool) bool { return p.StorageClass == "" })
}
