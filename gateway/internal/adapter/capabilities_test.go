package adapter

import "testing"

// TestSupportsStructuredOutputFlatProviders proves every non-Bedrock
// provider this codebase registers is unconditionally true, regardless
// of the model string -- SupportsStructuredOutput's per-provider (not
// per-model) answer for these four.
func TestSupportsStructuredOutputFlatProviders(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic", "gemini", "openaicompat"} {
		for _, model := range []string{"gpt-4o", "claude-sonnet-5", "gemini-2.5-flash", "anything"} {
			if !SupportsStructuredOutput(provider, model) {
				t.Errorf("SupportsStructuredOutput(%q, %q) = false, want true", provider, model)
			}
		}
	}
}

// TestSupportsStructuredOutputUnknownProviderIsFalse proves the default
// branch: a provider string this codebase doesn't register at all is
// never assumed to support structured output.
func TestSupportsStructuredOutputUnknownProviderIsFalse(t *testing.T) {
	if SupportsStructuredOutput("some-future-provider", "any-model") {
		t.Error("SupportsStructuredOutput for an unregistered provider = true, want false")
	}
}

// TestSupportsStructuredOutputBedrockIsPerModel is the load-bearing
// proof for Bedrock's genuinely different, per-model (not per-provider)
// capability check, live-verified against a real AWS Converse API call
// per capabilities.go's own doc comment: a Haiku-4.5-style Bedrock model
// ID -- carrying the same region/version prefix and date/version suffix
// real Bedrock IDs always do -- IS whitelisted, an unknown family is not,
// and global.anthropic.claude-sonnet-5 flipped from NOT whitelisted (a
// real ValidationException on 2026-09-10) to whitelisted (a real
// schema-conforming Converse answer on 2026-10-08) -- the list follows
// AWS's live behaviour, never documentation.
func TestSupportsStructuredOutputBedrockIsPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"global.anthropic.claude-sonnet-5", true},               // live-verified supported 2026-10-08 (rejected 2026-09-10)
		{"global.anthropic.claude-sonnet-5-5", false},            // sibling family, live-verified rejected 2026-10-08
		{"us.anthropic.claude-sonnet-5-5", false},                // same, us. profile
		{"anthropic.claude-sonnet-50-v1:0", false},               // prefix of a longer version component
		{"global.anthropic.claude-sonnet-5-20261101-v1:0", true}, // hypothetical dated Sonnet 5 ID: family boundary + date
		{"global.anthropic.claude-sonnet-5:0", true},
		{"anthropic.claude-sonnet-5-v1:0", true},               // "-vN" revision with no date             // bare ':N' revision
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", true}, // existing family, dated form
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"anthropic.claude-opus-4-6-20260101-v1:0", true},
		{"global.anthropic.claude-sonnet-4-6-20260101-v1:0", true},
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"global.anthropic.claude-opus-4-5-20250929-v1:0", true},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", false},
		{"anthropic.claude-3-haiku-20240307-v1:0", false},
		{"meta.llama3-70b-instruct-v1:0", false},
	}
	for _, tt := range tests {
		if got := SupportsStructuredOutput("bedrock", tt.model); got != tt.want {
			t.Errorf("SupportsStructuredOutput(bedrock, %q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

// TestAnthropicModelRejectsForcedToolChoice proves the substring
// allowlist matches every model family Anthropic has documented as
// rejecting forced tool_choice, including claude-opus-5-5 (added
// 2026-09-24 per docs/upgrade-research/upstream-provider-api-changes-
// 2026-09-24.md Finding 1 -- Anthropic's own 2026-09-22 release notes
// confirm Opus 5.5 inherits this restriction from Fable 5.1/Mythos 5.1),
// and that unrelated models are never falsely flagged.
func TestAnthropicModelRejectsForcedToolChoice(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"claude-fable-5-1", true},
		{"claude-mythos-5-1", true},
		{"claude-opus-5-5", true},
		{"anthropic.claude-opus-5-5-20260901-v1:0", true},
		{"global.anthropic.claude-fable-5-1-20260901-v1:0", true},
		{"claude-sonnet-5", false},
		{"claude-opus-4-6-20260101", false},
		{"gpt-4o", false},
	}
	for _, tt := range tests {
		if got := AnthropicModelRejectsForcedToolChoice(tt.model); got != tt.want {
			t.Errorf("AnthropicModelRejectsForcedToolChoice(%q) = %v, want %v", tt.model, got, tt.want)
		}
	}
}

// TestBedrockForwardsThinkingIsPerFamily is the live-probed decision table
// behind BedrockForwardsThinking (item 11 slice S4; the S2 and S4 Converse
// probes, 2026-10-10): the Claude 5.x generation accepts thinking.type
// "adaptive" and rejects "enabled" with a 400 naming adaptive; 4.6 accepts
// both; 4.5 and older accept "enabled" and reject "adaptive". A rejecting
// family drops the object (the request still succeeds, without thinking);
// every other case -- an accepting family, an unlisted family, a
// non-Anthropic model, a type the probes never covered -- forwards
// verbatim so the upstream's own answer is what the caller sees.
func TestBedrockForwardsThinkingIsPerFamily(t *testing.T) {
	tests := []struct {
		model, thinkingType string
		forward             bool
	}{
		// Claude 5.x -- proven: sonnet-5, sonnet-5-5, fable-5-1, haiku-5-5.
		{"global.anthropic.claude-sonnet-5", "adaptive", true},
		{"global.anthropic.claude-sonnet-5", "enabled", false},
		{"global.anthropic.claude-sonnet-5-5", "adaptive", true},
		{"global.anthropic.claude-sonnet-5-5", "enabled", false},
		{"global.anthropic.claude-fable-5-1", "adaptive", true},
		{"global.anthropic.claude-fable-5-1", "enabled", false},
		{"global.anthropic.claude-haiku-5-5", "adaptive", true},
		{"global.anthropic.claude-haiku-5-5", "enabled", false},
		// Claude 5.x -- inferred from the generation (IAM-denied to the probe).
		{"global.anthropic.claude-opus-5-5", "adaptive", true},
		{"global.anthropic.claude-opus-5-5", "enabled", false},
		{"global.anthropic.claude-opus-5", "enabled", false},
		// Claude 4.6 -- proven: sonnet-4-6; inferred: opus-4-6.
		{"global.anthropic.claude-sonnet-4-6", "adaptive", true},
		{"global.anthropic.claude-sonnet-4-6", "enabled", true},
		{"global.anthropic.claude-opus-4-6-v1", "adaptive", true},
		{"global.anthropic.claude-opus-4-6-v1", "enabled", true},
		// Claude 4.5 and older -- proven: haiku-4-5; inferred: sonnet-4-5, opus-4-5.
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "enabled", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "adaptive", false},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", "adaptive", false},
		{"global.anthropic.claude-opus-4-5-20251101-v1:0", "adaptive", false},
		{"global.anthropic.claude-opus-4-5-20251101-v1:0", "enabled", true},
		// Unlisted family, non-Anthropic model, unprobed type: forward.
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", "adaptive", true},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", "enabled", true},
		{"amazon.nova-pro-v1:0", "adaptive", true},
		{"global.anthropic.claude-fable-5-1", "disabled", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", "disabled", true},
	}
	for _, tt := range tests {
		forward, reason := BedrockForwardsThinking(tt.model, tt.thinkingType)
		if forward != tt.forward {
			t.Errorf("BedrockForwardsThinking(%q, %q) = %v (%q), want %v", tt.model, tt.thinkingType, forward, reason, tt.forward)
		}
		if !forward && reason == "" {
			t.Errorf("BedrockForwardsThinking(%q, %q) dropped with an empty reason", tt.model, tt.thinkingType)
		}
		if forward && reason != "" {
			t.Errorf("BedrockForwardsThinking(%q, %q) forwarded but returned reason %q, want empty", tt.model, tt.thinkingType, reason)
		}
	}
}

// TestBedrockThinkingCapabilityForCarriesEffortAndTopK pins the two
// columns slice S5 consumes (output_config.effort and top_k), probed in
// the same calls: 5.x accepts effort and rejects top_k ("deprecated for
// this model"); 4.6 accepts both; 4.5 rejects effort ("does not support
// the effort parameter") and accepts top_k. The second return is false
// for a family the table does not list.
func TestBedrockThinkingCapabilityForCarriesEffortAndTopK(t *testing.T) {
	tests := []struct {
		model string
		want  BedrockThinkingCapability
		known bool
	}{
		{"global.anthropic.claude-fable-5-1", BedrockThinkingCapability{Adaptive: true, Effort: true}, true},
		{"global.anthropic.claude-haiku-5-5", BedrockThinkingCapability{Adaptive: true, Effort: true}, true},
		{"global.anthropic.claude-sonnet-4-6", BedrockThinkingCapability{Enabled: true, Adaptive: true, Effort: true, TopK: true}, true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", BedrockThinkingCapability{Enabled: true, TopK: true}, true},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", BedrockThinkingCapability{}, false},
		{"amazon.nova-pro-v1:0", BedrockThinkingCapability{}, false},
	}
	for _, tt := range tests {
		got, known := BedrockThinkingCapabilityFor(tt.model)
		if known != tt.known || got != tt.want {
			t.Errorf("BedrockThinkingCapabilityFor(%q) = %+v, %v; want %+v, %v", tt.model, got, known, tt.want, tt.known)
		}
	}
}

// TestBedrockForwardsTopKIsPerFamily is the top_k half of the 2026-10-10
// probes (item 11 slice S5): the Claude 5.x generation rejects top_k
// ("deprecated for this model"), 4.6 and 4.5 accept it, and -- the same
// asymmetry as BedrockForwardsThinking -- an unlisted family or a
// non-Anthropic model forwards verbatim so the upstream's own answer is
// what the caller sees.
func TestBedrockForwardsTopKIsPerFamily(t *testing.T) {
	tests := []struct {
		model   string
		forward bool
	}{
		{"global.anthropic.claude-sonnet-5", false},
		{"global.anthropic.claude-fable-5-1", false},
		{"global.anthropic.claude-haiku-5-5", false},
		{"global.anthropic.claude-opus-5-5", false},
		{"global.anthropic.claude-sonnet-4-6", true},
		{"global.anthropic.claude-opus-4-6-v1", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", true},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", true},
		{"amazon.nova-pro-v1:0", true},
	}
	for _, tt := range tests {
		forward, reason := BedrockForwardsTopK(tt.model)
		if forward != tt.forward {
			t.Errorf("BedrockForwardsTopK(%q) = %v (%q), want %v", tt.model, forward, reason, tt.forward)
		}
		if !forward && reason == "" {
			t.Errorf("BedrockForwardsTopK(%q) dropped with an empty reason", tt.model)
		}
		if forward && reason != "" {
			t.Errorf("BedrockForwardsTopK(%q) forwarded with a reason %q, want empty", tt.model, reason)
		}
	}
}

// TestBedrockForwardsEffortIsPerFamily is the effort half of the 2026-10-10
// probes (item 11 slice S5): the Claude 5.x and 4.6 generations accept
// output_config.effort, 4.5 rejects it ("does not support the effort
// parameter"), and an unlisted family or a non-Anthropic model forwards
// verbatim, the same asymmetry as thinking and top_k.
func TestBedrockForwardsEffortIsPerFamily(t *testing.T) {
	tests := []struct {
		model   string
		forward bool
	}{
		{"global.anthropic.claude-sonnet-5", true},
		{"global.anthropic.claude-fable-5-1", true},
		{"global.anthropic.claude-haiku-5-5", true},
		{"global.anthropic.claude-sonnet-4-6", true},
		{"global.anthropic.claude-haiku-4-5-20251001-v1:0", false},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", false},
		{"global.anthropic.claude-opus-4-5-20251101-v1:0", false},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", true},
		{"amazon.nova-pro-v1:0", true},
	}
	for _, tt := range tests {
		forward, reason := BedrockForwardsEffort(tt.model)
		if forward != tt.forward {
			t.Errorf("BedrockForwardsEffort(%q) = %v (%q), want %v", tt.model, forward, reason, tt.forward)
		}
		if !forward && reason == "" {
			t.Errorf("BedrockForwardsEffort(%q) dropped with an empty reason", tt.model)
		}
	}
}
