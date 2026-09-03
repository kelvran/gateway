package inprocess

import (
	"container/list"
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kelvran/gateway/gateway/internal/cache"
)

// lexicalEntry is one stored Cache L3-lite candidate.
type lexicalEntry struct {
	signature   []uint64
	resp        []byte
	fingerprint map[string]struct{}
	writtenAt   time.Time
	modelID     string
	expiresAt   time.Time
}

// tenantBucket holds one tenant's own candidate set, independently
// LRU-capped. A separate bucket per tenant — never one shared list
// filtered by tenantID during Search — is what makes tenant isolation
// structural rather than a post-hoc filter, per THREAT_MODEL.md's
// KeyPooling mitigation ("baked into the vector-index partition itself").
type tenantBucket struct {
	entries *list.List // value type *lexicalEntry; front = most recently used
}

// LexicalCache implements cache.LexicalCache: an in-process,
// mutex-protected, per-tenant-partitioned brute-force Jaccard-similarity
// scan, per docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md's
// own research (no ANN needed at Kelvran's likely scale — a brute-force
// scan over an LRU-capped candidate set is cheap; see that RFC's
// Research Trail). Each tenant's cap is independent — total memory
// scales with active-tenant-count × maxEntries, a real, documented
// tradeoff against L1/L2's single shared-map cap, accepted here because
// true partitioning is a security requirement, not a style choice.
type LexicalCache struct {
	mu         sync.Mutex
	tenants    map[string]*tenantBucket
	maxEntries int
	now        func() time.Time
}

// NewLexicalCache constructs an empty LexicalCache using the real wall
// clock. maxEntries <= 0 uses the same defaultMaxEntries as Cache (L1/L2)
// — never "unbounded," applied per tenant.
func NewLexicalCache(maxEntries int) *LexicalCache {
	return NewLexicalCacheWithClock(maxEntries, time.Now)
}

// NewLexicalCacheWithClock constructs an empty LexicalCache using the
// given clock function, for deterministic TTL testing.
func NewLexicalCacheWithClock(maxEntries int, now func() time.Time) *LexicalCache {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &LexicalCache{
		tenants:    map[string]*tenantBucket{},
		maxEntries: maxEntries,
		now:        now,
	}
}

// Search implements cache.LexicalCache. Only ever scans tenantID's own
// bucket — a tenant with no entries yet returns zero candidates, not an
// error. Expired entries are lazily reaped on the scan they're
// encountered in, mirroring Cache's own lazy-TTL-expiry precedent.
func (c *LexicalCache) Search(_ context.Context, tenantID string, signature []uint64, k int) ([]cache.LexicalCandidate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.tenants[tenantID]
	if !ok {
		return nil, nil
	}

	now := c.now()
	type scored struct {
		elem  *list.Element
		entry *lexicalEntry
		sim   float64
	}
	var candidates []scored
	for e := bucket.entries.Front(); e != nil; {
		next := e.Next()
		entry := e.Value.(*lexicalEntry)
		if now.After(entry.expiresAt) {
			bucket.entries.Remove(e)
			e = next
			continue
		}
		if sim, ok := cache.JaccardEstimate(signature, entry.signature); ok {
			candidates = append(candidates, scored{elem: e, entry: entry, sim: sim})
		}
		e = next
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].sim > candidates[j].sim })
	if len(candidates) > k {
		candidates = candidates[:k]
	}

	result := make([]cache.LexicalCandidate, 0, len(candidates))
	for _, sc := range candidates {
		bucket.entries.MoveToFront(sc.elem) // a search hit counts as "used" for LRU purposes

		respCopy := make([]byte, len(sc.entry.resp))
		copy(respCopy, sc.entry.resp)
		fpCopy := make(map[string]struct{}, len(sc.entry.fingerprint))
		for k := range sc.entry.fingerprint {
			fpCopy[k] = struct{}{}
		}

		result = append(result, cache.LexicalCandidate{
			Resp:        respCopy,
			Similarity:  sc.sim,
			Fingerprint: fpCopy,
			WrittenAt:   sc.entry.writtenAt,
			ModelID:     sc.entry.modelID,
		})
	}
	return result, nil
}

// Put implements cache.LexicalCache. Creates tenantID's bucket on first
// write; inserting past maxEntries evicts that tenant's own
// least-recently-used entry — never another tenant's.
func (c *LexicalCache) Put(_ context.Context, tenantID string, signature []uint64, resp []byte, fingerprint map[string]struct{}, modelID string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.tenants[tenantID]
	if !ok {
		bucket = &tenantBucket{entries: list.New()}
		c.tenants[tenantID] = bucket
	}

	sigCopy := make([]uint64, len(signature))
	copy(sigCopy, signature)
	respCopy := make([]byte, len(resp))
	copy(respCopy, resp)
	fpCopy := make(map[string]struct{}, len(fingerprint))
	for k := range fingerprint {
		fpCopy[k] = struct{}{}
	}

	now := c.now()
	bucket.entries.PushFront(&lexicalEntry{
		signature:   sigCopy,
		resp:        respCopy,
		fingerprint: fpCopy,
		writtenAt:   now,
		modelID:     modelID,
		expiresAt:   now.Add(ttl),
	})
	if bucket.entries.Len() > c.maxEntries {
		bucket.entries.Remove(bucket.entries.Back())
	}
	return nil
}
