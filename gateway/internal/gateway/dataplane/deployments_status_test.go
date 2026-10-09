package dataplane

// ListDeployments backs GET /admin/deployments (RFC-3 decision 5, slice
// (c)): every configured deployment, sorted by name, with the static
// fields from the config and the live health, weight, latency factor and
// sticky flag read from the router.

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// newTestPipelineWithRouter mirrors newTestPipelineWithKeysAndBudget with a
// caller-built router, so a test can configure weights and sticky flags and
// drive probe results on the same instance the pipeline reads.
func newTestPipelineWithRouter(t *testing.T, deployments []Deployment, rt *router.Router) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
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
		Router:         rt,
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func TestListDeploymentsReportsStaticFieldsAndLiveRouterState(t *testing.T) {
	deployments := []Deployment{
		{Name: "zeta", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o-2024", BaseURL: "http://unused"},
		{Name: "alpha", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "embed", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p := newTestPipelineWithRouter(t, deployments, router.New([]router.Deployment{
		{Name: "zeta", Model: "gpt-4o", Weight: 3, Sticky: true},
		{Name: "alpha", Model: "gpt-4o"},
		{Name: "embed", Model: "text-embedding-3-small"},
	}, router.HealthConfig{}))

	got := p.ListDeployments()
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "embed" || got[2].Name != "zeta" {
		t.Fatalf("ListDeployments order/count = %+v, want alpha, embed, zeta", got)
	}
	zeta := got[2]
	if zeta.Model != "gpt-4o" || zeta.UpstreamModel != "gpt-4o-2024" || zeta.Provider != "openai" || zeta.Kind != "chat" {
		t.Errorf("static fields: %+v (Kind must normalise \"\" to chat)", zeta)
	}
	if !zeta.Healthy || zeta.Weight != 3 || zeta.LatencyFactorPercent != 0 || !zeta.Sticky {
		t.Errorf("live fields for zeta: %+v, want healthy, weight 3, latency 0, sticky", zeta)
	}
	if got[1].Kind != "embedding" || got[0].Weight != 1 || got[0].Sticky {
		t.Errorf("embed kind / alpha default weight / alpha sticky: %+v %+v", got[1], got[0])
	}

	// Live changes are visible: an admin weight update and probe failures.
	if err := p.UpdateDeploymentWeight(context.Background(), "alpha", 9); err != nil {
		t.Fatalf("UpdateDeploymentWeight: %v", err)
	}
	for i := 0; i < 3; i++ { // HealthConfig{} → UnhealthyThreshold 3
		p.router.ReportProbeResult("zeta", false)
	}
	p.router.SetLatencyFactor("alpha", 70)
	got = p.ListDeployments()
	if got[0].Weight != 9 || got[0].LatencyFactorPercent != 70 {
		t.Errorf("alpha after weight update and latency signal: %+v, want weight 9, latency 70", got[0])
	}
	if got[2].Healthy {
		t.Errorf("zeta after three failed probes must be unhealthy: %+v", got[2])
	}
}
