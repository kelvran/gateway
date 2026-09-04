package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"google.golang.org/protobuf/encoding/protojson"

	gatewayeventsv1 "github.com/kelvran/gateway/gateway/api/gatewayevents/v1"
	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestOutcomeForClassifiesEverySentinelError proves outcomeFor's
// errors.Is chain covers every rejection HandleChatCompletion/
// HandleChatCompletionStream can actually produce today, per
// docs/rfcs/2026-09-03-api-gatewayevents-contract.md's "Outcome must be
// derivable from finalize's existing err parameter alone" constraint.
func TestOutcomeForClassifiesEverySentinelError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want gatewayeventsv1.GatewayDecisionEvent_Outcome
	}{
		{"nil", nil, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_OK},
		{"missing header", identity.ErrMissingHeader, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_AUTH_FAILED},
		{"invalid key", identity.ErrInvalidKey, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_AUTH_FAILED},
		{"model not allowed", ErrModelNotAllowed, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_MODEL_NOT_ALLOWED},
		{"rate limited", ErrRateLimited, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_RATE_LIMITED},
		{"budget exceeded", ErrBudgetExceeded, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_BUDGET_EXCEEDED},
		{"no deployment", ErrNoDeployment, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_NO_DEPLOYMENT},
		{"guardrail blocked", ErrGuardrailBlocked, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_GUARDRAIL_BLOCKED},
		{"generic upstream error", context.DeadlineExceeded, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_UPSTREAM_ERROR},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outcomeFor(tt.err); got != tt.want {
				t.Errorf("outcomeFor(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// decodeLoggedGatewayEvent parses the last "gatewayevents_v1" field out of
// logBuf's JSON-per-line slog output and decodes it via the real
// generated Go bindings — proving the producer side of
// docs/rfcs/2026-09-03-api-gatewayevents-contract.md end-to-end, not just
// that outcomeFor's switch statement is correct in isolation.
func decodeLoggedGatewayEvent(t *testing.T, logBuf *bytes.Buffer) *gatewayeventsv1.GatewayDecisionEvent {
	t.Helper()
	var lastLine map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(logBuf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		lastLine = nil
		if err := json.Unmarshal(line, &lastLine); err != nil {
			t.Fatalf("unmarshaling log line %q: %v", line, err)
		}
	}
	if lastLine == nil {
		t.Fatal("no log lines captured")
	}
	raw, ok := lastLine["gatewayevents_v1"].(string)
	if !ok {
		t.Fatalf("log line %+v has no string gatewayevents_v1 field", lastLine)
	}
	event := &gatewayeventsv1.GatewayDecisionEvent{}
	if err := protojson.Unmarshal([]byte(raw), event); err != nil {
		t.Fatalf("protojson.Unmarshal(%q): %v", raw, err)
	}
	return event
}

func TestGatewayEventLoggedOnSuccessHasOutcomeOK(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-events", KeyHash: testHashOf("team-events"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, nil, keys, logger)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-events", adapter.ChatRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetOutcome() != gatewayeventsv1.GatewayDecisionEvent_OUTCOME_OK {
		t.Errorf("Outcome = %v, want OUTCOME_OK", event.GetOutcome())
	}
	if event.GetVirtualKeyId() != "team-events" {
		t.Errorf("VirtualKeyId = %q, want %q", event.GetVirtualKeyId(), "team-events")
	}
	if event.GetRequestedModel() != "gpt-4o" {
		t.Errorf("RequestedModel = %q, want %q", event.GetRequestedModel(), "gpt-4o")
	}
	if event.GetTraceId() == "" {
		t.Error("TraceId is empty, want a real trace ID from the OTel span")
	}
}

func TestGatewayEventLoggedOnRejectionHasCorrectOutcome(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{
			ID: "team-restricted", KeyHash: testHashOf("team-restricted"),
			RateLimitBurst: 100, RateLimitRefill: 100,
			AllowedModels: map[string]struct{}{"claude-opus-4": {}},
		},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("Upstream should never be called for a model-not-allowed rejection")
		return nil, nil
	}, nil, keys, logger)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-restricted", adapter.ChatRequest{Model: "gpt-4o"})
	if err == nil {
		t.Fatal("HandleChatCompletion succeeded, want ErrModelNotAllowed")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetOutcome() != gatewayeventsv1.GatewayDecisionEvent_OUTCOME_MODEL_NOT_ALLOWED {
		t.Errorf("Outcome = %v, want OUTCOME_MODEL_NOT_ALLOWED", event.GetOutcome())
	}
	if event.GetVirtualKeyId() != "team-restricted" {
		t.Errorf("VirtualKeyId = %q, want %q", event.GetVirtualKeyId(), "team-restricted")
	}
}

// failingRedisBackend is a ratelimit.RedisBackend whose Allow always
// errors — simulating a real Redis outage, deterministically, without a
// real Redis container, so the fail-open path
// (dataplane.checkRateLimit's second return value) can be exercised in a
// unit test per docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md.
type failingRedisBackend struct{}

func (failingRedisBackend) Allow(_ context.Context, _ string, _, _ float64) (bool, error) {
	return false, errors.New("simulated redis backend failure")
}

func (failingRedisBackend) Close() error { return nil }

// TestGatewayEventRateLimitFailOpenTrueWhenBackendErrors is the
// load-bearing proof that a Redis backend error surfaces as
// RateLimitFailOpen=true on the logged event — auditable in production,
// not just asserted in docs/rfcs/2026-09-03-distributed-rate-limiting.md's
// "second, independent control" argument.
func TestGatewayEventRateLimitFailOpenTrueWhenBackendErrors(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-failopen", KeyHash: testHashOf("team-failopen"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewRedisKeyLimiter(keyConfigsFromVirtualKeys(keys), failingRedisBackend{}),
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
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-failopen", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("HandleChatCompletion with a failing Redis backend: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if !event.GetRateLimitFailOpen() {
		t.Error("RateLimitFailOpen = false, want true when the rate limiter's backend errors")
	}
}

// TestGatewayEventRateLimitFailOpenFalseOnNormalPass proves the negative
// case: an ordinary in-memory (non-erroring) rate-limit pass must NOT be
// misreported as a fail-open.
func TestGatewayEventRateLimitFailOpenFalseOnNormalPass(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	keys := []identity.VirtualKey{
		{ID: "team-normal", KeyHash: testHashOf("team-normal"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, nil, keys, logger)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-normal", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetRateLimitFailOpen() {
		t.Error("RateLimitFailOpen = true for a normal in-memory rate-limit pass, want false")
	}
}

// TestGatewayEventFallbackDetailPopulatedOnFallback is the load-bearing
// proof that a real fallback records the ABANDONED (first) deployment's
// name and error, not the one ultimately used — the exact ordering bug
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md's "what a
// naive implementation would get wrong" section names explicitly.
func TestGatewayEventFallbackDetailPopulatedOnFallback(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	keys := []identity.VirtualKey{
		{ID: "team-fallback", KeyHash: testHashOf("team-fallback"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary" {
			return nil, errors.New("simulated upstream failure")
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments, keys, logger)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-fallback", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if !event.GetFallbackHappened() {
		t.Fatal("FallbackHappened = false, want true")
	}
	if event.GetFallbackFromDeployment() != "primary" {
		t.Errorf("FallbackFromDeployment = %q, want %q (the ABANDONED deployment, not the one ultimately used)", event.GetFallbackFromDeployment(), "primary")
	}
	if event.GetFallbackReason() == "" {
		t.Error("FallbackReason is empty, want the first attempt's error text")
	}
}

// TestGatewayEventFallbackDetailAbsentWithNoEligibleFallback proves an
// upstream error with no second deployment to fall back to is NOT
// misreported as a fallback — the outer "err != nil" is not itself
// evidence a fallback occurred.
func TestGatewayEventFallbackDetailAbsentWithNoEligibleFallback(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	keys := []identity.VirtualKey{
		{ID: "team-nofallback", KeyHash: testHashOf("team-nofallback"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return nil, errors.New("simulated upstream failure")
	}, nil, keys, logger)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-nofallback", adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("expected an upstream error with no eligible fallback deployment")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetFallbackHappened() {
		t.Error("FallbackHappened = true with only one deployment configured, want false")
	}
	if event.GetFallbackFromDeployment() != "" || event.GetFallbackReason() != "" {
		t.Errorf("fallback detail fields = (%q, %q), want both empty when no fallback happened", event.GetFallbackFromDeployment(), event.GetFallbackReason())
	}
}

// TestGatewayEventBudgetSpentUsdReflectsRealPriorSpend proves
// budget_spent_usd on a BUDGET_EXCEEDED rejection is the key's real
// cumulative spend from EARLIER requests, not a placeholder — the load-
// bearing proof for the field's whole reason to exist ("how close to the
// cap was this key").
func TestGatewayEventBudgetSpentUsdReflectsRealPriorSpend(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-budget", KeyHash: testHashOf("team-budget"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("0.0005")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// A real, nonzero price table entry — fakeOpenAIResponse's usage
	// (5 prompt + 3 completion tokens) times this price produces a real,
	// measurable cost, unlike the empty PriceTable{} most other tests use.
	priceTable := costaccounting.PriceTable{
		"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0001"), CompletionPerToken: decimal.RequireFromString("0.0001")},
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
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
		CostCalculator: costaccounting.NewCalculator(priceTable),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// First request succeeds and records real spend (5*0.0001 + 3*0.0001 = 0.0008).
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-budget", adapter.ChatRequest{Model: "gpt-4o"}); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	wantSpent := p.budget.SpentUSD("team-budget")
	if wantSpent.IsZero() {
		t.Fatal("expected nonzero spend recorded after the first request")
	}

	// Second request is rejected — cumulative spend (0.0008) already
	// exceeds the 0.0005 cap on its own, before this request's own cost
	// is ever computed (it never is, since the request is rejected) —
	// budget_spent_usd must reflect that spend AT DECISION TIME, i.e.
	// exactly wantSpent, not zero, not a different number.
	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-budget", adapter.ChatRequest{Model: "gpt-4o"}); err == nil {
		t.Fatal("expected the second request to be budget-rejected")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetOutcome() != gatewayeventsv1.GatewayDecisionEvent_OUTCOME_BUDGET_EXCEEDED {
		t.Fatalf("Outcome = %v, want OUTCOME_BUDGET_EXCEEDED", event.GetOutcome())
	}
	gotSpent, parseErr := decimal.NewFromString(event.GetBudgetSpentUsd())
	if parseErr != nil {
		t.Fatalf("parsing BudgetSpentUsd %q: %v", event.GetBudgetSpentUsd(), parseErr)
	}
	if !gotSpent.Equal(wantSpent) {
		t.Errorf("BudgetSpentUsd = %s, want %s (the real prior spend at decision time)", gotSpent, wantSpent)
	}
}

// TestGatewayEventStreamingFallbackFalseAfterFirstChunkSent is the
// streaming-specific proof of the exact failure mode
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md names
// explicitly: a streaming response that already sent a chunk to the
// client, then errors mid-stream, must report FallbackHappened=false —
// "this response errored" and "this response fell back" are never the
// same signal, even though err is non-nil on both.
func TestGatewayEventStreamingFallbackFalseAfterFirstChunkSent(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-stream", KeyHash: testHashOf("team-stream"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// Fail partway through the first real content chunk's own bytes, so
	// at least one full chunk is guaranteed to have already reached the
	// client before the read error surfaces — mirrors
	// streaming_test.go's TestHandleChatCompletionStreamNoFallbackAfterFirstByte.
	firstChunkEnd := strings.Index(realOpenAISSEStream, "\n\n") + len("\n\n")
	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
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
			t.Fatal("non-streaming Upstream should never be called by a streaming test")
			return nil, nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(realOpenAISSEStream), n: firstChunkEnd + 5}}, nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	rec := httptest.NewRecorder()
	err = p.HandleChatCompletionStream(context.Background(), "Bearer team-stream", adapter.ChatRequest{
		Model: "gpt-4o", Stream: true,
	}, rec)
	if err == nil {
		t.Fatal("expected an error from the mid-stream connection loss")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetFallbackHappened() {
		t.Error("FallbackHappened = true after a chunk was already sent, want false — errored is not the same as fell back")
	}
}

// newTestPipelineWithKeysAndLogger mirrors newTestPipelineWithKeysAndBudget
// (dataplane_test.go) exactly, except it lets a test supply its own
// logger (to capture and decode gatewayevents_v1 log output) instead of
// the always-discarding discardLogger().
func newTestPipelineWithKeysAndLogger(t *testing.T, upstream UpstreamCaller, deployments []Deployment, keys []identity.VirtualKey, logger *slog.Logger) *Pipeline {
	t.Helper()

	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if deployments == nil {
		deployments = []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
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
		Upstream:       upstream,
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}
