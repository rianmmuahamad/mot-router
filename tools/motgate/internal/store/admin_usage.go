package store

import (
	"context"
	"fmt"
)

// AdminUsage is bounded accounting data for the whole portal, with no user keys
// or request content. Buckets include records whose final token report is missing;
// those records contribute no estimated tokens.
type AdminUsage struct {
	Buckets    []UsageBucket `json:"buckets"`
	Since      int64         `json:"since"`
	Until      int64         `json:"until"`
	Unverified int64         `json:"unverified_requests"`
	Timezone   string        `json:"timezone"`
}

func (s *Store) AdminUsage(ctx context.Context, since, until, step int64) (*AdminUsage, error) {
	if step <= 0 || until <= since || until-since > 7*86400000 || step < 86400000 {
		return nil, fmt.Errorf("invalid admin usage bounds")
	}
	count := int((until - since + step - 1) / step)
	result := &AdminUsage{Buckets: make([]UsageBucket, count), Since: since, Until: until, Timezone: "Asia/Jakarta"}
	for i := range result.Buckets {
		result.Buckets[i].Timestamp = since + int64(i)*step
	}
	rows, err := s.db.QueryContext(ctx, `SELECT (ts-?)/?, COUNT(*), COALESCE(SUM(in_tokens),0), COALESCE(SUM(out_tokens),0), SUM(CASE WHEN accounting_status='unverified' THEN 1 ELSE 0 END)
		FROM user_requests WHERE ts >= ? AND ts < ? GROUP BY 1`, since, step, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var bucket UsageBucket
		var unverified int64
		if err := rows.Scan(&index, &bucket.Requests, &bucket.TokensIn, &bucket.TokensOut, &unverified); err != nil {
			return nil, err
		}
		if index >= 0 && index < count {
			bucket.Timestamp = result.Buckets[index].Timestamp
			result.Buckets[index] = bucket
			result.Unverified += unverified
		}
	}
	return result, rows.Err()
}
