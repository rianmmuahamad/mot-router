package gate

import (
	"container/list"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

const sessionCacheLimit = 4096
const timestampMask uint64 = 1<<48 - 1

type sessionKey struct {
	seed             [32]byte
	prefix           string
	hexLen, alnumLen int
}

type sessionEntry struct {
	key sessionKey
	id  string
}

type identifierGenerator struct {
	mu         sync.Mutex
	lastMillis uint64
	counter    uint64
	sessions   map[sessionKey]*list.Element
	order      list.List
}

var identifiers = identifierGenerator{sessions: make(map[sessionKey]*list.Element)}

// nextValue follows OpenCode Identifier.create: timestamp * 0x1000 + counter.
// The caller holds mu. Clock rollback and >4095 IDs/ms advance logical time
// rather than producing duplicate IDs or blocking a request.
func (g *identifierGenerator) nextValue(millis uint64) uint64 {
	return g.nextValueWithBits(millis, 12)
}
func (g *identifierGenerator) nextValueWithBits(millis uint64, bits int) uint64 {
	if millis > g.lastMillis {
		g.lastMillis, g.counter = millis, 0
	}
	g.counter++
	if g.counter >= uint64(1)<<bits {
		g.lastMillis++
		g.counter = 1
	}
	return g.lastMillis*(uint64(1)<<bits) + g.counter
}

func base62(hash [32]byte, length int) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	result := make([]byte, length)
	for i := range result {
		result[i] = alphabet[hash[i%len(hash)]%62]
	}
	return string(result)
}

// SessionID encodes a descending timestamp in the six-byte prefix, as the
// official CLI does. Hashing that prefix makes an ID regex-valid but invalid
// upstream. A bounded LRU keeps each conversation's canonical ID stable;
// hashed cache keys avoid retaining arbitrarily large caller strings.
func SessionID(seed, prefix string, hexLen, alnumLen int) string {
	if (prefix == "" || prefix == "ses_") && (hexLen <= 0 || hexLen == 12) && (alnumLen <= 0 || alnumLen == 14) {
		return SessionIDWithPolicy(seed, DefaultIdentity())
	}
	if prefix == "" {
		prefix = "ses_"
	}
	if hexLen <= 0 || hexLen > 64 {
		hexLen = DefaultSessionHex
	}
	if alnumLen <= 0 || alnumLen > 64 {
		alnumLen = DefaultSessionAlnum
	}
	key := sessionKey{sha256.Sum256([]byte(seed)), prefix, hexLen, alnumLen}
	g := &identifiers
	g.mu.Lock()
	defer g.mu.Unlock()
	if found := g.sessions[key]; found != nil {
		g.order.MoveToBack(found)
		return found.Value.(sessionEntry).id
	}
	value := g.nextValue(uint64(time.Now().UnixMilli()))
	hex := fmt.Sprintf("%012x", ^value&timestampMask)
	for len(hex) < hexLen {
		hex += hex
	}
	id := prefix + hex[:hexLen] + base62(key.seed, alnumLen)
	if g.order.Len() >= sessionCacheLimit {
		old := g.order.Front()
		delete(g.sessions, old.Value.(sessionEntry).key)
		g.order.Remove(old)
	}
	g.sessions[key] = g.order.PushBack(sessionEntry{key, id})
	return id
}

// RequestID uses the ascending timestamp prefix of Identifier.ascending.
// Every upstream attempt gets a fresh ID, including retries within one ms.
func RequestID(seed string) string {
	g := &identifiers
	g.mu.Lock()
	value := g.nextValue(uint64(time.Now().UnixMilli()))
	g.mu.Unlock()
	hash := sha256.Sum256([]byte(fmt.Sprintf("request:%s:%d", seed, value)))
	return fmt.Sprintf("msg_%012x", value&timestampMask) + base62(hash, 14)
}
