package dataplane

// TestTenantIsolationInvariantAcrossEveryCachePath is the consolidated
// invariant proof docs/upgrade-research/cache-2026-09-06.md Finding 3
// asks for: KeyPooling's own defense contract requires the
// authenticated-identity-derived namespace to survive EVERY cache lookup
// and write, not just the happy-path ones — the paper's headline finding
// is that all five gateways it tested BELIEVED they had isolation and
// didn't.
//
// L1 exact-match, L2 normalized-match, and singleflight-coalesced-miss
// tenant isolation each already have their own scattered proof elsewhere
// (TestHandleChatCompletionCacheIsolatedAcrossVirtualKeys,
// TestHandleChatCompletionL2CacheIsolatedAcrossVirtualKeys,
// TestHandleChatCompletionNeverCoalescesAcrossDifferentTenants). This
// file adds the two cache-touching paths those didn't cover — L3 (lexical
// near-duplicate) at the full HandleChatCompletion level, and the
// fallback/retry path — as one consolidated sweep, so a future new
// cache-touching path has an obvious place to be added rather than
// spawning yet another one-off file.
import (
	"context"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// twoTenantVirtualKeys is this file's own fixture — two virtual keys with
// distinct IDs and bearer secrets, reused by every sub-test below.
func twoTenantVirtualKeys() []identity.VirtualKey {
	return []identity.VirtualKey{
		{ID: "tenant-a", KeyHash: testHashOf("tenant-a-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "tenant-b", KeyHash: testHashOf("tenant-b-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
}

// TestTenantIsolationL3LexicalNearDuplicateNeverCrossesTenants proves the
// full pipeline's L3 layer — not just inprocess.LexicalCache's own
// lower-level TestLexicalSearchNeverCrossesTenantBoundary — never serves
// tenant A's lexical near-duplicate entry to tenant B, even though the
// two tenants' requests are lexically near-identical (same MinHash
// signature, same empty entity fingerprint) and would be a real L3 hit
// for tenant A's own second request.
func TestTenantIsolationL3LexicalNearDuplicateNeverCrossesTenants(t *testing.T) {
	var upstreamCalls int
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, twoTenantVirtualKeys())

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search works in a sorted array"}}}
	nearDuplicate := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search   works in a sorted array"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-a-secret", first); err != nil {
		t.Fatalf("tenant-a first request: %v", err)
	}
	// tenant-b's request is a lexical near-duplicate of tenant-a's — a
	// real L3 hit if it were tenant-a's own second request (proven by
	// TestHandleChatCompletionLexicalNearDuplicateHitsL3) — but must be a
	// genuine miss (a second real upstream call) here, since it's a
	// DIFFERENT tenant.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-b-secret", nearDuplicate); err != nil {
		t.Fatalf("tenant-b request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — tenant-b must never receive tenant-a's L3-cached response, even for a lexically near-identical request", upstreamCalls)
	}

	// Confirm tenant-a's OWN second identical request still hits L3 —
	// proving this is real tenant-scoping, not L3 being broken outright.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-a-secret", nearDuplicate); err != nil {
		t.Fatalf("tenant-a second request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want still 2 — tenant-a's own near-duplicate request should be a real L3 hit", upstreamCalls)
	}
}

// TestTenantIsolationFallbackServedResponseNeverCrossesTenants proves the
// fallback path (primary deployment errors, a second deployment serves
// the request instead) writes to cache under the CALLING tenant's own
// key, and that a different tenant's identical request never receives
// that fallback-served, cached response — it must independently go
// through the same primary-fails/fallback-succeeds path itself.
func TestTenantIsolationFallbackServedResponseNeverCrossesTenants(t *testing.T) {
	var primaryAttempts, fallbackAttempts int
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			primaryAttempts++
			return nil, errors.New("simulated upstream failure")
		}
		fallbackAttempts++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments, twoTenantVirtualKeys())

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "identical request for both tenants"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-a-secret", req); err != nil {
		t.Fatalf("tenant-a request (expected to succeed via fallback): %v", err)
	}
	if primaryAttempts != 1 || fallbackAttempts != 1 {
		t.Fatalf("after tenant-a's request: primaryAttempts=%d fallbackAttempts=%d, want 1 and 1", primaryAttempts, fallbackAttempts)
	}

	// tenant-b's identical request must NOT be served from tenant-a's
	// fallback-cached entry — it must independently retry primary (fail)
	// then fallback (succeed) again.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-b-secret", req); err != nil {
		t.Fatalf("tenant-b request (expected to succeed via its own fallback): %v", err)
	}
	if primaryAttempts != 2 || fallbackAttempts != 2 {
		t.Fatalf("after tenant-b's request: primaryAttempts=%d fallbackAttempts=%d, want 2 and 2 — tenant-b must never receive tenant-a's fallback-served, cached response", primaryAttempts, fallbackAttempts)
	}

	// tenant-a's OWN second identical request should now be a real cache
	// hit (no new primary/fallback attempts) — proving the fallback path
	// really did write to tenant-a's own cache namespace correctly, not
	// that caching from fallback is broken outright.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer tenant-a-secret", req); err != nil {
		t.Fatalf("tenant-a second request: %v", err)
	}
	if primaryAttempts != 2 || fallbackAttempts != 2 {
		t.Fatalf("after tenant-a's second request: primaryAttempts=%d fallbackAttempts=%d, want still 2 and 2 — tenant-a's own repeat request should be a cache hit", primaryAttempts, fallbackAttempts)
	}
}
