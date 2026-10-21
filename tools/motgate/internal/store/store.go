package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the motgate SQLite database holding portal users and their
// per-request usage. It is intentionally separate from mot-router's store: the
// users here belong to the portal, and mot-router only ever sees one internal
// master key.
type Store struct {
	db *sql.DB
}

// User is one portal account with an access key and an expiry.
type User struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"`
	Revoked      int    `json:"revoked"`
	RequestCount int64  `json:"request_count"`
	TokensIn     int64  `json:"tokens_in"`
	TokensOut    int64  `json:"tokens_out"`
	LastUsedAt   *int64 `json:"last_used_at"`
	PlanType     string `json:"plan_type"`
	TokenLimit   int64  `json:"token_limit"`
	QuotaUsed    int64  `json:"quota_used"`
	QuotaState   string `json:"quota_state"`
	QuotaPending string `json:"quota_pending_request_id"`
}

type Plan struct {
	Type       string
	TokenLimit int64
}

// UsageInput is one accounted inference request.
type UsageInput struct {
	UserID    int64
	Surface   string
	Model     string
	Status    int
	Latency   int64
	InTokens  int64
	OutTokens int64
	Measured  bool
	RequestID string
}

// Open creates the database and schema.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure data dir: %w", err)
	}
	dsn := filepath.Join(dataDir, "motgate.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// modernc/sqlite is safe with a single writer; cap it to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	if err := s.ensureCatalog(context.Background()); err != nil {
		return nil, fmt.Errorf("initialize landing catalog: %w", err)
	}
	for _, path := range []string{filepath.Join(dataDir, "motgate.db"), filepath.Join(dataDir, "motgate.db-wal"), filepath.Join(dataDir, "motgate.db-shm")} {
		if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("secure database: %w", err)
		}
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT    NOT NULL,
			key           TEXT    NOT NULL UNIQUE,
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER NOT NULL,
			revoked       INTEGER NOT NULL DEFAULT 0,
			request_count INTEGER NOT NULL DEFAULT 0,
			tokens_in     INTEGER NOT NULL DEFAULT 0,
			tokens_out    INTEGER NOT NULL DEFAULT 0,
			last_used_at  INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS user_requests (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id     INTEGER NOT NULL,
			ts          INTEGER NOT NULL,
			surface     TEXT    NOT NULL,
			model       TEXT    NOT NULL,
			status      INTEGER NOT NULL,
			latency_ms  INTEGER NOT NULL,
			in_tokens   INTEGER NOT NULL DEFAULT 0,
			out_tokens  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_requests ON user_requests (user_id, ts DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_user_requests_ts ON user_requests (ts)`,
		`CREATE TABLE IF NOT EXISTS quota_requests (
			request_id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, ts INTEGER NOT NULL,
			surface TEXT NOT NULL, model TEXT NOT NULL, state TEXT NOT NULL,
			in_tokens INTEGER NOT NULL DEFAULT 0, out_tokens INTEGER NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS catalog_settings (key TEXT PRIMARY KEY, data TEXT NOT NULL)`,
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN plan_type TEXT NOT NULL DEFAULT 'unlimited'`,
		`ALTER TABLE users ADD COLUMN token_limit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN quota_used INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN quota_state TEXT NOT NULL DEFAULT 'ready'`,
		`ALTER TABLE users ADD COLUMN quota_pending_request_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE user_requests ADD COLUMN accounting_status TEXT NOT NULL DEFAULT 'reported'`,
		`ALTER TABLE user_requests ADD COLUMN quota_request_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// CreateUser issues a new portal account with a `mg_` key and an expiry.
func (s *Store) CreateUser(name string, expiresAt int64) (*User, error) {
	return s.CreateUserWithPlan(name, expiresAt, Plan{Type: "unlimited"})
}

func (s *Store) CreateUserWithPlan(name string, expiresAt int64, plan Plan) (*User, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	key := "mg_" + hex.EncodeToString(buf)

	res, err := s.db.Exec(
		`INSERT INTO users (name, key, created_at, expires_at, plan_type, token_limit) VALUES (?, ?, ?, ?, ?, ?)`,
		name, key, time.Now().UnixMilli(), expiresAt, plan.Type, plan.TokenLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}
	return s.GetUser(id)
}

// GetUser fetches one user by id.
func (s *Store) GetUser(id int64) (*User, error) {
	row := s.db.QueryRow(userColumns+` WHERE id = ?`, id)
	return scanUser(row)
}

// LookupUser resolves a user by its key value.
func (s *Store) LookupUser(key string) (*User, error) {
	row := s.db.QueryRow(userColumns+` WHERE key = ?`, key)
	u, err := scanUser(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return u, err
}

// ListUsers returns every user, newest first.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(userColumns + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetExpiry sets a user's expiry timestamp.
func (s *Store) SetExpiry(id int64, expiresAt int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE users SET expires_at = ? WHERE id = ?`, expiresAt, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetRevoked enables or disables a user.
func (s *Store) SetRevoked(id int64, revoked bool) (bool, error) {
	v := 0
	if revoked {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE users SET revoked = ? WHERE id = ?`, v, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteUser removes a user permanently.
func (s *Store) DeleteUser(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		_, _ = s.db.Exec(`DELETE FROM user_requests WHERE user_id = ?`, id)
	}
	return n > 0, nil
}

// AddUsage records one request and folds its tokens into the user's counters.
func (s *Store) AddUsage(in UsageInput) error {
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(
		`INSERT INTO user_requests (user_id, ts, surface, model, status, latency_ms, in_tokens, out_tokens, accounting_status, quota_request_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.UserID, now, in.Surface, in.Model, in.Status, in.Latency, in.InTokens, in.OutTokens, "reported", in.RequestID,
	); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE users
		 SET request_count = request_count + 1,
		     tokens_in     = tokens_in + ?,
		     tokens_out    = tokens_out + ?,
		     last_used_at  = ?
		 WHERE id = ?`,
		in.InTokens, in.OutTokens, now, in.UserID,
	)
	return err
}

// Overview reports portal-wide counters for the admin dashboard.
type Overview struct {
	TotalUsers    int64 `json:"total_users"`
	ActiveUsers   int64 `json:"active_users"`
	ExpiredUsers  int64 `json:"expired_users"`
	RevokedUsers  int64 `json:"revoked_users"`
	RequestsToday int64 `json:"requests_today"`
	TokensToday   int64 `json:"tokens_today"`
}

func (s *Store) Overview() (*Overview, error) {
	now := time.Now().UnixMilli()
	dayStart := now - (now % 86400000)

	o := &Overview{}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&o.TotalUsers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE revoked = 0 AND (expires_at = 0 OR expires_at > ?)`,
		now).Scan(&o.ActiveUsers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE revoked = 0 AND expires_at != 0 AND expires_at <= ?`,
		now).Scan(&o.ExpiredUsers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE revoked = 1`).Scan(&o.RevokedUsers); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(in_tokens + out_tokens), 0) FROM user_requests WHERE ts >= ?`,
		dayStart).Scan(&o.RequestsToday, &o.TokensToday); err != nil {
		return nil, err
	}
	return o, nil
}

const userColumns = `SELECT id, name, key, created_at, expires_at, revoked,
	request_count, tokens_in, tokens_out, last_used_at, plan_type, token_limit, quota_used, quota_state, quota_pending_request_id FROM users`

// rowScanner unifies *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(r rowScanner) (*User, error) {
	var u User
	if err := r.Scan(&u.ID, &u.Name, &u.Key, &u.CreatedAt, &u.ExpiresAt, &u.Revoked,
		&u.RequestCount, &u.TokensIn, &u.TokensOut, &u.LastUsedAt, &u.PlanType, &u.TokenLimit, &u.QuotaUsed, &u.QuotaState, &u.QuotaPending); err != nil {
		return nil, err
	}
	return &u, nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}
