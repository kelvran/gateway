package inprocess

import (
	"context"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/cache"
)

var _ cache.LexicalCache = (*LexicalCache)(nil)

func sig(vals ...uint64) []uint64 { return vals }

func TestLexicalSearchOnEmptyCacheReturnsNoCandidatesNoError(t *testing.T) {
	c := NewLexicalCache(0)
	candidates, err := c.Search(context.Background(), "team-alpha", sig(1, 2, 3), 5)
	if err != nil {
		t.Fatalf("Search on empty cache returned error: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Search on empty cache returned %d candidates, want 0", len(candidates))
	}
}

func TestLexicalPutThenSearchReturnsCandidate(t *testing.T) {
	c := NewLexicalCache(0)
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", sig(1, 2, 3, 4), []byte("resp-1"), map[string]struct{}{"Paris": {}}, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}

	candidates, err := c.Search(ctx, "team-alpha", sig(1, 2, 3, 4), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Search returned %d candidates, want 1", len(candidates))
	}
	if string(candidates[0].Resp) != "resp-1" {
		t.Errorf("candidate.Resp = %q, want %q", candidates[0].Resp, "resp-1")
	}
	if candidates[0].Similarity != 1.0 {
		t.Errorf("candidate.Similarity = %v, want 1.0 for an identical signature", candidates[0].Similarity)
	}
	if _, ok := candidates[0].Fingerprint["Paris"]; !ok {
		t.Errorf("candidate.Fingerprint = %v, want it to contain %q", candidates[0].Fingerprint, "Paris")
	}
	if candidates[0].ModelID != "gpt-4o" {
		t.Errorf("candidate.ModelID = %q, want %q", candidates[0].ModelID, "gpt-4o")
	}
}

// TestLexicalSearchNeverCrossesTenantBoundary is the load-bearing
// tenant-isolation proof for docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md:
// two tenants with lexically-identical (same signature) stored entries
// must never have one tenant's Search return the other's candidate —
// structural partitioning, not a post-hoc filter, per THREAT_MODEL.md's
// KeyPooling mitigation.
func TestLexicalSearchNeverCrossesTenantBoundary(t *testing.T) {
	c := NewLexicalCache(0)
	ctx := context.Background()
	sharedSig := sig(10, 20, 30)

	if err := c.Put(ctx, "team-alpha", sharedSig, []byte("alpha-resp"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(team-alpha): %v", err)
	}
	if err := c.Put(ctx, "team-beta", sharedSig, []byte("beta-resp"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(team-beta): %v", err)
	}

	alphaCandidates, err := c.Search(ctx, "team-alpha", sharedSig, 5)
	if err != nil {
		t.Fatalf("Search(team-alpha): %v", err)
	}
	for _, cand := range alphaCandidates {
		if string(cand.Resp) == "beta-resp" {
			t.Fatal("team-alpha's Search returned team-beta's candidate — cross-tenant leakage")
		}
	}
	if len(alphaCandidates) != 1 || string(alphaCandidates[0].Resp) != "alpha-resp" {
		t.Errorf("team-alpha's Search = %v, want exactly its own 1 candidate", alphaCandidates)
	}

	betaCandidates, err := c.Search(ctx, "team-beta", sharedSig, 5)
	if err != nil {
		t.Fatalf("Search(team-beta): %v", err)
	}
	for _, cand := range betaCandidates {
		if string(cand.Resp) == "alpha-resp" {
			t.Fatal("team-beta's Search returned team-alpha's candidate — cross-tenant leakage")
		}
	}
}

// TestLexicalEvictionIsPerTenant proves capacity eviction never removes
// a DIFFERENT tenant's entry — each tenant's cap is independent.
func TestLexicalEvictionIsPerTenant(t *testing.T) {
	c := NewLexicalCache(1) // cap of 1 entry PER TENANT
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", sig(1), []byte("alpha-1"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(team-alpha, 1): %v", err)
	}
	if err := c.Put(ctx, "team-beta", sig(1), []byte("beta-1"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(team-beta, 1): %v", err)
	}
	// Overflow team-alpha's own cap of 1 — must evict team-alpha's entry
	// only, leaving team-beta's untouched.
	if err := c.Put(ctx, "team-alpha", sig(2), []byte("alpha-2"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(team-alpha, 2): %v", err)
	}

	betaCandidates, err := c.Search(ctx, "team-beta", sig(1), 5)
	if err != nil {
		t.Fatalf("Search(team-beta): %v", err)
	}
	if len(betaCandidates) != 1 || string(betaCandidates[0].Resp) != "beta-1" {
		t.Errorf("team-beta's entry was evicted by team-alpha's overflow — eviction is not per-tenant: %v", betaCandidates)
	}
}

func TestLexicalSearchExpiredEntryIsReapedAndNotReturned(t *testing.T) {
	clock := &staticClock{t: time.Now()}
	c := NewLexicalCacheWithClock(0, clock.now)
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", sig(1, 2, 3), []byte("resp"), nil, "gpt-4o", 10*time.Second); err != nil {
		t.Fatalf("Put: %v", err)
	}
	clock.Advance(11 * time.Second)

	candidates, err := c.Search(ctx, "team-alpha", sig(1, 2, 3), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Search after TTL expiry returned %d candidates, want 0", len(candidates))
	}
}

func TestLexicalSearchResultsSortedBySimilarityDescending(t *testing.T) {
	c := NewLexicalCache(0)
	ctx := context.Background()

	if err := c.Put(ctx, "team-alpha", sig(1, 2, 3, 4), []byte("low-sim"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(low-sim): %v", err)
	}
	if err := c.Put(ctx, "team-alpha", sig(1, 2, 3, 99), []byte("high-sim"), nil, "gpt-4o", time.Hour); err != nil {
		t.Fatalf("Put(high-sim): %v", err)
	}

	// Querying with a signature closer to "high-sim"'s.
	candidates, err := c.Search(ctx, "team-alpha", sig(1, 2, 3, 99), 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("Search returned %d candidates, want 2", len(candidates))
	}
	if string(candidates[0].Resp) != "high-sim" {
		t.Errorf("candidates[0].Resp = %q, want %q (highest similarity first)", candidates[0].Resp, "high-sim")
	}
	if candidates[0].Similarity < candidates[1].Similarity {
		t.Errorf("candidates not sorted by descending similarity: %v", candidates)
	}
}
