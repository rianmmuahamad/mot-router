package store

import (
	"context"
	"testing"
)

func TestAdminUsageBoundariesAndAllAccounts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, _ := s.CreateUser("first", 0)
	b, _ := s.CreateUser("second", 0)
	const start int64 = 1000000000
	const day int64 = 86400000
	for _, record := range []struct {
		user, ts, in, out int64
		accounting        string
	}{
		{a.ID, start - 1, 1000, 2000, "reported"},
		{a.ID, start, 10, 4, "reported"},
		{b.ID, start + day, 20, 6, "reported"},
		{a.ID, start + day + 1, 0, 0, "unverified"},
		{b.ID, start + 7*day - 1, 30, 8, "reported"},
		{a.ID, start + 7*day, 3000, 4000, "reported"},
	} {
		_, err = s.db.Exec(`INSERT INTO user_requests(user_id,ts,surface,model,status,latency_ms,in_tokens,out_tokens,accounting_status) VALUES(?,?,'openai','model',200,1,?,?,?)`, record.user, record.ts, record.in, record.out, record.accounting)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.AdminUsage(context.Background(), start, start+7*day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Buckets) != 7 || result.Unverified != 1 || result.Timezone != "Asia/Jakarta" {
		t.Fatalf("unexpected chart: %+v", result)
	}
	var input, output, requests int64
	for _, bucket := range result.Buckets {
		input += bucket.TokensIn
		output += bucket.TokensOut
		requests += bucket.Requests
	}
	if input != 60 || output != 18 || requests != 4 {
		t.Fatalf("wrong accounting totals: %d/%d, requests=%d", input, output, requests)
	}
	if result.Buckets[0].TokensIn != 10 || result.Buckets[1].Requests != 2 || result.Buckets[6].TokensIn != 30 {
		t.Fatalf("wrong daily buckets: %+v", result.Buckets)
	}
	for _, bounds := range [][3]int64{{start, start, day}, {start, start + 8*day, day}, {start, start + day, 0}, {start, start + day, 1}} {
		if _, err := s.AdminUsage(context.Background(), bounds[0], bounds[1], bounds[2]); err == nil {
			t.Fatal("invalid usage bounds allowed")
		}
	}
}
