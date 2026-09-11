package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

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
		AttrGenAIUsageCacheReadInputTokens,
		AttrGenAIUsageCacheCreationInputTokens,
		AttrKelvranPromptID,
		AttrKelvranPromptVersion,
		AttrKelvranResponseFormatRequestedNotEnforced,
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

// TestRecordChatCompletionResultEmitsCacheTokenAttributesOnlyWhenPositive
// closes a real backlog-audit finding: CacheReadTokens/CacheCreationTokens
// were already computed by cost accounting on every request, but never
// reached a span attribute at all until now.
func TestRecordChatCompletionResultEmitsCacheTokenAttributesOnlyWhenPositive(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CostUSD:             "0.0001",
		CacheReadTokens:     1800,
		CacheCreationTokens: 248,
	})
	span.End()

	attrs := sr.Ended()[0].Attributes()
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageCacheReadInputTokens)); !ok || v.AsInt64() != 1800 {
		t.Errorf("%s = %v, ok=%v, want 1800", AttrGenAIUsageCacheReadInputTokens, v, ok)
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrGenAIUsageCacheCreationInputTokens)); !ok || v.AsInt64() != 248 {
		t.Errorf("%s = %v, ok=%v, want 248", AttrGenAIUsageCacheCreationInputTokens, v, ok)
	}
}

// TestRecordChatCompletionResultEmitsPromptIDAndVersionOnlyWhenSet closes
// the other half of the same finding: a resolved server-side prompt
// template had zero corresponding observability signal at request time.
func TestRecordChatCompletionResultEmitsPromptIDAndVersionOnlyWhenSet(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CostUSD:       "0.0001",
		PromptID:      "support-triage",
		PromptVersion: 3,
	})
	span.End()

	attrs := sr.Ended()[0].Attributes()
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranPromptID)); !ok || v.AsString() != "support-triage" {
		t.Errorf("%s = %v, ok=%v, want %q", AttrKelvranPromptID, v, ok, "support-triage")
	}
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranPromptVersion)); !ok || v.AsInt64() != 3 {
		t.Errorf("%s = %v, ok=%v, want 3", AttrKelvranPromptVersion, v, ok)
	}
}

// TestRecordChatCompletionResultEmitsResponseFormatRequestedNotEnforcedOnlyWhenTrue
// closes the structured-output RFC's own disclosed Drawback: a request
// against an unsupported Bedrock model with ResponseFormat set previously
// had zero error/log/span signal that enforcement was silently skipped.
func TestRecordChatCompletionResultEmitsResponseFormatRequestedNotEnforcedOnlyWhenTrue(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CostUSD:                            "0.0001",
		ResponseFormatRequestedNotEnforced: true,
	})
	span.End()

	attrs := sr.Ended()[0].Attributes()
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranResponseFormatRequestedNotEnforced)); !ok || v.AsBool() != true {
		t.Errorf("%s = %v, ok=%v, want true", AttrKelvranResponseFormatRequestedNotEnforced, v, ok)
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
//
// This same constraint is why RecordChatCompletionMetrics's two new GenAI
// histograms (docs/rfcs/2026-09-07-gateway-genai-metrics.md) are verified
// INSIDE this same function's swap window below, rather than in a second,
// separate test function: this package's meter only ever delegates to a
// real MeterProvider once per test binary (confirmed empirically the same
// way — a second otel.SetMeterProvider call in a later test function in
// this package silently observes nothing, since delegation already
// resolved to this function's reader). One shared swap, multiple
// instruments verified from the one real reader, is the only correct way
// to add a second global-instrument proof to this package now that this
// function has already spent this binary's one delegation.
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

	// kelvran.cache.savings_usd, per
	// docs/rfcs/2026-09-10-gateway-cache-savings-metric.md — must be
	// verified inside THIS shared test function (see its own package doc
	// comment above): OTel Go's global meter only delegates to a real
	// MeterProvider once per test binary, so a separate new test function
	// would silently pass while verifying nothing.
	RecordCacheSavings(ctx, "L1", 0.002)
	RecordCacheSavings(ctx, "L1", 0.001)
	RecordCacheSavings(ctx, "L3", 0.0015)

	// kelvran.cache.lookup and kelvran.llm.spend_usd, per
	// docs/upgrade-research/cache-cost-observability-2026-09-11.md
	// Finding 1 -- same "must share this function's one delegation"
	// constraint as kelvran.cache.savings_usd above.
	RecordCacheLookup(ctx, "L1", true)
	RecordCacheLookup(ctx, "L3", true)
	RecordCacheLookup(ctx, "", false)
	RecordCacheLookup(ctx, "", false)
	RecordLLMSpend(ctx, 0.01)
	RecordLLMSpend(ctx, 0.02)

	// Three RecordChatCompletionMetrics scenarios, each given a unique
	// RequestModel so their attribute sets never collide into the same
	// histogram data point: a genuine billable success (both histograms
	// populated, no error.type), a non-billable cache hit/coalesced
	// follower replaying the same token counts (operation.duration still
	// recorded — every operation has a duration regardless of billing —
	// but token.usage must NOT be, per RecordChatCompletionMetrics's own
	// double-counting-avoidance doc comment), and a failure (billable is
	// always false on error in real code, tokens are always 0, but
	// operation.duration must still carry the conditional error.type
	// attribute).
	RecordChatCompletionMetrics(ctx, ChatCompletionResult{
		Provider:      "openai",
		RequestModel:  "genai-metrics-success",
		ResponseModel: "gpt-4o",
		InputTokens:   10,
		OutputTokens:  4,
		Billable:      true,
		Duration:      250 * time.Millisecond,
	})
	RecordChatCompletionMetrics(ctx, ChatCompletionResult{
		Provider:      "openai",
		RequestModel:  "genai-metrics-cache-hit",
		ResponseModel: "gpt-4o",
		InputTokens:   10,
		OutputTokens:  4,
		Billable:      false,
		Duration:      5 * time.Millisecond,
	})
	RecordChatCompletionMetrics(ctx, ChatCompletionResult{
		RequestModel: "genai-metrics-failure",
		Billable:     false,
		Duration:     2 * time.Millisecond,
		ErrorType:    "rate_limited",
	})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}

	counts := map[[2]string]int64{}
	savingsByLayer := map[string]float64{}
	// lookupCounts is keyed by (outcome, layer) -- layer is "" for every
	// miss, matching RecordCacheLookup's own "never a fabricated
	// empty-string layer on a miss" convention (it simply omits the
	// attribute, which reads back as "" via dp.Attributes.Value's own
	// not-present zero value).
	lookupCounts := map[[2]string]int64{}
	var totalSpendUSD float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "kelvran.cache.lookup":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.cache.lookup data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					outcome, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheLookupOutcome))
					layer, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheLayer))
					lookupCounts[[2]string{outcome.AsString(), layer.AsString()}] += dp.Value
				}
			case "kelvran.llm.spend_usd":
				sum, ok := m.Data.(metricdata.Sum[float64])
				if !ok {
					t.Fatalf("kelvran.llm.spend_usd data type = %T, want metricdata.Sum[float64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					totalSpendUSD += dp.Value
				}
			case "kelvran.cache.l3.gate_outcome":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.cache.l3.gate_outcome data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					gate, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Gate))
					outcome, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Outcome))
					counts[[2]string{gate.AsString(), outcome.AsString()}] += dp.Value
				}
			case "kelvran.cache.savings_usd":
				sum, ok := m.Data.(metricdata.Sum[float64])
				if !ok {
					t.Fatalf("kelvran.cache.savings_usd data type = %T, want metricdata.Sum[float64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					layer, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheLayer))
					savingsByLayer[layer.AsString()] += dp.Value
				}
			}
		}
	}

	const savingsEpsilon = 1e-9
	if got, want := savingsByLayer["L1"], 0.002+0.001; got < want-savingsEpsilon || got > want+savingsEpsilon {
		t.Errorf("kelvran.cache.savings_usd[L1] = %v, want %v (both L1 RecordCacheSavings calls summed)", got, want)
	}
	if got, want := savingsByLayer["L3"], 0.0015; got < want-savingsEpsilon || got > want+savingsEpsilon {
		t.Errorf("kelvran.cache.savings_usd[L3] = %v, want %v", got, want)
	}
	if got := savingsByLayer["L2"]; got != 0 {
		t.Errorf("kelvran.cache.savings_usd[L2] = %v, want 0 (never recorded)", got)
	}

	wantLookups := map[[2]string]int64{
		{"hit", "L1"}: 1,
		{"hit", "L3"}: 1,
		{"miss", ""}:  2,
	}
	for key, wantCount := range wantLookups {
		if got := lookupCounts[key]; got != wantCount {
			t.Errorf("lookupCounts[%v] = %d, want %d", key, got, wantCount)
		}
	}
	// A miss must never carry a layer attribute value -- confirms
	// RecordCacheLookup's own "omit the attribute, don't fabricate an
	// empty string" claim actually holds, not just that the counts add up.
	if got := lookupCounts[[2]string{"miss", "L1"}]; got != 0 {
		t.Errorf(`lookupCounts[{"miss","L1"}] = %d, want 0 (a miss never carries a layer)`, got)
	}

	const spendEpsilon = 1e-9
	if got, want := totalSpendUSD, 0.01+0.02; got < want-spendEpsilon || got > want+spendEpsilon {
		t.Errorf("kelvran.llm.spend_usd total = %v, want %v", got, want)
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

	// gen_ai.client.operation.duration: one data point per RequestModel,
	// keyed by it (each scenario above used a unique one).
	durationByModel := map[string]metricdata.HistogramDataPoint[float64]{}
	// gen_ai.client.token.usage: keyed by (RequestModel, token type) —
	// absent entirely for a (model, type) pair that must never have been
	// recorded.
	tokensByModelAndType := map[[2]string]metricdata.HistogramDataPoint[float64]{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "gen_ai.client.operation.duration":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.operation.duration data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					model, _ := dp.Attributes.Value(attribute.Key(AttrGenAIRequestModel))
					durationByModel[model.AsString()] = dp
				}
			case "gen_ai.client.token.usage":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.token.usage data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					model, _ := dp.Attributes.Value(attribute.Key(AttrGenAIRequestModel))
					tokenType, _ := dp.Attributes.Value(attribute.Key(AttrGenAITokenType))
					tokensByModelAndType[[2]string{model.AsString(), tokenType.AsString()}] = dp
				}
			}
		}
	}

	successDuration, ok := durationByModel["genai-metrics-success"]
	if !ok {
		t.Fatal("gen_ai.client.operation.duration has no data point for the success scenario")
	}
	if successDuration.Sum != (250 * time.Millisecond).Seconds() {
		t.Errorf("success scenario duration Sum = %v, want %v", successDuration.Sum, (250 * time.Millisecond).Seconds())
	}
	if _, hasErrorType := successDuration.Attributes.Value(attribute.Key(AttrErrorType)); hasErrorType {
		t.Error("success scenario's operation.duration data point has error.type set — must be absent on success")
	}

	cacheHitDuration, ok := durationByModel["genai-metrics-cache-hit"]
	if !ok {
		t.Fatal("gen_ai.client.operation.duration has no data point for the non-billable cache-hit scenario — duration must be recorded regardless of billable")
	}
	if cacheHitDuration.Sum != (5 * time.Millisecond).Seconds() {
		t.Errorf("cache-hit scenario duration Sum = %v, want %v", cacheHitDuration.Sum, (5 * time.Millisecond).Seconds())
	}

	failureDuration, ok := durationByModel["genai-metrics-failure"]
	if !ok {
		t.Fatal("gen_ai.client.operation.duration has no data point for the failure scenario")
	}
	errorType, hasErrorType := failureDuration.Attributes.Value(attribute.Key(AttrErrorType))
	if !hasErrorType || errorType.AsString() != "rate_limited" {
		t.Errorf("failure scenario's error.type = %v, hasErrorType=%v, want %q", errorType, hasErrorType, "rate_limited")
	}

	inputTokens, ok := tokensByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeInput}]
	if !ok || inputTokens.Sum != 10 {
		t.Errorf("success scenario input token usage = %v, ok=%v, want Sum=10", inputTokens, ok)
	}
	outputTokens, ok := tokensByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeOutput}]
	if !ok || outputTokens.Sum != 4 {
		t.Errorf("success scenario output token usage = %v, ok=%v, want Sum=4", outputTokens, ok)
	}

	// The load-bearing double-counting-avoidance proof: the cache-hit
	// scenario replayed the exact same InputTokens/OutputTokens as the
	// success scenario, but Billable=false — neither must ever appear in
	// gen_ai.client.token.usage.
	if _, ok := tokensByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeInput}]; ok {
		t.Error("non-billable cache-hit scenario recorded an input token.usage data point — must be suppressed")
	}
	if _, ok := tokensByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeOutput}]; ok {
		t.Error("non-billable cache-hit scenario recorded an output token.usage data point — must be suppressed")
	}
	if _, ok := tokensByModelAndType[[2]string{"genai-metrics-failure", GenAITokenTypeInput}]; ok {
		t.Error("failure scenario recorded an input token.usage data point — must be suppressed")
	}
}
