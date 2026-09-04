- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: Claude (agentic session), grounded via a 3-angle dynamic-workflow research pass + independent synthesis re-verification

## Summary

Implement `gateway/internal/adapter/openaicompat` for real — currently a full stub (`ToProvider`/`FromProvider` both unconditionally error, no `NewStreamDecoder` at all) — as a near-verbatim copy of the working `openai` adapter (`openai.go`/`stream.go`), closing both a non-streaming and a streaming gap in one pass for self-hosted OpenAI-compatible runtimes (vLLM, Ollama, TGI, llama.cpp, LocalAI). Add the one real piece of wiring this needs outside the adapter package: a `"openaicompat"` entry in `dataplane.go`'s `responseUnmarshalers` map.

## Motivation

`openaicompat.go`'s own doc comment already argues it's the adapter most worth implementing for real first, since self-hosted runtimes already speak the canonical (OpenAI Chat-Completions-shaped) dialect nearly natively — a claim this RFC's grounding research confirmed directly rather than assumed: the SSE framing, `[DONE]` sentinel, `stream_options.include_usage` mechanics, and tool-call JSON shape (an array of `{id, type, function:{name, arguments-as-string}}`) are uniformly OpenAI-compatible across all five runtimes checked, verified against each project's real source code (not just documentation, which is often silent on these specifics).

The research also found real, sourced compatibility risks at the response-*content* level (not the wire-*shape* level) worth naming explicitly:
- **`finish_reason` is not a closed set.** vLLM can emit `"abort"`/`"repetition"`; TGI can emit `"stop_sequence"` and — notably — never emits `"tool_calls"` even for a genuine tool-call response.
- **Tool-calling is opt-in and model/parser-gated** (vLLM needs `--enable-auto-tool-choice` + a named parser; llama.cpp needs `--jinja`; LocalAI needs per-backend parser config) and fails *soft*, not hard, when misconfigured — a target that doesn't actually support tool-calling silently returns plain text or malformed arguments, never an error.
- **The `model` request field's routing role varies**, from enforced-and-404-on-mismatch (vLLM, Ollama, LocalAI, llama.cpp in router mode) to purely decorative (llama.cpp's default single-model mode, which ignores the field entirely and always serves its one loaded model).
- **Unknown/extra response fields appear** (vLLM's `stop_reason`/`token_ids`/`kv_transfer_params`; llama.cpp's `timings`/`reasoning_content`).

Checked directly against the real code: every one of these is **already handled correctly by simply copying `openai.go`/`stream.go`'s existing design**, with zero additional code needed — `FinishReason` is already a bare `string`/`*string`, not a closed Go enum; Go's `encoding/json` already ignores unrecognized fields by default; nothing in `dataplane.go`/`streaming.go` branches on `finish_reason`'s value at all (only presence/nil checks). The only genuinely new requirement is a documentation note warning future callers never to detect tool calls by checking `finish_reason == "tool_calls"`.

## Detailed Design

### Scope

**In scope**: a real `openaicompat.Adapter` (non-streaming + streaming), mirroring `openai`'s implementation; the one required `dataplane.go` wiring addition.

**Explicitly out of scope**:
- Per-runtime detection/configuration (vLLM vs. Ollama vs. TGI vs. llama.cpp vs. LocalAI) — the wire-protocol shape is uniform across all five; runtime-specific server flags (tool-call parser selection, `--jinja`, etc.) are an operator deployment concern, not adapter code.
- `finish_reason` value validation/whitelisting — the field is already unconstrained, and nothing in this codebase branches on its value beyond presence.
- Any abstraction over `model`-field routing-enforcement differences — `Deployment.UpstreamModel` is already sent verbatim by the generic dataplane path; whether the target server enforces it is a deployment-configuration concern.
- Bedrock's binary EventStream framing and Gemini's native implementation — separate adapters, separate wire formats, unrelated to this RFC.

### Implementation: near-verbatim copy, not a redesign

`openaicompat/openaicompat.go` and `openaicompat/stream.go` duplicate `openai.go`'s/`stream.go`'s types and logic verbatim (`Request`/`Message`/`ToolCall`/`FunctionCall`/`Tool`/`FunctionDef`/`Response`/`Choice`/`Usage`, `ToProvider`/`FromProvider`/`toolCallsToProvider`/`toolCallsFromProvider`; `nativeStreamChunk`/`nativeStreamChoice`/`nativeStreamDelta`/`nativeToolCallDelta`/`nativeFunctionDelta`, `streamDecoder`/`NewStreamDecoder`/`Decode`/`toCanonicalDelta`) — **duplicated, not shared via a common types package**, matching this codebase's existing, unbroken convention that every adapter package is self-contained (confirmed: `anthropic` imports nothing from `openai`, and no adapter package imports another adapter package's types anywhere in the codebase). Introducing a shared types package now would be a new architectural pattern with zero precedent, and would make it harder to let `openaicompat` diverge later if a specific self-hosted quirk ever needs handling that shouldn't leak back into `openai`.

One doc-comment-only addition, not a code change: `openaicompat.Choice.FinishReason`/`nativeStreamChoice.FinishReason` get a note that self-hosted runtimes may emit non-OpenAI values (`"abort"`, `"repetition"`, `"stop_sequence"`) and that TGI in particular never emits `"tool_calls"` even when tool calls are present — so callers must detect tool calls by the presence of `message.tool_calls`/`delta.tool_calls`, never by `finish_reason` value.

### The one required wiring addition outside the adapter package

`gateway/internal/gateway/dataplane/dataplane.go`'s `responseUnmarshalers` map (currently exactly two entries: `"openai"`, `"anthropic"`) has no `"openaicompat"` entry — confirmed directly: `NewHTTPUpstreamCaller`'s non-streaming path returns `"no response unmarshaler registered for provider %q"` for `openaicompat` today, regardless of how correctly `BaseURL`/`UpstreamModel`/`APIKey` are configured. A third closure is added, mirroring the existing two exactly:

```go
"openaicompat": func(b []byte) (any, error) {
	var r openaicompat.Response
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("unmarshaling openaicompat response: %w", err)
	}
	return &r, nil
},
```

**Confirmed, not assumed, that nothing else needs to change:**
- `cmd/gateway/main.go`'s adapter registry already has `"openaicompat": openaicompat.New()` registered — no change needed.
- `dataplane.go`/`streaming.go`'s adapter lookup (`p.adapters[dep.Provider]`) and the `streaming.StreamingAdapter` type-assertion (`a.(streaming.StreamingAdapter)`) are both pure, provider-name-agnostic Go mechanisms with no allowlist or provider-specific branching — adding `NewStreamDecoder()` to `openaicompat.Adapter` is sufficient by itself to make the streaming assertion succeed.
- `setUpstreamAuthHeaders`'s only provider-specific branch is `"anthropic"` (a non-standard `x-api-key` header); `openaicompat` falls into the `default: Authorization: Bearer` branch already, matching every self-hosted runtime's expected auth convention.
- `BaseURL` is already used verbatim as the full POST endpoint URL, with no host-prefix/path-suffix logic — routing to any self-hosted `base_url` already works with zero code changes.

## Drawbacks

- Duplicating ~250 lines of near-identical code between `openai` and `openaicompat` is a real, accepted maintenance cost — a future fix to one (e.g. a new OpenAI wire-format field) won't automatically propagate to the other. Accepted because it matches this codebase's existing convention and preserves the ability for the two adapters to diverge safely later.
- The permissive `finish_reason`/unknown-field handling that makes this adapter robust against self-hosted quirks is a property of `openai.go`'s existing design, not a new, deliberately-tested defense — there's no fixture proving "an unrecognized `finish_reason` value doesn't break anything," since nothing in the codebase currently inspects that value at all. If a future change ever adds `finish_reason`-based branching, it needs to account for this from day one.
- No real per-runtime compatibility testing was done (no live vLLM/Ollama/TGI/llama.cpp/LocalAI instance was exercised) — this RFC's confidence rests on source-code research into each runtime's real behavior, not an end-to-end smoke test against a live self-hosted server. A real integration test against an actual self-hosted runtime remains a defensible future addition, not done here.

## Alternatives Considered

1. **A shared types package for OpenAI-wire-format-compatible adapters** — rejected: no precedent anywhere in this codebase, and would reduce the two adapters' ability to diverge safely if a self-hosted-specific quirk ever needs isolated handling.
2. **Per-runtime detection/configuration** (a `runtime: vllm|ollama|tgi|...` config field driving different validation/parsing) — rejected: the wire *shape* is uniform; server-side capability gating (tool-calling support, `model` enforcement) is real but is an operator deployment concern the gateway has no way to detect or influence over plain HTTP.
3. **Closed-enum `finish_reason` validation** — rejected: would require rejecting or coercing real, legitimate values (vLLM's `"abort"`, TGI's `"stop_sequence"`) that this RFC's research confirmed are real, current, sourced behavior, not bugs to guard against.
4. **Do nothing** (leave the stub) — rejected: `openaicompat.go`'s own doc comment already names this as the highest-value adapter to implement for real, and the research confirmed the implementation risk is low (wire-shape-uniform) once done as a near-verbatim copy.

## Unresolved Questions

- Should `openai.go`'s own doc comment also get the `finish_reason`-is-open-ended warning, since real OpenAI itself could theoretically add new values in the future? Left as a follow-up doc touch, not blocking this RFC.
- Should a future regression fixture demonstrate the "permissive decoding of an unknown field" property explicitly (e.g. a `response_openaicompat_native.json` variant carrying a vLLM-shaped extra field like `stop_reason`), making it a checked, visible regression rather than an implicit property of `encoding/json`'s default behavior? Named as a real, worthwhile future addition, not done in this pass.
- Whether a real integration test against one live self-hosted runtime (vLLM or Ollama, likely) is worth adding later, once this project has a CI environment that can host one — deferred, no such environment exists today.
