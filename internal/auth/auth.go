package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Role string

const (
	Admin    Role = "admin"
	Operator Role = "operator"
	ReadOnly Role = "read-only"
)

var Roles = []Role{Admin, Operator, ReadOnly}

func (r Role) Can(min Role) bool {
	rank := map[Role]int{ReadOnly: 0, Operator: 1, Admin: 2}
	return rank[r] >= rank[min]
}

func (r Role) Valid() bool {
	return r == Admin || r == Operator || r == ReadOnly
}

type User struct {
	Name       string `json:"name"`
	Hash       string `json:"hash"`
	Role       Role   `json:"role"`
	MustChange bool   `json:"mustChange,omitempty"`
}

const (
	secretName = "argus-users"
	secretKey  = "users.json"
	sessionTTL = 12 * time.Hour
)

var (
	ErrExists    = errors.New("user already exists")
	ErrNotFound  = errors.New("user not found")
	ErrLastAdmin = errors.New("at least one admin is required")
	ErrPassword  = errors.New("current password is incorrect")
)

type Secrets interface {
	Get(ctx context.Context, name string) (map[string][]byte, string, error)
	Create(ctx context.Context, name, typ string, data map[string][]byte) error
	Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error
}

type Store struct {
	secrets Secrets

	mu       sync.Mutex
	sessions map[string]Session
}

type Session struct {
	User       string
	Role       Role
	MustChange bool
	Expires    time.Time
}

func NewStore(secrets Secrets) *Store {
	return &Store{secrets: secrets, sessions: map[string]Session{}}
}

func RandomPassword() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func (s *Store) load(ctx context.Context) ([]User, string, error) {
	data, rv, err := s.secrets.Get(ctx, secretName)
	if apierrors.IsNotFound(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var us []User
	return us, rv, json.Unmarshal(data[secretKey], &us)
}

func (s *Store) modify(ctx context.Context, fn func([]User) ([]User, error)) error {
	for attempt := 0; ; attempt++ {
		us, rv, err := s.load(ctx)
		if err != nil {
			return err
		}
		if us, err = fn(us); err != nil {
			return err
		}
		b, err := json.Marshal(us)
		if err != nil {
			return err
		}
		data := map[string][]byte{secretKey: b}
		if rv == "" {
			err = s.secrets.Create(ctx, secretName, "Opaque", data)
		} else {
			err = s.secrets.Update(ctx, secretName, rv, data)
		}
		if (apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)) && attempt < 3 {
			continue
		}
		return err
	}
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	us, _, err := s.load(ctx)
	return us, err
}

func (s *Store) Bootstrap(ctx context.Context, password string) (bool, error) {
	created := false
	err := s.modify(ctx, func(us []User) ([]User, error) {
		if len(us) > 0 {
			return us, nil
		}
		h, err := hash(password)
		if err != nil {
			return nil, err
		}
		created = true
		return []User{{Name: "admin", Hash: h, Role: Admin, MustChange: true}}, nil
	})
	return created && err == nil, err
}

func (s *Store) Add(ctx context.Context, name string, role Role, password string) error {
	h, err := hash(password)
	if err != nil {
		return err
	}
	return s.modify(ctx, func(us []User) ([]User, error) {
		for _, u := range us {
			if u.Name == name {
				return nil, ErrExists
			}
		}
		return append(us, User{Name: name, Hash: h, Role: role, MustChange: true}), nil
	})
}

func (s *Store) Delete(ctx context.Context, name string) error {
	err := s.modify(ctx, func(us []User) ([]User, error) {
		out := us[:0]
		found := false
		for _, u := range us {
			if u.Name == name {
				found = true
				continue
			}
			out = append(out, u)
		}
		if !found {
			return nil, ErrNotFound
		}
		return out, checkAdmins(out)
	})
	if err == nil {
		s.dropSessions(name)
	}
	return err
}

func (s *Store) SetRole(ctx context.Context, name string, role Role) error {
	err := s.update(ctx, name, func(u *User) error {
		u.Role = role
		return nil
	})
	if err == nil {
		s.dropSessions(name)
	}
	return err
}

func (s *Store) ResetPassword(ctx context.Context, name, password string) error {
	h, err := hash(password)
	if err != nil {
		return err
	}
	err = s.update(ctx, name, func(u *User) error {
		u.Hash, u.MustChange = h, true
		return nil
	})
	if err == nil {
		s.dropSessions(name)
	}
	return err
}

func (s *Store) ChangePassword(ctx context.Context, tok, current, password string) error {
	sess, ok := s.Session(tok)
	if !ok {
		return ErrNotFound
	}
	h, err := hash(password)
	if err != nil {
		return err
	}
	err = s.update(ctx, sess.User, func(u *User) error {
		if bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(current)) != nil {
			return ErrPassword
		}
		u.Hash, u.MustChange = h, false
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.sessions {
		if v.User == sess.User && k != tok {
			delete(s.sessions, k)
		}
	}
	sess.MustChange = false
	s.sessions[tok] = sess
	return nil
}

func (s *Store) update(ctx context.Context, name string, fn func(*User) error) error {
	return s.modify(ctx, func(us []User) ([]User, error) {
		for i := range us {
			if us[i].Name == name {
				if err := fn(&us[i]); err != nil {
					return nil, err
				}
				return us, checkAdmins(us)
			}
		}
		return nil, ErrNotFound
	})
}

func checkAdmins(us []User) error {
	for _, u := range us {
		if u.Role == Admin {
			return nil
		}
	}
	return ErrLastAdmin
}

func hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

func (s *Store) Login(ctx context.Context, name, password string) (string, error) {
	us, err := s.Users(ctx)
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
	s.sessions[tok] = Session{User: u.Name, Role: u.Role, MustChange: u.MustChange, Expires: now.Add(sessionTTL)}
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

func (s *Store) dropSessions(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.sessions {
		if v.User == name {
			delete(s.sessions, k)
		}
	}
}

func (s *Store) EnsureAdmin(ctx context.Context, seed string) (string, error) {
	users, err := s.Users(ctx)
	if err != nil {
		return "", err
	}
	pw := seed
	if pw == "" {
		pw = RandomPassword()
	}
	switch {
	case len(users) == 0:
		if _, err := s.Bootstrap(ctx, pw); err != nil {
			return "", err
		}
	case seed == "" && pendingAdmin(users):
		if err := s.ResetPassword(ctx, "admin", pw); err != nil {
			return "", err
		}
	default:
		return "", nil
	}
	if seed != "" {
		return "", nil
	}
	return pw, nil
}

func pendingAdmin(users []User) bool {
	for _, u := range users {
		if u.Name == "admin" && u.MustChange {
			return true
		}
	}
	return false
}
