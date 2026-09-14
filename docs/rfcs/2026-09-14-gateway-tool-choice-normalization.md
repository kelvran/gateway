# RFC: Cross-provider `tool_choice` normalization

- **Status**: accepted
- **Date**: 2026-09-14
- **Author(s)**: gateway maintainers (via `docs/upgrade-research/advanced-tool-calling-structured-output-2026-09-14.md` Finding 2)

## Summary

Add a canonical `adapter.ToolChoice` field to `ChatRequest` letting a caller force "must call this specific tool," "must call some tool," or "must not call a tool" — today Kelvran has zero such field anywhere (`grep -rn "ToolChoice\|tool_choice" gateway/internal/adapter/` returns zero matches across all five adapters), despite every provider exposing real, materially different forcing semantics.

## Motivation

`docs/upgrade-research/advanced-tool-calling-structured-output-2026-09-14.md` Finding 2 confirmed, by direct grep against the real code, that no adapter's `ToProvider`/`FromProvider` handles tool-choice forcing at all. The four providers' native semantics differ enough that a single pass-through field cannot safely normalize without explicit per-adapter mapping:

- **Anthropic**: `tool_choice.type` ∈ {`auto`, `any`, `tool`, `none`}, plus a nested `disable_parallel_tool_use` boolean. `any`/`tool` are not universally available — they 400 on specific model variants (Claude Fable 5.1, Claude Mythos 5.1).
- **Bedrock Converse**: `toolChoice` is a strict union of `any`/`auto`/`tool`; `tool` (`SpecificToolChoice`) is restricted by AWS to Anthropic Claude 3 and Amazon Nova models only.
- **Gemini**: legacy `function_calling_config.mode` ∈ {`AUTO`, `ANY`, `NONE`} plus an `allowedFunctionNames` allow-list. (A newer Interactions API reportedly adds a fourth `VALIDATED` mode — not targeted by this RFC; see Unresolved Questions.)
- **OpenAI / openaicompat**: `tool_choice` is either a bare string (`"auto"`/`"none"`/`"required"`) or an object forcing one named function.

## Detailed Design

**Canonical schema** (`internal/adapter/types.go`):

```go
type ToolChoice struct {
    Mode                   string // "auto" | "required" | "none" | "tool"
    ToolName               string // only read when Mode == "tool"
    DisableParallelToolUse bool   // only Anthropic-family adapters read this; a no-op elsewhere
}
```

Added as `ChatRequest.ToolChoice *ToolChoice`. Nil (the default, and every `ChatRequest` built before this field existed) is a silent no-op for every adapter, matching `ResponseFormat`/`CacheControl`'s own established "unset is a no-op" convention.

**Per-adapter mapping**, mirroring `adapter.SupportsStructuredOutput`'s existing per-provider/per-model whitelist shape:

- **OpenAI / openaicompat**: a custom `ToolChoice` wire type with hand-written `MarshalJSON` — bare string for `auto`/`none`/`required`; `{"type":"function","function":{"name":...}}` for `tool`.
- **Anthropic**: `{"type": <auto|any|tool|none>, "name": ..., "disable_parallel_tool_use": ...}`. Errors (never silently downgrades) when `Mode` is `any`/`tool` and `Model` matches the `claude-fable-5-1`/`claude-mythos-5-1` substrings AWS/Anthropic document as rejecting forced modes.
- **Bedrock**: `toolConfig.toolChoice` union (`{"auto":{}}`/`{"any":{}}`/`{"tool":{"name":...}}`). Errors when `Mode == "tool"` and the model is not on a new `bedrockForcedToolChoiceModelSubstrings` whitelist (Claude 3 family + Nova), mirroring `bedrockModelSupportsStructuredOutput`'s exact shape.
- **Gemini**: legacy `toolConfig.functionCallingConfig` (`mode` ∈ `AUTO`/`ANY`/`NONE`, `allowedFunctionNames` populated only for `Mode == "tool"`, containing just `ToolName`).

Every mapping errors loudly (a real Go `error` from `ToProvider`, surfaced as a 4xx to the caller) rather than silently downgrading to `auto` on an unsupported mode/model combination — a deliberate choice, not the same silent-omission shape as the disclosed Bedrock structured-output gap (`THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row), since this is new capability being added, not an existing accepted v1 scope limit being reproduced a third time.

## Drawbacks

Five separate per-adapter mapping functions to maintain, each encoding a provider's own real (and potentially-changing) restriction list. `Direction`-style enums duplicated per adapter package rather than shared, matching this codebase's existing "adapters don't import each other" convention.

## Alternatives Considered

A single shared `mapToolChoice` helper in the `adapter` package: rejected — each provider's wire shape is genuinely different (union type vs. flat object vs. string-or-object), and a shared helper would just be an interface with one implementation per adapter anyway, adding a layer of indirection with no real code reuse.

## Unresolved Questions

- Whether Kelvran's Gemini adapter should ever target the newer Interactions API's fourth `VALIDATED` mode — deferred; the legacy `AUTO`/`ANY`/`NONE` surface is well-established and stable, and canonical `ToolChoice.Mode` has no `"validated"` value to map from yet.
- Whether `DisableParallelToolUse` should also gate Bedrock's `tool`-mode Claude-3/Nova restriction more granularly (e.g. a specific sub-model within Nova) — not modeled; treated as a coarse family-level check, matching `bedrockStructuredOutputModelSubstrings`' own precedent.
