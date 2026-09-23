package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
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

// newBedrockStructuredOutputPipelineTwoDeployments builds a
// two-deployment Bedrock Pipeline for the same canonical model -- one
// deployment on adapter.SupportsStructuredOutput's whitelist, one not --
// to prove rerouteToCapableDeploymentIfNeeded's real effect: the first
// attempt must reach the capable deployment whenever one exists in the
// same pool. Deliberately order-independent: this test does not rely on
// which of the two deployments WRR's own cursor happens to pick first.
func newBedrockStructuredOutputPipelineTwoDeployments(t *testing.T) *bedrockStructuredOutputHarness {
	t.Helper()

	authCred := "structured-output-bedrock-two-dep-cred"
	keys := []identity.VirtualKey{
		{ID: "structured-output-bedrock-two-dep-key", KeyHash: testHashOf(authCred), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	deployments := []Deployment{
		{Name: "unsupported", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: unsupportedBedrockStructuredOutputModel, BaseURL: "http://unused"},
		{Name: "supported", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "global.anthropic.claude-haiku-4-5-20251001-v1:0", BaseURL: "http://unused"},
	}

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

// TestHandleChatCompletionErrorsOnUnsupportedBedrockModelWithNoCapableAlternativeOnFirstAttempt
// is the dataplane-level, end-to-end proof that the fix for THREAT_MODEL.md's
// Gateway Elevation-of-Privilege row (2026-09-15 entry) actually closes the
// gap: a first-attempt (non-fallback) call through the real
// Pipeline.HandleChatCompletion path to a Bedrock deployment whose model is
// NOT on adapter.SupportsStructuredOutput's whitelist -- with no capable
// alternative anywhere in the same pool -- must now return
// adapter.ErrStructuredOutputUnsupported rather than silently proceeding.
// Supersedes the prior (2026-09-20) version of this test, which asserted the
// opposite -- a documented v1 scope limit accepted a silent omission as
// correct; checkResponseFormatEnforceable (dataplane.go) closes it.
func TestHandleChatCompletionErrorsOnUnsupportedBedrockModelWithNoCapableAlternativeOnFirstAttempt(t *testing.T) {
	h := newBedrockStructuredOutputPipeline(t, unsupportedBedrockStructuredOutputModel)

	_, err := h.pipeline.HandleChatCompletion(context.Background(), "Bearer "+"structured-output-bedrock-cred", "", "", structuredOutputChatRequest(), "")
	if !errors.Is(err, adapter.ErrStructuredOutputUnsupported) {
		t.Fatalf("HandleChatCompletion error = %v, want errors.Is(err, adapter.ErrStructuredOutputUnsupported)", err)
	}
	if h.captured != nil {
		t.Error("Upstream was called -- want the request rejected before ever reaching a deployment that would have silently dropped ResponseFormat")
	}
}

// TestHandleChatCompletionEmitsResponseFormatRequestedNotEnforcedSpanAttributeOnOldStyleFallback
// proves responseFormatRequestedNotEnforced's own attribute-emission code
// (dataplane.go) is still real, reachable code after the fix above, not
// dead code -- via the one remaining, disclosed, deliberately out-of-scope
// gap: checkResponseFormatEnforceable only gates the FIRST pick (this
// phase's own scope) and attemptFallbackChain's capabilityOK gate only
// gates a NEW-style (dep.FallbackChains configured) fallback hop -- the
// OLD-style single-fallback-via-router path (dataplane.go's "pre-existing
// single-fallback-via-router behavior, unchanged" branch, taken when the
// first-picked deployment has no FallbackChains configured AND its own
// upstream call errors) still has no capability check on the fallback
// target it picks. This is real, still-open, named follow-on work -- not
// silently fixed by this phase, and not silently dropped either.
func TestHandleChatCompletionEmitsResponseFormatRequestedNotEnforcedSpanAttributeOnOldStyleFallback(t *testing.T) {
	before := len(spanRecorder.Ended())

	authCred := "structured-output-old-style-fallback-cred"
	keys := []identity.VirtualKey{
		{ID: "structured-output-old-style-fallback-key", KeyHash: testHashOf(authCred), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	deployments := []Deployment{
		// No FallbackChains configured -- its own upstream failure takes
		// the OLD-style, capability-blind fallback branch.
		{Name: "capable-but-fails", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: "global.anthropic.claude-haiku-4-5-20251001-v1:0", BaseURL: "http://unused"},
		{Name: "incapable-fallback-target", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: unsupportedBedrockStructuredOutputModel, BaseURL: "http://unused"},
	}

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
			if dep.Name == "capable-but-fails" {
				return nil, fmt.Errorf("simulated upstream failure")
			}
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

	_, err = p.HandleChatCompletion(context.Background(), "Bearer "+authCred, "", "", structuredOutputChatRequest(), "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v, want the old-style fallback to succeed against the incapable target", err)
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	v, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranResponseFormatRequestedNotEnforced)
	if !ok || v.AsBool() != true {
		t.Errorf("%s = %v, ok=%v, want true -- the old-style fallback path has no capability gate on its own target", telemetry.AttrKelvranResponseFormatRequestedNotEnforced, v, ok)
	}
}

// TestHandleChatCompletionNeverEmitsResponseFormatRequestedNotEnforcedWhenSupported
// proves the negative: a Bedrock model that DOES support structured
// output must never carry this attribute, even though ResponseFormat was
// requested -- it was genuinely enforced, not silently skipped.
func TestHandleChatCompletionNeverEmitsResponseFormatRequestedNotEnforcedWhenSupported(t *testing.T) {
	before := len(spanRecorder.Ended())
	// Any model on adapter.SupportsStructuredOutput's real Bedrock
	// whitelist -- matches the live-verified allowlist named in
	// docs/rfcs/2026-09-12-gateway-structured-output-normalization.md.
	h := newBedrockStructuredOutputPipeline(t, "global.anthropic.claude-haiku-4-5-20251001-v1:0")

	_, err := h.pipeline.HandleChatCompletion(context.Background(), "Bearer "+"structured-output-bedrock-cred", "", "", structuredOutputChatRequest(), "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if _, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranResponseFormatRequestedNotEnforced); ok {
		t.Error("attribute is set even though this model genuinely supports and enforces structured output")
	}
}

// TestHandleChatCompletionNeverEmitsResponseFormatRequestedNotEnforcedOnAuthFailure
// is the guard this attribute's own dataplane.go wiring comment names
// explicitly: on any path where no deployment was ever resolved at all
// (dep is Deployment{}'s zero value), the attribute must never fire --
// a real deployment never got a chance to either honor or silently skip
// anything, so reporting it would be a false signal.
func TestHandleChatCompletionNeverEmitsResponseFormatRequestedNotEnforcedOnAuthFailure(t *testing.T) {
	before := len(spanRecorder.Ended())
	h := newBedrockStructuredOutputPipeline(t, unsupportedBedrockStructuredOutputModel)

	// Deliberately wrong bearer token -- fails auth before routing ever
	// selects a deployment.
	_, err := h.pipeline.HandleChatCompletion(context.Background(), "Bearer wrong-credential", "", "", structuredOutputChatRequest(), "")
	if err == nil {
		t.Fatal("expected an auth error")
	}

	spans := spansSince(before)
	if len(spans) != 1 {
		t.Fatalf("len(spans) = %d, want 1", len(spans))
	}
	if _, ok := spanAttr(t, spans[0].Attributes(), telemetry.AttrKelvranResponseFormatRequestedNotEnforced); ok {
		t.Error("attribute is set on an auth-failure span where no deployment was ever resolved")
	}
}

// TestHandleChatCompletionStreamErrorsOnUnsupportedBedrockModelWithNoCapableAlternative
// is streaming.go's own equivalent proof to
// TestHandleChatCompletionErrorsOnUnsupportedBedrockModelWithNoCapableAlternativeOnFirstAttempt
// above -- checkResponseFormatEnforceable is called from BOTH real
// first-pick call sites (dataplane.go's runMissPath, and
// HandleChatCompletionStream here), and this proves the streaming path
// genuinely got the fix too, not just the buffered one.
func TestHandleChatCompletionStreamErrorsOnUnsupportedBedrockModelWithNoCapableAlternative(t *testing.T) {
	var upstreamCalls int
	p := newStreamingTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (io.ReadCloser, error) {
		upstreamCalls++
		return nil, fmt.Errorf("upstream should never be called")
	}, []Deployment{{Name: "d1", Model: "claude-bedrock", Provider: "bedrock", UpstreamModel: unsupportedBedrockStructuredOutputModel, BaseURL: "http://unused"}},
		adapter.Registry{"bedrock": bedrock.New()})

	rec := httptest.NewRecorder()
	err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", structuredOutputChatRequest(), rec, "")
	if !errors.Is(err, adapter.ErrStructuredOutputUnsupported) {
		t.Fatalf("HandleChatCompletionStream error = %v, want errors.Is(err, adapter.ErrStructuredOutputUnsupported)", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 -- the request must be rejected before ever reaching a deployment that would have silently dropped ResponseFormat", upstreamCalls)
	}
}

// TestHandleChatCompletionReroutesFirstPickToCapableDeploymentWhenOneExists
// proves rerouteToCapableDeploymentIfNeeded's real effect end to end: when
// req.Model's own deployment pool contains at least one deployment that
// can honor ResponseFormat, the FIRST attempt must reach it -- schema
// enforcement must be genuinely applied -- even if WRR's own first pick
// landed on the incapable deployment. Contrast with
// TestHandleChatCompletionSilentlyOmitsStructuredOutputEnforcementForUnsupportedBedrockModelOnFirstAttempt
// above, whose single-deployment pool has no capable alternative to
// reroute to and must still omit enforcement.
func TestHandleChatCompletionReroutesFirstPickToCapableDeploymentWhenOneExists(t *testing.T) {
	h := newBedrockStructuredOutputPipelineTwoDeployments(t)

	resp, err := h.pipeline.HandleChatCompletion(context.Background(), "Bearer "+"structured-output-bedrock-two-dep-cred", "", "", structuredOutputChatRequest(), "")
	if err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("Choices is empty, want a normal response")
	}

	if h.captured == nil {
		t.Fatal("Upstream was never called")
	}
	if h.captured.AdditionalModelRequestFields == nil {
		t.Error("AdditionalModelRequestFields = nil, want schema enforcement applied -- a capable deployment exists in this model's own pool and the first pick should have been rerouted to it")
	}
}
