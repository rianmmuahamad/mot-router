package store

import (
	"database/sql"
	"fmt"
	"time"
)

// ProxyPool is a relay endpoint that upstream requests can be routed through.
// A Cloudflare Worker relay is deployed per pool, so each pool has a distinct
// egress path and its own request budget.
type ProxyPool struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	URL          string `json:"url"`
	Type         string `json:"type"` // "cloudflare", "deno", "vercel", "custom"
	NoProxy      string `json:"no_proxy"`
	Active       bool   `json:"active"`
	Strict       bool   `json:"strict"`
	CreatedAt    int64  `json:"created_at"`
	LastUsedAt   *int64 `json:"last_used_at"`
	LastError    string `json:"last_error"`
	LastTestedAt *int64 `json:"last_tested_at"`
	TestOK       *int   `json:"test_ok"`
	RequestCount int64  `json:"request_count"`
}

func (s *Store) migrateProxies() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS proxy_pools (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			name           TEXT    NOT NULL,
			url            TEXT    NOT NULL,
			type           TEXT    NOT NULL DEFAULT 'custom',
			no_proxy       TEXT    NOT NULL DEFAULT '',
			active         INTEGER NOT NULL DEFAULT 1,
			strict         INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL,
			last_used_at   INTEGER,
			last_error     TEXT    NOT NULL DEFAULT '',
			last_tested_at INTEGER,
			test_ok        INTEGER,
			request_count  INTEGER NOT NULL DEFAULT 0
		)`)
	if err != nil {
		return fmt.Errorf("migrate proxy_pools: %w", err)
	}
	return nil
}

// CreateProxyPool stores a new relay.
func (s *Store) CreateProxyPool(p *ProxyPool) (*ProxyPool, error) {
	active := 1
	if !p.Active {
		active = 0
	}
	strict := 0
	if p.Strict {
		strict = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO proxy_pools (name, url, type, no_proxy, active, strict, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.URL, p.Type, p.NoProxy, active, strict, time.Now().UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("insert proxy pool: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetProxyPool(id)
}

// GetProxyPool fetches one relay by id.
func (s *Store) GetProxyPool(id int64) (*ProxyPool, error) {
	row := s.db.QueryRow(`
		SELECT id, name, url, type, no_proxy, active, strict, created_at,
		       last_used_at, last_error, last_tested_at, test_ok, request_count
		  FROM proxy_pools WHERE id = ?`, id)
	return scanProxyPool(row)
}

// ListProxyPools returns every relay, oldest first so rotation order is stable.
func (s *Store) ListProxyPools() ([]ProxyPool, error) {
	rows, err := s.db.Query(`
		SELECT id, name, url, type, no_proxy, active, strict, created_at,
		       last_used_at, last_error, last_tested_at, test_ok, request_count
		  FROM proxy_pools ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list proxy pools: %w", err)
	}
	defer rows.Close()

	out := []ProxyPool{}
	for rows.Next() {
		p, err := scanProxyPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ActiveProxyPools returns relays eligible for use, in rotation order.
func (s *Store) ActiveProxyPools() ([]ProxyPool, error) {
	rows, err := s.db.Query(`
		SELECT id, name, url, type, no_proxy, active, strict, created_at,
		       last_used_at, last_error, last_tested_at, test_ok, request_count
		  FROM proxy_pools WHERE active = 1 ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list active proxy pools: %w", err)
	}
	defer rows.Close()

	out := []ProxyPool{}
	for rows.Next() {
		p, err := scanProxyPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// SetProxyPoolActive enables or disables a relay.
func (s *Store) SetProxyPoolActive(id int64, active bool) (bool, error) {
	v := 0
	if active {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE proxy_pools SET active = ? WHERE id = ?`, v, id)
	if err != nil {
		return false, fmt.Errorf("update proxy pool: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteProxyPool removes a relay.
func (s *Store) DeleteProxyPool(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM proxy_pools WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete proxy pool: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchProxyPool records a successful use.
func (s *Store) TouchProxyPool(id int64) error {
	_, err := s.db.Exec(
		`UPDATE proxy_pools
		    SET last_used_at = ?, request_count = request_count + 1, last_error = ''
		  WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// SetProxyPoolError records the last failure, used to skip a dead relay.
func (s *Store) SetProxyPoolError(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE proxy_pools SET last_error = ? WHERE id = ?`, msg, id)
	return err
}

// SetProxyPoolTest records the outcome of a connectivity test.
func (s *Store) SetProxyPoolTest(id int64, ok bool) error {
	v := 0
	if ok {
		v = 1
	}
	_, err := s.db.Exec(
		`UPDATE proxy_pools SET last_tested_at = ?, test_ok = ? WHERE id = ?`,
		time.Now().UnixMilli(), v, id)
	return err
}

func scanProxyPool(r rowScanner) (*ProxyPool, error) {
	var p ProxyPool
	var active, strict int
	if err := r.Scan(&p.ID, &p.Name, &p.URL, &p.Type, &p.NoProxy, &active, &strict,
		&p.CreatedAt, &p.LastUsedAt, &p.LastError, &p.LastTestedAt, &p.TestOK,
		&p.RequestCount); err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan proxy pool: %w", err)
	}
	p.Active = active == 1
	p.Strict = strict == 1
	return &p, nil
}
