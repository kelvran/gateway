package dataplane

import (
	"context"
	"sync"
	"testing"

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

// settlementCapturingBudgetBackend is a budget.RedisBackend that admits every
// reservation and records the context each Adjust (the settlement write that
// releases the reservation and books the real cost) arrived with.
type settlementCapturingBudgetBackend struct {
	mu           sync.Mutex
	adjustCalls  int
	adjustCtxErr error
}

func (b *settlementCapturingBudgetBackend) Reserve(_ context.Context, _ string, capNanoUSD, _ int64) (bool, int64, int64, error) {
	return true, capNanoUSD, 7, nil
}

func (b *settlementCapturingBudgetBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return true, nil
}

func (b *settlementCapturingBudgetBackend) Adjust(ctx context.Context, _ string, _, _ int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adjustCalls++
	b.adjustCtxErr = ctx.Err()
	return nil
}

func (b *settlementCapturingBudgetBackend) SpentNanoUSD(context.Context, string) (int64, error) {
	return 0, nil
}

func (b *settlementCapturingBudgetBackend) MarkAlertBucket(context.Context, string, float64, int64) (bool, error) {
	return false, nil
}

func (b *settlementCapturingBudgetBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (b *settlementCapturingBudgetBackend) Delete(context.Context, string) error { return nil }
func (b *settlementCapturingBudgetBackend) Close() error                         { return nil }

// TestBudgetSettlementRunsEvenWhenTheClientHasDisconnected pins the fix for a
// Redis-mode budget leak found while writing docs/operations/FAILURE-MODES.md:
// the deferred Reconcile used the request's own context, which a client
// disconnect cancels, and go-redis fails fast on a cancelled context -- so the
// full-headroom reservation Redis mode takes was never released and the key
// stayed locked at its cap for the rest of the window, with Redis perfectly
// healthy. Settlement must run under a context that carries the request's
// values but not its cancellation.
func TestBudgetSettlementRunsEvenWhenTheClientHasDisconnected(t *testing.T) {
	backend := &settlementCapturingBudgetBackend{}
	keys := []identity.VirtualKey{
		{ID: "team-settle", KeyHash: testHashOf("team-settle"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("10")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewRedisTracker(backend, discardLogger()),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(context.Context, Deployment, any) (any, error) {
			cancel() // the client went away while the upstream call was in flight
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, _ = p.HandleChatCompletion(ctx, "Bearer team-settle", "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, "")

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.adjustCalls != 1 {
		t.Fatalf("Adjust calls = %d, want exactly 1 settlement write", backend.adjustCalls)
	}
	if backend.adjustCtxErr != nil {
		t.Errorf("the settlement Adjust ran under a context with Err() = %v; a client disconnect must not cancel budget settlement (the reservation would leak and lock the key at its cap)", backend.adjustCtxErr)
	}
}

// settlementCapturingTPMBackend is a ratelimit.RedisBackend that admits every
// request and records the context the TPM settlement (AdjustTPM) arrived with.
type settlementCapturingTPMBackend struct {
	mu           sync.Mutex
	adjustCalls  int
	adjustCtxErr error
}

func (*settlementCapturingTPMBackend) Allow(context.Context, string, float64, float64) (bool, error) {
	return true, nil
}

func (*settlementCapturingTPMBackend) AllowTPM(context.Context, string, float64, float64, float64, float64) (bool, float64, error) {
	return true, 0, nil
}

func (b *settlementCapturingTPMBackend) AdjustTPM(ctx context.Context, _ string, _ float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adjustCalls++
	b.adjustCtxErr = ctx.Err()
	return nil
}

func (*settlementCapturingTPMBackend) Close() error { return nil }

// TestTPMSettlementRunsEvenWhenTheClientHasDisconnected is the Redis-TPM twin
// of the budget test above: ReconcileTPM's AdjustTPM must run under the
// settlement context, or a disconnect leaves the full-burst reservation in
// place until it decays.
func TestTPMSettlementRunsEvenWhenTheClientHasDisconnected(t *testing.T) {
	backend := &settlementCapturingTPMBackend{}
	const keyID = "team-settle-tpm"
	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewRedisKeyLimiter([]ratelimit.KeyConfig{{ID: keyID, Capacity: 100, RefillPerSecond: 100, TPMCapacity: 1000, TPMRefillPerSecond: 100}}, backend),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(context.Context, Deployment, any) (any, error) {
			cancel()
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, _ = p.HandleChatCompletion(ctx, "Bearer "+keyID, "", "", adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}, "")

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.adjustCalls != 1 {
		t.Fatalf("AdjustTPM calls = %d, want exactly 1 settlement write", backend.adjustCalls)
	}
	if backend.adjustCtxErr != nil {
		t.Errorf("the TPM settlement ran under a context with Err() = %v; a client disconnect must not cancel it", backend.adjustCtxErr)
	}
}

// TestEmbeddingsSettlementRunsEvenWhenTheClientHasDisconnected covers the
// third settlement site: HandleEmbeddings reconciles its reservation in its
// own deferred call, not through finalize.
func TestEmbeddingsSettlementRunsEvenWhenTheClientHasDisconnected(t *testing.T) {
	backend := &settlementCapturingBudgetBackend{}
	const keyID = "team-settle-embed"
	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("10")}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "e1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewRedisTracker(backend, discardLogger()),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(context.Context, Deployment, any) (any, error) {
			t.Fatal("chat Upstream should never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: func(context.Context, Deployment, any) (any, error) {
			cancel() // the client went away while the upstream call was in flight
			return &openai.EmbeddingResponseWire{Data: []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}}}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, _ = p.HandleEmbeddings(ctx, "Bearer "+keyID, "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}})

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.adjustCalls != 1 {
		t.Fatalf("Adjust calls = %d, want exactly 1 settlement write", backend.adjustCalls)
	}
	if backend.adjustCtxErr != nil {
		t.Errorf("the embeddings settlement Adjust ran under a context with Err() = %v; a client disconnect must not cancel it", backend.adjustCtxErr)
	}
}
