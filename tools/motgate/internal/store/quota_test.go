package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQuotaAdmissionSettlementAndUnverifiedHold(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.CreateUserWithPlan("quota", time.Now().Add(time.Hour).UnixMilli(), Plan{Type: "token", TokenLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true)
	if err != nil || id == "" {
		t.Fatalf("begin: %q %v", id, err)
	}
	if _, err = s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true); !errors.Is(err, ErrQuotaBlocked) {
		t.Fatalf("parallel request should be denied, got %v", err)
	}
	in := UsageInput{UserID: u.ID, Surface: "openai", Model: "deepseek-v4.1-flash", Status: 200, InTokens: 70, OutTokens: 40}
	if err = s.SettleQuotaRequest(context.Background(), id, in, true, true); err != nil {
		t.Fatal(err)
	}
	if err = s.SettleQuotaRequest(context.Background(), id, in, true, true); err != nil {
		t.Fatal("idempotent settlement:", err)
	}
	u, _ = s.GetUser(u.ID)
	if u.QuotaUsed != 110 || u.TokensIn != 70 || u.TokensOut != 40 || u.QuotaState != "ready" {
		t.Fatalf("unexpected settled account: %+v", u)
	}
	if _, err = s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true); !errors.Is(err, ErrQuotaBlocked) {
		t.Fatalf("overspent account admitted new request: %v", err)
	}
}

func TestLegacyAccountsMigrateToUnlimitedWithoutLosingStats(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "motgate.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE users(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,key TEXT NOT NULL UNIQUE,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0,request_count INTEGER NOT NULL DEFAULT 0,tokens_in INTEGER NOT NULL DEFAULT 0,tokens_out INTEGER NOT NULL DEFAULT 0,last_used_at INTEGER)`,
		`INSERT INTO users(name,key,created_at,expires_at,request_count,tokens_in,tokens_out) VALUES('legacy','mg_legacy',1,9999999999999,8,120,45)`,
		`CREATE TABLE user_requests(id INTEGER PRIMARY KEY,user_id INTEGER NOT NULL,ts INTEGER NOT NULL,surface TEXT NOT NULL,model TEXT NOT NULL,status INTEGER NOT NULL,latency_ms INTEGER NOT NULL,in_tokens INTEGER NOT NULL DEFAULT 0,out_tokens INTEGER NOT NULL DEFAULT 0)`,
	} {
		if _, err = db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.GetUser(1)
	if err != nil {
		t.Fatal(err)
	}
	if u.PlanType != "unlimited" || u.TokenLimit != 0 || u.QuotaUsed != 0 || u.RequestCount != 8 || u.TokensIn != 120 || u.TokensOut != 45 {
		t.Fatalf("legacy counters/plan changed during migration: %+v", u)
	}
}

func TestQuotaConcurrentAdmissionAllowsOnlyOne(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.CreateUserWithPlan("race", time.Now().Add(time.Hour).UnixMilli(), Plan{Type: "token", TokenLimit: 500})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, e := s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true)
			ids <- id
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	accepted, denied := 0, 0
	for e := range errs {
		if e == nil {
			accepted++
		} else if errors.Is(e, ErrQuotaBlocked) {
			denied++
		} else {
			t.Fatalf("unexpected concurrent admission error: %v", e)
		}
	}
	if accepted != 1 || denied != 1 {
		t.Fatalf("expected one admitted and one denied, got accepted=%d denied=%d", accepted, denied)
	}
}

func TestQuotaExpiryRevocationAndUserIsolation(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().UnixMilli()
	expired, _ := s.CreateUserWithPlan("expired", now-1, Plan{Type: "token", TokenLimit: 100})
	if _, err = s.BeginQuotaRequest(context.Background(), expired.ID, "openai", "deepseek-v4.1-flash", true); !errors.Is(err, ErrQuotaBlocked) {
		t.Fatalf("expired account admitted: %v", err)
	}
	revoked, _ := s.CreateUserWithPlan("revoked", now+60000, Plan{Type: "token", TokenLimit: 100})
	_, _ = s.SetRevoked(revoked.ID, true)
	if _, err = s.BeginQuotaRequest(context.Background(), revoked.ID, "openai", "deepseek-v4.1-flash", true); !errors.Is(err, ErrQuotaBlocked) {
		t.Fatalf("revoked account admitted: %v", err)
	}
	active, _ := s.CreateUserWithPlan("active", now+60000, Plan{Type: "token", TokenLimit: 100})
	if id, err := s.BeginQuotaRequest(context.Background(), active.ID, "openai", "deepseek-v4.1-flash", true); err != nil || id == "" {
		t.Fatalf("another account should retain its own quota: id=%q err=%v", id, err)
	}
}

func TestUnknownUsageRequiresAdminResolution(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, _ := s.CreateUserWithPlan("quota", time.Now().Add(time.Hour).UnixMilli(), Plan{Type: "token", TokenLimit: 100})
	id, _ := s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true)
	if err = s.SettleQuotaRequest(context.Background(), id, UsageInput{UserID: u.ID, Surface: "openai", Model: "deepseek-v4.1-flash", Status: 200}, false, true); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(u.ID)
	if u.QuotaState != "unknown" || u.QuotaUsed != 0 {
		t.Fatalf("unknown usage should quarantine without estimating: %+v", u)
	}
	if _, err = s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true); !errors.Is(err, ErrQuotaBlocked) {
		t.Fatalf("unknown request did not hold account: %v", err)
	}
	if err = s.ResolveUnknown(context.Background(), id, "bill", "", 12, 8); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(u.ID)
	if u.QuotaState != "ready" || u.QuotaUsed != 20 {
		t.Fatalf("admin billing did not resolve account: %+v", u)
	}
}

func TestQuotaRestartRecoveryAndTopUp(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.CreateUserWithPlan("quota", time.Now().Add(time.Hour).UnixMilli(), Plan{Type: "token", TokenLimit: 100})
	id, _ := s.BeginQuotaRequest(context.Background(), u.ID, "openai", "deepseek-v4.1-flash", true)
	if err = s.RecoverQuotaRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUser(u.ID)
	if u.QuotaState != "unknown" || u.QuotaPending != id {
		t.Fatalf("inflight request not recovered: %+v", u)
	}
	if err = s.ResolveUnknown(context.Background(), id, "waive", "No final usage available", 0, 0); err != nil {
		t.Fatal(err)
	}
	oldExpiry := u.ExpiresAt
	u, err = s.TopUp(context.Background(), u.ID, 50, 2, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if u.TokenLimit != 150 || u.ExpiresAt < oldExpiry+2*86400000-1000 {
		t.Fatalf("topup failed: %+v", u)
	}
	_ = s.Close()
}
