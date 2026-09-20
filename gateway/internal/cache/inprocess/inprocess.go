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
	"math/rand"
	"sync"
	"time"
)

// defaultMaxEntries is used when New/NewWithClock is given maxEntries <= 0.
// There is deliberately no "unbounded" mode — an unbounded in-process map
// is a real memory-growth risk this package used to have silently, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md's own motivation.
const defaultMaxEntries = 10_000

// defaultJitterFraction is New's real-production default (10%), per
// docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md — AWS's own
// caching-best-practices guidance (ttl = base + rand()*jitter) names this
// as a standard, trivial-to-implement thundering-herd-avoidance fix for a
// burst of entries written together (e.g. after a deploy, or a traffic
// spike) that would otherwise expire in near-lockstep.
const defaultJitterFraction = 0.10

// cacheEntry is a single stored response plus its absolute expiry time and
// the time of its most recent Put (writtenAt) — surfaced via Get per
// docs/upgrade-research/cache-2026-09-06.md Finding 6.
type cacheEntry struct {
	key       string
	data      []byte
	expiresAt time.Time
	writtenAt time.Time
}

// exactMatchTenantBucket holds one tenant's own entry set, independently
// LRU-capped — mirrors inprocess/lexical.go's LexicalCache tenantBucket
// exactly (named differently here only to avoid colliding with that
// exported-file-scoped type in this same package), added 2026-09-20 to
// close the same class of gap that file's own doc comment already named
// as a security requirement: a separate bucket per tenant, never one
// shared map filtered by tenantID, is what makes tenant eviction
// isolation structural rather than a post-hoc filter.
type exactMatchTenantBucket struct {
	entries map[string]*list.Element // value type *cacheEntry
	recency *list.List               // front = most recently used
}

// Cache is a mutex-protected, in-process, exact-match cache implementing
// cache.Cache, per-tenant-partitioned (see exactMatchTenantBucket) and bounded by a
// maximum entry count PER TENANT, with least-recently-used eviction once
// that per-tenant count is exceeded — never a shared cap across tenants,
// per the same 2026-09-20 finding exactMatchTenantBucket's own doc comment
// describes.
//
// The clock is injectable (via NewWithClock) so tests never need to sleep
// on wall-clock time to exercise TTL expiry, per docs/testing/TESTING.md
// §1's testing philosophy.
type Cache struct {
	mu             sync.Mutex
	tenants        map[string]*exactMatchTenantBucket
	maxEntries     int
	now            func() time.Time
	jitterFraction float64
	rand           func() float64
}

// New constructs an empty in-process Cache using the real wall clock,
// holding at most maxEntries entries (maxEntries <= 0 uses
// defaultMaxEntries — never "unbounded"), with real, on-by-default TTL
// jitter (defaultJitterFraction) via the real rand.Float64 — the
// production path, per docs/rfcs/2026-09-10-gateway-cache-ttl-jitter.md.
func New(maxEntries int) *Cache {
	return NewWithClockAndJitter(maxEntries, time.Now, defaultJitterFraction, rand.Float64)
}

// NewWithClock constructs an empty in-process Cache using the given clock
// function, for deterministic TTL testing. Deliberately ZERO jitter —
// this constructor's own long-standing contract is "deterministic TTL
// testing," which real (non-zero, non-deterministic) jitter would break
// for every existing exact-equality TTL-boundary test built against it.
// Callers wanting to test jitter itself use NewWithClockAndJitter
// directly, with an explicit jitterFraction and randFn.
func NewWithClock(maxEntries int, now func() time.Time) *Cache {
	return NewWithClockAndJitter(maxEntries, now, 0, func() float64 { return 0 })
}

// NewWithClockAndJitter is the fully-injectable constructor — mirrors
// ratelimit.RetryBackoff's own exact "inject the rand source, not just
// the clock" split (NewRetryBackoff vs. NewRetryBackoffWithRand). Put
// adds rand()*jitterFraction*ttl on top of the configured ttl —
// additive only, never shortening it.
func NewWithClockAndJitter(maxEntries int, now func() time.Time, jitterFraction float64, randFn func() float64) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &Cache{
		tenants:        map[string]*exactMatchTenantBucket{},
		maxEntries:     maxEntries,
		now:            now,
		jitterFraction: jitterFraction,
		rand:           randFn,
	}
}

// Get implements cache.Cache. Only ever looks within tenantID's own
// bucket — a tenant with no entries yet returns a miss, not an error,
// mirroring LexicalCache.Search's identical convention. A never-set key
// and an expired entry both return ok=false, err=nil — only this
// package's own bug would ever return a non-nil error here. A hit moves
// the entry to the front of that tenant's own recency list, so eviction
// (see Put) always removes that SAME tenant's least-recently *fetched*
// entry, not merely the least-recently *written* one, and never another
// tenant's.
func (c *Cache) Get(_ context.Context, tenantID, key string) ([]byte, time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.tenants[tenantID]
	if !ok {
		return nil, time.Time{}, false, nil
	}
	elem, found := bucket.entries[key]
	if !found {
		return nil, time.Time{}, false, nil
	}
	entry := elem.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.removeLocked(bucket, elem)
		return nil, time.Time{}, false, nil
	}
	bucket.recency.MoveToFront(elem)

	// Return a copy: cache.Cache's contract is value objects only, never
	// a pointer into anything outside this package (including our own
	// internal map's backing array), per docs/decisions/0002-cache-embedded-in-gateway.md.
	out := make([]byte, len(entry.data))
	copy(out, entry.data)
	return out, entry.writtenAt, true, nil
}

// Put implements cache.Cache. Creates tenantID's bucket on first write;
// inserting past maxEntries evicts that SAME tenant's own
// least-recently-used entry (the back of that tenant's own recency
// list) — never another tenant's. The capacity bound itself (originally
// added per docs/rfcs/2026-09-03-cache-l2-normalized-match.md's own
// Motivation section: this package previously had no capacity bound at
// all) is now applied per tenant, not globally, per this file's own
// 2026-09-20 tenant-partitioning fix (see exactMatchTenantBucket's own doc
// comment).
func (c *Cache) Put(_ context.Context, tenantID, key string, resp []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.tenants[tenantID]
	if !ok {
		bucket = &exactMatchTenantBucket{entries: make(map[string]*list.Element), recency: list.New()}
		c.tenants[tenantID] = bucket
	}

	data := make([]byte, len(resp))
	copy(data, resp)
	now := c.now()
	jitter := time.Duration(c.rand() * c.jitterFraction * float64(ttl))
	expiresAt := now.Add(ttl + jitter)

	if elem, found := bucket.entries[key]; found {
		elem.Value.(*cacheEntry).data = data
		elem.Value.(*cacheEntry).expiresAt = expiresAt
		elem.Value.(*cacheEntry).writtenAt = now
		bucket.recency.MoveToFront(elem)
		return nil
	}

	elem := bucket.recency.PushFront(&cacheEntry{key: key, data: data, expiresAt: expiresAt, writtenAt: now})
	bucket.entries[key] = elem

	if bucket.recency.Len() > c.maxEntries {
		c.removeLocked(bucket, bucket.recency.Back())
	}
	return nil
}

// Delete implements cache.Cache. Only ever removes from tenantID's own
// bucket. Removing a never-set key (or from a tenant with no bucket at
// all yet) is a no-op, not an error — a caller servicing an erasure
// request cares only that key is absent afterward, not whether it was
// ever present.
func (c *Cache) Delete(_ context.Context, tenantID, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.tenants[tenantID]
	if !ok {
		return nil
	}
	elem, found := bucket.entries[key]
	if !found {
		return nil
	}
	c.removeLocked(bucket, elem)
	return nil
}

// removeLocked deletes elem from both bucket's map and its own recency
// list. Callers must hold c.mu.
func (c *Cache) removeLocked(bucket *exactMatchTenantBucket, elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	delete(bucket.entries, entry.key)
	bucket.recency.Remove(elem)
}
