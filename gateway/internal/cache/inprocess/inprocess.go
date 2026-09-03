// Package inprocess implements cache.Cache as adapter #1 (the ACTIVE
// adapter, per gateway/ARCHITECTURE.md's Cache Subsystem package layout):
// an in-process, mutex-protected, exact-match-only L1 cache. This pass
// does not implement L3 (semantic match) — L3 specifically must never
// ship without the entity/freshness hard-gate (see THREAT_MODEL.md's
// CacheAttack row), so a partial L3 is not built here at all. L2
// (normalized match) is built on top of a second instance of this same
// type — see docs/rfcs/2026-09-03-cache-l2-normalized-match.md.
package inprocess

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// defaultMaxEntries is used when New/NewWithClock is given maxEntries <= 0.
// There is deliberately no "unbounded" mode — an unbounded in-process map
// is a real memory-growth risk this package used to have silently, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md's own motivation.
const defaultMaxEntries = 10_000

// cacheEntry is a single stored response plus its absolute expiry time.
type cacheEntry struct {
	key       string
	data      []byte
	expiresAt time.Time
}

// Cache is a mutex-protected, in-process, exact-match cache implementing
// cache.Cache, bounded by a maximum entry count with least-recently-used
// eviction once that count is exceeded.
//
// The clock is injectable (via NewWithClock) so tests never need to sleep
// on wall-clock time to exercise TTL expiry, per docs/testing/TESTING.md
// §1's testing philosophy.
type Cache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element // value type *cacheEntry
	recency    *list.List               // front = most recently used
	maxEntries int
	now        func() time.Time
}

// New constructs an empty in-process Cache using the real wall clock,
// holding at most maxEntries entries (maxEntries <= 0 uses
// defaultMaxEntries — never "unbounded").
func New(maxEntries int) *Cache {
	return NewWithClock(maxEntries, time.Now)
}

// NewWithClock constructs an empty in-process Cache using the given clock
// function, for deterministic TTL testing.
func NewWithClock(maxEntries int, now func() time.Time) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &Cache{
		entries:    make(map[string]*list.Element),
		recency:    list.New(),
		maxEntries: maxEntries,
		now:        now,
	}
}

// Get implements cache.Cache. A never-set key and an expired entry both
// return ok=false, err=nil — only this package's own bug would ever
// return a non-nil error here. A hit moves the entry to the front of the
// recency list, so eviction (see Put) always removes the least-recently
// *fetched* entry, not merely the least-recently *written* one.
func (c *Cache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, found := c.entries[key]
	if !found {
		return nil, false, nil
	}
	entry := elem.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.removeLocked(elem)
		return nil, false, nil
	}
	c.recency.MoveToFront(elem)

	// Return a copy: cache.Cache's contract is value objects only, never
	// a pointer into anything outside this package (including our own
	// internal map's backing array), per docs/decisions/0002-cache-embedded-in-gateway.md.
	out := make([]byte, len(entry.data))
	copy(out, entry.data)
	return out, true, nil
}

// Put implements cache.Cache. Inserting past maxEntries evicts the
// least-recently-used entry (the back of the recency list) — this is the
// prerequisite docs/rfcs/2026-09-03-cache-l2-normalized-match.md's own
// Motivation section describes: this package previously had no capacity
// bound at all.
func (c *Cache) Put(_ context.Context, key string, resp []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data := make([]byte, len(resp))
	copy(data, resp)
	expiresAt := c.now().Add(ttl)

	if elem, found := c.entries[key]; found {
		elem.Value.(*cacheEntry).data = data
		elem.Value.(*cacheEntry).expiresAt = expiresAt
		c.recency.MoveToFront(elem)
		return nil
	}

	elem := c.recency.PushFront(&cacheEntry{key: key, data: data, expiresAt: expiresAt})
	c.entries[key] = elem

	if c.recency.Len() > c.maxEntries {
		c.removeLocked(c.recency.Back())
	}
	return nil
}

// removeLocked deletes elem from both the map and the recency list.
// Callers must hold c.mu.
func (c *Cache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	c.recency.Remove(elem)
}
