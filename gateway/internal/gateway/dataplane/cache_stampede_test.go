package dataplane

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestHandleChatCompletionCoalescesConcurrentIdenticalCacheMisses proves
// the real fix for THREAT_MODEL.md's Cache Denial-of-Service row
// ("cache-miss storms causing redundant expensive upstream calls"): N
// concurrent requests for the exact same (tenant, model, messages) that
// all miss the cache must result in exactly ONE real upstream call, with
// every caller receiving the same response.
//
// Uses a real barrier (not a bare sleep) to maximize the chance all N
// goroutines have actually called HandleChatCompletion before the first
// one's upstream call is released — a short bounded wait after the
// barrier gives each goroutine's own pre-cache-check work (auth,
// rate-limit, budget) time to complete and register with the
// singleflight group, the same pragmatic pattern any test of a real
// concurrency primitive under genuine concurrent load uses.
func TestHandleChatCompletionCoalescesConcurrentIdenticalCacheMisses(t *testing.T) {
	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})

	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if upstreamCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	const n = 10
	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	var ready sync.WaitGroup
	ready.Add(n)
	go_ := make(chan struct{})

	var wg sync.WaitGroup
	results := make([]adapter.ChatResponse, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-go_
			results[i], errs[i] = p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
		}(i)
	}

	ready.Wait()
	close(go_)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real upstream call to start")
	}
	// Give every other goroutine's pre-cache-check work (auth, rate
	// limit, budget, cache lookups) real wall-clock time to complete and
	// register with the singleflight group before releasing the one real
	// call — see the test's own doc comment for why this is a pragmatic,
	// standard pattern here, not a design smell.
	time.Sleep(50 * time.Millisecond)
	close(release)

	wg.Wait()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstreamCalls = %d, want exactly 1 -- %d concurrent identical requests must coalesce into one real upstream call", got, n)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if results[i].Model != "gpt-4o" {
			t.Errorf("request %d: Model = %q, want %q", i, results[i].Model, "gpt-4o")
		}
	}
}

// TestHandleChatCompletionNeverCoalescesAcrossDifferentTenants proves the
// safety property runMissPath's own doc comment names: l1Key already
// bakes in the tenant ID, so two different virtual keys making the exact
// same request concurrently must NEVER share one upstream call —
// coalescing across tenants would be a cross-tenant correctness/isolation
// hazard, not just an inefficiency.
func TestHandleChatCompletionNeverCoalescesAcrossDifferentTenants(t *testing.T) {
	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	var startedOnce sync.Once
	bothStarted := make(chan struct{})
	var startCount atomic.Int64

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-beta", KeyHash: testHashOf("team-beta-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if startCount.Add(1) == 2 {
			startedOnce.Do(func() { close(bothStarted) })
		}
		<-release
		upstreamCalls.Add(1)
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	}()
	go func() {
		defer wg.Done()
		_, _ = p.HandleChatCompletion(context.Background(), "Bearer team-beta-secret", req)
	}()

	select {
	case <-bothStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for both tenants' upstream calls to start -- they must never coalesce into one")
	}
	close(release)
	wg.Wait()

	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstreamCalls = %d, want exactly 2 -- two different tenants' identical requests must never share one upstream call", got)
	}
}
