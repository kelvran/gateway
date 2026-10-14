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
// "claude-sonnet-5-5" added 2026-10-14 (round-5 Tier A item 2): the
// 2026-09-28 release notes list it, and tool_choice "any" was PROVEN a
// 400 on Bedrock Sonnet 5.5 the same day; Haiku 5.5 accepts a forced
// choice (its migration guide; proven) and stays off the list.
var anthropicForcedToolChoiceUnsupportedModelSubstrings = []string{
	"claude-fable-5-1",
	"claude-mythos-5-1",
	"claude-opus-5-5",
	"claude-sonnet-5-5",
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

// BedrockThinkingCapability is one Claude generation's answer to the four
// Anthropic request fields Bedrock Converse validates per model inside
// additionalModelRequestFields, live-probed on 2026-10-10 (item 11 slices
// S2 and S4; the full tables with AWS's own messages are in the gitignored
// scratch-pad/research/round4-lanes/sources/converse-*-probe-2026-10-10.md).
// Every rejection is a 400 ValidationException naming the field; nothing
// is dropped upstream.
type BedrockThinkingCapability struct {
	// Enabled: thinking {"type":"enabled","budget_tokens":N}.
	Enabled bool
	// Adaptive: thinking {"type":"adaptive"}.
	Adaptive bool
	// Effort: output_config {"effort": ...} (consumed by slice S5).
	Effort bool
	// TopK: top_k (consumed by slice S5).
	TopK bool
}

// bedrockThinkingGenerations lists the Claude families the table knows,
// by generation, matched with bedrockModelFamilyMatches (so
// "claude-sonnet-5" never admits "claude-sonnet-5-5"). PROVEN means the
// 2026-10-10 probes ran against that exact family; INFERRED means the
// family is placed by its generation only -- every Opus id was refused by
// the probing account's identity policy (bedrock_opus_4.8_deny, an
// explicit deny on bedrock:InvokeModel, hit on the baseline call too), and
// sonnet-4-5 was not requested. Re-probe an inferred family before
// relying on it, and never add a family on documentation alone
// (bedrockStructuredOutputModelSubstrings' own rule).
var bedrockThinkingGenerations = []struct {
	families   []string
	capability BedrockThinkingCapability
}{
	{
		// Claude 5.x: adaptive and effort; enabled is a 400 ("use
		// thinking.type.adaptive and output_config.effort"), top_k is a
		// 400 ("deprecated for this model"). PROVEN: sonnet-5, sonnet-5-5,
		// fable-5-1, haiku-5-5. INFERRED: opus-5, opus-5-5.
		families:   []string{"claude-sonnet-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-haiku-5-5", "claude-opus-5", "claude-opus-5-5"},
		capability: BedrockThinkingCapability{Adaptive: true, Effort: true},
	},
	{
		// Claude 4.6: all four. PROVEN: sonnet-4-6. INFERRED: opus-4-6.
		families:   []string{"claude-sonnet-4-6", "claude-opus-4-6"},
		capability: BedrockThinkingCapability{Enabled: true, Adaptive: true, Effort: true, TopK: true},
	},
	{
		// Claude 4.5: enabled and top_k; adaptive is a 400 ("adaptive
		// thinking is not supported on this model"), effort is a 400
		// ("does not support the effort parameter"). PROVEN: haiku-4-5.
		// INFERRED: sonnet-4-5, opus-4-5.
		families:   []string{"claude-haiku-4-5", "claude-sonnet-4-5", "claude-opus-4-5"},
		capability: BedrockThinkingCapability{Enabled: true, TopK: true},
	},
}

// BedrockThinkingCapabilityFor returns the table row for model (a Bedrock
// model ID with its region prefix and version suffix) and whether the
// table lists its family at all. Exported for the same reason
// BedrockModelSupportsForcedToolChoice is: adapter/bedrock consumes it,
// and the dataplane reads it to record a drop.
func BedrockThinkingCapabilityFor(model string) (BedrockThinkingCapability, bool) {
	for _, gen := range bedrockThinkingGenerations {
		for _, family := range gen.families {
			if bedrockModelFamilyMatches(model, family) {
				return gen.capability, true
			}
		}
	}
	return BedrockThinkingCapability{}, false
}

// BedrockForwardsThinking decides whether the bedrock adapter sends a
// canonical ChatRequest.Thinking of thinkingType to model and, when it
// does not, why (a short reason for the dataplane's request_field_dropped
// log line). The asymmetry is deliberate: only a type the model's family is
// proven (or, for an inferred family, placed by generation) to reject
// with a 400 is dropped -- the request then succeeds without thinking,
// which beats a 400 for a configuration the caller may not have chosen
// (a fallback chain or an alias can land a request on a different
// generation than the client assumed). Everything else forwards
// verbatim -- an accepting family, an unlisted family or a non-Anthropic
// model, and "disabled" (never probed) -- because for an unknown the
// upstream's own answer is the honest one, and a silent drop would hide
// a new model's real support exactly the way a stale whitelist hid
// structured output on Sonnet 5 (bedrockStructuredOutputModelSubstrings'
// doc comment).
func BedrockForwardsThinking(model, thinkingType string) (forward bool, reason string) {
	capability, known := BedrockThinkingCapabilityFor(model)
	if !known {
		return true, ""
	}
	switch thinkingType {
	case "enabled":
		if !capability.Enabled {
			return false, "the model's Claude generation rejects thinking.type enabled on Bedrock (it takes adaptive)"
		}
	case "adaptive":
		if !capability.Adaptive {
			return false, "the model's Claude generation rejects thinking.type adaptive on Bedrock (it takes enabled with budget_tokens)"
		}
	}
	return true, ""
}

// BedrockForwardsTopK decides whether the bedrock adapter sends a canonical
// ChatRequest.TopK to model through additionalModelRequestFields and, when
// it does not, why (item 11 slice S5). The same asymmetry as
// BedrockForwardsThinking: only a family the table knows rejects top_k --
// the Claude 5.x generation, a 400 "`top_k` is deprecated for this model"
// in the 2026-10-10 probes -- is dropped; an accepting family (4.6, 4.5),
// an unlisted family or a non-Anthropic model forwards verbatim.
func BedrockForwardsTopK(model string) (forward bool, reason string) {
	capability, known := BedrockThinkingCapabilityFor(model)
	if !known || capability.TopK {
		return true, ""
	}
	return false, "the model's Claude generation rejects top_k on Bedrock (deprecated for this model)"
}

// BedrockForwardsEffort decides whether the bedrock adapter sends a
// canonical ChatRequest.Effort to model as output_config.effort inside
// additionalModelRequestFields and, when it does not, why (item 11 slice
// S5). The same asymmetry as BedrockForwardsThinking: only a family the
// table knows rejects effort -- the Claude 4.5 generation, a 400 "This
// model does not support the effort parameter" in the 2026-10-10 probes --
// is dropped; an accepting family (5.x, 4.6), an unlisted family or a
// non-Anthropic model forwards verbatim.
func BedrockForwardsEffort(model string) (forward bool, reason string) {
	capability, known := BedrockThinkingCapabilityFor(model)
	if !known || capability.Effort {
		return true, ""
	}
	return false, "the model's Claude generation rejects output_config.effort on Bedrock (does not support the effort parameter)"
}

// SupportsToolResultParts reports whether provider can carry a role:"tool"
// message's Parts (item 11 slice S6): anthropic (tool_result content as a
// block array) and bedrock (toolResult.content blocks) take text, image and
// document parts; openai and openaicompat take text parts only (a Chat
// Completions tool message is a string or an array of text parts); gemini
// takes none (FunctionResponse.Response is a JSON object). media is true
// when at least one part is not text. A routing property for the dataplane
// (capabilityOKForRequest), like SupportsStructuredOutput.
func SupportsToolResultParts(provider string, media bool) bool {
	switch provider {
	case "anthropic", "bedrock":
		return true
	case "openai", "openaicompat":
		return !media
	default:
		return false
	}
}

// BedrockForwardKnownAnthropicBetas is the anthropic-beta allow-list a
// bedrock deployment with anthropic_beta_policy forward_known maps into
// Converse's additionalModelRequestFields.anthropic_beta (RFC-1 §5). Converse
// validates every value and 400s an unaccepted one with Anthropic's own
// "Unexpected value(s) … for the anthropic-beta header", so only values with
// a live 200 belong here, each dated. Probed 2026-10-10 (item 11 slice S2) on
// claude-sonnet-5-5, claude-sonnet-5, claude-sonnet-4-6 and
// claude-haiku-4-5-20251001-v1:0, all four accepting all four values below;
// rejected on all four, and so NOT listed: prompt-caching-scope-2026-01-05
// (a value Claude Code sends on every turn) and an unknown value.
var BedrockForwardKnownAnthropicBetas = []string{
	"claude-code-20250219",            // 2026-10-10: 200 on all four probed models
	"thinking-token-count-2026-05-13", // 2026-10-10: 200 on all four probed models
	"context-management-2025-06-27",   // 2026-10-10: 200 on all four probed models
	"interleaved-thinking-2025-05-14", // 2026-10-10: 200 on all four probed models
}

// BedrockForwardsAnthropicBeta reports whether value is in
// BedrockForwardKnownAnthropicBetas.
func BedrockForwardsAnthropicBeta(value string) bool {
	for _, v := range BedrockForwardKnownAnthropicBetas {
		if v == value {
			return true
		}
	}
	return false
}

// Claude 5.x request gates (round-5 research 2026-10-14, Tier A item 2;
// closes the 2026-10-08 live verification's F5). The Claude 5.x models
// validate three request shapes the 4.x generation accepted, each a 400
// upstream that Kelvran's client saw as a 502: non-default sampling, an
// assistant prefill, and thinking.type "disabled". The rules are
// provider-agnostic -- the direct API and Bedrock Converse run the same
// per-model validation; every PROVEN below is a Converse probe with
// max_tokens 1 on 2026-10-14 (no direct Anthropic key was available), and
// DOCUMENTED means the family is placed by Anthropic's own release note or
// migration guide alone, to be re-probed before relying on it. Matching is
// at a family boundary (claudeFamilyListed), so "claude-sonnet-5" never
// admits "claude-sonnet-5-5"; a family in no list is never gated, because
// for an unknown the upstream's own answer is the honest one
// (BedrockForwardsThinking's asymmetry).

// claudeFixedSamplingFamilies accept temperature only at 1 and top_p only at
// 0.99, reject the two together even at those values, and reject top_k at
// any value. PROVEN: sonnet-5-5, haiku-5-5, fable-5-1, and sonnet-5 (a
// direct Converse probe the same day: "`temperature` is deprecated for this
// model", "`top_p` is deprecated", "cannot both be specified"; F5 had seen
// temperature 0.3 rejected on 2026-10-08). DOCUMENTED: mythos-5-1 (Fable
// 5.1's twin by its release note). Opus 5.5 is
// deliberately absent: not probeable on the probing account (an explicit
// deny) and not documented as rejecting sampling, so the forward-by-default
// policy above applies -- a live 400 on Opus 5.5 sampling is the evidence
// that would add it.
var claudeFixedSamplingFamilies = []string{
	"claude-sonnet-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-mythos-5-1",
}

// ClaudeFixedTemperature and ClaudeFixedTopP are the only values the fixed-
// sampling families accept for the two fields.
const (
	ClaudeFixedTemperature = 1.0
	ClaudeFixedTopP        = 0.99
)

// SamplingDrops names, per sampling field, why the adapter about to run
// leaves the caller's value off the wire; "" means the value is forwarded.
// Returned by ClaudeSamplingDropsFor and read by both Claude-serving
// adapters (to build the wire) and by the dataplane (to log the drop with
// the same reason), so the two never disagree.
type SamplingDrops struct {
	Temperature string
	TopP        string
	TopK        string
}

// ClaudeSamplingDropsFor applies the fixed-sampling rule to the caller's
// temperature, top_p and top_k for model. Outside claudeFixedSamplingFamilies
// nothing is dropped. Inside, a temperature other than 1 and a top_p other
// than 0.99 are dropped (the model would 400); when both arrive at their
// accepted values top_p is dropped and temperature kept, because the pair is
// a 400 even then; top_k is dropped at any value. Dropping beats relaying the
// 400 for BedrockForwardsThinking's reason -- a fallback chain or an alias
// can land a request on a generation the client did not pick -- and the
// request then runs at the model's only sampling anyway. Exact float64
// equality is deliberate: JSON 1 and 0.99 decode to the same float64 as the
// two constants, and any other decoded value -- a near miss such as
// 0.9899999999 included -- is a different value, dropped like any other (the
// never-400 direction), never rounded into the accepted value and forwarded
// on the gateway's own guess.
func ClaudeSamplingDropsFor(model string, temperature, topP *float64, topK *int) SamplingDrops {
	if !claudeFamilyListed(model, claudeFixedSamplingFamilies) {
		return SamplingDrops{}
	}
	var drops SamplingDrops
	temperatureKept := false
	if temperature != nil {
		if *temperature == ClaudeFixedTemperature {
			temperatureKept = true
		} else {
			drops.Temperature = "the model's Claude generation accepts temperature only at 1 (fixed sampling)"
		}
	}
	if topP != nil {
		switch {
		case *topP != ClaudeFixedTopP:
			drops.TopP = "the model's Claude generation accepts top_p only at 0.99 (fixed sampling)"
		case temperatureKept:
			drops.TopP = "the model's Claude generation rejects temperature and top_p together even at their accepted values; temperature is kept"
		}
	}
	if topK != nil {
		drops.TopK = "the model's Claude generation rejects top_k at any value (fixed sampling)"
	}
	return drops
}

// claudeAssistantPrefillRejectingFamilies answer 400 to a conversation that
// ends on an assistant turn (the prefill idiom). PROVEN: sonnet-5-5,
// haiku-5-5, fable-5-1, and sonnet-4-6 and sonnet-5 (a direct Converse probe
// the same day: "This model does not support assistant message prefill";
// the Sonnet 5.5 migration guide names both). DOCUMENTED: mythos-5-1 (Fable
// 5.1's twin). Haiku 4.5 accepts prefill (proven, the control); Opus 5.5 is
// absent (unprobeable here, documented as accepting).
var claudeAssistantPrefillRejectingFamilies = []string{
	"claude-sonnet-4-6", "claude-sonnet-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1", "claude-mythos-5-1",
}

// ClaudeRejectsAssistantPrefill reports whether model is in
// claudeAssistantPrefillRejectingFamilies. The dataplane consults it before
// any upstream call and answers ErrAssistantPrefillUnsupported (a 400):
// unlike a sampling value, a prefill cannot be dropped without changing what
// the caller asked for.
func ClaudeRejectsAssistantPrefill(model string) bool {
	return claudeFamilyListed(model, claudeAssistantPrefillRejectingFamilies)
}

// ClaudeThinkingDisabledTreatment's answers for a canonical thinking.type
// "disabled" on a model.
const (
	// ThinkingDisabledForward: the model takes "disabled"; forwarded verbatim.
	ThinkingDisabledForward = ""
	// ThinkingDisabledBetweenTools: the model rejects "disabled" and documents
	// "between_tools" as its way to turn up-front thinking off; rewritten.
	ThinkingDisabledBetweenTools = "between_tools"
	// ThinkingDisabledDrop: thinking is always on for the model ("disabled"
	// and "between_tools" are both 400s); the field is dropped and the request
	// runs as one that never carried it.
	ThinkingDisabledDrop = "drop"
)

// claudeThinkingDisabledBetweenToolsFamilies: PROVEN sonnet-5-5 (both halves
// -- "disabled" a 400, "between_tools" a 200).
var claudeThinkingDisabledBetweenToolsFamilies = []string{"claude-sonnet-5-5"}

// claudeThinkingAlwaysOnFamilies: PROVEN fable-5-1 ("disabled" and
// "between_tools" both 400s). DOCUMENTED: opus-5-5 (its 2026-09-22 release
// note: "disabled" and "enabled" are 400s, adaptive thinking is always on),
// mythos-5-1 (Fable 5.1's twin). Haiku 5.5 and Sonnet 5 accept "disabled"
// (PROVEN; Sonnet 5 rejects "between_tools") and are in neither list.
var claudeThinkingAlwaysOnFamilies = []string{"claude-fable-5-1", "claude-mythos-5-1", "claude-opus-5-5"}

// ClaudeThinkingDisabledTreatment decides what both Claude-serving adapters
// do with a canonical thinking.type "disabled" for model, and what the
// dataplane logs about it.
func ClaudeThinkingDisabledTreatment(model string) string {
	switch {
	case claudeFamilyListed(model, claudeThinkingDisabledBetweenToolsFamilies):
		return ThinkingDisabledBetweenTools
	case claudeFamilyListed(model, claudeThinkingAlwaysOnFamilies):
		return ThinkingDisabledDrop
	default:
		return ThinkingDisabledForward
	}
}

// claudeFamilyListed reports whether model names one of families at a family
// boundary (bedrockModelFamilyMatches, which reads a bare direct-API id the
// same way as a region-prefixed, date-suffixed Bedrock one).
func claudeFamilyListed(model string, families []string) bool {
	for _, family := range families {
		if bedrockModelFamilyMatches(model, family) {
			return true
		}
	}
	return false
}
