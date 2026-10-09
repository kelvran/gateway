package dataplane

// Tests for kelvran.fallback.rescued, kelvran.fallback.outcome and
// kelvran.fallback.hops, per
// docs/rfcs/2026-10-09-gateway-fallback-rescued-and-prometheus-pull.md
// (plan item 13b). Every test diffs a before/after snapshot of the shared
// process-wide ManualReader (dataplaneTelemetryMetricsReaderForTest), the
// same pattern TestRateLimitFailOpenIncrementsMetricCounter uses, because
// the OTel global MeterProvider can only be installed once per process.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// fallbackTelemetrySnapshot holds the cumulative values the fallback
// telemetry tests diff.
type fallbackTelemetrySnapshot struct {
	// rescued is kelvran.fallback.rescued keyed by rescuedSnapshotKey.
	rescued map[string]int64
	// durationByOutcome is gen_ai.client.operation.duration's data-point
	// Count keyed by the kelvran.fallback.outcome attribute value.
	durationByOutcome map[string]uint64
	// tokenInstrumentsWithOutcome lists every token instrument whose data
	// points carry kelvran.fallback.outcome. The RFC puts the attribute on
	// the duration histogram only, so this must stay empty.
	tokenInstrumentsWithOutcome []string
}

func rescuedSnapshotKey(keyID, from, to, class string) string {
	return keyID + "|" + from + "|" + to + "|" + class
}

func (s fallbackTelemetrySnapshot) rescuedTotal() int64 {
	var total int64
	for _, v := range s.rescued {
		total += v
	}
	return total
}

// metricCarriesAttribute reports whether any data point of m (an int64 sum
// or a float64 histogram — the two shapes the token instruments use) has
// the attribute key.
func metricCarriesAttribute(t *testing.T, m metricdata.Metrics, key string) bool {
	t.Helper()
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range data.DataPoints {
			if _, ok := dp.Attributes.Value(attribute.Key(key)); ok {
				return true
			}
		}
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			if _, ok := dp.Attributes.Value(attribute.Key(key)); ok {
				return true
			}
		}
	default:
		t.Fatalf("%s data type = %T, want Sum[int64] or Histogram[float64]", m.Name, m.Data)
	}
	return false
}

func snapshotFallbackTelemetry(t *testing.T) fallbackTelemetrySnapshot {
	t.Helper()
	reader := dataplaneTelemetryMetricsReaderForTest()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	snap := fallbackTelemetrySnapshot{
		rescued:           map[string]int64{},
		durationByOutcome: map[string]uint64{},
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "kelvran.fallback.rescued":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.fallback.rescued data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					keyID, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranVirtualKeyID))
					from, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranFallbackFrom))
					to, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranDeploymentName))
					class, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranFallbackHopErrorClass))
					snap.rescued[rescuedSnapshotKey(keyID.AsString(), from.AsString(), to.AsString(), class.AsString())] += dp.Value
				}
			case "gen_ai.client.operation.duration":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.operation.duration data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					outcome, ok := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranFallbackOutcome))
					if !ok {
						continue
					}
					snap.durationByOutcome[outcome.AsString()] += dp.Count
				}
			case "gen_ai.client.inference.usage.input_tokens",
				"gen_ai.client.inference.usage.output_tokens",
				"gen_ai.client.inference.usage.cache_read.input_tokens",
				"gen_ai.client.inference.usage.cache_write.input_tokens",
				"gen_ai.client.inference.usage.reasoning.output_tokens",
				"gen_ai.client.inference.operation.input_tokens",
				"gen_ai.client.inference.operation.output_tokens":
				if metricCarriesAttribute(t, m, telemetry.AttrKelvranFallbackOutcome) {
					snap.tokenInstrumentsWithOutcome = append(snap.tokenInstrumentsWithOutcome, m.Name)
				}
			}
		}
	}
	return snap
}

// chatSpanSince returns the single "chat <model>" span ended since before.
func chatSpanSince(t *testing.T, before int) sdktrace.ReadOnlySpan {
	t.Helper()
	var chat []sdktrace.ReadOnlySpan
	for _, s := range spansSince(before) {
		if strings.HasPrefix(s.Name(), "chat ") {
			chat = append(chat, s)
		}
	}
	if len(chat) != 1 {
		t.Fatalf("got %d chat spans since the test started, want exactly 1", len(chat))
	}
	return chat[0]
}

func assertHopsAttr(t *testing.T, span sdktrace.ReadOnlySpan, want int64) {
	t.Helper()
	v, ok := spanAttr(t, span.Attributes(), telemetry.AttrKelvranFallbackHops)
	if want == 0 {
		if ok {
			t.Errorf("%s = %v on a request with no fallback, want the attribute absent", telemetry.AttrKelvranFallbackHops, v.AsInt64())
		}
		return
	}
	if !ok {
		t.Fatalf("%s absent from the chat span, want %d", telemetry.AttrKelvranFallbackHops, want)
	}
	if v.AsInt64() != want {
		t.Errorf("%s = %d, want %d", telemetry.AttrKelvranFallbackHops, v.AsInt64(), want)
	}
}

func assertNoOutcomeOnTokenInstruments(t *testing.T, snap fallbackTelemetrySnapshot) {
	t.Helper()
	if len(snap.tokenInstrumentsWithOutcome) != 0 {
		t.Errorf("%s leaked onto token instruments %v; the RFC puts it on gen_ai.client.operation.duration only", telemetry.AttrKelvranFallbackOutcome, snap.tokenInstrumentsWithOutcome)
	}
}

func chatRequest(model string) adapter.ChatRequest {
	return adapter.ChatRequest{Model: model, Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
}

func TestHandleChatCompletionRescuedFallbackRecordsCounterOutcomeAndHops(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary-cheap" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, budget.NewTracker(), nil)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), ""); err != nil {
		t.Fatalf("expected the fallback to succeed, got error: %v", err)
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary-cheap", "fallback-expensive", FallbackClassGeneric)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued{key=test-key, from=primary-cheap, to=fallback-expensive, class=generic} delta = %d, want 1 (all series: %v)", got, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want 1", got)
	}
	assertNoOutcomeOnTokenInstruments(t, after)
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 1)
}

func TestHandleChatCompletionTwoHopChainRecordsHopsAndFirstFailureClass(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	deployments := []Deployment{
		{
			Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused",
			FallbackChains: map[string][]string{FallbackClassContentPolicy: {"hop-1", "hop-2"}},
		},
		{Name: "hop-1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "hop-2", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		switch dep.Name {
		case "primary":
			return nil, &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}
		case "hop-1":
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "hop-1 unavailable"}
		default:
			return fakeOpenAIResponse(dep.UpstreamModel), nil
		}
	}, deployments)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Fatalf("expected the chain to succeed at hop-2, got error: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want [primary hop-1 hop-2]", calls)
	}

	after := snapshotFallbackTelemetry(t)
	// The class is the FIRST deployment's failure (content_policy), not
	// hop-1's generic 500; "to" is the hop that served.
	key := rescuedSnapshotKey("test-key", "primary", "hop-2", FallbackClassContentPolicy)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued{from=primary, to=hop-2, class=content_policy} delta = %d, want 1 (all series: %v)", got, after.rescued)
	}
	// Two hops reached an upstream call (hop-1 failed, hop-2 served).
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 2)
}

func TestHandleChatCompletionExhaustedFallbackRecordsOutcomeExhausted(t *testing.T) {
	before := snapshotFallbackTelemetry(t)

	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return nil, &UpstreamHTTPError{StatusCode: 500, Body: dep.Name + " unavailable"}
	}, budget.NewTracker(), nil)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), ""); err == nil {
		t.Fatal("expected an error once every deployment failed, got nil")
	}

	after := snapshotFallbackTelemetry(t)
	if got := after.rescuedTotal() - before.rescuedTotal(); got != 0 {
		t.Errorf("kelvran.fallback.rescued total delta = %d, want 0 (nothing was rescued)", got)
	}
	// Was red until runMissPath handed fallbackInfo out on its error branch
	// (the record used to be a local of the singleflight closure, and the
	// error return passed finalize a zero fallbackInfo{}); pins that the
	// exhausted buffered case reads "exhausted", not "none".
	if got := after.durationByOutcome[telemetry.FallbackOutcomeExhausted] - before.durationByOutcome[telemetry.FallbackOutcomeExhausted]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=exhausted} count delta = %d, want 1", got)
	}
}

func TestHandleChatCompletionWithoutFallbackRecordsOutcomeNone(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, budget.NewTracker(), nil)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	after := snapshotFallbackTelemetry(t)
	if got := after.rescuedTotal() - before.rescuedTotal(); got != 0 {
		t.Errorf("kelvran.fallback.rescued total delta = %d, want 0", got)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeNone] - before.durationByOutcome[telemetry.FallbackOutcomeNone]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=none} count delta = %d, want 1", got)
	}
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 0)
}

func TestHandleChatCompletionRescuedThenPostCallBlockedCountsAsRescued(t *testing.T) {
	before := snapshotFallbackTelemetry(t)

	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary-cheap" {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
		}
		// The fallback answered, so the upstream call is billed; the
		// post-call guardrail then blocks the Block-tier content.
		return fakeOpenAIResponseWithContent(dep.UpstreamModel, "sure, here it is: "+fakeCreditCardNumber), nil
	}, budget.NewTracker(), nil)

	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), "")
	if !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("err = %v, want ErrGuardrailBlocked from the post-call guardrail", err)
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary-cheap", "fallback-expensive", FallbackClassGeneric)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued delta = %d, want 1: the fallback produced the response finalize bills, even though the guardrail then blocked it (all series: %v)", got, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want 1", got)
	}
}

func streamingFallbackDeployments() []Deployment {
	return []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
}

func TestHandleChatCompletionStreamRescuedFallbackRecordsCounterAndOutcome(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		if dep.Name == "primary" {
			return nil, errors.New("simulated connection failure before any byte was read")
		}
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, streamingFallbackDeployments(), adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	req := chatRequest("gpt-4o")
	req.Stream = true
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err != nil {
		t.Fatalf("expected the fallback to succeed, got error: %v", err)
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary", "secondary", FallbackClassGeneric)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued{from=primary, to=secondary, class=generic} delta = %d, want 1 (all series: %v)", got, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want 1", got)
	}
	assertNoOutcomeOnTokenInstruments(t, after)
	// The legacy single-fallback path is one hop by definition.
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 1)
}

func TestHandleChatCompletionStreamFallbackHopFailingAfterFirstByteCountsAsRescued(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	firstChunkEnd := strings.Index(realOpenAISSEStream, "\n\n") + len("\n\n")

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		if dep.Name == "primary" {
			return nil, errors.New("simulated connection failure before any byte was read")
		}
		// The fallback hop delivers its first chunk and then dies: the
		// request errors, but a chunk reached the client, so finalize bills it.
		return nopCloserReader{&failAfterNBytesReader{r: strings.NewReader(realOpenAISSEStream), n: firstChunkEnd + 5}}, nil
	}, streamingFallbackDeployments(), adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	req := chatRequest("gpt-4o")
	req.Stream = true
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err == nil {
		t.Fatal("expected an error from the mid-stream connection loss on the fallback hop, got nil")
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Fatalf("the fallback hop's first chunk should have reached the client: %s", rec.Body.String())
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary", "secondary", FallbackClassGeneric)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued delta = %d, want 1: the hop that failed after its first byte is the billed rescue (all series: %v)", got, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want 1", got)
	}
}

func TestHandleChatCompletionStreamExhaustedFallbackRecordsOutcomeExhausted(t *testing.T) {
	before := snapshotFallbackTelemetry(t)

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return nil, errors.New(dep.Name + ": simulated connection failure before any byte was read")
	}, streamingFallbackDeployments(), adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	req := chatRequest("gpt-4o")
	req.Stream = true
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err == nil {
		t.Fatal("expected an error once both deployments failed, got nil")
	}

	after := snapshotFallbackTelemetry(t)
	if got := after.rescuedTotal() - before.rescuedTotal(); got != 0 {
		t.Errorf("kelvran.fallback.rescued total delta = %d, want 0", got)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeExhausted] - before.durationByOutcome[telemetry.FallbackOutcomeExhausted]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=exhausted} count delta = %d, want 1", got)
	}
}

// TestHandleChatCompletionLegacySingleFallbackRecordsFirstFailureClass covers
// the buffered legacy single-fallback site (nextEligibleDeployment, no
// fallback_chains): class from the first deployment's own error, hops = 1.
func TestHandleChatCompletionLegacySingleFallbackRecordsFirstFailureClass(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	deployments := []Deployment{
		{Name: "primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "secondary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	var calls []string
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		calls = append(calls, dep.Name)
		if dep.Name == "primary" {
			return nil, &UpstreamHTTPError{StatusCode: 400, Body: "content_policy_violation"}
		}
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, deployments)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Fatalf("expected the legacy fallback to succeed, got error: %v", err)
	}
	if len(calls) != 2 || calls[0] != "primary" || calls[1] != "secondary" {
		t.Fatalf("calls = %v, want [primary secondary]", calls)
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary", "secondary", FallbackClassContentPolicy)
	if got := after.rescued[key] - before.rescued[key]; got != 1 {
		t.Errorf("kelvran.fallback.rescued{from=primary, to=secondary, class=content_policy} delta = %d, want 1 (all series: %v)", got, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want 1", got)
	}
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 1)
}

func TestHandleChatCompletionStreamWithoutFallbackRecordsOutcomeNone(t *testing.T) {
	before := snapshotFallbackTelemetry(t)
	spansBefore := len(spanRecorder.Ended())

	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
	}, streamingFallbackDeployments(), adapter.Registry{"openai": openai.New()})

	rec := httptest.NewRecorder()
	req := chatRequest("gpt-4o")
	req.Stream = true
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}

	after := snapshotFallbackTelemetry(t)
	if got := after.rescuedTotal() - before.rescuedTotal(); got != 0 {
		t.Errorf("kelvran.fallback.rescued total delta = %d, want 0", got)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeNone] - before.durationByOutcome[telemetry.FallbackOutcomeNone]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=none} count delta = %d, want 1", got)
	}
	assertHopsAttr(t, chatSpanSince(t, spansBefore), 0)
}

// TestHandleChatCompletionCoalescedFollowersOfARescuedLeaderCountAsRescued pins
// the singleflight corner: byte-identical concurrent misses share one
// fallback walk; every follower receives the leader's fallback record (as its
// decision event always has) and so counts as a rescued request, while the
// upstream was called once per deployment and only the leader is billed.
func TestHandleChatCompletionCoalescedFollowersOfARescuedLeaderCountAsRescued(t *testing.T) {
	before := snapshotFallbackTelemetry(t)

	var primaryCalls, fallbackCalls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{})
	tracker := budget.NewTracker()
	p := newCrossModelFallbackPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		if dep.Name == "primary-cheap" {
			primaryCalls.Add(1)
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "primary-cheap unavailable"}
		}
		if fallbackCalls.Add(1) == 1 {
			close(started)
		}
		<-release
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, tracker, nil)

	const n = 4
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			_, errs[i] = p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), "")
		}(i)
	}
	ready.Wait()
	close(start)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the fallback upstream call to start")
	}
	time.Sleep(50 * time.Millisecond) // let the followers reach missGroup.Do
	close(release)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("primaryCalls = %d, fallbackCalls = %d, want 1 and 1 (the misses must have coalesced into one fallback walk)", primaryCalls.Load(), fallbackCalls.Load())
	}
	// Only the leader is billed: gpt-4o's price for 5 prompt + 3 completion tokens, once.
	if spent := tracker.SpentUSD(context.Background(), "test-key", 0); !spent.Equal(decimal.NewFromFloat(0.0000425)) {
		t.Errorf("spent = %s, want 0.0000425 (one billed rescue, followers unbilled)", spent)
	}

	after := snapshotFallbackTelemetry(t)
	key := rescuedSnapshotKey("test-key", "primary-cheap", "fallback-expensive", FallbackClassGeneric)
	if got := after.rescued[key] - before.rescued[key]; got != n {
		t.Errorf("kelvran.fallback.rescued delta = %d, want %d: every coalesced follower shares the leader's rescue (all series: %v)", got, n, after.rescued)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeRescued] - before.durationByOutcome[telemetry.FallbackOutcomeRescued]; got != n {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=rescued} count delta = %d, want %d", got, n)
	}
	if got := after.durationByOutcome[telemetry.FallbackOutcomeNone] - before.durationByOutcome[telemetry.FallbackOutcomeNone]; got != 0 {
		t.Errorf("gen_ai.client.operation.duration{kelvran.fallback.outcome=none} count delta = %d, want 0", got)
	}
}

// TestGatewayEventReportsFallbackOnFailedBufferedRequest pins the decision
// event consequence of handing fallbackInfo out of runMissPath's error
// branch: a failed buffered request that attempted a fallback now carries
// fallback_happened / fallback_from_deployment / fallback_reason (the
// streaming path always did).
func TestGatewayEventReportsFallbackOnFailedBufferedRequest(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	p := newTestPipelineWithKeysAndLogger(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return nil, &UpstreamHTTPError{StatusCode: 500, Body: dep.Name + " unavailable"}
	}, crossModelFallbackDeployments(), defaultTestVirtualKeys(), logger)

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", chatRequest("gpt-4o-mini"), ""); err == nil {
		t.Fatal("expected an error once every deployment failed, got nil")
	}

	event := decodeLoggedGatewayEvent(t, &logBuf)
	if !event.GetFallbackHappened() {
		t.Fatal("fallback_happened = false on a failed buffered request that walked its fallback chain; want true")
	}
	if got := event.GetFallbackFromDeployment(); got != "primary-cheap" {
		t.Errorf("fallback_from_deployment = %q, want %q", got, "primary-cheap")
	}
	if event.GetFallbackReason() == "" {
		t.Error("fallback_reason is empty, want the first deployment's error")
	}
}
