package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// embeddingsTestFakeCredential builds a fake bearer value via
// concatenation rather than a literal, mirroring
// gracefulShutdownTestSecret's own established convention (cmd/gateway)
// so it doesn't read as a real credential to secret-scanning tooling.
func embeddingsTestFakeCredential(suffix string) string {
	return "not-a-real-" + "embeddings-test-" + suffix
}

// newEmbeddingTestPipeline builds a *Pipeline wired for embeddings —
// newTestPipeline's own helper doesn't set EmbeddingUpstream, so this is
// a small, parallel constructor rather than growing that shared helper's
// parameter list for a capability most of its callers don't need.
func newEmbeddingTestPipeline(t *testing.T, embeddingUpstream UpstreamCaller, deployments []Deployment, priceTable costaccounting.PriceTable) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		// A real, generous BudgetUSD cap (never zero/"unlimited") --
		// Reserve is a real no-op for an unlimited key (capUSD.Sign()
		// <= 0 short-circuits before ever reserving/tracking anything),
		// so Reconcile never runs and real cost never gets applied to
		// SpentUSD -- the exact same behavior the chat pipeline already
		// has. A capped key is what actually exercises cost tracking.
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if priceTable == nil {
		priceTable = costaccounting.PriceTable{}
	}
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     budget.NewTracker(),
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai":  openai.New(),
			"bedrock": bedrock.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(priceTable),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("chat Upstream should never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: embeddingUpstream,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleEmbeddingsRoutesToOpenAIProvider is the real end-to-end
// proof: a request against an openai-provider, Kind=="embedding"
// deployment reaches the upstream carrying the OpenAI-native translated
// request, and the canonical response comes back correctly shaped.
func TestHandleEmbeddingsRoutesToOpenAIProvider(t *testing.T) {
	var gotReq *openai.EmbeddingRequest
	deployments := []Deployment{{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	p := newEmbeddingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		var ok bool
		gotReq, ok = req.(*openai.EmbeddingRequest)
		if !ok {
			t.Fatalf("providerReq = %T, want *openai.EmbeddingRequest", req)
		}
		return &openai.EmbeddingResponseWire{
			Model: "text-embedding-3-small",
			Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2, 0.3}}},
			Usage: openai.EmbeddingUsageWire{PromptTokens: 2, TotalTokens: 2},
		}, nil
	}, deployments, nil)

	resp, err := p.HandleEmbeddings(context.Background(), "Bearer test-key", adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"},
	})
	if err != nil {
		t.Fatalf("HandleEmbeddings: %v", err)
	}
	if gotReq == nil || len(gotReq.Input) != 1 || gotReq.Input[0] != "hello" {
		t.Fatalf("upstream received %+v, want Input=[hello]", gotReq)
	}
	if resp.Model != "text-embedding-3-small" {
		t.Errorf("resp.Model = %q, want text-embedding-3-small", resp.Model)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 3 {
		t.Fatalf("resp.Data = %+v, want one 3-dimensional embedding", resp.Data)
	}
}

// TestHandleEmbeddingsRoutesToBedrockInvokeModel mirrors the OpenAI proof
// for Bedrock's Titan InvokeModel path.
func TestHandleEmbeddingsRoutesToBedrockInvokeModel(t *testing.T) {
	var gotReq *bedrock.EmbeddingRequest
	deployments := []Deployment{{Name: "emb1", Model: "titan-embed", Provider: "bedrock", UpstreamModel: "amazon.titan-embed-text-v2:0", BaseURL: "http://unused", Region: "us-east-1", Kind: "embedding"}}
	p := newEmbeddingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		var ok bool
		gotReq, ok = req.(*bedrock.EmbeddingRequest)
		if !ok {
			t.Fatalf("providerReq = %T, want *bedrock.EmbeddingRequest", req)
		}
		return &bedrock.EmbeddingResponseWire{Embedding: []float64{0.4, 0.5}, InputTextTokenCount: 3}, nil
	}, deployments, nil)

	resp, err := p.HandleEmbeddings(context.Background(), "Bearer test-key", adapter.EmbeddingRequest{
		Model: "titan-embed", Input: []string{"hello world"},
	})
	if err != nil {
		t.Fatalf("HandleEmbeddings: %v", err)
	}
	if gotReq == nil || gotReq.InputText != "hello world" {
		t.Fatalf("upstream received %+v, want InputText=\"hello world\"", gotReq)
	}
	if len(resp.Data) != 1 || len(resp.Data[0].Embedding) != 2 {
		t.Fatalf("resp.Data = %+v, want one 2-dimensional embedding", resp.Data)
	}
}

// TestHandleEmbeddingsRejectsWrongCredentialBeforeUpstreamCall proves the
// auth gate runs before any upstream call on the embeddings route,
// exactly like chat.
func TestHandleEmbeddingsRejectsWrongCredentialBeforeUpstreamCall(t *testing.T) {
	deployments := []Deployment{{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	var upstreamCalls int
	p := newEmbeddingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return &openai.EmbeddingResponseWire{Data: []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}}}, nil
	}, deployments, nil)

	_, err := p.HandleEmbeddings(context.Background(), "Bearer "+embeddingsTestFakeCredential("wrong-credential"), adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"},
	})
	if err == nil {
		t.Fatal("HandleEmbeddings with a wrong credential: got nil error, want an auth error")
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 -- a rejected request must never reach the upstream", upstreamCalls)
	}
}

// TestHandleEmbeddingsRejectsModelNotAllowedForVirtualKey proves
// AllowedModels is honored on the embeddings route exactly like chat.
func TestHandleEmbeddingsRejectsModelNotAllowedForVirtualKey(t *testing.T) {
	deployments := []Deployment{{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	var upstreamCalls int
	restrictedCredential := embeddingsTestFakeCredential("restricted-key")
	keys := []identity.VirtualKey{
		{ID: "restricted-key", KeyHash: testHashOf(restrictedCredential), RateLimitBurst: 100, RateLimitRefill: 100, AllowedModels: map[string]struct{}{"some-other-model": {}}},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("chat Upstream should never be called by an embeddings test")
			return nil, nil
		},
		EmbeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			upstreamCalls++
			return &openai.EmbeddingResponseWire{Data: []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}}}, nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	_, err = p.HandleEmbeddings(context.Background(), "Bearer "+restrictedCredential, adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"},
	})
	if !errors.Is(err, ErrModelNotAllowed) {
		t.Errorf("err = %v, want ErrModelNotAllowed", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0", upstreamCalls)
	}
}

// TestHandleEmbeddingsEmitsCostAccountingEvent proves the real cost
// (priced from the upstream's own returned usage) is reserved-then-
// reconciled into the virtual key's real spend -- not left at $0 or
// double-counted.
func TestHandleEmbeddingsEmitsCostAccountingEvent(t *testing.T) {
	deployments := []Deployment{{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}}
	priceTable := costaccounting.PriceTable{
		"text-embedding-3-small": costaccounting.ModelPrice{PromptPerToken: decimal.RequireFromString("0.001")},
	}
	p := newEmbeddingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return &openai.EmbeddingResponseWire{
			Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}},
			Usage: openai.EmbeddingUsageWire{PromptTokens: 100, TotalTokens: 100},
		}, nil
	}, deployments, priceTable)

	if _, err := p.HandleEmbeddings(context.Background(), "Bearer test-key", adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hello"},
	}); err != nil {
		t.Fatalf("HandleEmbeddings: %v", err)
	}

	spent := p.budget.SpentUSD("test-key", 0)
	want := decimal.RequireFromString("0.1") // 100 tokens * 0.001/token
	if !spent.Equal(want) {
		t.Errorf("SpentUSD = %v, want %v (100 prompt tokens * 0.001/token)", spent, want)
	}
}

// TestHandleEmbeddingsRejectsDeploymentThatIsNotKindEmbedding proves the
// defense-in-depth guard: a deployment resolved for the request's model
// but with Kind != "embedding" is rejected, never silently proxied.
func TestHandleEmbeddingsRejectsDeploymentThatIsNotKindEmbedding(t *testing.T) {
	deployments := []Deployment{{Name: "chat1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}} // Kind left at zero value ("").
	var upstreamCalls int
	p := newEmbeddingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return &openai.EmbeddingResponseWire{Data: []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1}}}}, nil
	}, deployments, nil)

	_, err := p.HandleEmbeddings(context.Background(), "Bearer test-key", adapter.EmbeddingRequest{
		Model: "gpt-4o", Input: []string{"hello"},
	})
	if !errors.Is(err, ErrNotAnEmbeddingDeployment) {
		t.Errorf("err = %v, want ErrNotAnEmbeddingDeployment", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0", upstreamCalls)
	}
}
