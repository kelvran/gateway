package telemetry

import (
	"context"
	"errors"
	"sync"
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
		AttrKelvranSavingsUSD,
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

// TestRecordChatCompletionResultEmitsSavingsUSDOnlyOnCacheHit proves
// AttrKelvranSavingsUSD is set to the real notional value on a genuine
// cache hit, closing a real gap this attribute didn't exist to close
// before: kelvran.cache.savings_usd (an aggregate Prometheus counter,
// dimensioned by cache layer only) has no per-agent-run breakdown, and
// GatewayDecisionEvent.SavingsUsd (a durable log/proto field) carries
// no cardinality-safe span-level signal either -- this is the one place
// a Grafana panel querying Tempo by kelvran.agent_run_id could actually
// break savings down per agent run. Per
// docs/upgrade-research/competitor-feature-parity-2026-09-14.md Finding 2.
func TestRecordChatCompletionResultEmitsSavingsUSDOnlyOnCacheHit(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	tracer := tp.Tracer("result_test")

	_, span := tracer.Start(t.Context(), "test-span")
	RecordChatCompletionResult(span, ChatCompletionResult{
		CacheHit:   true,
		CostUSD:    "0.0008",
		SavingsUSD: "0.0008",
	})
	span.End()

	attrs := sr.Ended()[0].Attributes()
	if v, ok := attrValue(t, attrs, attribute.Key(AttrKelvranSavingsUSD)); !ok || v.AsString() != "0.0008" {
		t.Errorf("%s = %v, ok=%v, want %q", AttrKelvranSavingsUSD, v, ok, "0.0008")
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

var (
	cacheL3TelemetryMetricsOnce   sync.Once
	cacheL3TelemetryMetricsReader *sdkmetric.ManualReader
)

// cacheL3TelemetryMetricsReaderForTest returns the ManualReader backing
// this test binary's real global-meter-delegated instruments.
//
// Deliberately a package-level singleton, not a fresh reader per call: the
// go.opentelemetry.io/otel global MeterProvider delegate is bound via a
// process-lifetime sync.Once (internal/global/state.go's own
// delegateMeterOnce, confirmed by reading that source directly) — the
// FIRST otel.SetMeterProvider call in this test binary's entire process
// permanently wires every package-level instrument obtained from the
// global default delegating meter (telemetry.go's counters/histograms
// among them) to that call's MeterProvider, and every later
// otel.SetMeterProvider call in the SAME process is a no-op for those
// already-delegated instruments. Under `go test -count>1` — which reruns
// this test function's body in the SAME process N times, since package
// vars (including this one) are never reset between reruns — the original
// "create a brand-new reader and SetMeterProvider every run" form only
// ever observed real data on the first run: every later run's fresh reader
// received zero recordings, because the instruments were already
// permanently delegated elsewhere. Calling SetMeterProvider exactly once
// across every rerun, and asserting the DELTA a run's own Record* calls
// produced (see snapshotCacheL3Telemetry below) rather than an absolute
// value, is what actually holds regardless of how many times this process
// replays the test.
func cacheL3TelemetryMetricsReaderForTest() *sdkmetric.ManualReader {
	cacheL3TelemetryMetricsOnce.Do(func() {
		cacheL3TelemetryMetricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(cacheL3TelemetryMetricsReader)))
	})
	return cacheL3TelemetryMetricsReader
}

// cacheL3TelemetrySnapshot holds every value
// TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome asserts against,
// extracted from one metricdata.ResourceMetrics collection. Two snapshots
// (before/after the test's own Record* calls) are diffed rather than
// either being read in isolation — see
// cacheL3TelemetryMetricsReaderForTest's doc comment for why an absolute
// reading is not safe to assert against.
type cacheL3TelemetrySnapshot struct {
	gateOutcomeCounts                      map[[2]string]int64
	savingsByLayer                         map[string]float64
	lookupCounts                           map[[2]string]int64
	totalSpendUSD                          float64
	persistenceFailedByStoreKind           map[string]int64
	configPropagationSubscribeStoppedTotal int64
	durationSumByModel                     map[string]float64
	// durationErrorTypeByModel is intentionally NOT diffed by its caller —
	// whether error.type is set on a given model's data point is a static
	// property of that attribute set, not a cumulative value, so reading
	// it from the "after" snapshot alone is correct.
	durationErrorTypeByModel map[string]attrPresence
	tokenSumByModelAndType   map[[2]string]float64
}

type attrPresence struct {
	val string
	ok  bool
}

func snapshotCacheL3Telemetry(t *testing.T, rm metricdata.ResourceMetrics) cacheL3TelemetrySnapshot {
	t.Helper()
	snap := cacheL3TelemetrySnapshot{
		gateOutcomeCounts:            map[[2]string]int64{},
		savingsByLayer:               map[string]float64{},
		lookupCounts:                 map[[2]string]int64{},
		persistenceFailedByStoreKind: map[string]int64{},
		durationSumByModel:           map[string]float64{},
		durationErrorTypeByModel:     map[string]attrPresence{},
		tokenSumByModelAndType:       map[[2]string]float64{},
	}
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
					snap.lookupCounts[[2]string{outcome.AsString(), layer.AsString()}] += dp.Value
				}
			case "kelvran.llm.spend_usd":
				sum, ok := m.Data.(metricdata.Sum[float64])
				if !ok {
					t.Fatalf("kelvran.llm.spend_usd data type = %T, want metricdata.Sum[float64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					snap.totalSpendUSD += dp.Value
				}
			case "kelvran.persistence.failed":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.persistence.failed data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					storeKind, _ := dp.Attributes.Value(attribute.Key(AttrKelvranPersistenceStoreKind))
					snap.persistenceFailedByStoreKind[storeKind.AsString()] += dp.Value
				}
			case "kelvran.configpropagation.subscribe_stopped":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.configpropagation.subscribe_stopped data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					snap.configPropagationSubscribeStoppedTotal += dp.Value
				}
			case "kelvran.cache.l3.gate_outcome":
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("kelvran.cache.l3.gate_outcome data type = %T, want metricdata.Sum[int64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					gate, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Gate))
					outcome, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheL3Outcome))
					snap.gateOutcomeCounts[[2]string{gate.AsString(), outcome.AsString()}] += dp.Value
				}
			case "kelvran.cache.savings_usd":
				sum, ok := m.Data.(metricdata.Sum[float64])
				if !ok {
					t.Fatalf("kelvran.cache.savings_usd data type = %T, want metricdata.Sum[float64]", m.Data)
				}
				for _, dp := range sum.DataPoints {
					layer, _ := dp.Attributes.Value(attribute.Key(AttrKelvranCacheLayer))
					snap.savingsByLayer[layer.AsString()] += dp.Value
				}
			case "gen_ai.client.operation.duration":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.operation.duration data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					model, _ := dp.Attributes.Value(attribute.Key(AttrGenAIRequestModel))
					snap.durationSumByModel[model.AsString()] += dp.Sum
					errorType, hasErrorType := dp.Attributes.Value(attribute.Key(AttrErrorType))
					snap.durationErrorTypeByModel[model.AsString()] = attrPresence{errorType.AsString(), hasErrorType}
				}
			case "gen_ai.client.token.usage":
				hist, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("gen_ai.client.token.usage data type = %T, want metricdata.Histogram[float64]", m.Data)
				}
				for _, dp := range hist.DataPoints {
					model, _ := dp.Attributes.Value(attribute.Key(AttrGenAIRequestModel))
					tokenType, _ := dp.Attributes.Value(attribute.Key(AttrGenAITokenType))
					snap.tokenSumByModelAndType[[2]string{model.AsString(), tokenType.AsString()}] += dp.Sum
				}
			}
		}
	}
	return snap
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
//
// Reads a before/after snapshot pair rather than one absolute reading —
// see cacheL3TelemetryMetricsReaderForTest's own doc comment for why: this
// same delegation constraint means a naive absolute reading only ever
// passes on a test binary's first `go test -count` iteration.
func TestRecordCacheL3GateOutcomeIncrementsPerGateAndOutcome(t *testing.T) {
	reader := cacheL3TelemetryMetricsReaderForTest()
	ctx := context.Background()

	var before metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotCacheL3Telemetry(t, before)

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

	// kelvran.persistence.failed, per
	// docs/upgrade-research/admin-operator-experience-2026-09-14.md —
	// same "must share this function's one delegation" constraint.
	RecordPersistenceFailed(ctx, "budget", "team-budget-persist-failure")
	RecordPersistenceFailed(ctx, "identity", "team-identity-persist-failure")
	RecordPersistenceFailed(ctx, "identity", "team-identity-persist-failure")

	// kelvran.configpropagation.subscribe_stopped -- same "must share
	// this function's one delegation" constraint.
	RecordConfigPropagationSubscribeStopped(ctx)
	RecordConfigPropagationSubscribeStopped(ctx)

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
	afterSnap := snapshotCacheL3Telemetry(t, rm)

	const savingsEpsilon = 1e-9
	if got, want := afterSnap.savingsByLayer["L1"]-beforeSnap.savingsByLayer["L1"], 0.002+0.001; got < want-savingsEpsilon || got > want+savingsEpsilon {
		t.Errorf("kelvran.cache.savings_usd[L1] delta = %v, want %v (both L1 RecordCacheSavings calls summed)", got, want)
	}
	if got, want := afterSnap.savingsByLayer["L3"]-beforeSnap.savingsByLayer["L3"], 0.0015; got < want-savingsEpsilon || got > want+savingsEpsilon {
		t.Errorf("kelvran.cache.savings_usd[L3] delta = %v, want %v", got, want)
	}
	if got := afterSnap.savingsByLayer["L2"] - beforeSnap.savingsByLayer["L2"]; got != 0 {
		t.Errorf("kelvran.cache.savings_usd[L2] delta = %v, want 0 (never recorded)", got)
	}

	wantLookups := map[[2]string]int64{
		{"hit", "L1"}: 1,
		{"hit", "L3"}: 1,
		{"miss", ""}:  2,
	}
	for key, wantCount := range wantLookups {
		if got := afterSnap.lookupCounts[key] - beforeSnap.lookupCounts[key]; got != wantCount {
			t.Errorf("lookupCounts[%v] delta = %d, want %d", key, got, wantCount)
		}
	}
	// A miss must never carry a layer attribute value -- confirms
	// RecordCacheLookup's own "omit the attribute, don't fabricate an
	// empty string" claim actually holds, not just that the counts add up.
	if got := afterSnap.lookupCounts[[2]string{"miss", "L1"}] - beforeSnap.lookupCounts[[2]string{"miss", "L1"}]; got != 0 {
		t.Errorf(`lookupCounts[{"miss","L1"}] delta = %d, want 0 (a miss never carries a layer)`, got)
	}

	const spendEpsilon = 1e-9
	if got, want := afterSnap.totalSpendUSD-beforeSnap.totalSpendUSD, 0.01+0.02; got < want-spendEpsilon || got > want+spendEpsilon {
		t.Errorf("kelvran.llm.spend_usd total delta = %v, want %v", got, want)
	}

	want := map[[2]string]int64{
		{CacheL3GateVolatileBypass, "reject"}:   1,
		{CacheL3GateVolatileBypass, "pass"}:     2,
		{CacheL3GateEntityMismatch, "reject"}:   1,
		{CacheL3GateFreshnessRiskModel, "pass"}: 1,
	}
	for key, wantCount := range want {
		if got := afterSnap.gateOutcomeCounts[key] - beforeSnap.gateOutcomeCounts[key]; got != wantCount {
			t.Errorf("gateOutcomeCounts[%v] delta = %d, want %d", key, got, wantCount)
		}
	}
	// Nothing else must have been recorded — e.g. entity_mismatch's
	// "reject" call must never bleed into a "pass" data point too.
	if got := afterSnap.gateOutcomeCounts[[2]string{CacheL3GateEntityMismatch, "pass"}] - beforeSnap.gateOutcomeCounts[[2]string{CacheL3GateEntityMismatch, "pass"}]; got != 0 {
		t.Errorf("entity_mismatch pass count delta = %d, want 0 (only reject was ever recorded)", got)
	}
	if got := afterSnap.gateOutcomeCounts[[2]string{CacheL3GateFreshnessRiskModel, "reject"}] - beforeSnap.gateOutcomeCounts[[2]string{CacheL3GateFreshnessRiskModel, "reject"}]; got != 0 {
		t.Errorf("freshness_risk_model reject count delta = %d, want 0 (only pass was ever recorded)", got)
	}

	const durationEpsilon = 1e-9
	successSum := afterSnap.durationSumByModel["genai-metrics-success"] - beforeSnap.durationSumByModel["genai-metrics-success"]
	if want := (250 * time.Millisecond).Seconds(); successSum < want-durationEpsilon || successSum > want+durationEpsilon {
		t.Errorf("success scenario duration Sum delta = %v, want %v", successSum, want)
	}
	if et := afterSnap.durationErrorTypeByModel["genai-metrics-success"]; et.ok {
		t.Error("success scenario's operation.duration data point has error.type set — must be absent on success")
	}

	cacheHitSum := afterSnap.durationSumByModel["genai-metrics-cache-hit"] - beforeSnap.durationSumByModel["genai-metrics-cache-hit"]
	if want := (5 * time.Millisecond).Seconds(); cacheHitSum < want-durationEpsilon || cacheHitSum > want+durationEpsilon {
		t.Errorf("cache-hit scenario duration Sum delta = %v, want %v (duration must be recorded regardless of billable)", cacheHitSum, want)
	}

	failureSum := afterSnap.durationSumByModel["genai-metrics-failure"] - beforeSnap.durationSumByModel["genai-metrics-failure"]
	if want := (2 * time.Millisecond).Seconds(); failureSum < want-durationEpsilon || failureSum > want+durationEpsilon {
		t.Errorf("failure scenario duration Sum delta = %v, want %v", failureSum, want)
	}
	if et := afterSnap.durationErrorTypeByModel["genai-metrics-failure"]; !et.ok || et.val != "rate_limited" {
		t.Errorf("failure scenario's error.type = %v, hasErrorType=%v, want %q", et.val, et.ok, "rate_limited")
	}

	if got := afterSnap.tokenSumByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeInput}] - beforeSnap.tokenSumByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeInput}]; got != 10 {
		t.Errorf("success scenario input token usage delta = %v, want 10", got)
	}
	if got := afterSnap.tokenSumByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeOutput}] - beforeSnap.tokenSumByModelAndType[[2]string{"genai-metrics-success", GenAITokenTypeOutput}]; got != 4 {
		t.Errorf("success scenario output token usage delta = %v, want 4", got)
	}

	// The load-bearing double-counting-avoidance proof: the cache-hit
	// scenario replayed the exact same InputTokens/OutputTokens as the
	// success scenario, but Billable=false — neither must ever contribute
	// a delta to gen_ai.client.token.usage.
	if got := afterSnap.tokenSumByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeInput}] - beforeSnap.tokenSumByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeInput}]; got != 0 {
		t.Errorf("non-billable cache-hit scenario recorded an input token.usage delta of %v — must be suppressed", got)
	}
	if got := afterSnap.tokenSumByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeOutput}] - beforeSnap.tokenSumByModelAndType[[2]string{"genai-metrics-cache-hit", GenAITokenTypeOutput}]; got != 0 {
		t.Errorf("non-billable cache-hit scenario recorded an output token.usage delta of %v — must be suppressed", got)
	}
	if got := afterSnap.tokenSumByModelAndType[[2]string{"genai-metrics-failure", GenAITokenTypeInput}] - beforeSnap.tokenSumByModelAndType[[2]string{"genai-metrics-failure", GenAITokenTypeInput}]; got != 0 {
		t.Errorf("failure scenario recorded an input token.usage delta of %v — must be suppressed", got)
	}

	if got := afterSnap.persistenceFailedByStoreKind["budget"] - beforeSnap.persistenceFailedByStoreKind["budget"]; got != 1 {
		t.Errorf("kelvran.persistence.failed[store_kind=budget] delta = %d, want 1", got)
	}
	if got := afterSnap.persistenceFailedByStoreKind["identity"] - beforeSnap.persistenceFailedByStoreKind["identity"]; got != 2 {
		t.Errorf("kelvran.persistence.failed[store_kind=identity] delta = %d, want 2", got)
	}

	if got := afterSnap.configPropagationSubscribeStoppedTotal - beforeSnap.configPropagationSubscribeStoppedTotal; got != 2 {
		t.Errorf("kelvran.configpropagation.subscribe_stopped delta = %d, want 2", got)
	}
}
