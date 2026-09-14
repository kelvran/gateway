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

// bedrockForcedToolChoiceModelSubstrings is the exact set of Bedrock
// model-family substrings AWS documents as supporting ToolChoice's
// SpecificToolChoice ("tool" mode) -- per
// docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ToolChoice.html,
// restricted to "Anthropic Claude 3 and Amazon Nova models" only. A
// separately-tracked list from bedrockStructuredOutputModelSubstrings
// (that whitelist gates a different capability, output_config.format;
// AWS documents the two capability lists independently, and they are
// not guaranteed to stay identical over time) -- per
// docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md.
var bedrockForcedToolChoiceModelSubstrings = []string{
	"claude-3",
	"nova",
}

// BedrockModelSupportsForcedToolChoice reports whether model (a Bedrock
// model ID) matches one of bedrockForcedToolChoiceModelSubstrings.
// Exported (unlike bedrockModelSupportsStructuredOutput) so
// internal/adapter/bedrock can call it directly when mapping
// ChatRequest.ToolChoice's "tool" mode -- adapter/bedrock imports
// adapter already, for ChatRequest/ToolChoice itself, so this adds no
// new dependency edge.
func BedrockModelSupportsForcedToolChoice(model string) bool {
	for _, substr := range bedrockForcedToolChoiceModelSubstrings {
		if strings.Contains(model, substr) {
			return true
		}
	}
	return false
}

// anthropicForcedToolChoiceUnsupportedModelSubstrings is the exact set
// of Claude model-family substrings that reject Anthropic's own
// tool_choice "any"/"tool" forced modes with a 400, per
// docs/upgrade-research/advanced-tool-calling-structured-output-2026-09-14.md
// Finding 2 -- Anthropic's own documented workaround for these specific
// model variants is "auto" + strict tool use or structured outputs
// instead of forcing. Matched by substring, mirroring every other
// per-model whitelist/blocklist in this file, for the same real-model-ID
// reason (region/version prefixes and date/version suffixes around the
// family name).
var anthropicForcedToolChoiceUnsupportedModelSubstrings = []string{
	"claude-fable-5-1",
	"claude-mythos-5-1",
}

// AnthropicModelRejectsForcedToolChoice reports whether model (an
// Anthropic model ID, direct API or Bedrock's Anthropic-family) matches
// one of anthropicForcedToolChoiceUnsupportedModelSubstrings -- i.e.
// whether tool_choice "any"/"tool" would be rejected for this model.
// Exported so both internal/adapter/anthropic and internal/adapter/
// bedrock (Bedrock's own Anthropic-family models share this same real
// restriction) can call it directly.
func AnthropicModelRejectsForcedToolChoice(model string) bool {
	for _, substr := range anthropicForcedToolChoiceUnsupportedModelSubstrings {
		if strings.Contains(model, substr) {
			return true
		}
	}
	return false
}
