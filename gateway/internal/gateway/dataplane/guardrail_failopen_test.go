package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

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

// erroringGuardrailDetector is a guardrail.Detector whose Detect always
// fails — the test double for the fail-open/fail-closed detector-error
// path. Its category decides which: guardrail.DefaultPolicy's
// ErrorActions make prompt_injection Warn (fail-open) and credential
// Block (fail-closed).
type erroringGuardrailDetector struct{ category guardrail.Category }

func (d erroringGuardrailDetector) Name() string                 { return "erroring_" + string(d.category) }
func (d erroringGuardrailDetector) Category() guardrail.Category { return d.category }
func (d erroringGuardrailDetector) Detect(context.Context, string) ([]guardrail.Finding, error) {
	return nil, errors.New("detector backend unreachable")
}

// guardrailFailOpenTestPipeline builds a pipeline whose only guardrail
// detector is an erroringGuardrailDetector of the given category,
// registered for exactly one virtual key, with fake buffered, streaming
// and embeddings upstreams so every one of the dataplane's five
// guardrails.Check call sites can be driven through the public handlers.
func guardrailFailOpenTestPipeline(t *testing.T, keyID string, category guardrail.Category) *Pipeline {
	t.Helper()
	keys := []identity.VirtualKey{
		{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
		{Name: "emb1", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://unused", Kind: "embedding"},
	}
	p, err := NewPipeline(Config{
		Verifier: verifier,
		Limiter:  ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:   budget.NewTracker(),
		Cache:    inprocess.New(0),
		CacheL2:  inprocess.New(0),
		CacheL3:  inprocess.NewLexicalCache(0),
		Guardrails: guardrail.NewEngine(
			[]guardrail.Detector{erroringGuardrailDetector{category: category}},
			guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return fakeOpenAIResponse("gpt-4o"), nil
		},
		UpstreamStream: func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
			return nopCloserReader{strings.NewReader(realOpenAISSEStream)}, nil
		},
		EmbeddingUpstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			return &openai.EmbeddingResponseWire{
				Model: "text-embedding-3-small",
				Data:  []openai.EmbeddingDataWire{{Index: 0, Embedding: []float64{0.1, 0.2, 0.3}}},
				Usage: openai.EmbeddingUsageWire{PromptTokens: 2, TotalTokens: 2},
			}, nil
		},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// TestGuardrailFailOpenIncrementsMetricCounter is the load-bearing proof
// for kelvran.guardrail.fail_open across all five dataplane call sites:
//
//   - buffered chat: a Warn-tier detector error lets the request through
//     and increments the counter once for precall (the prompt) and once
//     for postcall (the response), attributed with key id + stage;
//   - streaming chat: same two stages, via HandleChatCompletionStream's
//     own precall site and finishStreamedResponse's audit-only postcall;
//   - embeddings: a two-input request increments the embeddings stage
//     exactly ONCE — the counter's unit is {request}, not {input};
//   - a Block-tier detector error fails closed (ErrGuardrailBlocked) and
//     must NOT increment anything — a blocked request is not "allowed
//     through with coverage missing".
//
// Shares dataplaneTelemetryMetricsReaderForTest's single global
// ManualReader and diffs before/after snapshots, per this package's
// one-delegation-per-test-binary constraint (see that helper's doc).
func TestGuardrailFailOpenIncrementsMetricCounter(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	const (
		bufferedKey   = "team-guardrail-failopen-buffered"
		streamingKey  = "team-guardrail-failopen-streaming"
		embeddingsKey = "team-guardrail-failopen-embeddings"
		failClosedKey = "team-guardrail-failclosed"
	)
	chatReq := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	buffered := guardrailFailOpenTestPipeline(t, bufferedKey, guardrail.CategoryPromptInjection)
	if _, err := buffered.HandleChatCompletion(context.Background(), "Bearer "+bufferedKey, "", "", chatReq, ""); err != nil {
		t.Fatalf("buffered HandleChatCompletion with a Warn-tier erroring detector: %v, want nil (fail-open)", err)
	}

	streaming := guardrailFailOpenTestPipeline(t, streamingKey, guardrail.CategoryPromptInjection)
	streamReq := chatReq
	streamReq.Stream = true
	if err := streaming.HandleChatCompletionStream(context.Background(), "Bearer "+streamingKey, "", "", streamReq, httptest.NewRecorder(), ""); err != nil {
		t.Fatalf("HandleChatCompletionStream with a Warn-tier erroring detector: %v, want nil (fail-open)", err)
	}

	embeddings := guardrailFailOpenTestPipeline(t, embeddingsKey, guardrail.CategoryPromptInjection)
	if _, err := embeddings.HandleEmbeddings(context.Background(), "Bearer "+embeddingsKey, "", adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"first input", "second input"},
	}); err != nil {
		t.Fatalf("HandleEmbeddings with a Warn-tier erroring detector: %v, want nil (fail-open)", err)
	}

	failClosed := guardrailFailOpenTestPipeline(t, failClosedKey, guardrail.CategoryCredential)
	if _, err := failClosed.HandleChatCompletion(context.Background(), "Bearer "+failClosedKey, "", "", chatReq, ""); !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("HandleChatCompletion with a Block-tier erroring detector: err = %v, want ErrGuardrailBlocked (fail-closed)", err)
	}
	if _, err := failClosed.HandleEmbeddings(context.Background(), "Bearer "+failClosedKey, "", adapter.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"first input"},
	}); !errors.Is(err, ErrGuardrailBlocked) {
		t.Fatalf("HandleEmbeddings with a Block-tier erroring detector: err = %v, want ErrGuardrailBlocked (fail-closed)", err)
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	delta := func(keyID, stage string) int64 {
		k := guardrailFailOpenSnapshotKey(keyID, stage)
		return afterSnap.guardrailFailOpenByKeyStage[k] - beforeSnap.guardrailFailOpenByKeyStage[k]
	}
	allStages := []string{telemetry.GuardrailStagePrecall, telemetry.GuardrailStagePostcall, telemetry.GuardrailStageEmbeddings}

	want := map[string]map[string]int64{
		bufferedKey:   {telemetry.GuardrailStagePrecall: 1, telemetry.GuardrailStagePostcall: 1, telemetry.GuardrailStageEmbeddings: 0},
		streamingKey:  {telemetry.GuardrailStagePrecall: 1, telemetry.GuardrailStagePostcall: 1, telemetry.GuardrailStageEmbeddings: 0},
		embeddingsKey: {telemetry.GuardrailStagePrecall: 0, telemetry.GuardrailStagePostcall: 0, telemetry.GuardrailStageEmbeddings: 1},
		failClosedKey: {telemetry.GuardrailStagePrecall: 0, telemetry.GuardrailStagePostcall: 0, telemetry.GuardrailStageEmbeddings: 0},
	}
	for keyID, byStage := range want {
		for _, stage := range allStages {
			if got := delta(keyID, stage); got != byStage[stage] {
				t.Errorf("kelvran.guardrail.fail_open[key=%s,stage=%s] delta = %d, want %d", keyID, stage, got, byStage[stage])
			}
		}
	}
}
