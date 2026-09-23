package dataplane

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// newTestPipelineWithConcurrency mirrors dataplane_test.go's own
// newTestPipelineWithKeys, plus one extra field (Config.Concurrency) none
// of that file's existing helpers expose -- kept in this file, right next
// to its only caller, rather than growing the shared helper file for a
// single-test need.
func newTestPipelineWithConcurrency(t *testing.T, upstream UpstreamCaller, deployments []Deployment, concurrency *ratelimit.ConcurrencyLimiter) *Pipeline {
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
			"openai": openai.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Concurrency:    concurrency,
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestHandleChatCompletionInFlightByAgentRunReflectsARealInFlightRequest
// is the real, end-to-end proof that a request carrying a specific
// agent_run_id -- set via the same OTel Baggage mechanism
// telemetry.AgentRunIDFromContext reads (confirmed by reading that
// function's real implementation first) -- shows up correctly in a
// subsequent pipeline.InFlightByAgentRun(vk.ID) call WHILE that request
// is still genuinely outstanding, not a synthetic/mocked check. Uses a
// real, deliberately blocking fake upstream (released only after this
// test's own InFlightByAgentRun read completes), mirroring
// cache_stampede_test.go's own established "release/started channel
// barrier" pattern for exactly this class of test rather than inventing
// a new one. Deliberately uses an UNCAPPED ConcurrencyLimiter (no
// configured MaxInFlight for "test-key" at all) -- the critical case
// this whole feature exists for, per ratelimit.ConcurrencyLimiter's own
// doc comment.
func TestHandleChatCompletionInFlightByAgentRunReflectsARealInFlightRequest(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	p := newTestPipelineWithConcurrency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		close(started)
		<-release
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}},
		ratelimit.NewConcurrencyLimiter(nil))

	member, err := baggage.NewMember("agent_run_id", "run-live-inflight")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{
			Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}},
		}, "")
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real upstream call to start")
	}

	// The load-bearing assertion: read while the request above is
	// genuinely still blocked in the fake upstream, not yet released.
	total, byRun := p.InFlightByAgentRun("test-key")
	if total != 1 {
		t.Fatalf("total = %d, want 1 while the request is genuinely in flight", total)
	}
	if byRun["run-live-inflight"] != 1 {
		t.Fatalf("byRun[run-live-inflight] = %d, want 1, full map: %v", byRun["run-live-inflight"], byRun)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HandleChatCompletion to finish")
	}

	total, byRun = p.InFlightByAgentRun("test-key")
	if total != 0 {
		t.Fatalf("total after the request finished = %d, want 0 -- the slot must be released", total)
	}
	if len(byRun) != 0 {
		t.Fatalf("byRun after the request finished = %v, want empty -- a finished agent run must not linger in the breakdown", byRun)
	}
}

// TestHandleChatCompletionInFlightByAgentRunIsZeroWithNoConcurrencyConfigured
// proves InFlightByAgentRun's own nil-safety convention end to end: a
// Pipeline built with Config.Concurrency left unset (the common case)
// reports (0, empty map) rather than panicking or reporting stale data,
// exactly mirroring checkConcurrency's own "nil means unconfigured, not
// broken" rule.
func TestHandleChatCompletionInFlightByAgentRunIsZeroWithNoConcurrencyConfigured(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", adapter.ChatRequest{
		Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}},
	}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	total, byRun := p.InFlightByAgentRun("test-key")
	if total != 0 {
		t.Errorf("total = %d, want 0 -- Config.Concurrency was never configured on this pipeline", total)
	}
	if byRun == nil || len(byRun) != 0 {
		t.Errorf("byRun = %v, want a non-nil empty map", byRun)
	}
}
