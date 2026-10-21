package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type QuotaIssue struct {
	RequestID     string `json:"request_id"`
	Model         string `json:"model"`
	Surface       string `json:"surface"`
	StartedAt     int64  `json:"started_at"`
	Reason        string `json:"reason"`
	PartialInput  int64  `json:"partial_input_tokens"`
	PartialOutput int64  `json:"partial_output_tokens"`
}

func (s *Store) GetQuotaIssue(ctx context.Context, requestID string) (*QuotaIssue, error) {
	var q QuotaIssue
	err := s.db.QueryRowContext(ctx, `SELECT request_id,model,surface,ts,reason,in_tokens,out_tokens FROM quota_requests WHERE request_id=? AND state='unknown'`, requestID).Scan(&q.RequestID, &q.Model, &q.Surface, &q.StartedAt, &q.Reason, &q.PartialInput, &q.PartialOutput)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

var ErrQuotaBlocked = errors.New("token quota account is not ready")

func quotaID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

// BeginQuotaRequest atomically admits at most one inference for a token-plan user.
func (s *Store) BeginQuotaRequest(ctx context.Context, userID int64, surface, model string, tokenModel bool) (string, error) {
	id, e := quotaID()
	if e != nil {
		return "", e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var plan, state string
	var limit, used, expires int64
	var revoked int
	e = tx.QueryRowContext(ctx, `SELECT plan_type,quota_state,token_limit,quota_used,expires_at,revoked FROM users WHERE id=?`, userID).Scan(&plan, &state, &limit, &used, &expires, &revoked)
	if e != nil {
		return "", e
	}
	if plan != "token" {
		return "", nil
	}
	if revoked != 0 || (expires > 0 && time.Now().UnixMilli() >= expires) || used >= limit || state != "ready" || !tokenModel {
		return "", ErrQuotaBlocked
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO quota_requests(request_id,user_id,ts,surface,model,state) VALUES(?,?,?,?,?,'inflight')`, id, userID, time.Now().UnixMilli(), surface, model)
	if e != nil {
		return "", e
	}
	_, e = tx.ExecContext(ctx, `UPDATE users SET quota_state='inflight',quota_pending_request_id=? WHERE id=? AND quota_state='ready'`, id, userID)
	if e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	return id, nil
}

// SettleQuotaRequest records the response once. Missing final usage after a
// successful response quarantines the account until an administrator resolves it.
func (s *Store) SettleQuotaRequest(ctx context.Context, id string, in UsageInput, complete, hadResponse bool) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var userID int64
	var state string
	var requestedModel string
	e = tx.QueryRowContext(ctx, `SELECT user_id,state,model FROM quota_requests WHERE request_id=?`, id).Scan(&userID, &state, &requestedModel)
	if e != nil {
		return e
	}
	if state != "inflight" {
		return nil
	}
	var plan string
	if e = tx.QueryRowContext(ctx, `SELECT plan_type FROM users WHERE id=?`, userID).Scan(&plan); e != nil {
		return e
	}
	if plan != "token" {
		_, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state='done' WHERE request_id=?`, id)
		if e != nil {
			return e
		}
		return tx.Commit()
	}
	accounting := "unverified"
	qstate := "ready"
	reqState := "unknown"
	if complete && in.InTokens >= 0 && in.OutTokens >= 0 && in.Model != "" && in.Model == requestedModel {
		accounting = "reported"
		qstate = "ready"
		reqState = "done"
	} else if !hadResponse || in.Status < 200 || (in.Status >= 300 && in.Status < 500) {
		accounting = "not_charged"
		qstate = "ready"
		reqState = "done"
	}
	if reqState == "unknown" {
		qstate = "unknown"
	}
	_, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state=?,in_tokens=?,out_tokens=?,reason=? WHERE request_id=?`, reqState, in.InTokens, in.OutTokens, func() string {
		if reqState == "unknown" {
			return "Upstream response ended without complete token usage"
		}
		return ""
	}(), id)
	if e != nil {
		return e
	}
	storedIn, storedOut := in.InTokens, in.OutTokens
	if accounting != "reported" {
		storedIn, storedOut = 0, 0
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO user_requests(user_id,ts,surface,model,status,latency_ms,in_tokens,out_tokens,accounting_status,quota_request_id) VALUES(?,?,?,?,?,?,?,?,?,?)`, userID, time.Now().UnixMilli(), in.Surface, in.Model, in.Status, in.Latency, storedIn, storedOut, accounting, id)
	if e != nil {
		return e
	}
	if accounting == "reported" {
		_, e = tx.ExecContext(ctx, `UPDATE users SET request_count=request_count+1,tokens_in=tokens_in+?,tokens_out=tokens_out+?,quota_used=quota_used+?,last_used_at=?,quota_state=?,quota_pending_request_id='' WHERE id=?`, in.InTokens, in.OutTokens, in.InTokens+in.OutTokens, time.Now().UnixMilli(), qstate, userID)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE users SET request_count=request_count+1,last_used_at=?,quota_state=?,quota_pending_request_id=? WHERE id=?`, time.Now().UnixMilli(), qstate, func() string {
			if reqState == "unknown" {
				return id
			}
			return ""
		}(), userID)
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}

// QuarantineQuotaRequest keeps the account blocked if even the atomic settle
// transaction fails, while making the issue visible and resolvable by admin.
func (s *Store) QuarantineQuotaRequest(ctx context.Context, id, reason string) error {
	if len(reason) > 240 {
		reason = reason[:240]
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var userID, ts int64
	var surface, model string
	if e = tx.QueryRowContext(ctx, `SELECT user_id,ts,surface,model FROM quota_requests WHERE request_id=? AND state='inflight'`, id).Scan(&userID, &ts, &surface, &model); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state='unknown',reason=? WHERE request_id=? AND state='inflight'`, "Quota settlement failed; admin review required: "+reason, id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE users SET quota_state='unknown',quota_pending_request_id=? WHERE id=? AND plan_type='token'`, id, userID); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO user_requests(user_id,ts,surface,model,status,latency_ms,in_tokens,out_tokens,accounting_status,quota_request_id) VALUES(?,?,?,?,0,0,0,0,'unverified',?)`, userID, ts, surface, model, id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE users SET request_count=request_count+1 WHERE id=? AND plan_type='token'`, userID); e != nil {
		return e
	}
	return tx.Commit()
}

// RecoverQuotaRequests quarantines requests interrupted by a process restart.
func (s *Store) RecoverQuotaRequests(ctx context.Context) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, `SELECT request_id,user_id,ts,surface,model FROM quota_requests WHERE state='inflight'`)
	if e != nil {
		return e
	}
	type item struct {
		id             string
		user           int64
		ts             int64
		surface, model string
	}
	var all []item
	for rows.Next() {
		var it item
		if e = rows.Scan(&it.id, &it.user, &it.ts, &it.surface, &it.model); e != nil {
			rows.Close()
			return e
		}
		all = append(all, it)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return e
	}
	rows.Close()
	for _, it := range all {
		if _, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state='unknown',reason='Motgate restarted during inference; usage requires admin review' WHERE request_id=?`, it.id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE users SET quota_state='unknown',quota_pending_request_id=? WHERE id=? AND plan_type='token'`, it.id, it.user); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO user_requests(user_id,ts,surface,model,status,latency_ms,in_tokens,out_tokens,accounting_status,quota_request_id) VALUES(?,?,?,?,0,0,0,0,'unverified',?)`, it.user, it.ts, it.surface, it.model, it.id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE users SET request_count=request_count+1 WHERE id=? AND plan_type='token'`, it.user); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) TopUp(ctx context.Context, id int64, tokens int64, days int, now int64) (*User, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var exp int64
	var plan string
	var currentLimit int64
	if e = tx.QueryRowContext(ctx, `SELECT expires_at,plan_type,token_limit FROM users WHERE id=?`, id).Scan(&exp, &plan, &currentLimit); e != nil {
		return nil, e
	}
	if tokens > 0 && (currentLimit < 0 || tokens > int64(^uint64(0)>>1)-currentLimit) {
		return nil, fmt.Errorf("token quota exceeds supported integer range")
	}
	if plan != "token" && tokens > 0 && days == 0 {
		return nil, fmt.Errorf("add an expiry duration when converting unlimited to token quota")
	}
	base := now
	if exp > base {
		base = exp
	}
	newExpiry := exp
	if days > 0 {
		newExpiry = base + int64(days)*86400000
	}
	if tokens > 0 {
		if _, e = tx.ExecContext(ctx, `UPDATE users SET plan_type='token',token_limit=token_limit+?,expires_at=? WHERE id=?`, tokens, newExpiry, id); e != nil {
			return nil, e
		}
	} else if days > 0 {
		if _, e = tx.ExecContext(ctx, `UPDATE users SET expires_at=? WHERE id=?`, newExpiry, id); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return s.GetUser(id)
}

// ResolveUnknown applies actual supplied counts or explicitly waives a request.
func (s *Store) ResolveUnknown(ctx context.Context, requestID, mode, reason string, inTokens, outTokens int64) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var userID int64
	var state string
	if e = tx.QueryRowContext(ctx, `SELECT user_id,state FROM quota_requests WHERE request_id=?`, requestID).Scan(&userID, &state); e != nil {
		return e
	}
	if state != "unknown" {
		return fmt.Errorf("request is not awaiting review")
	}
	if mode == "bill" {
		if inTokens < 0 || outTokens < 0 {
			return fmt.Errorf("token counts must be non-negative")
		}
		if _, e = tx.ExecContext(ctx, `UPDATE users SET quota_used=quota_used+?,tokens_in=tokens_in+?,tokens_out=tokens_out+? WHERE id=?`, inTokens+outTokens, inTokens, outTokens, userID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state='done',in_tokens=?,out_tokens=?,reason='Admin supplied reported token usage' WHERE request_id=?`, inTokens, outTokens, requestID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE user_requests SET in_tokens=?,out_tokens=?,accounting_status='reported' WHERE quota_request_id=?`, inTokens, outTokens, requestID); e != nil {
			return e
		}
	} else if mode == "waive" {
		if len(reason) < 4 {
			return fmt.Errorf("waiver reason is required")
		}
		if _, e = tx.ExecContext(ctx, `UPDATE quota_requests SET state='waived',reason=? WHERE request_id=?`, reason, requestID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE user_requests SET accounting_status='waived' WHERE quota_request_id=?`, requestID); e != nil {
			return e
		}
	} else {
		return fmt.Errorf("mode must be bill or waive")
	}
	var pending string
	if e = tx.QueryRowContext(ctx, `SELECT quota_pending_request_id FROM users WHERE id=?`, userID).Scan(&pending); e != nil {
		return e
	}
	if pending == requestID {
		if _, e = tx.ExecContext(ctx, `UPDATE users SET quota_state='ready',quota_pending_request_id='' WHERE id=?`, userID); e != nil {
			return e
		}
	}
	return tx.Commit()
}
