package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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
	Name       string   `json:"name"`
	Hash       string   `json:"hash"`
	Role       Role     `json:"role"`
	MustChange bool     `json:"mustChange,omitempty"`
	SSO        bool     `json:"sso,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	TOTP       string   `json:"totp,omitempty"`
}

const (
	secretName  = "argus-users"
	secretKey   = "users.json"
	sessionTTL  = 12 * time.Hour
	rememberTTL = 30 * 24 * time.Hour
	idleTimeout = 2 * time.Hour
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
	secrets  Secrets
	key      string
	org      string
	sessions *Sessions
}

type Session struct {
	User       string
	Org        string
	Role       Role
	MustChange bool
	SSO        bool
	Scopes     []string
	Expires    time.Time
	Idle       bool
	Seen       time.Time
}

type Sessions struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	m       map[string]Session
	dirty   bool
}

const sessionsSecret = "argus-sessions"

func tokenKey(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (s *Store) storeFor(org string) *Store {
	if org == s.org {
		return s
	}
	return s.ForOrg(org)
}

func (s *Store) PersistSessions(ctx context.Context) {
	if data, _, err := s.secrets.Get(ctx, sessionsSecret); err == nil {
		var m map[string]Session
		if json.Unmarshal(data["sessions"], &m) == nil {
			users := map[string][]User{}
			for k, v := range m {
				us, ok := users[v.Org]
				if !ok {
					us, _ = s.storeFor(v.Org).Users(ctx)
					users[v.Org] = us
				}
				if time.Now().After(v.Expires) || !slices.ContainsFunc(us, func(u User) bool {
					role, scopes := s.storeFor(v.Org).Access(ctx, u)
					return u.Name == v.User && role == v.Role && slices.Equal(scopes, v.Scopes)
				}) {
					delete(m, k)
				}
			}
			s.sessions.mu.Lock()
			for k, v := range m {
				if _, ok := s.sessions.m[k]; !ok {
					s.sessions.m[k] = v
				}
			}
			s.sessions.mu.Unlock()
		}
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushSessions(ctx)
		}
	}
}

func (s *Store) flushSessions(ctx context.Context) error {
	s.sessions.flushMu.Lock()
	defer s.sessions.flushMu.Unlock()
	s.sessions.mu.Lock()
	if !s.sessions.dirty {
		s.sessions.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(s.sessions.m)
	s.sessions.dirty = false
	s.sessions.mu.Unlock()
	if err != nil {
		return err
	}
	data := map[string][]byte{"sessions": b}
	_, rv, err := s.secrets.Get(ctx, sessionsSecret)
	switch {
	case apierrors.IsNotFound(err):
		err = s.secrets.Create(ctx, sessionsSecret, "Opaque", data)
	case err == nil:
		err = s.secrets.Update(ctx, sessionsSecret, rv, data)
	}
	if err != nil {
		s.sessions.mu.Lock()
		s.sessions.dirty = true
		s.sessions.mu.Unlock()
	}
	return err
}

func (s *Store) revoked() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.flushSessions(ctx); err != nil {
		log.Printf("sessions: persist revocation: %v", err)
	}
}

func (s *Store) DropOrg(org string) {
	s.sessions.mu.Lock()
	for k, v := range s.sessions.m {
		if v.Org == org {
			delete(s.sessions.m, k)
			s.sessions.dirty = true
		}
	}
	s.sessions.mu.Unlock()
	s.revoked()
}

func NewStore(secrets Secrets) *Store {
	return &Store{secrets: secrets, key: secretName, sessions: &Sessions{m: map[string]Session{}}}
}

func (s *Store) ForOrg(org string) *Store {
	return &Store{secrets: s.secrets, key: secretName + "-" + org, org: org, sessions: s.sessions}
}

func (s *Store) NewSession(sess Session, remember bool) string {
	sess.Org = s.org
	return s.sessions.add(sess, remember)
}

func (ss *Sessions) add(sess Session, remember bool) string {
	b := make([]byte, 32)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	ttl := sessionTTL
	if remember {
		ttl = rememberTTL
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	now := time.Now()
	for k, v := range ss.m {
		if now.After(v.Expires) {
			delete(ss.m, k)
		}
	}
	sess.Expires, sess.Idle, sess.Seen = now.Add(ttl), !remember, now
	ss.m[tokenKey(tok)] = sess
	ss.dirty = true
	return tok
}

func RandomPassword() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func (s *Store) load(ctx context.Context) ([]User, string, error) {
	data, rv, err := s.secrets.Get(ctx, s.key)
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
			err = s.secrets.Create(ctx, s.key, "Opaque", data)
		} else {
			err = s.secrets.Update(ctx, s.key, rv, data)
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

func (s *Store) UpsertSSO(ctx context.Context, name, subject string, role Role, scopes, groups []string) error {
	var drop []string
	err := s.modify(ctx, func(us []User) ([]User, error) {
		drop = nil
		for i := range us {
			if us[i].SSO && us[i].Subject == subject {
				if us[i].Name != name {
					for _, o := range us {
						if o.Name == name {
							return nil, fmt.Errorf("user name %s is already taken", name)
						}
					}
					drop = append(drop, us[i].Name)
				}
				if us[i].Role != role || !slices.Equal(us[i].Scopes, scopes) {
					drop = append(drop, us[i].Name)
				}
				us[i].Name, us[i].Role, us[i].Scopes, us[i].Groups = name, role, scopes, groups
				return us, nil
			}
		}
		for i := range us {
			if us[i].Name != name {
				continue
			}
			if !us[i].SSO {
				return nil, fmt.Errorf("local user %s already exists", name)
			}
			if us[i].Subject != "" {
				return nil, fmt.Errorf("user name %s belongs to another identity", name)
			}
			us[i].Subject = subject
			if us[i].Role != role || !slices.Equal(us[i].Scopes, scopes) {
				drop = append(drop, name)
			}
			us[i].Role, us[i].Scopes, us[i].Groups = role, scopes, groups
			return us, nil
		}
		return append(us, User{Name: name, Role: role, SSO: true, Subject: subject, Scopes: scopes, Groups: groups}), nil
	})
	for _, n := range drop {
		s.dropSessions(n)
	}
	return err
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
		if u.SSO {
			return ErrSSO
		}
		u.Role = role
		return nil
	})
	if err == nil {
		s.dropSessions(name)
	}
	return err
}

func (s *Store) SetScopes(ctx context.Context, name string, scopes []string) error {
	err := s.update(ctx, name, func(u *User) error {
		if u.SSO {
			return ErrSSO
		}
		u.Scopes = scopes
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
		if u.SSO {
			return ErrSSO
		}
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
	s.sessions.mu.Lock()
	for k, v := range s.sessions.m {
		if v.Org == s.org && v.User == sess.User && k != tokenKey(tok) {
			delete(s.sessions.m, k)
		}
	}
	sess.MustChange = false
	s.sessions.m[tokenKey(tok)] = sess
	s.sessions.dirty = true
	s.sessions.mu.Unlock()
	s.revoked()
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

var ErrSSO = errors.New("single sign-on users are managed by the identity provider")

func checkAdmins(us []User) error {
	for _, u := range us {
		if u.Role == Admin && !u.SSO && len(u.Scopes) == 0 {
			return nil
		}
	}
	return ErrLastAdmin
}

func hash(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(h), err
}

func (s *Store) Verify(ctx context.Context, name, password string) (User, bool, error) {
	us, err := s.Users(ctx)
	if err != nil {
		return User{}, false, err
	}
	for _, u := range us {
		if u.Name == name && u.Hash != "" && bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)) == nil {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

func (s *Store) Login(ctx context.Context, name, password string, remember bool) (string, error) {
	u, ok, err := s.Verify(ctx, name, password)
	if !ok {
		return "", err
	}
	return s.StartSession(ctx, u, remember), nil
}

func (s *Store) Session(tok string) (Session, bool) {
	s.sessions.mu.Lock()
	defer s.sessions.mu.Unlock()
	k := tokenKey(tok)
	v, ok := s.sessions.m[k]
	now := time.Now()
	if ok && v.Idle && now.Sub(v.Seen) > idleTimeout {
		delete(s.sessions.m, k)
		s.sessions.dirty, ok = true, false
	}
	if !ok || now.After(v.Expires) {
		return Session{}, false
	}
	if now.Sub(v.Seen) > 5*time.Minute {
		s.sessions.dirty = true
	}
	v.Seen = now
	s.sessions.m[k] = v
	return v, true
}

func (s *Store) Logout(tok string) {
	s.sessions.mu.Lock()
	delete(s.sessions.m, tokenKey(tok))
	s.sessions.dirty = true
	s.sessions.mu.Unlock()
	s.revoked()
}

func (s *Store) dropSessions(name string) {
	s.sessions.mu.Lock()
	for k, v := range s.sessions.m {
		if v.Org == s.org && v.User == name {
			delete(s.sessions.m, k)
			s.sessions.dirty = true
		}
	}
	s.sessions.mu.Unlock()
	s.revoked()
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

func (s *Store) Secrets() Secrets { return s.secrets }
