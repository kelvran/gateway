package adapter

import "strings"

// bedrockStructuredOutputModelSubstrings is the exact, hardcoded set of
// Bedrock-hosted Claude model-family substrings Anthropic documents as
// supporting output_config.format via additionalModelRequestFields,
// live-verified 2026-09-10 (a real Converse API call against
// "additionalModelRequestFields": {"output_config": {"format": {"type":
// "json_schema", "schema": ...}}} for global.anthropic.claude-sonnet-5 --
// NOT in this set -- returned a clean, distinguishable AWS
// ValidationException: "output_config.format: Extra inputs are not
// permitted", proving this is a real, per-model, AWS-enforced
// restriction, not a stale-docs guess).
//
// Matched by substring (strings.Contains), not exact equality, mirroring
// this codebase's existing convention for matching a variable string
// against a set of known substrings (see
// internal/gateway/dataplane/fallback.go's containsAnyKeyword) --
// real Bedrock model IDs carry a region/version prefix and a
// date/version suffix around the family name itself (e.g.
// "global.anthropic.claude-haiku-4-5-20251001-v1:0"), so exact-string
// matching would miss every real ID.
var bedrockStructuredOutputModelSubstrings = []string{
	"claude-opus-4-6",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-opus-4-5",
	"claude-haiku-4-5",
}

// SupportsStructuredOutput reports whether provider/model can natively
// enforce ChatRequest.ResponseFormat. Every provider
// other than Bedrock is a flat per-provider answer -- openai, anthropic
// (direct API), gemini, and openaicompat all support it unconditionally
// today. Bedrock alone needs a per-MODEL check, not a per-provider one:
// AWS's own live behavior (see bedrockStructuredOutputModelSubstrings'
// doc comment) restricts output_config.format to a specific whitelist of
// Bedrock-hosted Claude models, and calling it against an unsupported
// model is a real, enforced rejection, not a silent no-op.
func SupportsStructuredOutput(provider, model string) bool {
	switch provider {
	case "openai", "anthropic", "gemini", "openaicompat":
		return true
	case "bedrock":
		return bedrockModelSupportsStructuredOutput(model)
	default:
		return false
	}
}

// bedrockModelSupportsStructuredOutput reports whether model (a Bedrock
// model ID, carrying its own region/version prefix and date/version
// suffix) matches one of the whitelisted Claude model families in
// bedrockStructuredOutputModelSubstrings.
func bedrockModelSupportsStructuredOutput(model string) bool {
	for _, substr := range bedrockStructuredOutputModelSubstrings {
		if strings.Contains(model, substr) {
			return true
		}
	}
	return false
}
