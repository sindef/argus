package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const tokenSecret = "argus-tokens"

type Token struct {
	Name    string    `json:"name"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

func (s *Store) tokens(ctx context.Context) ([]Token, string, error) {
	data, rv, err := s.secrets.Get(ctx, tokenSecret)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var ts []Token
	return ts, rv, json.Unmarshal(data[secretKey], &ts)
}

func (s *Store) saveTokens(ctx context.Context, ts []Token, rv string) error {
	b, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	data := map[string][]byte{secretKey: b}
	if rv == "" {
		return s.secrets.Create(ctx, tokenSecret, "Opaque", data)
	}
	return s.secrets.Update(ctx, tokenSecret, rv, data)
}

func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	ts, _, err := s.tokens(ctx)
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	return ts, err
}

func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	ts, rv, err := s.tokens(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if t.Name == name {
			return "", ErrExists
		}
	}
	b := make([]byte, 32)
	rand.Read(b)
	plain := "argus_" + hex.EncodeToString(b)
	ts = append(ts, Token{Name: name, Hash: tokenHash(plain), Created: time.Now().UTC()})
	return plain, s.saveTokens(ctx, ts, rv)
}

func (s *Store) DeleteToken(ctx context.Context, name string) error {
	ts, rv, err := s.tokens(ctx)
	if err != nil {
		return err
	}
	out := ts[:0]
	for _, t := range ts {
		if t.Name != name {
			out = append(out, t)
		}
	}
	if len(out) == len(ts) {
		return ErrNotFound
	}
	return s.saveTokens(ctx, out, rv)
}

func (s *Store) CheckToken(ctx context.Context, plain string) (string, bool) {
	ts, _, err := s.tokens(ctx)
	if err != nil {
		return "", false
	}
	h := tokenHash(plain)
	for _, t := range ts {
		if subtle.ConstantTimeCompare([]byte(t.Hash), []byte(h)) == 1 {
			return t.Name, true
		}
	}
	return "", false
}

func tokenHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
