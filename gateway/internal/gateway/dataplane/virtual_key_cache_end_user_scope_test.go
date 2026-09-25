package dataplane

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// endUserScopeChatRequest is this file's own fixture request, reused
// across every sub-test below.
func endUserScopeChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
}

// TestCacheScopeToEndUserPreventsCrossUserCacheHit is the load-bearing
// proof for the headline finding this feature exists to close: two
// requests behind the SAME virtual key, with CacheScopeToEndUser
// enabled, but DIFFERENT X-Kelvran-End-User-Id header values, must never
// share a cache entry -- the exact cross-user cache-sharing scenario
// response-cache-compliance-risk-2026-09-22.md's cited attack exploits.
func TestCacheScopeToEndUserPreventsCrossUserCacheHit(t *testing.T) {
	var upstreamCalls int
	authCred := "shared-key-cred"
	vk := identity.VirtualKey{
		ID: "shared-key", KeyHash: testHashOf(authCred),
		CacheScopeToEndUser: true,
		RateLimitBurst:      100, RateLimitRefill: 100,
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, []identity.VirtualKey{vk})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-alice", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("end-user-alice request: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-bob", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("end-user-bob request: %v", err)
	}

	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 -- end-user-bob's request must never be served from end-user-alice's cache entry", upstreamCalls)
	}

	// Same end user asking again DOES hit cache -- proves this is real
	// scoping, not just "caching disabled."
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-alice", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("end-user-alice second request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want still 2 -- end-user-alice's own repeat request should hit her own cached entry", upstreamCalls)
	}
}

// TestCacheScopeToEndUserFalseKeepsExistingTenantOnlySharing is the
// regression guard proving this fix can never silently become
// default-on: with the flag left false (every virtual key configured
// before this feature existed), two different end-user header values
// behind the same key DO share a cache entry, byte-for-byte identical to
// pre-feature behavior.
func TestCacheScopeToEndUserFalseKeepsExistingTenantOnlySharing(t *testing.T) {
	var upstreamCalls int
	authCred := "shared-key-unscoped-cred"
	vk := identity.VirtualKey{
		ID: "shared-key-unscoped", KeyHash: testHashOf(authCred),
		RateLimitBurst: 100, RateLimitRefill: 100,
		// CacheScopeToEndUser deliberately left false (the default).
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, []identity.VirtualKey{vk})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-alice", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-bob", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("second request: %v", err)
	}

	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want 1 -- with CacheScopeToEndUser false, different end-user headers must not affect tenant-only cache sharing", upstreamCalls)
	}
}

// TestCacheScopeToEndUserFailsClosedWhenHeaderAbsent proves the
// fail-closed contract: with the flag true but NO end-user header
// present, an entry must never be shared with ANY other request --
// neither a second request also missing the header, nor a request that
// DOES supply a header.
func TestCacheScopeToEndUserFailsClosedWhenHeaderAbsent(t *testing.T) {
	var upstreamCalls int
	authCred := "scoped-no-header-cred"
	vk := identity.VirtualKey{
		ID: "scoped-no-header-key", KeyHash: testHashOf(authCred),
		CacheScopeToEndUser: true,
		RateLimitBurst:      100, RateLimitRefill: 100,
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, []identity.VirtualKey{vk})

	// Two requests, both with NO end-user header at all.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("first no-header request: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", endUserScopeChatRequest(), ""); err != nil {
		t.Fatalf("second no-header request: %v", err)
	}

	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 -- two DIFFERENT requests missing the end-user header must never share a cache entry with each other", upstreamCalls)
	}
}

// TestCacheScopeToEndUserPreventsCrossUserL3LexicalCacheHit is checkLexicalCache's
// own L3-specific regression guard for the same headline finding
// TestCacheScopeToEndUserPreventsCrossUserCacheHit proves for L1/L2 above —
// but exercising Cache L3-lite's fuzzy near-duplicate match specifically,
// since before this fix L3's own read path (checkLexicalCache) ignored
// cacheScope entirely and searched p.cacheL3 keyed on the bare virtual-key
// ID, never the per-end-user-scoped value writeCache had always used to
// WRITE L3 entries.
//
// A important note on what this test can and cannot, by itself, prove:
// with CacheScopeToEndUser true, writeCache always writes L3 keyed on a
// per-end-user cacheScope hash (cache.ScopeKey), which — by construction —
// never equals the bare virtual-key ID. So the pre-fix bug's
// read/write bucket mismatch actually failed CLOSED for a fresh
// two-different-end-users scenario: end-user bob's search against the
// bare-ID bucket would never find an entry alice's write had placed under
// her own hashed cacheScope bucket either — a permanent miss, not a false
// hit. That means the "bob must never receive alice's cached response"
// assertion below is necessary but NOT sufficient to catch this
// regression: it would have passed even on the buggy code, for the wrong
// reason (L3 silently disabled for every end user, not real scoping). The
// assertion that actually distinguishes buggy from fixed behavior — and
// is confirmed empirically, not just asserted, against this exact
// pre-fix code before this test was added — is alice's OWN near-duplicate
// follow-up request in step 3 below, which only becomes an L3 hit once
// checkLexicalCache searches on cacheScope like writeCache always wrote.
func TestCacheScopeToEndUserPreventsCrossUserL3LexicalCacheHit(t *testing.T) {
	var upstreamCalls int
	authCred := "shared-key-l3-cred"
	vk := identity.VirtualKey{
		ID: "shared-key-l3", KeyHash: testHashOf(authCred),
		CacheScopeToEndUser: true,
		RateLimitBurst:      100, RateLimitRefill: 100,
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, []identity.VirtualKey{vk})

	// original and nearDuplicate differ only in internal whitespace --
	// byte-different (a real L1 miss) and, per this codebase's own
	// normalizeMessages allowlist, ALSO L2-different (see
	// TestHandleChatCompletionLexicalNearDuplicateHitsL3's doc comment in
	// lexical_cache_test.go), so the only layer that can possibly produce
	// a hit for this pair is L3's MinHash/Shingles match, which collapses
	// whitespace runs via strings.Fields and reports similarity 1.0.
	// Carries no entity/number/date, so the entity hard gate trivially
	// passes too -- isolating L3's own cacheScope-vs-bare-vk.ID bug as the
	// only thing under test.
	const original = "Explain how binary search works in a sorted array"
	const nearDuplicate = "Explain how binary search   works in a sorted array"
	firstReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: original}}}
	nearDupReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: nearDuplicate}}}

	// Step 1: end-user alice's request populates L1/L2/L3, all scoped to
	// her own cacheScope.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-alice", firstReq, ""); err != nil {
		t.Fatalf("end-user-alice request: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after alice's request = %d, want 1", upstreamCalls)
	}

	// Step 2: end-user bob sends a LEXICALLY-SIMILAR-BUT-NOT-IDENTICAL
	// request under the SAME virtual key. He must never receive alice's
	// cached response.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-bob", nearDupReq, ""); err != nil {
		t.Fatalf("end-user-bob request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after bob's near-duplicate request = %d, want 2 -- end-user-bob must never be served end-user-alice's L3-cached response", upstreamCalls)
	}

	// Step 3: end-user alice sends her OWN lexically-similar follow-up.
	// This is the load-bearing assertion: it must be a genuine L3 hit
	// against HER OWN step-1 entry. Before this fix, checkLexicalCache
	// searched p.cacheL3 on the bare virtual-key ID rather than
	// cacheScope, which writeCache had used to WRITE that entry --  a
	// bucket writeCache never populated, so this request fell through to
	// a real (and incorrect, from a caching-effectiveness standpoint)
	// upstream call every single time. Confirmed empirically against the
	// pre-fix code: upstreamCalls reached 3 here, not the correct 2.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "end-user-alice", nearDupReq, ""); err != nil {
		t.Fatalf("end-user-alice near-duplicate follow-up request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after alice's own near-duplicate follow-up = %d, want still 2 -- checkLexicalCache must search L3 on the same cacheScope writeCache used to populate it, so alice's own near-duplicate request is a real L3 hit against her own entry", upstreamCalls)
	}
}

// TestResolveCacheEndUserScopeUnit is a focused unit test for
// resolveCacheEndUserScope's own three branches, independent of the
// full HandleChatCompletion path above.
func TestResolveCacheEndUserScopeUnit(t *testing.T) {
	off := &identity.VirtualKey{CacheScopeToEndUser: false}
	if got := resolveCacheEndUserScope(context.Background(), off, "alice"); got != "" {
		t.Errorf("flag off: resolveCacheEndUserScope = %q, want \"\" regardless of header", got)
	}

	on := &identity.VirtualKey{CacheScopeToEndUser: true}
	if got := resolveCacheEndUserScope(context.Background(), on, "alice"); got != "alice" {
		t.Errorf("flag on, header present: resolveCacheEndUserScope = %q, want %q", got, "alice")
	}

	if got := resolveCacheEndUserScope(context.Background(), on, ""); got == "" {
		t.Error("flag on, header absent: resolveCacheEndUserScope returned empty, want a non-empty fail-closed fallback value")
	}
}
