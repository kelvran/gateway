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
// per capabilities.go's own doc comment: global.anthropic.claude-sonnet-5
// is NOT whitelisted (a real ValidationException confirms AWS itself
// rejects output_config.format for it), while a Haiku-4.5-style Bedrock
// model ID -- carrying the same region/version prefix and date/version
// suffix real Bedrock IDs always do -- IS whitelisted.
func TestSupportsStructuredOutputBedrockIsPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"global.anthropic.claude-sonnet-5", false},
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
