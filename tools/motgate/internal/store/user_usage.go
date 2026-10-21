package store

import (
	"context"
	"fmt"
	"strings"
)

// RequestUsage contains accounting metadata only, never prompts or credentials.
type RequestUsage struct {
	ID         int64  `json:"id"`
	Timestamp  int64  `json:"timestamp"`
	Surface    string `json:"surface"`
	Model      string `json:"model"`
	Status     int    `json:"status"`
	Latency    int64  `json:"latency_ms"`
	TokensIn   int64  `json:"tokens_in"`
	TokensOut  int64  `json:"tokens_out"`
	Accounting string `json:"accounting_status"`
}
type UsageBucket struct {
	Timestamp int64 `json:"timestamp"`
	Requests  int64 `json:"requests"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}
type UserUsage struct {
	Requests []RequestUsage `json:"requests"`
	Total    int64          `json:"total"`
	Page     int            `json:"page"`
	Limit    int            `json:"limit"`
	Models   []string       `json:"models"`
	Buckets  []UsageBucket  `json:"buckets"`
	Since    int64          `json:"since"`
	Until    int64          `json:"until"`
}

// UserUsage performs bounded reads scoped to one authenticated account. The
// chart covers a rolling period; the paginated history covers all account use.
func (s *Store) UserUsage(ctx context.Context, userID int64, since, until, step int64, page, limit int, model, search string) (*UserUsage, error) {
	if step <= 0 || until <= since || (until-since)/step > 32 || page < 1 || page > 10000 || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("invalid usage bounds")
	}
	result := &UserUsage{Requests: []RequestUsage{}, Models: []string{}, Buckets: []UsageBucket{}, Page: page, Limit: limit, Since: since, Until: until}
	where := "user_id = ?"
	args := []any{userID}
	if model != "" {
		where += " AND model = ?"
		args = append(args, model)
	}
	if search != "" {
		where += ` AND model LIKE ? ESCAPE '\'`
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(search)
		args = append(args, "%"+escaped+"%")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_requests WHERE "+where, args...).Scan(&result.Total); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, ts, surface, model, status, latency_ms, in_tokens, out_tokens, accounting_status FROM user_requests WHERE `+where+` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, (page-1)*limit)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r RequestUsage
		if err = rows.Scan(&r.ID, &r.Timestamp, &r.Surface, &r.Model, &r.Status, &r.Latency, &r.TokensIn, &r.TokensOut, &r.Accounting); err != nil {
			rows.Close()
			return nil, err
		}
		result.Requests = append(result.Requests, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT DISTINCT model FROM user_requests WHERE user_id = ? AND model != '' ORDER BY model LIMIT 200`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m string
		if err = rows.Scan(&m); err != nil {
			rows.Close()
			return nil, err
		}
		result.Models = append(result.Models, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	count := int((until - since + step - 1) / step)
	for i := 0; i < count; i++ {
		result.Buckets = append(result.Buckets, UsageBucket{Timestamp: since + int64(i)*step})
	}
	rows, err = s.db.QueryContext(ctx, `SELECT (ts - ?) / ?, COUNT(*), COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0) FROM user_requests WHERE user_id = ? AND ts >= ? AND ts < ? GROUP BY 1`, since, step, userID, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var b UsageBucket
		if err = rows.Scan(&index, &b.Requests, &b.TokensIn, &b.TokensOut); err != nil {
			return nil, err
		}
		if index >= 0 && index < count {
			b.Timestamp = result.Buckets[index].Timestamp
			result.Buckets[index] = b
		}
	}
	return result, rows.Err()
}
