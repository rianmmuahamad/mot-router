package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database holding API keys, request logs, and settings.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// DB exposes the underlying database handle for custom reporting queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

// APIKey is a router-issued credential. It is distinct from any upstream
// OpenCode Zen key and is never forwarded upstream.
type APIKey struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	CreatedAt    int64  `json:"created_at"`
	LastUsedAt   *int64 `json:"last_used_at"`
	RequestCount int64  `json:"request_count"`
	Revoked      int    `json:"revoked"`
}

// RequestRow is one logged inference request.
type RequestRow struct {
	ID      int64   `json:"id"`
	Ts      int64   `json:"ts"`
	KeyID   *int64  `json:"key_id"`
	KeyName *string `json:"key_name"`
	Surface string  `json:"surface"`
	Model   string  `json:"model"`
	// RequestedModel is what the client asked for, before provider routing and
	// alias resolution. Model is what was actually sent upstream.
	RequestedModel string  `json:"requested_model"`
	Provider       string  `json:"provider"`
	Status         int     `json:"status"`
	LatencyMs      int64   `json:"latency_ms"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	Error          *string `json:"error"`
	Client         *string `json:"client"`
	Relay          string  `json:"relay"`
}

// ModelCount is a per-model request tally.
type ModelCount struct {
	Model string `json:"model"`
	N     int64  `json:"n"`
}

// Stats is the dashboard summary.
type Stats struct {
	Total        int64        `json:"total"`
	OK           int64        `json:"ok"`
	Failed       int64        `json:"failed"`
	InputTokens  int64        `json:"input_tokens"`
	OutputTokens int64        `json:"output_tokens"`
	AvgMs        int64        `json:"avg_ms"`
	Last24h      int64        `json:"last_24h"`
	ByModel      []ModelCount `json:"by_model"`
}

// LogInput is the payload recorded for each proxied request.
type LogInput struct {
	KeyID          *int64
	KeyName        *string
	Surface        string
	Model          string
	RequestedModel string
	Provider       string
	Status         int
	LatencyMs      int64
	InputTokens    int64
	OutputTokens   int64
	Error          string
	Client         string
	Relay          string
}

// Open creates the database and schema, and seeds a default key on first run.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	dsn := filepath.Join(dataDir, "mot-router.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
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
	if err := s.migrateProxies(); err != nil {
		return nil, err
	}
	keys, err := s.ListKeys()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		if _, err := s.CreateKey("default"); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS api_keys (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT    NOT NULL,
			key           TEXT    NOT NULL UNIQUE,
			created_at    INTEGER NOT NULL,
			last_used_at  INTEGER,
			request_count INTEGER NOT NULL DEFAULT 0,
			revoked       INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			ts              INTEGER NOT NULL,
			key_id          INTEGER,
			key_name        TEXT,
			surface         TEXT    NOT NULL,
			model           TEXT    NOT NULL,
			requested_model TEXT    NOT NULL DEFAULT '',
			provider        TEXT    NOT NULL DEFAULT '',
			status          INTEGER NOT NULL,
			latency_ms      INTEGER NOT NULL,
			input_tokens    INTEGER NOT NULL DEFAULT 0,
			output_tokens   INTEGER NOT NULL DEFAULT 0,
			error           TEXT,
			client          TEXT,
			relay           TEXT    NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests (ts DESC)`,
	}
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// Backfill columns added after the initial release. SQLite has no
	// `ADD COLUMN IF NOT EXISTS`, so a duplicate-column error is expected and
	// means the column is already present.
	for _, col := range []struct{ name, ddl string }{
		{"requested_model", `ALTER TABLE requests ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`},
		{"provider", `ALTER TABLE requests ADD COLUMN provider TEXT NOT NULL DEFAULT ''`},
		{"relay", `ALTER TABLE requests ADD COLUMN relay TEXT NOT NULL DEFAULT ''`},
	} {
		if _, err := s.db.Exec(col.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("add column %s: %w", col.name, err)
		}
	}

	return nil
}

// CreateKey issues a new router credential with the `mr_` prefix.
func (s *Store) CreateKey(name string) (*APIKey, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	key := "mr_" + hex.EncodeToString(buf)

	res, err := s.db.Exec(
		`INSERT INTO api_keys (name, key, created_at) VALUES (?, ?, ?)`,
		name, key, time.Now().UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("insert key: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.GetKey(id)
}

// GetKey fetches one key by id.
func (s *Store) GetKey(id int64) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, name, key, created_at, last_used_at, request_count, revoked
		   FROM api_keys WHERE id = ?`, id)
	return scanKey(row)
}

// ListKeys returns every key, newest first.
func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query(
		`SELECT id, name, key, created_at, last_used_at, request_count, revoked
		   FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer rows.Close()

	keys := []APIKey{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, *k)
	}
	return keys, rows.Err()
}

// FindValidKey resolves an active key by its secret value.
func (s *Store) FindValidKey(key string) (*APIKey, error) {
	row := s.db.QueryRow(
		`SELECT id, name, key, created_at, last_used_at, request_count, revoked
		   FROM api_keys WHERE key = ? AND revoked = 0`, key)
	k, err := scanKey(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// SetKeyRevoked enables or disables a key. Revoked keys fail authentication
// immediately.
func (s *Store) SetKeyRevoked(id int64, revoked bool) (bool, error) {
	val := 0
	if revoked {
		val = 1
	}
	res, err := s.db.Exec(`UPDATE api_keys SET revoked = ? WHERE id = ?`, val, id)
	if err != nil {
		return false, fmt.Errorf("update key: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteKey removes a key permanently.
func (s *Store) DeleteKey(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete key: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchKey records key usage. Called once per proxied request.
func (s *Store) TouchKey(id int64) error {
	_, err := s.db.Exec(
		`UPDATE api_keys
		    SET last_used_at = ?, request_count = request_count + 1
		  WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// LogRequest records a proxied request for the dashboard.
func (s *Store) LogRequest(in LogInput) error {
	_, err := s.db.Exec(
		`INSERT INTO requests
		   (ts, key_id, key_name, surface, model, requested_model, provider,
		    status, latency_ms, input_tokens, output_tokens, error, client, relay)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), in.KeyID, in.KeyName, in.Surface, in.Model,
		in.RequestedModel, in.Provider,
		in.Status, in.LatencyMs, in.InputTokens, in.OutputTokens,
		nullIfEmpty(in.Error), nullIfEmpty(in.Client), in.Relay,
	)
	return err
}

// RecentRequests returns the newest logged requests.
func (s *Store) RecentRequests(limit int) ([]RequestRow, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, key_id, key_name, surface, model, requested_model, provider,
		        status, latency_ms, input_tokens, output_tokens, error, client, relay
		   FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent requests: %w", err)
	}
	defer rows.Close()

	out := []RequestRow{}
	for rows.Next() {
		var r RequestRow
		if err := rows.Scan(&r.ID, &r.Ts, &r.KeyID, &r.KeyName, &r.Surface, &r.Model,
			&r.RequestedModel, &r.Provider,
			&r.Status, &r.LatencyMs, &r.InputTokens, &r.OutputTokens, &r.Error, &r.Client,
			&r.Relay); err != nil {
			return nil, fmt.Errorf("scan request: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearLogs removes all request history. Keys and settings are untouched.
func (s *Store) ClearLogs() error {
	_, err := s.db.Exec(`DELETE FROM requests`)
	return err
}

// Stats aggregates counters for the dashboard summary cards.
func (s *Store) Stats() (*Stats, error) {
	var st Stats
	err := s.db.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN status >= 200 AND status < 400 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(input_tokens), 0),
		        COALESCE(SUM(output_tokens), 0),
		        COALESCE(CAST(AVG(latency_ms) AS INTEGER), 0)
		   FROM requests`).
		Scan(&st.Total, &st.OK, &st.InputTokens, &st.OutputTokens, &st.AvgMs)
	if err != nil {
		return nil, fmt.Errorf("stats totals: %w", err)
	}
	st.Failed = st.Total - st.OK

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM requests WHERE ts > ?`,
		time.Now().Add(-24*time.Hour).UnixMilli()).Scan(&st.Last24h); err != nil {
		return nil, fmt.Errorf("stats 24h: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT model, COUNT(*) AS n FROM requests
		  GROUP BY model ORDER BY n DESC LIMIT 10`)
	if err != nil {
		return nil, fmt.Errorf("stats by model: %w", err)
	}
	defer rows.Close()
	st.ByModel = []ModelCount{}
	for rows.Next() {
		var m ModelCount
		if err := rows.Scan(&m.Model, &m.N); err != nil {
			return nil, fmt.Errorf("scan model count: %w", err)
		}
		st.ByModel = append(st.ByModel, m)
	}
	return &st, rows.Err()
}

// GetSetting reads a configuration value, returning "" when unset.
func (s *Store) GetSetting(key string) string {
	var value string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value); err != nil {
		return ""
	}
	return value
}

// SetSetting writes a configuration value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// rowScanner unifies *sql.Row and *sql.Rows so scanKey serves both.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanKey(r rowScanner) (*APIKey, error) {
	var k APIKey
	if err := r.Scan(&k.ID, &k.Name, &k.Key, &k.CreatedAt, &k.LastUsedAt,
		&k.RequestCount, &k.Revoked); err != nil {
		return nil, err
	}
	return &k, nil
}

// ListSettings returns every setting whose key starts with prefix, with the
// prefix stripped. Used to enumerate model aliases for the dashboard.
func (s *Store) ListSettings(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(
		`SELECT key, value FROM settings WHERE key LIKE ? ORDER BY key`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		out[strings.TrimPrefix(k, prefix)] = v
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
