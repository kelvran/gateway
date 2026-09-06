package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func attrValue(t *testing.T, attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	t.Helper()
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func TestRecordChatCompletionResultSuccessSetsAllAttributes(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		VirtualKeyID:   "team-alpha",
		Provider:       "openai",
		DeploymentName: "gpt4o-primary",
		ResponseModel:  "gpt-4o",
		ResponseID:     "chatcmpl-1",
		FinishReasons:  []string{"stop"},
		InputTokens:    10,
		OutputTokens:   5,
		CacheHit:       false,
		CostUSD:        "0.0001",
		AgentRunID:     "run-abc123",
	})
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("len(sr.Ended()) = %d, want 1", len(ended))
	}
	attrs := ended[0].Attributes()

	wantString := map[string]string{
		AttrKelvranVirtualKeyID:   "team-alpha",
		AttrGenAIProviderName:     "openai",
		AttrKelvranDeploymentName: "gpt4o-primary",
		AttrGenAIResponseModel:    "gpt-4o",
		AttrGenAIResponseID:       "chatcmpl-1",
		AttrKelvranAgentRunID:     "run-abc123",
	}
	for key, want := range wantString {
		v, ok := attrValue(t, attrs, attribute.Key(key))
		if !ok {
			t.Errorf("attribute %q not set", key)
			continue
		}
		if v.AsString() != want {
			t.Errorf("attribute %q = %q, want %q", key, v.AsString(), want)
		}
	}

	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageInputTokens)); !ok || v.AsInt64() != 10 {
		t.Errorf("%s = %v, ok=%v, want 10", AttrGenAIUsageInputTokens, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageOutputTokens)); !ok || v.AsInt64() != 5 {
		t.Errorf("%s = %v, ok=%v, want 5", AttrGenAIUsageOutputTokens, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCacheHit)); !ok || v.AsBool() != false {
		t.Errorf("%s = %v, ok=%v, want false", AttrKelvranCacheHit, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCostUSD)); !ok || v.AsString() != "0.0001" {
		t.Errorf("%s = %v, ok=%v, want %q", AttrKelvranCostUSD, v, ok, "0.0001")
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIResponseFinishReasons)); !ok || len(v.AsStringSlice()) != 1 || v.AsStringSlice()[0] != "stop" {
		t.Errorf("%s = %v, ok=%v, want [stop]", AttrGenAIResponseFinishReasons, v, ok)
	}

	if ended[0].Status().Code == codes.Error {
		t.Errorf("status = Error on a successful result, want Unset/Ok")
	}
}

func TestRecordChatCompletionResultSkipsEmptyOptionalFields(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	// VirtualKeyID empty (as on an auth failure), AgentRunID empty (as
	// when the caller never sent one), FinishReasons nil.
	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CacheHit: true,
		// "0", not "" — matches what decimal.Zero.String() actually
		// produces in real code (dataplane.go's finalize never passes a
		// truly empty string; a genuinely-zero cost still formats as "0").
		CostUSD: "0",
	})
	span.End()

	ended := sr.Ended()
	attrs := ended[0].Attributes()

	for _, key := range []string{
		AttrKelvranVirtualKeyID,
		AttrKelvranAgentRunID,
		AttrGenAIProviderName,
		AttrKelvranDeploymentName,
		AttrGenAIResponseModel,
		AttrGenAIResponseID,
		AttrGenAIResponseFinishReasons,
		AttrGenAIUsageInputTokens,
		AttrGenAIUsageOutputTokens,
	} {
		if _, ok := attrValue(t, attrs, attribute.Key(key)); ok {
			t.Errorf("attribute %q is set on a result with no value for it — must be absent, not an empty placeholder", key)
		}
	}

	// cache.hit and cost.usd are always set, even when cost is 0 — zero is
	// a real, meaningful value here, not "unknown."
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCacheHit)); !ok || v.AsBool() != true {
		t.Errorf("%s = %v, ok=%v, want true", AttrKelvranCacheHit, v, ok)
	}
	if _, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCostUSD)); !ok {
		t.Errorf("%s not set even though it's always meaningful", AttrKelvranCostUSD)
	}
}

// TestRecordChatCompletionResultEmitsAgeForL1AndL2HitsNotJustL3 proves
// docs/upgrade-research/cache-2026-09-06.md Finding 6: cache-hit age is no
// longer L3-exclusive — every hit layer reports it, only similarity stays
// L3-only.
func TestRecordChatCompletionResultEmitsAgeForL1AndL2HitsNotJustL3(t *testing.T) {
	for _, layer := range []string{"L1", "L2", "L3"} {
		t.Run(layer, func(t *testing.T) {
			sr := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
			t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
			tracer := tp.Tracer("result_test")

			_, span := tracer.Start(t.Context(), "test-span")
			RecordChatCompletionResult(span, ChatCompletionResult{
				CacheHit:        true,
				CacheLayer:      layer,
				CacheSimilarity: 0.95,
				CacheAgeMs:      1234,
				CostUSD:         "0",
			})
			span.End()

			attrs := sr.Ended()[0].Attributes()

			ageV, ok := attrValue(t, attrs, attribute.Key(AttrKelvranCacheAgeMs))
			if !ok || ageV.AsFloat64() != 1234 {
				t.Errorf("layer %q: %s = %v, ok=%v, want 1234 (age must be reported for every hit layer)", layer, AttrKelvranCacheAgeMs, ageV, ok)
			}

			simV, simOK := attrValue(t, attrs, attribute.Key(AttrKelvranCacheSimilarity))
			if layer == "L3" {
				if !simOK || simV.AsFloat64() != 0.95 {
					t.Errorf("layer %q: %s = %v, ok=%v, want 0.95", layer, AttrKelvranCacheSimilarity, simV, simOK)
				}
			} else if simOK {
				t.Errorf("layer %q: %s is set — similarity must stay L3-only", layer, AttrKelvranCacheSimilarity)
			}
		})
	}
}

func TestRecordChatCompletionResultErrorRecordsErrorAndSetsStatus(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	wantErr := errors.New("simulated upstream failure")
	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{Err: wantErr})
	span.End()

	ended := sr.Ended()
	if got := ended[0].Status().Code; got != codes.Error {
		t.Errorf("status code = %v, want codes.Error", got)
	}
	if got := ended[0].Status().Description; got != wantErr.Error() {
		t.Errorf("status description = %q, want %q", got, wantErr.Error())
	}

	events := ended[0].Events()
	var foundErrorEvent bool
	for _, e := range events {
		if e.Name == "exception" {
			foundErrorEvent = true
		}
	}
	if !foundErrorEvent {
		t.Error("no \"exception\" event recorded — RecordError did not fire")
	}
}

// TestGenAIProviderNameRemapsToWellKnownRegistryValues is the
// load-bearing proof for docs/rfcs/2026-09-05-gateway-gen-ai-provider-
// name-validation.md: Kelvran's own internal provider identifiers for
// bedrock/gemini don't match the real OTel semantic-conventions
// registry's well-known gen_ai.provider.name values, and must be
// remapped on the way out; openai/anthropic already match verbatim and
// must NOT be altered; openaicompat has no well-known value in the
// registry at all and must pass through verbatim rather than being
// forced into a wrong mapping.
func TestGenAIProviderNameRemapsToWellKnownRegistryValues(t *testing.T) {
	cases := []struct {
		internal string
		want     string
	}{
		{"openai", "openai"},
		{"anthropic", "anthropic"},
		{"bedrock", "aws.bedrock"},
		{"gemini", "gcp.gemini"},
		{"openaicompat", "openaicompat"},
	}
	for _, c := range cases {
		if got := genAIProviderName(c.internal); got != c.want {
			t.Errorf("genAIProviderName(%q) = %q, want %q", c.internal, got, c.want)
		}
	}
}

// TestRecordChatCompletionResultRemapsProviderOnSpan proves the mapping
// actually reaches the real emitted span attribute, not just the
// helper function in isolation.
func TestRecordChatCompletionResultRemapsProviderOnSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{Provider: "bedrock"})
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("len(sr.Ended()) = %d, want 1", len(ended))
	}
	v, ok := attrValue(t, ended[0].Attributes(), attribute.Key(AttrGenAIProviderName))
	if !ok {
		t.Fatal("gen_ai.provider.name attribute not set")
	}
	if v.AsString() != "aws.bedrock" {
		t.Errorf("gen_ai.provider.name = %q, want %q", v.AsString(), "aws.bedrock")
	}
}

// TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome proves
// docs/upgrade-research/cache-2026-09-06.md Finding 1's per-gate ablation
// counter dimensions correctly by (gate, outcome) — a query for one gate's
// reject count must never pick up another gate's, or the opposite outcome.
//
// This test lives here (unit-testing RecordCacheL3GateOutcome directly, in
// the package that owns cacheL3GateOutcomeCounter), not as a full
// HandleChatCompletion pipeline test in internal/gateway/dataplane — that
// package already has its own otel.SetMeterProvider-swapping test
// (TestRateLimitFailOpenIncrementsMetricCounter) sharing the SAME
// package-level global meter, and OTel Go's global meter/instrument
// delegation resolves to whichever concrete MeterProvider is active the
// FIRST time an instrument obtained from that meter is actually used —
// for the lifetime of that test binary, not per SetMeterProvider call.
// Two tests in ONE package's test binary both swapping providers for
// DIFFERENT instruments off the SAME shared meter therefore collide
// (confirmed empirically); testing this package's own Record* function in
// isolation, as every other test in this file already does, avoids that
// collision entirely rather than trying to out-order it.
func TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	prevProvider := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	defer otel.SetMeterProvider(prevProvider)

	ctx := context.Background()
	RecordCacheL3GateOutcome(ctx, CacheL3GateVolatileBypass, true)
	RecordCacheL3GateOutcome(ctx, CacheL3GateVolatileBypass, false)
	RecordCacheL3GateOutcome(ctx, CacheL3GateVolatileBypass, false)
	RecordCacheL3GateOutcome(ctx, CacheL3GateEntityMismatch, true)
	RecordCacheL3GateOutcome(ctx, CacheL3GateFreshnessRiskModel, false)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}

	counts := map[[2]string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kelvran.cache.l3.gate_outcome" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("kelvran.cache.l3.gate_outcome data type = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				gate, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Gate))
				outcome, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Outcome))
				counts[[2]string{gate.AsString(), outcome.AsString()}] += dp.Value
			}
		}
	}

	want := map[[2]string]int64{
		{CacheL3GateVolatileBypass, "reject"}:   1,
		{CacheL3GateVolatileBypass, "pass"}:     2,
		{CacheL3GateEntityMismatch, "reject"}:   1,
		{CacheL3GateFreshnessRiskModel, "pass"}: 1,
	}
	for key, wantCount := range want {
		if got := counts[key]; got != wantCount {
			t.Errorf("counts[%v] = %d, want %d", key, got, wantCount)
		}
	}
	// Nothing else must have been recorded — e.g. entity_mismatch's
	// "reject" call must never bleed into a "pass" data point too.
	if got := counts[[2]string{CacheL3GateEntityMismatch, "pass"}]; got != 0 {
		t.Errorf("entity_mismatch pass count = %d, want 0 (only reject was ever recorded)", got)
	}
	if got := counts[[2]string{CacheL3GateFreshnessRiskModel, "reject"}]; got != 0 {
		t.Errorf("freshness_risk_model reject count = %d, want 0 (only pass was ever recorded)", got)
	}
}
