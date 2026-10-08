package dataplane

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

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

// newModelsTestPipeline builds a pipeline whose upstreams fail the test if
// called -- GET /v1/models must never reach a provider.
func newModelsTestPipeline(t *testing.T, keys []identity.VirtualKey, deployments []Deployment, metadata map[string]ModelMetadata) *Pipeline {
	t.Helper()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	neverCalled := func(_ context.Context, dep Deployment, _ any) (any, error) {
		t.Errorf("upstream called for %q: listing models must not reach a provider", dep.Name)
		return nil, errors.New("unexpected upstream call")
	}
	p, err := NewPipeline(Config{
		Verifier:          verifier,
		Limiter:           ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:            budget.NewTracker(),
		Cache:             inprocess.New(0),
		CacheL2:           inprocess.New(0),
		CacheL3:           inprocess.NewLexicalCache(0),
		Guardrails:        guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:          adapter.Registry{"openai": openai.New(), "bedrock": bedrock.New()},
		Router:            testRouter(deployments),
		Deployments:       deployments,
		ModelMetadata:     metadata,
		CostCalculator:    costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:          neverCalled,
		EmbeddingUpstream: neverCalled,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// catalogDeployments: two deployments for gpt-4o (one provider), two for
// planner across two providers, one embedding deployment.
func catalogDeployments() []Deployment {
	return []Deployment{
		{Name: "gpt4o-a", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "gpt4o-b", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o-2024-08-06", BaseURL: "http://unused"},
		{Name: "planner-bedrock", Model: "planner", Provider: "bedrock", UpstreamModel: "anthropic.claude-sonnet", BaseURL: "http://unused", Region: "us-east-1", Kind: "chat"},
		{Name: "planner-openai", Model: "planner", Provider: "openai", UpstreamModel: "gpt-4.1", BaseURL: "http://unused", Kind: "chat"},
		{Name: "titan", Model: "titan-embed", Provider: "bedrock", UpstreamModel: "amazon.titan-embed-text-v2:0", BaseURL: "http://unused", Region: "us-east-1", Kind: "embedding"},
	}
}

func unrestrictedKey() []identity.VirtualKey {
	return []identity.VirtualKey{{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100}}
}

func TestHandleListModelsRequiresBearer(t *testing.T) {
	p := newModelsTestPipeline(t, unrestrictedKey(), catalogDeployments(), nil)
	cases := map[string]error{
		"":                 identity.ErrMissingHeader, // no header at all
		"Basic dGVzdA==":   identity.ErrMissingHeader, // wrong scheme
		"Bearer wrong-key": identity.ErrInvalidKey,    // right scheme, unknown key
	}
	for header, want := range cases {
		_, err := p.HandleListModels(context.Background(), header, "127.0.0.1:1234")
		if !errors.Is(err, want) {
			t.Errorf("Authorization %q: err = %v, want %v", header, err, want)
		}
	}
}

func TestHandleListModelsGroupsDeploymentsByCanonicalModel(t *testing.T) {
	p := newModelsTestPipeline(t, unrestrictedKey(), catalogDeployments(), nil)
	got, err := p.HandleListModels(context.Background(), "Bearer test-key", "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("HandleListModels: %v", err)
	}
	want := []ModelInfo{
		{ID: "gpt-4o", Kind: "chat", Providers: []string{"openai"}},
		{ID: "planner", Kind: "chat", Providers: []string{"bedrock", "openai"}},
		{ID: "titan-embed", Kind: "embedding", Providers: []string{"bedrock"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("models =\n%+v\nwant\n%+v", got, want)
	}
}

func TestHandleListModelsFiltersByAllowedModels(t *testing.T) {
	keys := []identity.VirtualKey{{
		ID: "scoped", KeyHash: testHashOf("scoped-secret"), RateLimitBurst: 100, RateLimitRefill: 100,
		AllowedModels: map[string]struct{}{"planner": {}},
	}}
	p := newModelsTestPipeline(t, keys, catalogDeployments(), nil)
	got, err := p.HandleListModels(context.Background(), "Bearer scoped-secret", "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("HandleListModels: %v", err)
	}
	if len(got) != 1 || got[0].ID != "planner" {
		t.Errorf("scoped key sees %+v, want only planner", got)
	}
}

func TestHandleListModelsHonoursSourceIPAllowlist(t *testing.T) {
	_, tenNet, _ := net.ParseCIDR("10.0.0.0/8")
	keys := []identity.VirtualKey{{
		ID: "office", KeyHash: testHashOf("office-secret"), RateLimitBurst: 100, RateLimitRefill: 100,
		AllowedSourceCIDRs: []*net.IPNet{tenNet},
	}}
	p := newModelsTestPipeline(t, keys, catalogDeployments(), nil)
	if _, err := p.HandleListModels(context.Background(), "Bearer office-secret", "192.168.1.10:5555"); !errors.Is(err, ErrSourceIPNotAllowed) {
		t.Errorf("from outside the allowlist: err = %v, want ErrSourceIPNotAllowed", err)
	}
	if got, err := p.HandleListModels(context.Background(), "Bearer office-secret", "10.1.2.3:5555"); err != nil || len(got) != 3 {
		t.Errorf("from inside the allowlist: models = %d, err = %v; want 3 models, nil", len(got), err)
	}
}

func TestHandleListModelsReportsOperatorMetadata(t *testing.T) {
	metadata := map[string]ModelMetadata{
		"planner": {DisplayName: "Planner (Claude Sonnet)", Description: "Routes to Bedrock with an OpenAI fallback."},
	}
	p := newModelsTestPipeline(t, unrestrictedKey(), catalogDeployments(), metadata)
	got, err := p.HandleListModels(context.Background(), "Bearer test-key", "127.0.0.1:1234")
	if err != nil {
		t.Fatalf("HandleListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if p := byID["planner"]; p.DisplayName != "Planner (Claude Sonnet)" || p.Description != "Routes to Bedrock with an OpenAI fallback." {
		t.Errorf("planner metadata = %+v", p)
	}
	if g := byID["gpt-4o"]; g.DisplayName != "" || g.Description != "" {
		t.Errorf("gpt-4o without metadata = %+v, want empty display name and description", g)
	}
}

func TestCatalogLoadedAtIsSetAtConstruction(t *testing.T) {
	before := time.Now()
	p := newModelsTestPipeline(t, unrestrictedKey(), catalogDeployments(), nil)
	at := p.CatalogLoadedAt()
	if at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("CatalogLoadedAt = %v, want within a second of construction (%v)", at, before)
	}
}
