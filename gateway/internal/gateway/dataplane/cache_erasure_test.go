package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/cache"
)

// TestEraseCacheEntryRemovesARealCacheHit is the load-bearing proof for
// dataplane.Pipeline.EraseCacheEntry, closing the real gap named in
// docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md
// Finding 4: cache.Cache.Delete existed with zero live callers. Proven
// via OBSERVABLE BEHAVIOR (the upstream call count), not just internal
// cache-layer state -- a real miss, a real hit (proving the entry
// existed), a real erasure, then a real miss again (proving the
// erasure actually took effect, not just that the method returned nil).
func TestEraseCacheEntryRemovesARealCacheHit(t *testing.T) {
	upstreamCalls := 0
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "erase-me"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("first (miss) call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d after the first call, want 1", upstreamCalls)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("second (hit) call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d after a cache hit, want still 1", upstreamCalls)
	}

	result, err := p.EraseCacheEntry(context.Background(), "test-key", "", req)
	if err != nil {
		t.Fatalf("EraseCacheEntry: %v", err)
	}
	if !result.L1Found {
		t.Error("L1Found = false, want true -- the entry existed before erasure")
	}
	if !result.L2Found {
		t.Error("L2Found = false, want true -- writeCache populates L2 on every miss too")
	}

	// Verify the erasure actually took effect by looking L1/L2 up
	// directly via the SAME key-computation EraseCacheEntry itself
	// uses -- NOT by re-running HandleChatCompletion a third time,
	// which would confound this assertion: an identical follow-up
	// request still falls through to Cache L3-lite (the lexical
	// near-duplicate layer, populated by the SAME original write and
	// deliberately NOT touched by EraseCacheEntry, per its own doc
	// comment) and gets served from there instead, with zero new
	// upstream call, even though L1/L2 are genuinely gone. See
	// TestEraseCacheEntryDoesNotPreventAnIdenticalFollowUpFromHittingL3
	// below for that real, disclosed consequence proven directly.
	respFmtFP := responseFormatFingerprint(req.ResponseFormat)
	l1Key := cache.Key("test-key", req.Model, serializeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version(), respFmtFP, "", "")
	l2Key := cache.NormalizedKey("test-key", req.Model, normalizeMessages(req.Messages), req.Temperature, req.MaxTokens, p.guardrails.Version(), respFmtFP, "", "")
	if _, _, ok, _ := p.cache.Get(context.Background(), "test-key", l1Key); ok {
		t.Error("L1 entry still present after EraseCacheEntry")
	}
	if _, _, ok, _ := p.cacheL2.Get(context.Background(), "test-key", l2Key); ok {
		t.Error("L2 entry still present after EraseCacheEntry")
	}
}

// TestEraseCacheEntryDoesNotPreventAnIdenticalFollowUpFromHittingL3
// proves, directly and empirically (not just as a theoretical caveat in
// a doc comment), the real practical consequence of EraseCacheEntry's
// disclosed L3 scope limit: writeCache populates L1, L2, AND L3 on
// every miss, so erasing only L1/L2 does NOT stop a byte-identical
// follow-up request from still being served -- from L3 instead of L1 --
// with zero new upstream call. An operator relying on this endpoint for
// a real data-subject request must understand a repeat of the EXACT SAME
// text can still return cached content until L3's own TTL expires.
func TestEraseCacheEntryDoesNotPreventAnIdenticalFollowUpFromHittingL3(t *testing.T) {
	upstreamCalls := 0
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "l3-still-serves-me"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("first (miss) call: %v", err)
	}
	if _, err := p.EraseCacheEntry(context.Background(), "test-key", "", req); err != nil {
		t.Fatalf("EraseCacheEntry: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("post-erasure call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d after an identical post-erasure request, want still 1 -- L3 is expected to still serve this hit, per EraseCacheEntry's own disclosed scope", upstreamCalls)
	}
}

// TestEraseCacheEntryOnNeverCachedRequestReportsNotFoundNotError proves
// erasing a request that was never cached is a real, successful no-op
// (both found flags false, no error) -- matching cache.Cache.Delete's
// own "never-set is a no-op, not an error" contract, not silently
// promoted into a 404-shaped failure at this layer.
func TestEraseCacheEntryOnNeverCachedRequestReportsNotFoundNotError(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "never-requested"}}}

	result, err := p.EraseCacheEntry(context.Background(), "test-key", "", req)
	if err != nil {
		t.Fatalf("EraseCacheEntry: %v", err)
	}
	if result.L1Found || result.L2Found {
		t.Errorf("result = %+v, want both false for a request that was never cached", result)
	}
}

// TestEraseCacheEntryRejectsPromptAndMessagesBothSet proves
// EraseCacheEntry rejects the same malformed-input shape
// HandleChatCompletion itself already rejects, since it reuses
// resolvePromptIfSet rather than duplicating that validation.
func TestEraseCacheEntryRejectsPromptAndMessagesBothSet(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		PromptID: "some-prompt",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	_, err := p.EraseCacheEntry(context.Background(), "test-key", "", req)
	if !errors.Is(err, ErrPromptAndMessagesBothSet) {
		t.Errorf("EraseCacheEntry error = %v, want ErrPromptAndMessagesBothSet", err)
	}
}
