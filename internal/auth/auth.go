package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"argus/internal/kube"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Role string

const (
	Admin    Role = "admin"
	Operator Role = "operator"
	ReadOnly Role = "read-only"
)

func (r Role) Can(min Role) bool {
	rank := map[Role]int{ReadOnly: 0, Operator: 1, Admin: 2}
	return rank[r] >= rank[min]
}

type User struct {
	Name string `json:"name"`
	Hash string `json:"hash"`
	Role Role   `json:"role"`
}

const (
	secretName = "argus-users"
	secretKey  = "users.json"
	sessionTTL = 12 * time.Hour
)

var ErrExists = errors.New("users already configured")

type Store struct {
	secrets kube.Secrets

	mu       sync.Mutex
	sessions map[string]Session
}

type Session struct {
	User    string
	Role    Role
	Expires time.Time
}

func NewStore(secrets kube.Secrets) *Store {
	return &Store{secrets: secrets, sessions: map[string]Session{}}
}

func (s *Store) users(ctx context.Context) ([]User, error) {
	data, err := s.secrets.Get(ctx, secretName)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var us []User
	return us, json.Unmarshal(data[secretKey], &us)
}

func (s *Store) Configured(ctx context.Context) (bool, error) {
	us, err := s.users(ctx)
	return len(us) > 0, err
}

func (s *Store) CreateAdmin(ctx context.Context, name, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	b, err := json.Marshal([]User{{Name: name, Hash: string(hash), Role: Admin}})
	if err != nil {
		return err
	}
	err = s.secrets.Create(ctx, secretName, "Opaque", map[string][]byte{secretKey: b})
	if apierrors.IsAlreadyExists(err) {
		return ErrExists
	}
	return err
}

func (s *Store) Login(ctx context.Context, name, password string) (string, error) {
	us, err := s.users(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range us {
		if u.Name == name && bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)) == nil {
			return s.newSession(u), nil
		}
	}
	return "", nil
}

func (s *Store) newSession(u User) string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.Expires) {
			delete(s.sessions, k)
		}
	}
	s.sessions[tok] = Session{User: u.Name, Role: u.Role, Expires: now.Add(sessionTTL)}
	return tok
}

func (s *Store) Session(tok string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.sessions[tok]
	if !ok || time.Now().After(v.Expires) {
		return Session{}, false
	}
	return v, true
}

func (s *Store) Logout(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tok)
}
