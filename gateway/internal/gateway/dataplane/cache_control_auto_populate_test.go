package dataplane

// Full-pipeline proof for docs/rfcs/2026-09-07-gateway-cache-control-
// auto-populate.md's per-deployment opt-out (b): the adapter-level tests
// in internal/adapter/anthropic and internal/adapter/bedrock already
// prove the auto-populate mechanism itself works correctly given
// adapter.ChatRequest.DisableCacheControlAutoPopulate; this file proves
// the actual WIRING reaches that field from a real, config-shaped
// Deployment through callDeployment (the buffered path) — not just that
// the mechanism works in isolation. The two streaming call sites
// (streamDeployment, streamDeploymentBedrock in streaming.go) carry the
// byte-identical one-line wiring (upstreamReq.DisableCacheControlAutoPopulate
// = dep.DisableCacheControlAutoPopulate), verified by direct code review
// rather than a second and third dataplane-level test — a named scope
// reduction, per that RFC's own Verification section.

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// newAnthropicTestPipeline is dataplane_test.go's newTestPipeline, with
// the "anthropic" adapter registered instead of "openai" — this
// package's other Pipeline-building test helpers all hardcode the
// openai registry entry, so this file builds its own rather than
// widening a shared helper's signature for one feature's tests.
func newAnthropicTestPipeline(t *testing.T, upstream UpstreamCaller, deployments []Deployment) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
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
			"anthropic": anthropic.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func fakeAnthropicResponse(model string) *anthropic.Response {
	return &anthropic.Response{
		ID:         "msg_fake",
		Model:      model,
		Role:       "assistant",
		Content:    []anthropic.ContentBlock{{Type: "text", Text: "hi"}},
		StopReason: "end_turn",
		Usage:      anthropic.Usage{InputTokens: 5, OutputTokens: 1},
	}
}

// TestCallDeploymentThreadsDisableCacheControlAutoPopulateFromDeployment
// drives two real deployments (same provider, different canonical
// models so each is deterministically selected by its own request) end
// to end through HandleChatCompletion -> runMissPath -> callDeployment ->
// anthropic.ToProvider, and inspects the actual *anthropic.Request the
// pipeline would have sent upstream for each: the deployment with the
// opt-out unset (the default) must produce an auto-populated
// cache_control marker on its system block; the deployment with the
// opt-out set must produce none at all.
func TestCallDeploymentThreadsDisableCacheControlAutoPopulateFromDeployment(t *testing.T) {
	captured := map[string]*anthropic.Request{}
	upstream := func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		req, ok := providerReq.(*anthropic.Request)
		if !ok {
			t.Fatalf("upstream received %T for deployment %q, want *anthropic.Request", providerReq, dep.Name)
		}
		captured[dep.Name] = req
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}

	deployments := []Deployment{
		{Name: "auto-on", Model: "claude-auto-on", Provider: "anthropic", UpstreamModel: "claude-opus-4", BaseURL: "http://unused"},
		{Name: "auto-off", Model: "claude-auto-off", Provider: "anthropic", UpstreamModel: "claude-opus-4", BaseURL: "http://unused", DisableCacheControlAutoPopulate: true},
	}
	p := newAnthropicTestPipeline(t, upstream, deployments)

	messages := []adapter.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "hi"},
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "claude-auto-on", Messages: messages}); err != nil {
		t.Fatalf("HandleChatCompletion(claude-auto-on): %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "claude-auto-off", Messages: messages}); err != nil {
		t.Fatalf("HandleChatCompletion(claude-auto-off): %v", err)
	}

	onReq, ok := captured["auto-on"]
	if !ok {
		t.Fatal("no request captured for deployment \"auto-on\"")
	}
	if len(onReq.System) != 1 || onReq.System[0].CacheControl == nil {
		t.Errorf("auto-on deployment's real upstream-bound System = %+v, want an auto-populated cache_control marker -- dataplane must thread Deployment.DisableCacheControlAutoPopulate=false through to ToProvider", onReq.System)
	}

	offReq, ok := captured["auto-off"]
	if !ok {
		t.Fatal("no request captured for deployment \"auto-off\"")
	}
	if len(offReq.System) != 1 || offReq.System[0].CacheControl != nil {
		t.Errorf("auto-off deployment's real upstream-bound System = %+v, want NO cache_control marker -- dataplane must thread this deployment's own DisableCacheControlAutoPopulate=true through to ToProvider", offReq.System)
	}
}
