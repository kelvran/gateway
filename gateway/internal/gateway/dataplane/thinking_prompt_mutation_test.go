package dataplane

// Closes the CLOSABLE half of a confirmed test-coverage gap named in
// docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md's
// Addendum (2026-09-24): whether Kelvran's own live-mutable prompt
// management (internal/prompt's resolvePromptIfSet) genuinely
// reproduces "a signed thinking block forwarded under a mutated
// prefix" end to end -- the exact hazard ThinkingBindingMode's
// deliberate non-strict ("drop_block") default exists to survive.
//
// Does NOT and cannot close the RFC's own separate Unresolved
// Questions (whether Anthropic's LIVE API actually behaves as
// anthropic.go assumes -- exact header/field names, whether
// "drop_block" really lets the request succeed): that came from one
// unverified sandboxed WebFetch call and needs a human with real
// Anthropic credentials doing manual verification. No test in this
// file proves anything about Anthropic's live behavior -- only that
// Kelvran's own request-construction mechanism genuinely produces the
// hazardous combination it's meant to defend against.

import (
	"context"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
)

// TestPromptTemplateMutationBetweenCallsForwardsStaleSignedThinkingBlockUnderANewPrefix
// proves Kelvran's own resolvePromptIfSet + prompt-template mutation
// genuinely reproduces the hazard: two calls against the SAME PromptID,
// with only the EARLIER template message edited between them while a
// LATER assistant message's own real ReasoningBlock (Signature+Text) is
// left untouched. resolveMessages (internal/prompt/prompt.go) only ever
// substitutes Content/Parts[].Text -- ReasoningBlocks passes through
// byte-for-byte unchanged, confirmed by direct read of that function --
// so this reproduces "signed thinking block forwarded under a mutated
// prefix" without inventing any client-side replay mechanism.
func TestPromptTemplateMutationBetweenCallsForwardsStaleSignedThinkingBlockUnderANewPrefix(t *testing.T) {
	var captured []*anthropic.Request
	deployments := []Deployment{{Name: "d1", Model: "claude-opus-5-5", Provider: "anthropic", UpstreamModel: "claude-opus-5-5", BaseURL: "http://unused"}}
	p := newAnthropicTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		req, ok := providerReq.(*anthropic.Request)
		if !ok {
			t.Fatalf("providerReq is %T, want *anthropic.Request", providerReq)
		}
		captured = append(captured, req)
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}, deployments)

	const reasoningSignature = "sig-captured-under-v1-prefix"
	reasoningBearingMessage := adapter.Message{
		Role:    "assistant",
		Content: "here's my answer",
		ReasoningBlocks: []adapter.ReasoningBlock{
			{Sequence: 0, Text: "let me think about this...", Signature: reasoningSignature},
		},
	}

	if _, err := p.UpsertPrompt("thinking-replay", []adapter.Message{
		{Role: "system", Content: "prefix version one"},
		reasoningBearingMessage,
	}); err != nil {
		t.Fatalf("UpsertPrompt v1: %v", err)
	}
	// v2: ONLY the earlier system/prefix message changes -- the
	// reasoning-bearing message is byte-for-byte identical to v1.
	if _, err := p.UpsertPrompt("thinking-replay", []adapter.Message{
		{Role: "system", Content: "prefix version two, genuinely different wording"},
		reasoningBearingMessage,
	}); err != nil {
		t.Fatalf("UpsertPrompt v2: %v", err)
	}

	ctx := context.Background()
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "claude-opus-5-5", PromptID: "thinking-replay", PromptVersion: 1}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (version 1): %v", err)
	}
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "claude-opus-5-5", PromptID: "thinking-replay", PromptVersion: 2}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (version 2): %v", err)
	}

	if len(captured) != 2 {
		t.Fatalf("captured %d upstream requests, want 2 (promptFingerprint must bust any would-be cache collision between the two versions)", len(captured))
	}

	// A role:"system" message is pulled OUT of Messages entirely into
	// Request.System (Anthropic has no system-role message; see
	// ToProvider's own doc comment) -- the prefix mutation must be read
	// from there, not from Messages[0].
	if len(captured[0].System) != 1 || len(captured[1].System) != 1 {
		t.Fatalf("captured[0].System/captured[1].System lengths = %d/%d, want 1/1", len(captured[0].System), len(captured[1].System))
	}
	prefix1 := captured[0].System[0].Text
	prefix2 := captured[1].System[0].Text
	if prefix1 == prefix2 {
		t.Fatalf("prefix message identical across both calls (%q) -- the mutation setup itself is broken, this test proves nothing", prefix1)
	}

	// The system message was pulled out of Messages, so the only
	// remaining message is the reasoning-bearing one, at index 0. It
	// has no ToolCalls, so reasoningBlocksToProvider (anthropic.go)
	// emits its single Sequence:0 block FIRST, before the "text"
	// content block -- Content[0] is the "thinking" block, Content[1]
	// is "here's my answer".
	if len(captured[0].Messages) != 1 || len(captured[1].Messages) != 1 {
		t.Fatalf("captured[0].Messages/captured[1].Messages lengths = %d/%d, want 1/1 (the system message should have been pulled out entirely)", len(captured[0].Messages), len(captured[1].Messages))
	}
	sig1 := captured[0].Messages[0].Content[0].Signature
	sig2 := captured[1].Messages[0].Content[0].Signature
	if sig1 != reasoningSignature || sig2 != reasoningSignature {
		t.Fatalf("thinking block signatures = %q, %q, want both %q -- ReasoningBlocks must forward byte-identically regardless of prefix mutation", sig1, sig2, reasoningSignature)
	}
	if sig1 != sig2 {
		t.Errorf("thinking block signature changed between calls (%q -> %q) -- the hazard this test exists to reproduce (stale signature under a NEW prefix) did not actually occur", sig1, sig2)
	}
}

// TestReplayedThinkingBlockAfterPromptMutationDefaultsToNonStrictBetaAndField
// proves the wiring this session's own earlier work already unit-tested
// in isolation (TestToProviderThinkingBindingDefaultsToNonStrictForQualifyingModel)
// also holds when REACHED via the real prompt-mutation trigger above,
// not just a hand-built fixture: both calls must produce a Thinking
// block with PrefixMismatchBehavior == "drop_block" (Kelvran's own
// deliberate non-strict default), regardless of the prefix mutation
// between them.
func TestReplayedThinkingBlockAfterPromptMutationDefaultsToNonStrictBetaAndField(t *testing.T) {
	var captured []*anthropic.Request
	deployments := []Deployment{{Name: "d1", Model: "claude-opus-5-5", Provider: "anthropic", UpstreamModel: "claude-opus-5-5", BaseURL: "http://unused"}}
	p := newAnthropicTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		req, ok := providerReq.(*anthropic.Request)
		if !ok {
			t.Fatalf("providerReq is %T, want *anthropic.Request", providerReq)
		}
		captured = append(captured, req)
		return fakeAnthropicResponse(dep.UpstreamModel), nil
	}, deployments)

	reasoningBearingMessage := adapter.Message{
		Role:            "assistant",
		Content:         "here's my answer",
		ReasoningBlocks: []adapter.ReasoningBlock{{Sequence: 0, Text: "thinking...", Signature: "sig-any"}},
	}
	if _, err := p.UpsertPrompt("thinking-replay-defaults", []adapter.Message{
		{Role: "system", Content: "v1"}, reasoningBearingMessage,
	}); err != nil {
		t.Fatalf("UpsertPrompt v1: %v", err)
	}
	if _, err := p.UpsertPrompt("thinking-replay-defaults", []adapter.Message{
		{Role: "system", Content: "v2, mutated"}, reasoningBearingMessage,
	}); err != nil {
		t.Fatalf("UpsertPrompt v2: %v", err)
	}

	ctx := context.Background()
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "claude-opus-5-5", PromptID: "thinking-replay-defaults", PromptVersion: 1}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (version 1): %v", err)
	}
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", "", "", adapter.ChatRequest{Model: "claude-opus-5-5", PromptID: "thinking-replay-defaults", PromptVersion: 2}, ""); err != nil {
		t.Fatalf("HandleChatCompletion (version 2): %v", err)
	}

	if len(captured) != 2 {
		t.Fatalf("captured %d upstream requests, want 2", len(captured))
	}
	for i, req := range captured {
		if req.Thinking == nil || req.Thinking.BlockBinding == nil {
			t.Fatalf("call %d: req.Thinking.BlockBinding = nil, want populated (claude-opus-5-5 qualifies for preserved-thinking binding)", i+1)
		}
		if got := req.Thinking.BlockBinding.PrefixMismatchBehavior; got != "drop_block" {
			t.Errorf("call %d: PrefixMismatchBehavior = %q, want %q (Kelvran's own deliberate non-strict default, unaffected by the prefix mutation)", i+1, got, "drop_block")
		}
	}
}
