- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Implement a real `gateway/internal/adapter/gemini` adapter (non-streaming + streaming), closing the last stub adapter `PRD.md` names in its explicit v1 scope line: *"a unified request/response schema across OpenAI, Anthropic, Gemini, and OpenAI-compatible self-hosted runtimes."* `gemini.go` is currently a full, honest stub — `ToProvider`/`FromProvider` both unconditionally error, per `docs/rfcs/2026-09-02-initial-code-scaffolding.md`'s original scoping ("one near-identity adapter [openai] and one genuine-translation adapter [anthropic]... a third real adapter would not prove anything new about the seam"). That scoping call was correct for the scaffolding pass, but `PRD.md`'s v1 line has named Gemini specifically since before this session started — this closes that real, still-open gap. `gateway/internal/adapter/bedrock` remains a stub and is **deliberately not addressed here** — Bedrock is not named in `PRD.md`'s v1 line, and treating it identically to Gemini would be a scope decision this RFC doesn't have standing to make; flagged for a founder call, not silently bundled in.

## Motivation

Confirmed directly against the live tree: `gemini.go` (39 lines) is the same shape `openaicompat.go` was before its own recent RFC — both methods return `fmt.Errorf("adapter %q not implemented...")`. `PRD.md` line 26 explicitly names Gemini in the v1 scope line; this is committed v1 work, not a speculative addition.

Grounded via a 3-angle dynamic-workflow research pass (wire format, streaming/auth, function-calling/finish-reasons) plus independent, direct re-verification against Google's real, live API discovery document (`https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta`, fetched and parsed directly — not secondhand) before writing this RFC, since the research's own claims about `FunctionCall`/`FunctionResponse`'s exact field shape were the single highest-risk-of-being-wrong claim and needed to be checked against the actual schema, not documentation prose. Two real findings came out of that independent check that the research pass itself did not surface:

1. **`FunctionCall`/`FunctionResponse` both have an optional `id` field**, confirmed directly from the discovery schema (`FunctionResponse.id`: "Optional. The identifier of the function call this response is for. Populated by the client to match the corresponding function call `id`.") — better than the research synthesis assumed (it recommended "pass it through... and fall back to positional order... don't build a matching algorithm"). Since it's optional and not guaranteed to always be echoed by the model, this RFC still needs a `name`-resolution path regardless (see Detailed Design) — the `id` match is an enhancement layered on top, not a replacement for it.
2. **A real architectural mismatch with Kelvran's existing per-deployment config**, found only by reading `gateway/internal/gateway/dataplane/dataplane.go` directly (not part of the grounding research's scope): every other provider Kelvran supports (OpenAI, Anthropic, openaicompat) uses **one URL for both buffered and streaming calls**, differing only via a body `"stream"` flag — `NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller` both just POST to `dep.BaseURL` verbatim. Gemini's real API uses **two different URL method suffixes** — `POST .../{model}:generateContent` (buffered) vs `POST .../{model}:streamGenerateContent?alt=sse` (streaming), confirmed directly from the discovery document's `resources.models.methods` — not a body-flag difference at all. `Deployment.BaseURL` has no field for a second URL, and adding one would be a config-schema change affecting every provider for a need only Gemini has. See Detailed Design for the resolution.

## Detailed Design

### Field mapping (`ToProvider`/`FromProvider`)

Confirmed directly against the live discovery schema (`Content`, `Part`, `FunctionCall`, `FunctionResponse`, `GenerationConfig`, `UsageMetadata`, `Candidate` — fetched 2026-09-04):

| Canonical | Gemini native | Notes |
|---|---|---|
| `Message{role:"system"}` | `systemInstruction` (top-level `Content`) | Gemini's `contents[].role` accepts only `"user"`/`"model"` (confirmed: `Content.role`'s description says exactly this) — no in-array system role at all, same hazard class Anthropic's adapter already solved; hoist out, join with `"\n\n"` (matching the Anthropic adapter's own convention). |
| `Message{role:"user"/"assistant"}` | `Content{role:"user"/"model", parts:[...]}` | `"assistant"` → `"model"`; text content becomes a `{"text": ...}` part. |
| `Message{role:"tool"}` | `Content{role:"user", parts:[{"functionResponse": {...}}]}` | No `"tool"`/`"function"` role exists in `contents[].role` — a function result is a `user`-role content carrying a `functionResponse` part (real, sourced Gemini convention). |
| `ToolCall{ID, Name, ArgumentsJSON}` | `Part.functionCall{id, name, args}` | `args` is a native JSON object (`additionalProperties: any`), not a string — parse `ArgumentsJSON` via `json.Unmarshal` into `map[string]any`, exactly the Anthropic adapter's existing `Input map[string]any` pattern (`anthropic.go` lines 139-149). `id` is always set from the canonical `ToolCall.ID` (always non-empty in canonical messages — see Alternatives Considered #3 for what happens on the reverse direction when the model never echoes one). |
| Tool-result `Message{role:"tool", Content, ToolCallID}` | `Part.functionResponse{id, name, response}` | **Two real hazards, not one:** (a) `name` is **required** on `FunctionResponse` but the canonical tool-role message only carries `ToolCallID`, not the originating call's `Name` — `ToProvider` must build a `map[string]string` (`ToolCall.ID` → `ToolCall.Name`) by scanning every prior assistant message's `ToolCalls` in order before it can honestly populate this field; (b) `response` must be a JSON **object** (`additionalProperties: any`), never a bare string, unlike every other provider Kelvran supports where a tool result is plain text — wrapped as `{"result": content}`, an explicit, named convention (not an arbitrary key choice — "result" is Gemini's own example key in the schema's field description). `id` is set from `ToolCallID` when non-empty (an enhancement over positional-only matching, now that the schema confirms it's real). |
| `ToolDef{Name, Description, ParametersJSON}` | `Tool.functionDeclarations[].{name, description, parameters}` | `parameters` is `map[string]any` (parsed, not raw-passthrough) — same parse-at-the-boundary pattern as Anthropic's `InputSchema`. |
| `ChatRequest.Temperature`/`MaxTokens` | `generationConfig.temperature`/`maxOutputTokens` | Direct 1:1, both confirmed real field names. |
| `ChatResponse.Usage` | `usageMetadata.{promptTokenCount, candidatesTokenCount, totalTokenCount}` | Direct 1:1, confirmed real field names (not `completion_tokens`-shaped like OpenAI). |
| `Choice.FinishReason` | `candidates[].finishReason` | See finish-reason mapping below — a 22-value real enum, not the ~6-value set OpenAI/Anthropic use. |

### finishReason mapping

Confirmed the full, real 22-value enum directly from the discovery schema (`Candidate.finishReason.enum`). Mirrors Anthropic's `finishReasonFromStopReason` pattern (`anthropic/stream.go`) — a plain function, forward-compatible default (unrecognized/future values pass through the raw string rather than erroring):

- `STOP` **with** a `functionCall` part present in the candidate → canonical `"tool_calls"` (Gemini has no distinct "I stopped to call a tool" reason the way OpenAI's `tool_calls` value is — must be detected by scanning parts, not by `finishReason`'s string value, the same class of caveat `openaicompat`'s own `Choice.FinishReason` doc comment already warns about for TGI).
- `STOP` (no `functionCall`) → `"stop"`.
- `MAX_TOKENS` → `"length"`.
- `SAFETY`, `PROHIBITED_CONTENT`, `SPII`, `BLOCKLIST` → `"content_filter"`.
- `MALFORMED_FUNCTION_CALL`, `UNEXPECTED_TOOL_CALL`, `TOO_MANY_TOOL_CALLS`, `MISSING_THOUGHT_SIGNATURE`, `MALFORMED_RESPONSE` → `FromProvider` returns a **typed error**, not a fake successful `Choice` — these mean the model's own tool-call machinery broke, not that it produced a usable answer; matches this codebase's "never fabricate success" convention (`Run.cost_usd: None`, `EvalCase` tier-spoofing rejection, etc.).
- `RECITATION`, `LANGUAGE`, `OTHER`, `ESCALATION`, `PUP_LIMITED_DISABLED`, `FINISH_REASON_UNSPECIFIED`/empty → `"stop"`, forward-compatible pass-through-by-default (the raw value is still visible in logs via the request/response, not this function's job to preserve further).
- `IMAGE_SAFETY`, `IMAGE_PROHIBITED_CONTENT`, `IMAGE_OTHER`, `NO_IMAGE`, `IMAGE_RECITATION` → `"stop"` (image-generation-specific reasons; this adapter's first pass is text/tool-use only, per Alternatives Considered — never reachable in practice today, handled only so the mapping function is total, not partial).

### Streaming: reuses `streaming.SSEEvent`/`Reader` as-is; `Decode` never signals `done`

Confirmed real, genuine SSE framing (`data:` lines) via the discovery document's own method definition and independently verified `streamGenerateContent` and `generateContent` share the **exact same response schema** (`GenerateContentResponse`) — Gemini has no separate "chunk" type; each SSE frame is a complete, independently-parseable `GenerateContentResponse`, simplifying `gemini/stream.go` to reuse the same `Response`/`Candidate`/`Content`/`Part` structs `gemini.go` already defines for the buffered path, just parsed per-event. `usageMetadata` rides on the final chunk (confirmed: `Candidate.finishReason` and `usageMetadata` are both marked "Output only," populated once the model has finished, not per-token) — same "may arrive only on the last chunk" contract `streaming.ChatCompletionChunk.Usage`'s existing doc comment already states generically.

**No `[DONE]`-style terminal sentinel exists.** Confirmed by directly reading `gateway/internal/gateway/dataplane/streaming.go`'s real streaming loop (lines 274-300): it already breaks on `io.EOF` from `reader.Next()` **regardless of whether `Decode` ever returns `done=true`** — this is not a new mechanism this RFC needs to add, it's an existing, already-general property of the loop (confirmed by direct code read, not assumed). `gemini/stream.go`'s `Decode` therefore always returns `done=false`; the stream ends when the upstream connection closes and `Reader.Next()` returns `io.EOF`, exactly like how Kelvran's own SSE `Reader` is documented to behave for exactly this case.

### The real architectural gap: one `BaseURL`, two real Gemini endpoints

Every existing provider's `Deployment.BaseURL` is used identically for both buffered and streaming calls (`dataplane.go`'s `NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller`, confirmed by direct read). Gemini's real API needs `POST .../{model}:generateContent` for buffered and `POST .../{model}:streamGenerateContent?alt=sse` for streaming — two different URL paths, not a body flag. **Resolution: derive the streaming URL from the configured (buffered) `base_url` at call time**, via a small new provider-keyed function in `dataplane.go` (mirroring `setUpstreamAuthHeaders`'s existing per-provider-switch precedent exactly, not a new abstraction):

```go
// streamUpstreamURL returns the URL to use for a streaming upstream call,
// deriving it from dep.BaseURL for providers whose streaming endpoint is a
// genuinely different URL (Gemini) rather than a body-flag difference
// (every other provider today) -- see docs/rfcs/2026-09-04-gemini-adapter.md.
func streamUpstreamURL(dep Deployment) (string, error) {
	if dep.Provider != "gemini" {
		return dep.BaseURL, nil
	}
	u, err := url.Parse(dep.BaseURL)
	if err != nil {
		return "", fmt.Errorf("parsing gemini base_url %q: %w", dep.BaseURL, err)
	}
	if !strings.HasSuffix(u.Path, ":generateContent") {
		return "", fmt.Errorf("gemini base_url %q must end in \":generateContent\"", dep.BaseURL)
	}
	u.Path = strings.TrimSuffix(u.Path, ":generateContent") + ":streamGenerateContent"
	q := u.Query()
	q.Set("alt", "sse")
	u.RawQuery = q.Encode()
	return u.String(), nil
}
```

Called from `NewHTTPUpstreamStreamCaller` in place of the current bare `dep.BaseURL` reference — a one-call-site change, zero config-schema change, zero effect on any other provider (the early `return dep.BaseURL, nil` for every non-Gemini provider keeps this function's behavior for them byte-identical to today). Operators configure `base_url` as the buffered `:generateContent` endpoint, exactly matching every other provider's existing `base_url` convention (the full literal upstream URL) — streaming's URL is derived, never separately configured.

### Authentication

Confirmed via research and cross-checked against the discovery document's parameter conventions: Gemini accepts an API key via the `x-goog-api-key` header (as well as a `?key=` query param, which this RFC doesn't use — a header keeps the URL-derivation logic above free of query-string collisions with `alt=sse`). One new case in `setUpstreamAuthHeaders` (`dataplane.go`), mirroring the existing `"anthropic"` case exactly:

```go
case "gemini":
    httpReq.Header.Set("x-goog-api-key", dep.APIKey)
```

### `dataplane.go`'s `responseUnmarshalers` map

One new entry, identical shape to the existing three:

```go
"gemini": func(b []byte) (any, error) {
    var r gemini.Response
    if err := json.Unmarshal(b, &r); err != nil {
        return nil, fmt.Errorf("unmarshaling gemini response: %w", err)
    }
    return &r, nil
},
```

## Drawbacks

- **Real added complexity vs. every other adapter**: the `streamUpstreamURL` per-provider URL-derivation function is a genuinely new kind of special-case in `dataplane.go` (every prior provider needed only an auth-header special-case). Accepted because the alternative (a new `Deployment.StreamBaseURL` config field, unused by every non-Gemini provider) is worse — a config-schema change for a need exactly one provider has, versus a small, well-contained, clearly-commented function.
- `FunctionResponse.name` resolution requires `ToProvider` to scan message history for a matching `ToolCall.ID` before it can honestly build the response part — real, necessary complexity this RFC's own grounding research didn't originally surface (found only by direct schema inspection). If a `role:"tool"` message's `ToolCallID` doesn't match any prior `ToolCall.ID` in the same request (a malformed caller input), `ToProvider` returns a typed error rather than sending Gemini a `functionResponse` with an empty/guessed `name`.
- No multi-modal, code-execution, grounding/search-tool, or safety-rating support this pass — see Alternatives Considered.
- Bedrock's stub is untouched — a deliberate non-decision, not silently bundled with Gemini's real gap (see Summary).

## Alternatives Considered

1. **Add a second `Deployment.StreamBaseURL` config field** — rejected; a schema change every other provider would carry but never use, versus a self-contained per-provider URL-derivation function with zero schema impact.
2. **Require operators to configure two separate `gemini` deployments (one streaming, one buffered)** — rejected; breaks the existing one-`Model`-to-one-set-of-deployments mental model and would require the router/fallback logic to somehow know which deployment variant to pick per request mode, a much larger change for no real benefit over deriving the URL.
3. **Never populate `functionCall`/`functionResponse.id`, rely on positional/name-only matching** (the grounding research's own original recommendation) — rejected once the live schema confirmed `id` is real and documented as exactly this correlation mechanism; using it when available is strictly more correct than positional-only matching, at zero extra cost (the canonical `ToolCall.ID` is always available to pass through).
4. **Treat Bedrock identically to Gemini in this same pass** — rejected; `PRD.md`'s v1 line names Gemini specifically, not Bedrock. Silently scope-expanding to Bedrock would be a real, undiscussed decision, not an obvious inclusion.
5. **Support multi-modal parts (`inlineData`/`fileData`) now, since the schema is already visible** — rejected; no canonical `Message` field exists for non-text content today, and canonical-schema changes affect every adapter, not just Gemini's — out of this RFC's scope, named as a real future item, not silently dropped.

## Unresolved Questions

- Whether `candidateCount > 1` (Gemini's native support for multiple response candidates) is ever worth wiring to canonical `Choices[]` — deferred; no real caller needs more than one candidate today (YAGNI).
- Whether `thoughtSignature` (Gemini-3 "thinking" models' opaque continuation token) ever needs a canonical home — deferred; no canonical field exists for it, and guessing its shape now would be speculative.
- Vertex AI's OAuth2/service-account credential flow (as opposed to the plain API-key `generativelanguage.googleapis.com` surface this RFC targets) is a separate, larger auth-transport question — not addressed here.

## Research Trail

Grounded via a 3-angle dynamic-workflow research pass (wire-format shape, streaming framing + auth, function-calling + finish-reasons) plus a synthesis stage that independently re-verified the highest-risk claims against live sources. Before writing this RFC, further independently re-verified directly: fetched and parsed `https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta` (Google's own live API discovery document) for the exact `FunctionCall`/`FunctionResponse`/`Content`/`Part`/`GenerationConfig`/`UsageMetadata`/`Candidate` schemas and the real `models.generateContent`/`models.streamGenerateContent` HTTP method/path definitions — catching that `FunctionCall`/`FunctionResponse` both have a real (if optional) `id` field, contradicting the research synthesis's own more conservative recommendation. Also read `gateway/internal/gateway/dataplane/dataplane.go`, `gateway/internal/gateway/dataplane/streaming.go`, `gateway/internal/adapter/anthropic/anthropic.go`, `gateway/internal/adapter/openai/stream.go`, `gateway/internal/streaming/{types.go,reader.go}`, and `gateway/internal/adapter/types.go` directly to confirm Kelvran's own existing conventions before mapping Gemini onto them — the one-`BaseURL`-per-deployment architectural gap was found this way, not by the grounding research (which was scoped to the Gemini API itself, not Kelvran's own dataplane assumptions).
