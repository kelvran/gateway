package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestUpsertVirtualKeyAddsANewKeyImmediatelyUsable proves the real,
// load-bearing claim docs/rfcs/2026-09-05-gateway-admin-api.md exists to
// deliver: a virtual key added via UpsertVirtualKey — never present in
// the Pipeline's original construction — authenticates and completes a
// real request on the very next call, no restart.
func TestUpsertVirtualKeyAddsANewKeyImmediatelyUsable(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	// Before the upsert, this brand-new key must not exist.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer brand-new-secret", adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("HandleChatCompletion succeeded before the key was ever added")
	}

	err := p.UpsertVirtualKey(
		identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("brand-new-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100},
	)
	if err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer brand-new-secret", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("HandleChatCompletion after UpsertVirtualKey: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("resp.Model = %q, want %q", resp.Model, "gpt-4o")
	}

	// The pre-existing key from construction must still work unchanged.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("pre-existing key stopped working after an unrelated upsert: %v", err)
	}
}

// TestUpsertVirtualKeyRegistersTheRateLimiterBeforeItCanBeReached proves
// the exact hazard the RFC's Design section found by tracing the code: a
// new key must never become authenticatable before its rate-limit bucket
// exists, or the very first request against it would nil-pointer panic
// (in-memory mode) rather than being cleanly rate-limited/allowed.
func TestUpsertVirtualKeyRegistersTheRateLimiterBeforeItCanBeReached(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	if err := p.UpsertVirtualKey(
		identity.VirtualKey{ID: "team-delta", KeyHash: testHashOf("delta-secret"), RateLimitBurst: 1, RateLimitRefill: 0},
		ratelimit.KeyConfig{ID: "team-delta", Capacity: 1, RefillPerSecond: 0},
	); err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}

	// Would panic on a nil *TokenBucket before Register existed, not
	// just fail an assertion — this call not panicking is the proof.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer delta-secret", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("first call against a freshly-upserted key: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer delta-secret", adapter.ChatRequest{Model: "gpt-4o"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second call (capacity 1, zero refill) = %v, want ErrRateLimited — Register must have used the real configured capacity, not an unlimited default", err)
	}
}

// TestUpsertVirtualKeyReplacesAnExistingKeysBudget proves an upsert of an
// already-configured ID is a real update, not silently ignored because
// the ID already existed.
func TestUpsertVirtualKeyReplacesAnExistingKeysBudget(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	// Restrict test-key to a model it will then be denied for, proving
	// the upsert's AllowedModels actually took effect.
	err := p.UpsertVirtualKey(
		identity.VirtualKey{
			ID:              "test-key",
			KeyHash:         testHashOf("test-key"),
			AllowedModels:   map[string]struct{}{"gpt-3.5-turbo": {}},
			RateLimitBurst:  100,
			RateLimitRefill: 100,
		},
		ratelimit.KeyConfig{ID: "test-key", Capacity: 100, RefillPerSecond: 100},
	)
	if err != nil {
		t.Fatalf("UpsertVirtualKey: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}); !errors.Is(err, ErrModelNotAllowed) {
		t.Fatalf("HandleChatCompletion after restricting AllowedModels = %v, want ErrModelNotAllowed", err)
	}
}

func TestDeleteVirtualKeyRemovesAccess(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-beta", KeyHash: testHashOf("beta-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	if err := p.DeleteVirtualKey("team-beta"); err != nil {
		t.Fatalf("DeleteVirtualKey: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer beta-secret", adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("HandleChatCompletion succeeded with a deleted key's token")
	}
	// The remaining key must be entirely unaffected.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("surviving key stopped working after deleting an unrelated one: %v", err)
	}
}

func TestDeleteVirtualKeyRefusesToDeleteTheLastKey(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	err := p.DeleteVirtualKey("test-key")
	if !errors.Is(err, ErrCannotDeleteLastVirtualKey) {
		t.Fatalf("DeleteVirtualKey(last key) = %v, want ErrCannotDeleteLastVirtualKey", err)
	}

	// Refused, unchanged: the key must still work.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("the refused-deletion key stopped working: %v", err)
	}
}

func TestDeleteVirtualKeyUnknownNameReturnsNotFound(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	err := p.DeleteVirtualKey("never-configured")
	if !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("DeleteVirtualKey(unknown) = %v, want ErrVirtualKeyNotFound", err)
	}
}

// TestConcurrentUpsertDeleteVirtualKeyUnderRace hammers UpsertVirtualKey/
// DeleteVirtualKey from many goroutines simultaneously, run under -race
// (per go test -race). Mirrors internal/prompt's own
// TestConcurrentUpsertGetResolveUnderRace: confirms no data race (the
// race detector itself would fail this test) and no lost write --
// before the CAS-retry fix, a plain Load-then-Store here meant a losing
// concurrent writer's Store call could silently overwrite an earlier
// winner's Store, discarding that earlier caller's create/delete even
// though its own HTTP call already returned success. Every one of the
// concurrent Upserts (distinct new IDs) and Deletes (distinct
// pre-existing IDs) must be reflected in the final Verifier state, never
// silently dropped.
func TestConcurrentUpsertDeleteVirtualKeyUnderRace(t *testing.T) {
	const numUpserts = 50
	const numDeletes = 20

	seedKeys := []identity.VirtualKey{
		{ID: "seed", KeyHash: testHashOf("seed-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	for i := 0; i < numDeletes; i++ {
		seedKeys = append(seedKeys, identity.VirtualKey{
			ID: fmt.Sprintf("del-%d", i), KeyHash: testHashOf(fmt.Sprintf("del-secret-%d", i)),
			RateLimitBurst: 100, RateLimitRefill: 100,
		})
	}

	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, seedKeys)

	var wg sync.WaitGroup
	for i := 0; i < numUpserts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vk := identity.VirtualKey{
				ID: fmt.Sprintf("key-%d", i), KeyHash: testHashOf(fmt.Sprintf("key-secret-%d", i)),
				RateLimitBurst: 100, RateLimitRefill: 100,
			}
			if err := p.UpsertVirtualKey(vk, ratelimit.KeyConfig{ID: vk.ID, Capacity: 100, RefillPerSecond: 100}); err != nil {
				t.Errorf("UpsertVirtualKey(key-%d): %v", i, err)
			}
		}(i)
	}
	for i := 0; i < numDeletes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := p.DeleteVirtualKey(fmt.Sprintf("del-%d", i)); err != nil {
				t.Errorf("DeleteVirtualKey(del-%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	finalKeys := p.verifier.Load().Keys()
	byID := make(map[string]bool, len(finalKeys))
	for _, k := range finalKeys {
		byID[k.ID] = true
	}

	if !byID["seed"] {
		t.Error("\"seed\" is missing from the final Verifier -- it was never touched by any concurrent writer")
	}
	for i := 0; i < numUpserts; i++ {
		id := fmt.Sprintf("key-%d", i)
		if !byID[id] {
			t.Errorf("%q is missing from the final Verifier -- a concurrent Upsert was silently lost", id)
		}
	}
	for i := 0; i < numDeletes; i++ {
		id := fmt.Sprintf("del-%d", i)
		if byID[id] {
			t.Errorf("%q is still present in the final Verifier -- a concurrent Delete was silently lost", id)
		}
	}

	wantTotal := 1 + numUpserts // seed + every new key; every del-* key removed
	if len(finalKeys) != wantTotal {
		t.Errorf("final Verifier has %d keys, want %d", len(finalKeys), wantTotal)
	}
}
