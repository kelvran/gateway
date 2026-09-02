// Package inprocess implements cache.Cache as adapter #1 (the ACTIVE
// adapter, per gateway/ARCHITECTURE.md's Cache Subsystem package layout):
// an in-process, mutex-protected, exact-match-only L1 cache. This pass
// does not implement L2 (normalized match) or L3 (semantic match) — those
// are PRD.md Phase 1 work, and L3 specifically must never ship without the
// entity/freshness hard-gate (see THREAT_MODEL.md's CacheAttack row), so a
// partial L3 is not built here at all.
package inprocess

import (
	"context"
	"sync"
	"time"
)

// cacheEntry is a single stored response plus its absolute expiry time.
type cacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// Cache is a mutex-protected, in-process, exact-match cache implementing
// cache.Cache.
//
// The clock is injectable (via NewWithClock) so tests never need to sleep
// on wall-clock time to exercise TTL expiry, per docs/testing/TESTING.md
// §1's testing philosophy.
type Cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	now     func() time.Time
}

// New constructs an empty in-process Cache using the real wall clock.
func New() *Cache {
	return NewWithClock(time.Now)
}

// NewWithClock constructs an empty in-process Cache using the given clock
// function, for deterministic TTL testing.
func NewWithClock(now func() time.Time) *Cache {
	return &Cache{
		entries: make(map[string]cacheEntry),
		now:     now,
	}
}

// Get implements cache.Cache. A never-set key and an expired entry both
// return ok=false, err=nil — only this package's own bug would ever
// return a non-nil error here.
func (c *Cache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, found := c.entries[key]
	if !found {
		return nil, false, nil
	}
	if c.now().After(entry.expiresAt) {
		delete(c.entries, key)
		return nil, false, nil
	}

	// Return a copy: cache.Cache's contract is value objects only, never
	// a pointer into anything outside this package (including our own
	// internal map's backing array), per docs/decisions/0002-cache-embedded-in-gateway.md.
	out := make([]byte, len(entry.data))
	copy(out, entry.data)
	return out, true, nil
}

// Put implements cache.Cache.
func (c *Cache) Put(_ context.Context, key string, resp []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data := make([]byte, len(resp))
	copy(data, resp)
	c.entries[key] = cacheEntry{
		data:      data,
		expiresAt: c.now().Add(ttl),
	}
	return nil
}
