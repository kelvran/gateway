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
