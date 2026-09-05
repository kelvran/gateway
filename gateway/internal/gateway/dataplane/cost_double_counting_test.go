package dataplane

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// pricedTestPipeline is like newTestPipelineWithKeysAndBudget but with a
// real, nonzero PriceTable — needed here (and nowhere else in this
// package) because these tests assert on the actual dollar amount
// budget.Tracker records, which a zero price table can never distinguish
// from "recorded twice."
func pricedTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment, tracker *budget.Tracker) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:    verifier,
		Limiter:     ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:      tracker,
		Cache:       inprocess.New(0),
		CacheL2:     inprocess.New(0),
		CacheL3:     inprocess.NewLexicalCache(0),
		Guardrails:  guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:    adapter.Registry{"openai": openai.New()},
		Router:      testRouter(deployments),
		Deployments: deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{
			"gpt-4o": {
				PromptPerToken:     decimal.NewFromFloat(0.001),
				CompletionPerToken: decimal.NewFromFloat(0.002),
			},
		}),
		Upstream: upstream,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionCacheHitDoesNotRechargeBudget is the
// load-bearing proof for docs/rfcs/2026-09-05-gateway-cost-double-
// counting.md's "zero charge" decision: a cache hit must never re-record
// the same cost against a virtual key's budget a second time. Before
// this fix, finalize called budget.Record unconditionally whenever
// err == nil, so a repeat cache hit silently doubled a key's tracked
// spend for work that incurred zero incremental upstream cost.
func TestHandleChatCompletionCacheHitDoesNotRechargeBudget(t *testing.T) {
	var upstreamCalls int
	tracker := budget.NewTracker()
	p := pricedTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, tracker)

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("first (real-miss) call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first call = %d, want 1", upstreamCalls)
	}
	spentAfterMiss := tracker.SpentUSD("test-key", 0)
	if spentAfterMiss.IsZero() {
		t.Fatal("spentAfterMiss = 0, want a real nonzero cost — the price table fixture is broken")
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("second (cache-hit) call: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after second (cache-hit) call = %d, want still 1", upstreamCalls)
	}
	spentAfterHit := tracker.SpentUSD("test-key", 0)
	if !spentAfterHit.Equal(spentAfterMiss) {
		t.Errorf("spentAfterHit = %s, want unchanged from spentAfterMiss = %s — a cache hit must never re-charge budget", spentAfterHit, spentAfterMiss)
	}
}

// TestHandleChatCompletionCoalescedFollowerDoesNotRechargeBudget proves
// the same "zero charge" decision for singleflight-coalesced followers:
// N concurrent identical cache misses share exactly one real upstream
// call (per docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md),
// and must therefore also share exactly one real budget charge — not N
// charges for the one real dollar cost actually incurred.
func TestHandleChatCompletionCoalescedFollowerDoesNotRechargeBudget(t *testing.T) {
	var upstreamCalls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})
	tracker := budget.NewTracker()

	p := pricedTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if upstreamCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, tracker)

	const n = 10
	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}

	var ready sync.WaitGroup
	ready.Add(n)
	go_ := make(chan struct{})

	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-go_
			_, errs[i] = p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
		}(i)
	}

	ready.Wait()
	close(go_)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real upstream call to start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	wg.Wait()

	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstreamCalls = %d, want exactly 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	spent := tracker.SpentUSD("test-key", 0)
	if spent.IsZero() {
		t.Fatal("spent = 0, want a real nonzero cost — the price table fixture is broken")
	}
	wantSingleCharge := costaccounting.NewCalculator(costaccounting.PriceTable{
		"gpt-4o": {
			PromptPerToken:     decimal.NewFromFloat(0.001),
			CompletionPerToken: decimal.NewFromFloat(0.002),
		},
	}).Calculate("gpt-4o", costaccounting.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8})
	if !spent.Equal(wantSingleCharge) {
		t.Errorf("spent = %s, want exactly one real charge (%s) — %d coalesced followers must never each re-charge budget for the one real upstream call", spent, wantSingleCharge, n-1)
	}
}
