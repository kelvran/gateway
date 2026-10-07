package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestHandleChatCompletionRecordsReasoningTokens is the end-to-end proof
// for reasoning-token tracking through the real pipeline: an upstream
// response carrying usage.completion_tokens_details.reasoning_tokens is
// decoded by the real OpenAI adapter into adapter.Usage.ReasoningTokens,
// threaded by finalize into telemetry.ChatCompletionResult, and lands as
// a gen_ai.client.inference.usage.reasoning.output_tokens data point
// attributed to the request model. Shares this package's single global
// ManualReader (dataplaneTelemetryMetricsReaderForTest) and diffs
// before/after snapshots, per its one-delegation-per-test-binary rule.
func TestHandleChatCompletionRecordsReasoningTokens(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	const model = "reasoning-metrics-model"
	const keyID = "team-reasoning-metrics"
	keys := []identity.VirtualKey{{ID: keyID, KeyHash: testHashOf(keyID), RateLimitBurst: 100, RateLimitRefill: 100}}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: model, Provider: "openai", UpstreamModel: "o4-mini", BaseURL: "http://unused"}}
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
			return &openai.Response{
				ID:    "chatcmpl-reasoning",
				Model: "o4-mini",
				Choices: []openai.Choice{
					{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello"`)}, FinishReason: "stop"},
				},
				Usage: openai.Usage{
					PromptTokens: 10, CompletionTokens: 30, TotalTokens: 40,
					CompletionTokensDetails: &openai.CompletionTokensDetails{ReasoningTokens: 21},
				},
			}, nil
		},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	resp, err := p.HandleChatCompletion(context.Background(), "Bearer "+keyID, "", "", adapter.ChatRequest{Model: model, Messages: []adapter.Message{{Role: "user", Content: "think hard"}}}, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if resp.Usage.ReasoningTokens != 21 || resp.Usage.CompletionTokens != 30 {
		t.Errorf("resp.Usage = %+v, want ReasoningTokens 21 as a subset of CompletionTokens 30", resp.Usage)
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.reasoningTokensByModel[model] - beforeSnap.reasoningTokensByModel[model]; got != 21 {
		t.Errorf("gen_ai.client.inference.usage.reasoning.output_tokens[model=%s] delta = %d, want 21", model, got)
	}
}

// TestHandleChatCompletionStreamBedrockRecordsReasoningTokens is the Bedrock
// streaming twin of the OpenAI buffered proof above, through the REAL
// streaming handler and the real adapter: a wire-accurate ConverseStream
// sequence whose messageStop carries additionalModelResponseFields (the
// document Converse builds for the /usage/output_tokens_details path
// ToProvider requests on Anthropic models) followed by the metadata event
// with TokenUsage. The decoder must stash the thinking count at messageStop
// and fold it into the usage built at metadata; finalize must thread it to
// the counter; and the cached response must carry it, which the second
// (cache-hit) request's fake-streamed body makes observable -- the same
// technique TestHandleChatCompletionStreamBedrockFullSequenceDecodesCorrectly
// uses for total_tokens.
func TestHandleChatCompletionStreamBedrockRecordsReasoningTokens(t *testing.T) {
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	const model = "bedrock-reasoning-stream-model"
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		bedrockWireEvent("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Hel"}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo!"}}`),
		bedrockWireEvent("contentBlockStop", `{"contentBlockIndex":0}`),
		bedrockWireEvent("messageStop", `{"stopReason":"end_turn","additionalModelResponseFields":{"usage":{"output_tokens_details":{"thinking_tokens":17}}}}`),
		bedrockWireEvent("metadata", `{"usage":{"inputTokens":9,"outputTokens":40,"totalTokens":49}}`),
	})
	upstreamCalls := 0
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return io.NopCloser(bytes.NewReader(wire)), nil
	}, []Deployment{{Name: "d1", Model: model, Provider: "bedrock", UpstreamModel: "anthropic.claude-sonnet-5", BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})

	req := adapter.ChatRequest{Model: model, Stream: true, Messages: []adapter.Message{{Role: "user", Content: "think about it"}}}
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, httptest.NewRecorder(), ""); err != nil {
		t.Fatalf("HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1", upstreamCalls)
	}

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)
	if got := afterSnap.reasoningTokensByModel[model] - beforeSnap.reasoningTokensByModel[model]; got != 17 {
		t.Errorf("gen_ai.client.inference.usage.reasoning.output_tokens[model=%s] delta = %d, want 17 (stashed from messageStop, folded in at metadata, threaded by finalize)", model, got)
	}

	// The cache-hit replay serialises the cached ChatResponse's usage into
	// the fake stream, so the subset must be visible there alongside the
	// unchanged totals -- and the upstream must not be called again.
	rec2 := httptest.NewRecorder()
	if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec2, ""); err != nil {
		t.Fatalf("second (cache-hit) HandleChatCompletionStream: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after cache-hit call = %d, want still 1", upstreamCalls)
	}
	body2 := rec2.Body.String()
	if !strings.Contains(body2, `"reasoning_tokens":17`) || !strings.Contains(body2, `"completion_tokens":40`) || !strings.Contains(body2, `"total_tokens":49`) {
		t.Errorf("cache-hit body missing reasoning_tokens:17 / completion_tokens:40 / total_tokens:49 (reasoning is a subset, totals unchanged): %s", body2)
	}
}
