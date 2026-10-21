package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// IdentityPolicy is data, never downloaded or executed code. Unsupported new
// algorithms remain visible failures rather than being installed blindly.
type IdentityPolicy struct {
	Mode              string `json:"mode"`
	CounterBits       int    `json:"counter_bits"`
	TimestampBytes    int    `json:"timestamp_bytes"`
	SuffixLength      int    `json:"suffix_length"`
	SessionDescending bool   `json:"session_descending"`
	Epoch             int64  `json:"epoch,omitempty"`
}

func DefaultIdentity() IdentityPolicy {
	return IdentityPolicy{Mode: "timestamp", CounterBits: 12, TimestampBytes: 6, SuffixLength: 14, SessionDescending: true}
}
func (p IdentityPolicy) Normalized() IdentityPolicy {
	if p.Mode == "" {
		return DefaultIdentity()
	}
	return p
}
func (p IdentityPolicy) Valid() bool {
	p = p.Normalized()
	return (p.Mode == "timestamp" || p.Mode == "hash") && p.CounterBits >= 8 && p.CounterBits <= 16 && p.TimestampBytes >= 6 && p.TimestampBytes <= 8 && p.SuffixLength >= 8 && p.SuffixLength <= 32
}

func SessionIDWithPolicy(seed string, policy IdentityPolicy) string {
	p := policy.Normalized()
	if !p.Valid() {
		p = DefaultIdentity()
	}
	encoded, _ := json.Marshal(p)
	key := sessionKey{sha256.Sum256(append(encoded, []byte(seed)...)), "ses_", p.TimestampBytes * 2, p.SuffixLength}
	g := &identifiers
	g.mu.Lock()
	defer g.mu.Unlock()
	if found := g.sessions[key]; found != nil {
		g.order.MoveToBack(found)
		return found.Value.(sessionEntry).id
	}
	value := g.nextValueWithBits(uint64(time.Now().UnixMilli()), p.CounterBits)
	if p.SessionDescending {
		value = ^value
	}
	timestamp := encodedPrefix(value, p.TimestampBytes)
	if p.Mode == "hash" {
		timestamp = hex.EncodeToString(key.seed[:p.TimestampBytes])
	}
	id := "ses_" + timestamp + base62(key.seed, p.SuffixLength)
	if g.order.Len() >= sessionCacheLimit {
		old := g.order.Front()
		delete(g.sessions, old.Value.(sessionEntry).key)
		g.order.Remove(old)
	}
	g.sessions[key] = g.order.PushBack(sessionEntry{key, id})
	return id
}

func RequestIDWithPolicy(seed string, policy IdentityPolicy) string {
	p := policy.Normalized()
	if !p.Valid() {
		p = DefaultIdentity()
	}
	g := &identifiers
	g.mu.Lock()
	value := g.nextValueWithBits(uint64(time.Now().UnixMilli()), p.CounterBits)
	g.mu.Unlock()
	hash := sha256.Sum256([]byte(fmt.Sprintf("request:%s:%d", seed, value)))
	prefix := encodedPrefix(value, p.TimestampBytes)
	if p.Mode == "hash" {
		prefix = hex.EncodeToString(hash[:p.TimestampBytes])
	}
	return "msg_" + prefix + base62(hash, p.SuffixLength)
}
func encodedPrefix(value uint64, bytes int) string {
	if bytes < 8 {
		value &= (uint64(1) << (bytes * 8)) - 1
	}
	return fmt.Sprintf("%0*x", bytes*2, value)
}
