package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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
	"github.com/kelvran/gateway/gateway/internal/telemetry"
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
		{"deployment capacity (concurrency)", &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_DEPLOYMENT_CAPACITY},
		{"deployment capacity (rate_limit)", &DeploymentCapacityError{Deployment: "d1", Reason: "rate_limit"}, gatewayeventsv1.GatewayDecisionEvent_OUTCOME_DEPLOYMENT_CAPACITY},
		{"wrapped deployment capacity is still classified through errors.As", fmt.Errorf("upstream call to deployment %q: %w", "d1", &DeploymentCapacityError{Deployment: "d1", Reason: "concurrency"}), gatewayeventsv1.GatewayDecisionEvent_OUTCOME_DEPLOYMENT_CAPACITY},
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

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-events", adapter.ChatRequest{Model: "gpt-4o"}, "")
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

	_, err := p.HandleChatCompletion(context.Background(), "Bearer team-restricted", adapter.ChatRequest{Model: "gpt-4o"}, "")
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-failopen", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion with a failing Redis backend: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if !event.GetRateLimitFailOpen() {
		t.Error("RateLimitFailOpen = false, want true when the rate limiter's backend errors")
	}
}

// attackerModelForRateLimitFailOpenTest is a deliberately attacker-shaped
// model string used by TestRateLimitFailOpenIncrementsMetricCounter — a
// package-level const (not a local one) so snapshotDataplaneTelemetry can
// also reference it when checking the gen_ai.request.model
// cardinality-DoS regression.
const attackerModelForRateLimitFailOpenTest = "attacker-unique-model-name-12345-do-not-let-this-become-a-metric-label"

var (
	dataplaneTelemetryMetricsOnce   sync.Once
	dataplaneTelemetryMetricsReader *sdkmetric.ManualReader
)

// dataplaneTelemetryMetricsReaderForTest returns the ManualReader
// backing this test binary's real global-meter-delegated instruments —
// see gateway/internal/budget/persistence_test.go's
// budgetPersistenceMetricsReaderForTest doc comment for the underlying
// go.opentelemetry.io/otel constraint this works around (a
// process-lifetime sync.Once inside otel's own internal/global package
// permanently binds every package-level instrument obtained from the
// global meter to whichever MeterProvider FIRST calls
// otel.SetMeterProvider in this test binary's process). Sharing one
// reader across every `go test -count>1` rerun, and asserting the DELTA
// a run's own request(s) produced (below) rather than an absolute
// value, is what actually holds regardless of how many times this
// process replays the test. This is also this package's own
// one-delegation-per-test-binary constraint (mirrors
// telemetry/result_test.go's identical rationale) — every
// otel-metric-verifying scenario in this package's test binary must
// share this same reader, never a second SetMeterProvider call.
func dataplaneTelemetryMetricsReaderForTest() *sdkmetric.ManualReader {
	dataplaneTelemetryMetricsOnce.Do(func() {
		dataplaneTelemetryMetricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(dataplaneTelemetryMetricsReader)))
	})
	return dataplaneTelemetryMetricsReader
}

// dataplaneTelemetrySnapshot holds every value
// TestRateLimitFailOpenIncrementsMetricCounter asserts against.
// sawAttackerModel/sawUnresolvedSentinel are read from the "after"
// snapshot only (never diffed) — attribute PRESENCE on a given model
// string is a static property of a single call's own attribute set, not
// a cumulative value, so it doesn't need delta treatment.
type dataplaneTelemetrySnapshot struct {
	failOpenByKeyID       map[string]int64
	cacheLookupTotal      int64
	sawAttackerModel      bool
	sawUnresolvedSentinel bool
}

func snapshotDataplaneTelemetry(t *testing.T, rm metricdata.ResourceMetrics) dataplaneTelemetrySnapshot {
	t.Helper()
	snap := dataplaneTelemetrySnapshot{failOpenByKeyID: map[string]int64{}}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "kelvran.ratelimit.fail_open":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.ratelimit.fail_open data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					keyID, hasAttr := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranVirtualKeyID))
					if hasAttr {
						snap.failOpenByKeyID[keyID.AsString()] += dp.Value
					}
				}
			case "kelvran.cache.lookup":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.cache.lookup data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					snap.cacheLookupTotal += dp.Value
				}
			case "gen_ai.client.operation.duration":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.operation.duration data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					model, hasAttr := dp.Attributes.Value(attribute.Key(telemetry.AttrGenAIRequestModel))
					if !hasAttr {
						continue
					}
					switch model.AsString() {
					case attackerModelForRateLimitFailOpenTest:
						snap.sawAttackerModel = true
					case "unresolved":
						snap.sawUnresolvedSentinel = true
					}
				}
			}
		}
	}
	return snap
}

// TestRateLimitFailOpenIncrementsMetricCounter is the load-bearing proof
// for the aggregate, alertable signal itself — per
// docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md,
// telemetry.RecordRateLimitFailOpen exists specifically so an operator can
// alert on "how many times has this happened recently" without scanning
// logs/traces. A ManualReader installed as the global MeterProvider before
// the request runs proves the real otel.Meter obtained at telemetry
// package-init time (before any real provider existed) still delegates to
// it — the same re-delegation guarantee this codebase already relies on
// for Tracer. Reads a before/after snapshot pair, not one absolute
// reading — see dataplaneTelemetryMetricsReaderForTest's own doc comment
// for why.
func TestRateLimitFailOpenIncrementsMetricCounter(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	testKeyID := "team-failopen-metric"
	keys := []identity.VirtualKey{
		{ID: testKeyID, KeyHash: testHashOf(testKeyID), RateLimitBurst: 100, RateLimitRefill: 100},
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
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	authHeader := "Bearer " + testKeyID
	if _, err := p.HandleChatCompletion(context.Background(), authHeader, adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion with a failing Redis backend: %v", err)
	}

	// A Round-5 backlog-audit finding, verified in this same shared-meter
	// test function (this package's own one-delegation-per-test-binary
	// constraint — see telemetry/result_test.go's identical rationale):
	// a request that returns BEFORE ever reaching the cache-check stage
	// must never be recorded as a kelvran.cache.lookup "miss." The call
	// above legitimately reaches the cache-check stage (rate-limit
	// fail-open still falls through to cache checks normally) and
	// records exactly one real miss (an empty cache). This second call
	// uses a bearer token for a key that was never registered at all —
	// an auth failure returning long before any cache check — and must
	// add ZERO further kelvran.cache.lookup data points.
	//
	// Also doubles as the regression proof for the real bug fixed in
	// telemetry.ChatCompletionResult.RequestModel's own doc comment
	// (found via the kelvran-full-power-sweep audit, 2026-09-17): this
	// exact auth-failure path (no deployment ever resolved) used to pass
	// req.Model straight through, unbounded, into a metric attribute —
	// an unauthenticated caller sending a unique, garbage model string on
	// every call could mint a new gen_ai.request.model time series per
	// request. Reusing THIS call (rather than a second, separate test
	// function that swaps the provider again — this package's own
	// one-delegation-per-test-binary constraint, see above) with a
	// deliberately attacker-shaped model string checks that.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer no-such-key", adapter.ChatRequest{Model: attackerModelForRateLimitFailOpenTest}, ""); err == nil {
		t.Fatal("HandleChatCompletion with an unregistered bearer token returned nil error, want an auth failure")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, rm)

	if got := afterSnap.failOpenByKeyID[testKeyID] - beforeSnap.failOpenByKeyID[testKeyID]; got != 1 {
		t.Errorf("kelvran.ratelimit.fail_open[key_id=%s] delta = %d, want 1", testKeyID, got)
	}
	if got := afterSnap.cacheLookupTotal - beforeSnap.cacheLookupTotal; got != 1 {
		t.Errorf("kelvran.cache.lookup total delta = %d, want 1 — the auth-failure call never reached the cache-check stage and must not be counted as a miss", got)
	}
	if afterSnap.sawAttackerModel {
		t.Error("gen_ai.request.model carried the raw, attacker-controlled model string through to a metric attribute — unbounded-cardinality DoS is NOT fixed")
	}
	if !afterSnap.sawUnresolvedSentinel {
		t.Error(`gen_ai.request.model never recorded the "unresolved" sentinel for a request that never resolved a deployment`)
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-normal", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-fallback", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-nofallback", adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
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
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-budget", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	wantSpent := p.budget.SpentUSD("team-budget", 0)
	if wantSpent.IsZero() {
		t.Fatal("expected nonzero spend recorded after the first request")
	}

	// Second request is rejected — cumulative spend (0.0008) already
	// exceeds the 0.0005 cap on its own, before this request's own cost
	// is ever computed (it never is, since the request is rejected) —
	// budget_spent_usd must reflect that spend AT DECISION TIME, i.e.
	// exactly wantSpent, not zero, not a different number.
	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-budget", adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
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

// TestGatewayEventCarriesAgentRunIDAndCostUSD proves
// docs/rfcs/2026-09-12-gateway-cost-attribution-aggregation.md's fix:
// GatewayDecisionEvent (the one contract built for durable, offline
// analysis) must carry this request's own agent_run_id and real cost —
// previously the two only ever co-occurred on the ephemeral per-request
// OTel span, never on this event, closing the real gap
// THREAT_MODEL.md's Gateway Repudiation row was corrected to name.
func TestGatewayEventCarriesAgentRunIDAndCostUSD(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "team-agentrun", KeyHash: testHashOf("team-agentrun"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	// A real, nonzero price table entry so CostUsd is a genuine,
	// nonzero fact to assert on, not just "0" (which the fix must also
	// get right, per BudgetSpentUsd's own "0 is real, not absent"
	// convention — covered by every OTHER test in this file that
	// doesn't set a price table).
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

	member, err := baggage.NewMember("agent_run_id", "run-cost-attrib-1")
	if err != nil {
		t.Fatalf("baggage.NewMember: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage.New: %v", err)
	}
	ctx := baggage.ContextWithBaggage(context.Background(), bag)

	if _, err := p.HandleChatCompletion(ctx, "Bearer team-agentrun", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetAgentRunId() != "run-cost-attrib-1" {
		t.Errorf("AgentRunId = %q, want %q", event.GetAgentRunId(), "run-cost-attrib-1")
	}
	gotCost, parseErr := decimal.NewFromString(event.GetCostUsd())
	if parseErr != nil {
		t.Fatalf("parsing CostUsd %q: %v", event.GetCostUsd(), parseErr)
	}
	// fakeOpenAIResponse: 5 prompt + 3 completion tokens, both priced at
	// 0.0001/token = 0.0008.
	if want := decimal.RequireFromString("0.0008"); !gotCost.Equal(want) {
		t.Errorf("CostUsd = %s, want %s", gotCost, want)
	}
}

// TestGatewayEventCarriesSavingsUSD proves
// docs/rfcs/2026-09-12-gateway-cache-savings-agent-attribution.md's fix:
// GatewayDecisionEvent must carry the real notional cache-savings figure
// on a genuine cache hit, and "" (never a fabricated "0") on a miss —
// closing the SAVINGS half of PRD.md:41's agent-run cost-attribution
// success metric (Round 6 already closed the SPEND half via
// AgentRunId/CostUsd, proven by the sibling test above).
func TestGatewayEventCarriesSavingsUSD(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
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

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "savings-usd-test"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, ""); err != nil {
		t.Fatalf("first (real-miss) call: %v", err)
	}
	missEvent := decodeLoggedGatewayEvent(t, &logBuf)
	if missEvent.GetSavingsUsd() != "" {
		t.Errorf("SavingsUsd on a cache MISS = %q, want \"\" (never a fabricated cost for a request that was never a cache hit)", missEvent.GetSavingsUsd())
	}

	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req, ""); err != nil {
		t.Fatalf("second (cache-hit) call: %v", err)
	}
	hitEvent := decodeLoggedGatewayEvent(t, &logBuf)
	gotSavings, parseErr := decimal.NewFromString(hitEvent.GetSavingsUsd())
	if parseErr != nil {
		t.Fatalf("parsing SavingsUsd %q: %v", hitEvent.GetSavingsUsd(), parseErr)
	}
	// fakeOpenAIResponse: 5 prompt + 3 completion tokens, both priced at
	// 0.0001/token = 0.0008 — the same notional would-have-cost the miss
	// call's own CostUsd already carried, now also on SavingsUsd since
	// resp.Usage survives the cache round-trip intact.
	if want := decimal.RequireFromString("0.0008"); !gotSavings.Equal(want) {
		t.Errorf("SavingsUsd on a cache hit = %s, want %s", gotSavings, want)
	}
}

// TestGatewayEventCarriesFinishReasonOnSuccessAndOmitsItOnRejection is the
// regression proof for a real end-to-end research finding
// (docs/upgrade-research/cost-aware-cascading-tier1-2026-09-20.md): the
// primary choice's own FinishReason was already computed on every real
// request (responseWasTruncated's own scan), but never surfaced past
// that one cache-gating check. Proves both halves of
// primaryFinishReason's own contract: the real value on a genuine
// success (never "" just because a response happened to exist), and ""
// on a rejection that never reached an upstream call at all (resp stays
// at its zero value, never a fabricated placeholder).
func TestGatewayEventCarriesFinishReasonOnSuccessAndOmitsItOnRejection(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
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
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	successEvent := decodeLoggedGatewayEvent(t, &logBuf)
	if got := successEvent.GetFinishReason(); got != "stop" {
		t.Errorf("FinishReason on success = %q, want %q (fakeOpenAIResponse's own Choices[0].FinishReason)", got, "stop")
	}

	logBuf.Reset()
	if _, err := p.HandleChatCompletion(context.Background(), "", adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
		t.Fatal("HandleChatCompletion with no Authorization header: got nil error, want an auth error")
	}
	rejectionEvent := decodeLoggedGatewayEvent(t, &logBuf)
	if got := rejectionEvent.GetFinishReason(); got != "" {
		t.Errorf("FinishReason on an auth rejection (resp never populated) = %q, want \"\"", got)
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
	}, rec, "")
	if err == nil {
		t.Fatal("expected an error from the mid-stream connection loss")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetFallbackHappened() {
		t.Error("FallbackHappened = true after a chunk was already sent, want false — errored is not the same as fell back")
	}
}

// TestGatewayDecisionEventCarriesBillingSubjectIDWhenConfigured proves
// docs/upgrade-research/billing-monetization-integration-2026-09-15.md's
// field: a virtual key's own identity.VirtualKey.BillingSubjectID reaches
// the logged GatewayDecisionEvent's BillingSubjectId, the one durable
// export surface a future billing-platform ingestion consumer would read
// from — not just held on the in-memory VirtualKey struct.
func TestGatewayDecisionEventCarriesBillingSubjectIDWhenConfigured(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	keys := []identity.VirtualKey{
		{ID: "team-bill1", KeyHash: testHashOf("team-bill1"), RateLimitBurst: 100, RateLimitRefill: 100, BillingSubjectID: "cust_acme_12345"},
	}
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, nil, keys, logger)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-bill1", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if event.GetBillingSubjectId() != "cust_acme_12345" {
		t.Errorf("BillingSubjectId = %q, want %q", event.GetBillingSubjectId(), "cust_acme_12345")
	}
}

// TestBudgetReserveAndReconcileAreUnaffectedByBillingSubjectIDField is a
// deliberate negative test proving the decoupling constraint named in
// BillingSubjectID's own doc comment (api/gatewayevents/v1/gatewayevents.
// proto) and in the Kong/OpenMeter precedent from
// docs/upgrade-research/billing-monetization-integration-2026-09-15.md:
// budget.Tracker.Reserve/Reconcile's synchronous enforcement path must
// never read this field. Two virtual keys, identical in every real
// enforcement input (same BudgetUSD cap, same requests, same price table)
// except that only one carries a BillingSubjectID, must accrue IDENTICAL
// real spend — if Reserve/Reconcile ever branched on BillingSubjectID,
// this would be the first place that divergence would show up.
func TestBudgetReserveAndReconcileAreUnaffectedByBillingSubjectIDField(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	keys := []identity.VirtualKey{
		{ID: "team-bill2", KeyHash: testHashOf("team-bill2"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000"), BillingSubjectID: "cust_billed_67890"},
		{ID: "team-bill3", KeyHash: testHashOf("team-bill3"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
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

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-bill2", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (with BillingSubjectID): %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer team-bill3", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (without BillingSubjectID): %v", err)
	}
	spentWith := p.budget.SpentUSD("team-bill2", 0)
	spentWithout := p.budget.SpentUSD("team-bill3", 0)
	if spentWith.IsZero() {
		t.Fatal("expected nonzero spend recorded for the key with BillingSubjectID set")
	}
	if !spentWith.Equal(spentWithout) {
		t.Errorf("SpentUSD diverged between an otherwise-identical key with BillingSubjectID set (%s) and without (%s) — Reserve/Reconcile must never read this field", spentWith, spentWithout)
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
