package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/baggage"

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

// TestGetVirtualKeyInFlightUnknownNameReturns404 mirrors
// TestGetVirtualKeySpendUnknownNameReturns404 for the new
// observability-only inflight route.
func TestGetVirtualKeyInFlightUnknownNameReturns404(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential()}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/does-not-exist/inflight", fakeAdminCredential(), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET .../inflight for an unknown key: status = %d, want 404", rec.Code)
	}
}

// TestGetVirtualKeyInFlightReturnsCorrectShapeForAnExistingKey is the
// real, end-to-end happy-path proof: a genuinely in-flight request (a
// blocking fake upstream, released only after this test's own read of
// the new admin route completes) shows up through GET
// /admin/virtual_keys/{name}/inflight with the correct 200 shape, broken
// down by the agent_run_id actually carried on that request's own
// context -- via the same OTel Baggage mechanism
// telemetry.AgentRunIDFromContext reads (confirmed by reading that
// function's real implementation). Deliberately uses an UNCAPPED
// ConcurrencyLimiter (no configured MaxInFlight for "test-key" at all)
// -- the critical case this whole feature exists for, per
// ratelimit.ConcurrencyLimiter's own doc comment: an operator must still
// see an accurate breakdown for a key with no configured cap, not just
// for a capped one.
//
// Generates the in-flight load via pipeline.HandleChatCompletion called
// directly, not through HTTP -- admin's own mux has no client-facing
// chat-completion route to drive this through, mirroring every other
// test in this package that needs a real completed/in-flight request
// (e.g. TestDeleteVirtualKeyViaHTTPAlsoErasesItsBudgetSpend).
func TestGetVirtualKeyInFlightReturnsCorrectShapeForAnExistingKey(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	keys := []identity.VirtualKey{
		{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []dataplane.Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	pipeline, err := dataplane.NewPipeline(dataplane.Config{
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
		Router:         router.New([]router.Deployment{{Name: "d1", Model: "gpt-4o"}}, router.HealthConfig{}),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Logger:         discardLogger(),
		// Deliberately uncapped -- no ConcurrencyConfig entry for
		// "test-key" at all -- see this test's own doc comment.
		Concurrency: ratelimit.NewConcurrencyLimiter(nil),
		Upstream: func(ctx context.Context, dep dataplane.Deployment, req any) (any, error) {
			close(started)
			<-release
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

	member, err := baggage.NewMember("agent_run_id", "run-inflight-test")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = pipeline.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "gpt-4o"}, "")
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real upstream call to start")
	}

	logger, buf := capturingLogger()
	h := Handler(testConfig(), pipeline, Credentials{Admin: fakeAdminCredential()}, logger, nil)
	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/inflight", fakeAdminCredential(), "")

	close(release)
	wg.Wait()

	if rec.Code != http.StatusOK {
		t.Fatalf("GET .../inflight: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
	var got virtualKeyInFlightResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	if got.TotalInFlight != 1 {
		t.Errorf("TotalInFlight = %d, want 1 -- an uncapped key's own in-flight count must still be accurately reported", got.TotalInFlight)
	}
	if got.ByAgentRunID["run-inflight-test"] != 1 {
		t.Errorf("ByAgentRunID[run-inflight-test] = %d, want 1, full map: %v", got.ByAgentRunID["run-inflight-test"], got.ByAgentRunID)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "admin_virtual_key_inflight_read") || !strings.Contains(logOutput, "name=test-key") {
		t.Errorf("expected an admin_virtual_key_inflight_read audit-log entry naming test-key; got: %s", logOutput)
	}
}

// TestGetVirtualKeyInFlightRequiresAdminOrViewerCredential proves this
// route sits at GET /admin/virtual_keys's own tier (admin-or-viewer),
// per its own route-registration comment: a bare wrong credential is
// rejected, but the viewer tier -- narrower than admin, broader than the
// spend route's own CostViewer tier -- authenticates it successfully.
func TestGetVirtualKeyInFlightRequiresAdminOrViewerCredential(t *testing.T) {
	h := Handler(testConfig(), newTestPipeline(t), Credentials{Admin: fakeAdminCredential(), Viewer: fakeViewerCredential()}, discardLogger(), nil)

	rec := doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/inflight", "wrong-value-entirely", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET .../inflight with a wrong credential: status = %d, want 401", rec.Code)
	}

	rec = doRequest(t, h, http.MethodGet, "/admin/virtual_keys/test-key/inflight", fakeViewerCredential(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET .../inflight with the viewer credential: status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}
}
