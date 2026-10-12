package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sort"
	"time"
)

const tokenSecret = "argus-tokens"

type Token struct {
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	Scope   string    `json:"scope,omitempty"`
	Org     string    `json:"org,omitempty"`
	User    string    `json:"user,omitempty"`
}

const (
	MetricsScope = "metrics"
	ReadScope    = "read"
)

func (s *Store) tokens(ctx context.Context) ([]Token, string, error) {
	var ts []Token
	rv, err := LoadJSON(ctx, s.secrets, tokenSecret, secretKey, &ts)
	return ts, rv, err
}

func (s *Store) saveTokens(ctx context.Context, ts []Token, rv string) error {
	return SaveJSON(ctx, s.secrets, tokenSecret, secretKey, rv, ts)
}

func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	ts, _, err := s.tokens(ctx)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	return ts, err
}

func (s *Store) CreateToken(ctx context.Context, name, scope string) (string, error) {
	return s.AddToken(ctx, Token{Name: name, Scope: scope})
}

func (s *Store) AddToken(ctx context.Context, t Token) (string, error) {
	ts, rv, err := s.tokens(ctx)
	if err != nil {
		return "", err
	}
	for _, o := range ts {
		if o.Name == t.Name && o.Org == t.Org && o.User == t.User {
			return "", ErrExists
		}
	}
	b := make([]byte, 32)
	rand.Read(b)
	plain := "argus_" + hex.EncodeToString(b)
	t.Hash, t.Created = tokenHash(plain), time.Now().UTC()
	ts = append(ts, t)
	return plain, s.saveTokens(ctx, ts, rv)
}

func (s *Store) DeleteToken(ctx context.Context, name string) error {
	return s.RemoveToken(ctx, "", "", name)
}

func (s *Store) RemoveToken(ctx context.Context, org, user, name string) error {
	ts, rv, err := s.tokens(ctx)
	if err != nil {
		return err
	}
	out := ts[:0]
	for _, t := range ts {
		if t.Name != name || t.Org != org || t.User != user {
			out = append(out, t)
		}
	}
	if len(out) == len(ts) {
		return ErrNotFound
	}
	return s.saveTokens(ctx, out, rv)
}

func (s *Store) CheckToken(ctx context.Context, plain string) (Token, bool) {
	ts, _, err := s.tokens(ctx)
	if err != nil {
		return Token{}, false
	}
	h := tokenHash(plain)
	for _, t := range ts {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			return t, true
		}
	}
	return Token{}, false
}

func tokenHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
