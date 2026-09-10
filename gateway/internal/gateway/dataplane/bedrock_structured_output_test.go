package dataplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// unsupportedBedrockStructuredOutputModel is NOT on
// adapter.SupportsStructuredOutput's Bedrock whitelist -- the exact same
// unsupported model ID
// TestToProviderResponseFormatOmitsAdditionalModelRequestFieldsOnUnsupportedModel
// (gateway/internal/adapter/bedrock/bedrock_test.go) already proves the
// omission against at the isolated adapter-unit level.
const unsupportedBedrockStructuredOutputModel = "anthropic.claude-3-5-sonnet-20241022-v2:0"

// bedrockStructuredOutputHarness bundles the Pipeline built by
// newBedrockStructuredOutputPipeline together with the native
// *bedrock.Request its fake Upstream closure most recently captured --
// populated only once HandleChatCompletion has actually run.
type bedrockStructuredOutputHarness struct {
	pipeline *Pipeline
	captured *bedrock.Request
}

// newBedrockStructuredOutputPipeline builds a single-deployment Bedrock
// Pipeline whose fake Upstream closure records the native *bedrock.Request
// it was handed -- mirroring streamrunaway_test.go's own
// TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletionBedrock
// harness-construction pattern (NewPipeline with a real bedrock.New()
// adapter and a fake Upstream closure), adapted for the buffered
// HandleChatCompletion path instead of the streaming one.
func newBedrockStructuredOutputPipeline(t *testing.T, upstreamModel string) *bedrockStructuredOutputHarness {
	t.Helper()

	authCred := "structured-output-bedrock-cred"
	keys := []identity.VirtualKey{
		{ID: "structured-output-bedrock-key", KeyHash: testHashOf(authCred), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	deployments := []Deployment{{
		Name: "d1", Model: "claude-bedrock", Provider: "bedrock",
		UpstreamModel: upstreamModel, BaseURL: "http://unused",
	}}

	h := &bedrockStructuredOutputHarness{}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"bedrock": bedrock.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			h.captured = req.(*bedrock.Request)
			return &bedrock.Response{
				Output: bedrock.Output{Message: bedrock.Message{
					Role:    "assistant",
					Content: []bedrock.ContentBlock{{Text: "ok"}},
				}},
				StopReason: "end_turn",
				Usage:      bedrock.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	h.pipeline = p
	return h
}

func structuredOutputChatRequest() adapter.ChatRequest {
	return adapter.ChatRequest{
		Model:    "claude-bedrock",
		Messages: []adapter.Message{{Role: "user", Content: "give me JSON"}},
		ResponseFormat: &adapter.ResponseFormat{
			Type: "json_schema",
			JSONSchema: &adapter.JSONSchema{
				Name:   "weather_response",
				Schema: json.RawMessage(`{"type":"object"}`),
			},
		},
	}
}

// TestHandleChatCompletionSilentlyOmitsStructuredOutputEnforcementForUnsupportedBedrockModelOnFirstAttempt
// is the dataplane-level, end-to-end proof for THREAT_MODEL.md's Gateway
// Elevation-of-Privilege row (2026-09-15 entry): a first-attempt
// (non-fallback) call through the real Pipeline.HandleChatCompletion path
// to a Bedrock deployment whose model is NOT on
// adapter.SupportsStructuredOutput's whitelist must (a) succeed as an
// ordinary response, with (b) additionalModelRequestFields genuinely
// absent from what was sent upstream -- the same omission
// TestToProviderResponseFormatOmitsAdditionalModelRequestFieldsOnUnsupportedModel
// already proves at the isolated adapter-unit level, now proven through
// the real pipeline (auth, rate limiting, budget, cache, adapter
// dispatch) rather than by calling ToProvider directly.
func TestHandleChatCompletionSilentlyOmitsStructuredOutputEnforcementForUnsupportedBedrockModelOnFirstAttempt(t *testing.T) {
	h := newBedrockStructuredOutputPipeline(t, unsupportedBedrockStructuredOutputModel)

	resp, err := h.pipeline.HandleChatCompletion(context.Background(), "Bearer "+"structured-output-bedrock-cred", structuredOutputChatRequest())
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v, want a normal successful response -- the documented v1 scope limit is a silent omission, never an error", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("Choices is empty, want a normal response")
	}

	if h.captured == nil {
		t.Fatal("Upstream was never called")
	}
	if h.captured.AdditionalModelRequestFields != nil {
		t.Errorf("AdditionalModelRequestFields = %v, want nil -- schema enforcement must be silently omitted for a first-attempt call to a Bedrock model outside the structured-output whitelist", h.captured.AdditionalModelRequestFields)
	}
}
