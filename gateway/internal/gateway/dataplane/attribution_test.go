package dataplane

// Tests for how the dataplane consumes telemetry.Attribution (plan item
// 13a, docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md):
// bounded fields on metrics and log lines, identifiers on the span only,
// the per-key, global and unauthenticated identifier switches, and the
// rule that attribution never influences the cache key.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

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

var identifierAttrKeys = []string{
	telemetry.AttrKelvranClaudeCodeSessionID,
	telemetry.AttrKelvranClaudeCodeAgentID,
	telemetry.AttrKelvranClaudeCodeParentAgentID,
	telemetry.AttrKelvranClaudeCodePromptID,
	telemetry.AttrKelvranClaudeCodeAgentType,
}

// attributionDims is the "(tool|class)" key the snapshot below uses for the
// two bounded dimensions.
func attributionDims(tool, class string) string { return tool + "|" + class }

// attributionMetricsSnapshot holds, per instrument name, the attribute key
// set of every data point; the spend sum keyed by (key|tool|class); and the
// gen_ai.client.operation.duration count keyed by (tool|class). Every value
// is cumulative across the test binary (one shared ManualReader), so tests
// assert the DELTA their own request produced, never an absolute.
type attributionMetricsSnapshot struct {
	spendByDims         map[string]float64
	durationCountByDims map[string]uint64
	instrumentKeys      map[string]map[string]bool
}

func snapshotAttributionMetrics(t *testing.T) attributionMetricsSnapshot {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := dataplaneTelemetryMetricsReaderForTest().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	snap := attributionMetricsSnapshot{
		spendByDims:         map[string]float64{},
		durationCountByDims: map[string]uint64{},
		instrumentKeys:      map[string]map[string]bool{},
	}
	note := func(name string, set attribute.Set) {
		if snap.instrumentKeys[name] == nil {
			snap.instrumentKeys[name] = map[string]bool{}
		}
		for _, kv := range set.ToSlice() {
			snap.instrumentKeys[name][string(kv.Key)] = true
		}
	}
	dims := func(set attribute.Set) string {
		tool, _ := set.Value(attribute.Key(telemetry.AttrKelvranClientTool))
		class, _ := set.Value(attribute.Key(telemetry.AttrKelvranClaudeCodeRequestClass))
		return attributionDims(tool.AsString(), class.AsString())
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[float64]:
				for _, dp := range data.DataPoints {
					note(m.Name, dp.Attributes)
					if m.Name == "kelvran.llm.spend_usd" {
						key, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranVirtualKeyID))
						snap.spendByDims[key.AsString()+"|"+dims(dp.Attributes)] += dp.Value
					}
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					note(m.Name, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					note(m.Name, dp.Attributes)
					if m.Name == "gen_ai.client.operation.duration" {
						snap.durationCountByDims[dims(dp.Attributes)] += dp.Count
					}
				}
			}
		}
	}
	return snap
}

func assertNoIdentifierOnAnyInstrument(t *testing.T, snap attributionMetricsSnapshot) {
	t.Helper()
	for name, keys := range snap.instrumentKeys {
		for _, id := range identifierAttrKeys {
			if keys[id] {
				t.Errorf("identifier attribute %s found on instrument %s; identifiers are span-only", id, name)
			}
		}
	}
}

func attributedCtx(a telemetry.Attribution) context.Context {
	return telemetry.WithAttribution(context.Background(), a)
}

func fullAttribution() telemetry.Attribution {
	return telemetry.Attribution{
		ClientTool:         telemetry.ClientToolOpenAIPython,
		SessionID:          "sess-attr-1",
		AgentID:            "agent-attr-1",
		ParentAgentID:      "parent-attr-1",
		ClaudeCodePromptID: "prompt-attr-1",
		RequestClass:       "main",
		AgentType:          "Explore",
		Compaction:         "auto",
		PrevToolDurations:  "Bash=10;Read=5",
		PrevToolCount:      2,
		PrevToolTotalMS:    15,
	}
}

func spanAttrString(t *testing.T, attrs []attribute.KeyValue, key string) (string, bool) {
	t.Helper()
	v, ok := spanAttr(t, attrs, key)
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

// chatDeployment is the one gpt-4o deployment every chat test here routes to.
func chatDeployment() []Deployment {
	return []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
}

// gpt4oPriceTable prices gpt-4o so finalize records a positive
// kelvran.llm.spend_usd value (the shared newTestPipeline* helpers use an
// empty price table, under which spend is always 0).
func gpt4oPriceTable() costaccounting.PriceTable {
	return costaccounting.PriceTable{
		"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
	}
}

func TestFinalizeRecordsAttributionOnSpanMetricsAndLogLine(t *testing.T) {
	before := snapshotAttributionMetrics(t)
	spansBefore := len(spanRecorder.Ended())
	var logBuf bytes.Buffer
	p := newAttributionTestPipeline(t, attributionPipelineOptions{
		deployments: chatDeployment(),
		priceTable:  gpt4oPriceTable(),
		upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})

	if _, err := p.HandleChatCompletion(attributedCtx(fullAttribution()), "Bearer test-key", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	span := chatSpanSince(t, spansBefore)
	for key, want := range map[string]string{
		telemetry.AttrKelvranClientTool:                  telemetry.ClientToolOpenAIPython,
		telemetry.AttrKelvranClaudeCodeSessionID:         "sess-attr-1",
		telemetry.AttrKelvranClaudeCodeAgentID:           "agent-attr-1",
		telemetry.AttrKelvranClaudeCodeParentAgentID:     "parent-attr-1",
		telemetry.AttrKelvranClaudeCodePromptID:          "prompt-attr-1",
		telemetry.AttrKelvranClaudeCodeRequestClass:      "main",
		telemetry.AttrKelvranClaudeCodeAgentType:         "Explore",
		telemetry.AttrKelvranClaudeCodeCompaction:        "auto",
		telemetry.AttrKelvranClaudeCodePrevToolDurations: "Bash=10;Read=5",
	} {
		if got, ok := spanAttrString(t, span.Attributes(), key); !ok || got != want {
			t.Errorf("span %s = %q (present=%v), want %q", key, got, ok, want)
		}
	}
	if v, ok := spanAttr(t, span.Attributes(), telemetry.AttrKelvranClaudeCodePrevToolTotalMS); !ok || v.AsInt64() != 15 {
		t.Errorf("span prev_tool_total_ms = %v (present=%v), want 15", v, ok)
	}

	after := snapshotAttributionMetrics(t)
	// Deltas, not presence: sibling tests record under the same dims and
	// the reader is cumulative across -count reruns, so only the delta this
	// request produced proves finalize threaded THIS request's attribution
	// into the spend counter and the duration histogram.
	dims := attributionDims(telemetry.ClientToolOpenAIPython, "main")
	if got := after.spendByDims["test-key|"+dims] - before.spendByDims["test-key|"+dims]; got <= 0 {
		t.Errorf("kelvran.llm.spend_usd{test-key|%s} delta = %v, want > 0 (priced gpt-4o, 5 prompt + 3 completion tokens)", dims, got)
	}
	if got := after.durationCountByDims[dims] - before.durationCountByDims[dims]; got != 1 {
		t.Errorf("gen_ai.client.operation.duration{%s} count delta = %d, want 1", dims, got)
	}
	// The token instruments must NOT carry the attribution dimensions: the
	// RFC confines them to the duration histogram's own attribute slice and
	// the spend counter, so a future `attrs = append(attrs, ...)` regression
	// would multiply every token series by the tool×class product.
	for _, name := range []string{"gen_ai.client.inference.usage.input_tokens", "gen_ai.client.inference.usage.output_tokens"} {
		keys := after.instrumentKeys[name]
		if len(keys) == 0 {
			t.Errorf("%s has no data points; the priced, billable request should have recorded tokens", name)
		}
		for _, k := range []string{telemetry.AttrKelvranClientTool, telemetry.AttrKelvranClaudeCodeRequestClass} {
			if keys[k] {
				t.Errorf("%s carries %s; attribution dimensions belong on the duration histogram and spend counter only", name, k)
			}
		}
	}
	assertNoIdentifierOnAnyInstrument(t, after)

	logs := logBuf.String()
	if !strings.Contains(logs, `"client_tool":"openai_python"`) || !strings.Contains(logs, `"request_class":"main"`) {
		t.Errorf("chat_completion line must carry client_tool and request_class: %s", logs)
	}
	for _, id := range []string{"sess-attr-1", "agent-attr-1", "parent-attr-1", "prompt-attr-1", "Explore"} {
		if strings.Contains(logs, id) {
			t.Errorf("identifier %q leaked into a log line: %s", id, logs)
		}
	}
}

func TestPerKeyAttributionIDsDisabledBlanksIdentifiersKeepsBounded(t *testing.T) {
	spansBefore := len(spanRecorder.Ended())
	keys := defaultTestVirtualKeys()
	for i := range keys {
		if keys[i].ID == "test-key" {
			keys[i].AttributionIDsDisabled = true
		}
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, chatDeployment(), keys)

	if _, err := p.HandleChatCompletion(attributedCtx(fullAttribution()), "Bearer test-key", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	span := chatSpanSince(t, spansBefore)
	for _, id := range identifierAttrKeys {
		if _, ok := spanAttr(t, span.Attributes(), id); ok {
			t.Errorf("per-key identifier capture off: %s must be absent from the span", id)
		}
	}
	if got, ok := spanAttrString(t, span.Attributes(), telemetry.AttrKelvranClientTool); !ok || got != telemetry.ClientToolOpenAIPython {
		t.Errorf("bounded client.tool must survive the per-key switch; got %q present=%v", got, ok)
	}
	if got, ok := spanAttrString(t, span.Attributes(), telemetry.AttrKelvranClaudeCodeRequestClass); !ok || got != "main" {
		t.Errorf("bounded request_class must survive the per-key switch; got %q present=%v", got, ok)
	}
}

func TestPipelineAttributionIDsDisabledGloballyBlanksIdentifiers(t *testing.T) {
	spansBefore := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, chatDeployment())
	p.attributionIDsDisabled = true

	if _, err := p.HandleChatCompletion(attributedCtx(fullAttribution()), "Bearer test-key", "", "", chatRequest("gpt-4o"), ""); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	span := chatSpanSince(t, spansBefore)
	for _, id := range identifierAttrKeys {
		if _, ok := spanAttr(t, span.Attributes(), id); ok {
			t.Errorf("global identifier capture off: %s must be absent from the span", id)
		}
	}
	if got, ok := spanAttrString(t, span.Attributes(), telemetry.AttrKelvranClientTool); !ok || got != telemetry.ClientToolOpenAIPython {
		t.Errorf("bounded client.tool must survive the global switch; got %q present=%v", got, ok)
	}
}

// TestUnauthenticatedRequestNeverCarriesIdentifiers: finalize records the
// chat span even when Verify fails (vk == nil), and an unauthenticated
// request has no attribution owner — a client of an opted-out key that
// presents a revoked or mistyped token must not have its session, agent and
// prompt ids recorded just because the per-key switch could not be looked
// up. Identifiers are therefore blanked whenever vk is nil, regardless of
// the global switch; the bounded fields still land.
func TestUnauthenticatedRequestNeverCarriesIdentifiers(t *testing.T) {
	spansBefore := len(spanRecorder.Ended())
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream must not be called for an unauthenticated request")
		return nil, nil
	}, chatDeployment())

	_, err := p.HandleChatCompletion(attributedCtx(fullAttribution()), "Bearer wrong-key", "", "", chatRequest("gpt-4o"), "")
	if err == nil {
		t.Fatal("HandleChatCompletion with an unknown bearer token must fail")
	}
	span := chatSpanSince(t, spansBefore)
	for _, id := range identifierAttrKeys {
		if v, ok := spanAttr(t, span.Attributes(), id); ok {
			t.Errorf("unauthenticated request: %s = %q on the span; identifiers need an authenticated owner", id, v.AsString())
		}
	}
	if got, ok := spanAttrString(t, span.Attributes(), telemetry.AttrKelvranClientTool); !ok || got != telemetry.ClientToolOpenAIPython {
		t.Errorf("bounded client.tool must still be recorded for a rejected request; got %q present=%v", got, ok)
	}
}

func TestAttributionNeverChangesTheCacheKey(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, chatDeployment())
	req := chatRequest("gpt-4o")
	first := fullAttribution()
	second := telemetry.Attribution{ClientTool: telemetry.ClientToolCurl, SessionID: "a-different-session", RequestClass: "subagent"}
	if _, err := p.HandleChatCompletion(attributedCtx(first), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := p.HandleChatCompletion(attributedCtx(second), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("second: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1: two requests that differ only in attribution must share one cache entry", upstreamCalls)
	}
}

func TestEmbeddingsSpendAndLogCarryBoundedAttribution(t *testing.T) {
	before := snapshotAttributionMetrics(t)
	var logBuf bytes.Buffer
	p := newAttributionTestPipeline(t, attributionPipelineOptions{
		deployments: []Deployment{{Name: "e1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"}},
		priceTable: costaccounting.PriceTable{
			"text-embedding-3-small": {PromptPerToken: decimal.RequireFromString("0.00000002")},
		},
		embeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2, 0.3}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 2, TotalTokens: 2},
			}, nil
		},
		logger: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	})

	ctx := attributedCtx(telemetry.Attribution{ClientTool: telemetry.ClientToolCurl, SessionID: "sess-embed", RequestClass: "main"})
	if _, err := p.HandleEmbeddings(ctx, "Bearer test-key", "", adapter.EmbeddingRequest{Model: "text-embedding-3-small", Input: []string{"hello"}}); err != nil {
		t.Fatalf("HandleEmbeddings: %v", err)
	}
	after := snapshotAttributionMetrics(t)
	key := "test-key|" + attributionDims(telemetry.ClientToolCurl, "main")
	if got := after.spendByDims[key] - before.spendByDims[key]; got <= 0 {
		t.Errorf("embeddings kelvran.llm.spend_usd{%s} delta = %v, want > 0", key, got)
	}
	assertNoIdentifierOnAnyInstrument(t, after)
	logs := logBuf.String()
	if !strings.Contains(logs, `"client_tool":"curl"`) || !strings.Contains(logs, `"request_class":"main"`) {
		t.Errorf("embeddings line must carry client_tool and request_class: %s", logs)
	}
	if strings.Contains(logs, "sess-embed") {
		t.Errorf("identifier leaked into the embeddings log line: %s", logs)
	}
}

func TestVirtualKeyPayloadRoundTripPreservesAttributionIDsDisabled(t *testing.T) {
	original := identity.VirtualKey{ID: "rt-attr", KeyHash: "hash", AttributionIDsDisabled: true}
	if got := payloadToVirtualKey(virtualKeyToPayload(original)); !got.AttributionIDsDisabled {
		t.Fatal("AttributionIDsDisabled lost in the configpropagation payload round trip")
	}
}

// attributionPipelineOptions configures newAttributionTestPipeline. Unset
// fields take these defaults: one budgeted "test-key", an empty price
// table, a chat upstream that fails the test if called, a discard logger.
type attributionPipelineOptions struct {
	deployments       []Deployment
	keys              []identity.VirtualKey
	priceTable        costaccounting.PriceTable
	upstream          UpstreamCaller
	embeddingUpstream UpstreamCaller
	logger            *slog.Logger
}

// newAttributionTestPipeline mirrors newTestPipelineWithKeysAndLogger
// (gatewayevents_test.go) and newEmbeddingTestPipeline (embeddings_test.go)
// with a caller-supplied price table, logger and embedding upstream, so a
// spend DELTA and a log line can both be asserted for chat and embeddings.
func newAttributionTestPipeline(t *testing.T, o attributionPipelineOptions) *Pipeline {
	t.Helper()
	keys := o.keys
	if keys == nil {
		keys = []identity.VirtualKey{
			{ID: "test-key", KeyHash: testHashOf("test-key"), RateLimitBurst: 100, RateLimitRefill: 100, BudgetUSD: decimal.RequireFromString("1000")},
		}
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	upstream := o.upstream
	if upstream == nil {
		upstream = func(ctx context.Context, dep Deployment, req any) (any, error) {
			t.Fatal("chat Upstream should not be called by this test")
			return nil, nil
		}
	}
	logger := o.logger
	if logger == nil {
		logger = discardLogger()
	}
	p, err := NewPipeline(Config{
		Verifier:          verifier,
		Limiter:           ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:            budget.NewTracker(),
		Cache:             inprocess.New(0),
		CacheL2:           inprocess.New(0),
		CacheL3:           inprocess.NewLexicalCache(0),
		Guardrails:        guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:          adapter.Registry{"openai": openai.New()},
		Router:            testRouter(o.deployments),
		Deployments:       o.deployments,
		CostCalculator:    costaccounting.NewCalculator(o.priceTable),
		Upstream:          upstream,
		EmbeddingUpstream: o.embeddingUpstream,
		Logger:            logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}
