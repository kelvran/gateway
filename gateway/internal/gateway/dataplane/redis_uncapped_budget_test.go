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

// windowBackend is the smallest budget.RedisBackend with Redis's observable
// semantics: a window per key opened by Reserve, Adjust landing only on a
// matching epoch, SpentNanoUSD reading the window.
type windowBackend struct {
	mu    sync.Mutex
	spent map[string]int64
}

const windowBackendEpoch = int64(4242)

func (b *windowBackend) Reserve(_ context.Context, keyID string, capNanoUSD, _ int64) (bool, int64, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, open := b.spent[keyID]; !open {
		b.spent[keyID] = 0
	}
	if capNanoUSD <= 0 {
		return true, 0, windowBackendEpoch, nil
	}
	headroom := capNanoUSD - b.spent[keyID]
	if headroom <= 0 {
		return false, 0, windowBackendEpoch, nil
	}
	b.spent[keyID] = capNanoUSD
	return true, headroom, windowBackendEpoch, nil
}
func (b *windowBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return true, nil
}
func (b *windowBackend) Adjust(_ context.Context, keyID string, delta, epoch int64, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, open := b.spent[keyID]; !open || epoch != windowBackendEpoch {
		return nil
	}
	b.spent[keyID] = max(b.spent[keyID]+delta, 0)
	return nil
}
func (b *windowBackend) SpentNanoUSD(_ context.Context, keyID string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent[keyID], nil
}
func (b *windowBackend) MarkAlertBucket(context.Context, string, float64, int64) (bool, error) {
	return false, nil
}
func (b *windowBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (b *windowBackend) Delete(_ context.Context, keyID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.spent, keyID)
	return nil
}
func (b *windowBackend) Close() error { return nil }

// TestRedisBudgetModeRecordsAnUncappedKeysSpendEndToEnd drives a chat
// completion and an embeddings call on an uncapped key through a pipeline
// whose budget tracker is in Redis mode (a window-keeping fake stands in for
// Redis) and reads the key's spend back: the figure `kelvran spend --by key`
// and GET /admin/virtual_keys/{name}/spend show. Until 2026-10-12 it read 0,
// because an uncapped key never opened a window for the settlement to land
// on.
func TestRedisBudgetModeRecordsAnUncappedKeysSpendEndToEnd(t *testing.T) {
	keys := []identity.VirtualKey{{ID: "free-key", KeyHash: testHashOf("free-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatal(err)
	}
	tracker := budget.NewRedisTracker(&windowBackend{spent: map[string]int64{}}, discardLogger())
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
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
			"gpt-4o":                 {PromptPerToken: decimal.NewFromFloat(0.001), CompletionPerToken: decimal.NewFromFloat(0.002)},
			"text-embedding-3-small": {PromptPerToken: decimal.NewFromFloat(0.0001)},
		}),
		Upstream: func(_ context.Context, dep Deployment, _ any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		EmbeddingUpstream: func(context.Context, Deployment, any) (any, error) {
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 20, TotalTokens: 20},
			}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := p.HandleChatCompletion(ctx, "Bearer free-key", "", "", ledgerChatReq, ""); err != nil {
		t.Fatal(err)
	}
	afterChat := tracker.SpentUSD(ctx, "free-key", 0)
	if afterChat.IsZero() {
		t.Fatal("Redis-mode spend after a chat completion on an uncapped key = 0; the window was never opened")
	}
	if _, err := p.HandleEmbeddings(ctx, "Bearer free-key", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err != nil {
		t.Fatal(err)
	}
	want := afterChat.Add(decimal.RequireFromString("0.002"))
	if got := tracker.SpentUSD(ctx, "free-key", 0); !got.Equal(want) {
		t.Fatalf("Redis-mode spend after chat + embeddings = %s, want %s", got, want)
	}
}
