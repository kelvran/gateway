package adapter

import "strings"

// bedrockStructuredOutputModelSubstrings is the hardcoded set of
// Bedrock-hosted Claude model FAMILIES that accept output_config.format
// via additionalModelRequestFields. The original five came from
// Anthropic's own Bedrock allowlist documentation (RFC
// 2026-09-12-gateway-structured-output-normalization, "Live
// verification"); claude-haiku-4-5 (2026-09-10) and claude-sonnet-4-5
// (2026-09-23, see bedrock.go) were then live-confirmed positive.
// claude-sonnet-5 was live-confirmed NEGATIVE on 2026-09-10 (AWS
// ValidationException "output_config.format: Extra inputs are not
// permitted") and deliberately left out; on 2026-10-08 the identical
// json_schema Converse call against global.anthropic.claude-sonnet-5
// returned a schema-conforming answer (stopReason end_turn), so it was
// added -- found live when an unmodified third-party client
// (Deep-Research's planner, response_format json_object) got a hard
// ErrStructuredOutputUnsupported for a Sonnet 5 deployment that Bedrock
// had quietly started supporting, exactly the "stale whitelist
// understates support" risk the RFC named. On the same day
// global.anthropic.claude-sonnet-5-5 and us.anthropic.claude-sonnet-5-5
// were live-confirmed still NEGATIVE, which is why matching is by
// FAMILY BOUNDARY (bedrockModelFamilyMatches), not a bare substring:
// "claude-sonnet-5" must not admit "claude-sonnet-5-5". AWS extends this
// set over time; re-verify with that one call when a new Claude family
// ships on Bedrock, and never add a family on documentation alone.
var bedrockStructuredOutputModelSubstrings = []string{
	"claude-sonnet-5",
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
	for _, family := range bedrockStructuredOutputModelSubstrings {
		if bedrockModelFamilyMatches(model, family) {
			return true
		}
	}
	return false
}

// bedrockModelFamilyMatches reports whether model names exactly the Claude
// family (e.g. "claude-sonnet-5"), as opposed to a sibling whose ID merely
// starts with it ("claude-sonnet-5-5"). Real Bedrock IDs wrap the family in
// an optional region/provider prefix and an optional "-YYYYMMDD-vN:M" (or
// bare ":N") suffix -- "global.anthropic.claude-sonnet-5",
// "global.anthropic.claude-haiku-4-5-20251001-v1:0",
// "us.anthropic.claude-sonnet-4-5-20250929-v1:0" -- so the character after
// the family must be the end of the ID, a ':' version separator, or a '-'
// that starts a date (8 digits) or a "vN" revision. A '-' followed by a
// short digit run is another version component, i.e. a different family,
// and is rejected: that is what keeps "claude-sonnet-5" from admitting the
// still-unsupported Sonnet 5.5 (live-verified rejected 2026-10-08).
func bedrockModelFamilyMatches(model, family string) bool {
	for i := strings.Index(model, family); i >= 0; {
		rest := model[i+len(family):]
		if bedrockFamilySuffixOK(rest) {
			return true
		}
		next := strings.Index(model[i+1:], family)
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

func bedrockFamilySuffixOK(rest string) bool {
	if rest == "" || rest[0] == ':' {
		return true
	}
	if rest[0] != '-' || len(rest) < 2 {
		return false
	}
	rest = rest[1:]
	if len(rest) >= 2 && rest[0] == 'v' && isASCIIDigit(rest[1]) {
		return true
	}
	digits := 0
	for digits < len(rest) && isASCIIDigit(rest[digits]) {
		digits++
	}
	return digits >= 8
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

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
// family name). "claude-opus-5-5" added 2026-09-24 per
// docs/upgrade-research/upstream-provider-api-changes-2026-09-24.md
// Finding 1 -- Anthropic's own 2026-09-22 release notes confirm Opus 5.5
// inherits this same restriction from Fable 5.1/Mythos 5.1.
var anthropicForcedToolChoiceUnsupportedModelSubstrings = []string{
	"claude-fable-5-1",
	"claude-mythos-5-1",
	"claude-opus-5-5",
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
