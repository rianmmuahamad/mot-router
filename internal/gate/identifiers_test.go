package gate

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanonicalIdentifierTimestampAndStableSession(t *testing.T) {
	before := uint64(time.Now().UnixMilli())
	session := SessionID("timestamp-test", "ses_", 12, 14)
	request := RequestID("timestamp-test")
	after := uint64(time.Now().UnixMilli())
	for _, item := range []struct {
		id         string
		descending bool
	}{{session, true}, {request, false}} {
		if len(item.id) != 30 {
			t.Fatalf("invalid identifier length: %s", item.id)
		}
		raw, err := strconv.ParseUint(item.id[4:16], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		if item.descending {
			raw = ^raw & timestampMask
		}
		stamp, counter := raw>>12, raw&0xfff
		// The official CLI stores the low 48 bits: 36 timestamp bits and
		// 12 counter bits. Compare the same modulo window, not Unix epoch.
		start, end := before&(timestampMask>>12), after&(timestampMask>>12)
		if stamp < start || stamp > end || counter == 0 {
			t.Fatalf("prefix does not encode current timestamp/counter: %s stamp=%d counter=%d", item.id, stamp, counter)
		}
		for _, c := range item.id[16:] {
			if !strings.ContainsRune("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", c) {
				t.Fatal("suffix is not base62")
			}
		}
	}
	if SessionID("timestamp-test", "ses_", 12, 14) != session {
		t.Fatal("session changed between turns")
	}
	if RequestID("timestamp-test") == request {
		t.Fatal("request reused an old message identifier")
	}
}

func TestIdentifierCounterOverflowAndClockRollback(t *testing.T) {
	g := identifierGenerator{}
	previous := uint64(0)
	for i := 0; i < 5000; i++ {
		value := g.nextValue(1234)
		if value <= previous || value&0xfff == 0 {
			t.Fatal("counter overflow produced duplicate/invalid identifier")
		}
		previous = value
	}
	if g.nextValue(1000) <= previous {
		t.Fatal("clock rollback broke monotonicity")
	}
}

func TestIdentifierCacheBoundAndConcurrentRequestUniqueness(t *testing.T) {
	for i := 0; i < sessionCacheLimit+10; i++ {
		SessionID(fmt.Sprintf("cache-bound-%d", i), "ses_", 12, 14)
	}
	identifiers.mu.Lock()
	size := len(identifiers.sessions)
	identifiers.mu.Unlock()
	if size > sessionCacheLimit {
		t.Fatalf("unbounded session cache: %d", size)
	}
	var wg sync.WaitGroup
	ids := make(chan string, 200)
	for range 200 {
		wg.Add(1)
		go func() { defer wg.Done(); ids <- RequestID("same-conversation") }()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatal("concurrent request ID collision")
		}
		seen[id] = true
	}
}
