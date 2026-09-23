package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// newTwoRegionTestPipeline mirrors newTestPipeline's own construction
// pattern exactly, but with two same-model deployments in different
// regions -- the shape needed to prove an Admin-API-set AllowedRegions
// constraint actually reroutes a real request, per
// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md.
// Not folded into newTestPipeline itself, which every other test in this
// package relies on staying single-deployment.
func newTwoRegionTestPipeline(t *testing.T, served *string) *dataplane.Pipeline {
	t.Helper()

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []dataplane.Deployment{
		{Name: "us", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "us-east-1"},
		{Name: "eu", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused", Region: "eu-west-1"},
	}
	p, err := dataplane.NewPipeline(dataplane.Config{
		Verifier: verifier,
		Limiter: ratelimit.NewInMemoryKeyLimiter([]ratelimit.KeyConfig{
			{ID: "test-key", Capacity: 100, RefillPerSecond: 100},
		}),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         router.New([]router.Deployment{{Name: "us", Model: "gpt-4o"}, {Name: "eu", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			*served = dep.Name
			return &openai.Response{
				ID: "chatcmpl-fake", Model: dep.UpstreamModel,
				Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hi"`)}, FinishReason: "stop"}},
				Usage:   openai.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestUpsertVirtualKeyWithAllowedRegionsIsEnforced proves the live Admin
// API mutation surface for allowed_regions actually wires through to real
// routing enforcement -- not just that the request struct has the field,
// mirroring TestUpsertVirtualKeyWithPerModelRateLimitIsEnforced's own
// completeness discipline.
func TestUpsertVirtualKeyWithAllowedRegionsIsEnforced(t *testing.T) {
	var served string
	pipeline := newTwoRegionTestPipeline(t, &served)
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	newBearerValue := "region-constrained-test-value"
	body := `{"key_hash":"` + testHashOf(newBearerValue) + `","allowed_regions":["eu-west-1"]}`
	rec := doRequest(t, h, http.MethodPost, "/admin/virtual_keys/team-region", fakeAdminCredential(), body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST status = %d, want 204, body: %s", rec.Code, rec.Body.String())
	}

	authHeader := "Bearer " + newBearerValue
	if _, err := pipeline.HandleChatCompletion(context.Background(), authHeader, "", "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if served != "eu" {
		t.Errorf("served by %q, want eu -- the Admin-API-configured allowed_regions constraint should have rerouted the first pick", served)
	}
}
