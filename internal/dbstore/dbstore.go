package dbstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	_ "modernc.org/sqlite"
)

var resource = schema.GroupResource{Resource: "secrets"}

type Record struct {
	Version int               `json:"version"`
	Type    string            `json:"type"`
	Data    map[string][]byte `json:"data"`
}

type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	pg   bool
}

func (s *Store) q(query string) string {
	if !s.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func loadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		return key, f.Close()
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be readable by group or others (chmod 600)", path)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must hold a 32-byte key", path)
	}
	return key, nil
}

func Open(dbPath, keyPath string) (*Store, error) {
	key, err := loadKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Store{aead: aead, pg: strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://")}
	blob := "BLOB"
	if s.pg {
		s.db, err = sql.Open("pgx", dbPath)
		blob = "BYTEA"
	} else {
		s.db, err = sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
		s.db.SetMaxOpenConns(1)
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS records (name TEXT PRIMARY KEY, version INTEGER NOT NULL, type TEXT NOT NULL, data ` + blob + ` NOT NULL)`); err != nil {
		s.db.Close()
		return nil, err
	}
	if !s.pg {
		if err := os.Chmod(dbPath, 0o600); err != nil {
			s.db.Close()
			return nil, err
		}
	}
	return s, nil
}

const (
	leaseTTL   = 15 * time.Second
	leaseRenew = 5 * time.Second
)

func (s *Store) Shared() bool { return s.pg }

func (s *Store) Lead(ctx context.Context, holder string) (<-chan error, error) {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS leader (id INTEGER PRIMARY KEY, holder TEXT NOT NULL, expires TIMESTAMPTZ NOT NULL)`); err != nil {
		return nil, err
	}
	for {
		res, err := s.db.ExecContext(ctx, `INSERT INTO leader (id, holder, expires) VALUES (1, $1, now() + $2::interval)
			ON CONFLICT (id) DO UPDATE SET holder = EXCLUDED.holder, expires = EXCLUDED.expires WHERE leader.expires < now() OR leader.holder = EXCLUDED.holder`, holder, leaseTTL.String())
		if err == nil {
			if n, _ := res.RowsAffected(); n == 1 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(leaseRenew / 2):
		}
	}
	lost := make(chan error, 1)
	go func() {
		renewed := time.Now()
		for range time.Tick(leaseRenew) {
			rctx, cancel := context.WithTimeout(context.Background(), leaseRenew/2)
			res, err := s.db.ExecContext(rctx, `UPDATE leader SET expires = now() + $2::interval WHERE id = 1 AND holder = $1`, holder, leaseTTL.String())
			cancel()
			if err == nil {
				if n, _ := res.RowsAffected(); n == 1 {
					renewed = time.Now()
					continue
				}
				err = errors.New("another Manager holds the lease")
			}
			if time.Since(renewed) >= 2*leaseRenew {
				lost <- err
				return
			}
		}
	}()
	return lost, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seal(name string, data map[string][]byte) ([]byte, error) {
	plain, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(name)), nil
}

func (s *Store) open(name string, blob []byte) (map[string][]byte, error) {
	n := s.aead.NonceSize()
	if len(blob) < n {
		return nil, fmt.Errorf("record %s is corrupt", name)
	}
	plain, err := s.aead.Open(nil, blob[:n], blob[n:], []byte(name))
	if err != nil {
		return nil, fmt.Errorf("record %s cannot be decrypted with this key", name)
	}
	var data map[string][]byte
	return data, json.Unmarshal(plain, &data)
}

func (s *Store) Get(ctx context.Context, name string) (map[string][]byte, string, error) {
	var version int
	var blob []byte
	err := s.db.QueryRowContext(ctx, s.q(`SELECT version, data FROM records WHERE name = ?`), name).Scan(&version, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", apierrors.NewNotFound(resource, name)
	}
	if err != nil {
		return nil, "", err
	}
	data, err := s.open(name, blob)
	return data, strconv.Itoa(version), err
}

func (s *Store) Create(ctx context.Context, name, typ string, data map[string][]byte) error {
	blob, err := s.seal(name, data)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q(`INSERT INTO records (name, version, type, data) VALUES (?, 1, ?, ?) ON CONFLICT(name) DO NOTHING`), name, typ, blob)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apierrors.NewAlreadyExists(resource, name)
	}
	return nil
}

func (s *Store) Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error {
	version, err := strconv.Atoi(resourceVersion)
	if err != nil {
		return apierrors.NewConflict(resource, name, errors.New("invalid version"))
	}
	blob, err := s.seal(name, data)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE records SET version = version + 1, data = ? WHERE name = ? AND version = ?`), blob, name, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, _, err := s.Get(ctx, name); err != nil {
			return err
		}
		return apierrors.NewConflict(resource, name, errors.New("modified"))
	}
	return nil
}

func (s *Store) Export(ctx context.Context) (map[string]Record, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT name, version, type, data FROM records ORDER BY name`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Record{}
	for rows.Next() {
		var name string
		var rec Record
		var blob []byte
		if err := rows.Scan(&name, &rec.Version, &rec.Type, &blob); err != nil {
			return nil, err
		}
		if rec.Data, err = s.open(name, blob); err != nil {
			return nil, err
		}
		out[name] = rec
	}
	return out, rows.Err()
}

func (s *Store) Empty(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM records`)).Scan(&n)
	return n == 0, err
}

func (s *Store) Replace(ctx context.Context, recs map[string]Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM records`)); err != nil {
		return err
	}
	for name, rec := range recs {
		blob, err := s.seal(name, rec.Data)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO records (name, version, type, data) VALUES (?, ?, ?, ?)`), name, max(rec.Version, 1), rec.Type, blob); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ImportFiles(ctx context.Context, dir string) (int, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		return 0, err
	}
	recs := map[string]Record{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return 0, err
		}
		var rec Record
		if err := json.Unmarshal(b, &rec); err != nil {
			return 0, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		recs[strings.TrimSuffix(filepath.Base(f), ".json")] = rec
	}
	if err := s.Replace(ctx, recs); err != nil {
		return 0, err
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}
