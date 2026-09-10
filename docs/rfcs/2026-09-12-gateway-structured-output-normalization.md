# RFC: Cross-provider structured-output/JSON-schema normalization

## Status

Accepted, implementing 2026-09-12.

## Summary

Adds a new opt-in `ResponseFormat` field to the canonical `ChatRequest`, translated per-adapter into each provider's own native schema-enforcement mechanism (OpenAI's `response_format`, Anthropic's `output_config.format`, Gemini's `response_schema`/`response_mime_type`, and — for a live-verified allowlist of Bedrock-hosted Claude models only — Bedrock's `additionalModelRequestFields` passthrough). Also adds a `ToolDef.Strict` marker wired into Anthropic's tool-input-validation surface, a fallback-chain capability-awareness gate so a multi-hop chain never routes a schema-bearing request to a target that can't honor it, and a cache-key fold so two requests differing only in required output shape can never collide.

## Motivation

`docs/upgrade-research/gateway-next-upgrade-round3-2026-09-11.md`'s Finding 1 confirmed Kelvran has zero equivalent to a capability all three major first-party providers now ship natively, and at least two comparable gateways (LiteLLM, Requesty) already normalize behind one request shape — a genuine, well-precedented gap, not a speculative one. A follow-up Explore pass confirmed the gap is real by direct grep: no `ResponseFormat`/`JSONSchema`-shaped field exists anywhere in `gateway/internal/adapter/types.go` today.

## Detailed Design

### Canonical schema (`gateway/internal/adapter/types.go`)

```go
type JSONSchema struct {
    Name   string          `json:"name"`
    Strict bool            `json:"strict,omitempty"`
    Schema json.RawMessage `json:"schema"`
}
type ResponseFormat struct {
    Type       string      `json:"type"`
    JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}
```

`ChatRequest.ResponseFormat *ResponseFormat` (nil = today's exact byte-identical behavior, matching every existing optional-pointer field's convention). `ToolDef.Strict bool` (wired only into Anthropic — see "Named scope limit" below).

### Live verification of Bedrock's real capability — the central finding of this RFC

Anthropic's current docs (`platform.claude.com/docs/en/docs/build-with-claude/structured-outputs`, fetched directly for this RFC) confirm the real wire shape is a top-level `output_config: {format: {type: "json_schema", schema: {...}}}` sibling of `model`/`messages`, with no beta header required. Those same docs state Bedrock support is real but restricted: *"On Amazon Bedrock, structured outputs are available for Claude Opus 4.6, Claude Sonnet 4.6, Claude Sonnet 4.5, Claude Opus 4.5, and Claude Haiku 4.5"* — a named allowlist that does **not** include Claude Sonnet 5, the model Kelvran's own `evals` judge tooling actually uses.

Per this codebase's established "verify against a real API call, never assume a sibling provider's mechanism transfers" discipline (the precedent set by `docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`'s own Bedrock `cachePoint` verification), a live Converse call was made directly (real AWS credentials, `us-east-1`, `additionalModelRequestFields: {"output_config": {"format": {"type": "json_schema", "schema": {...}}}}`) against two real models:

- `global.anthropic.claude-haiku-4-5-20251001-v1:0` (on Anthropic's Bedrock allowlist): **succeeded**, returned `{"is_fruit": true, "name": "banana"}` — schema-conforming, `stopReason: "end_turn"`.
- `global.anthropic.claude-sonnet-5` (not on the allowlist): **failed cleanly** — `ValidationException: The model returned the following errors: output_config.format: Extra inputs are not permitted`.

This confirms two things beyond what the docs alone would justify: (1) the `additionalModelRequestFields` passthrough mechanism genuinely works end-to-end for a supported model — not merely accepted, but honored; (2) AWS enforces the model restriction server-side with a real, distinguishable error, not a silent ignore. **Design consequence**: `SupportsStructuredOutput` must be a per-model check for Bedrock specifically, not a flat per-provider boolean like every other adapter gets.

### `gateway/internal/adapter/capabilities.go` (new)

```go
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
```

Bedrock's allowlist is matched by `strings.Contains` against 5 hardcoded model-family substrings (`claude-opus-4-6`, `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-opus-4-5`, `claude-haiku-4-5`), not exact equality — real Bedrock model IDs carry a region/version prefix and date/version suffix around the family name (e.g. `global.anthropic.claude-haiku-4-5-20251001-v1:0`), so exact-string matching would miss every real ID. Mirrors `fallback.go`'s existing `containsAnyKeyword` convention, the only precedent in this codebase for "match a variable string against a set of known substrings."

### Per-adapter wiring

- **OpenAI** (`openai/openai.go`): `Request.ResponseFormat`, verified against `openai-python`'s own `response_format_json_schema.py` shape (`{type, json_schema: {name, schema, strict?, description?}}`) — matches this RFC's canonical shape exactly, no translation surprises.
- **openaicompat**: identical wiring to OpenAI, plus a doc comment noting enforcement fidelity varies by self-hosted backend/runtime (mirrors the CacheControl RFC's own precedent for treating openaicompat as OpenAI-shaped-but-not-OpenAI-backed).
- **Anthropic** (`anthropic/anthropic.go`): `Request.OutputConfig` translating into `output_config.format.{type,schema}`; `Tool.Strict *bool` mirrors the `CacheControl` RFC's own tool-level marker placement (a sibling key on the tool object, only emits `"strict":true`, never a spurious `false`).
- **Gemini** (`gemini/gemini.go`): `GenerationConfig.ResponseMimeType`/`ResponseSchema`. Gemini's schema dialect is a documented OpenAPI-3.0 subset, not full JSON Schema — noted as an open fidelity caveat (not a confirmed-safe translation), since round-3 research explicitly refuted a specific claim about how Gemini handles unsupported schema keywords.
- **Bedrock** (`bedrock/bedrock.go`): `Request.AdditionalModelRequestFields map[string]any` — the first use of this escape hatch anywhere in this codebase, correcting `gateway/ARCHITECTURE.md`'s stale claim that no such field exists (fixed in the same edit, matching `AGENTS.md`'s own named "doc-vs-code staleness" gotcha-remediation pattern). Populated only when `SupportsStructuredOutput("bedrock", req.Model)` is true for the model actually being called.

### Named scope limit: `ToolDef.Strict` wired only into Anthropic, not Bedrock

Anthropic's own docs confirm `strict` shares `output_config.format`'s grammar mechanism as an independently-composable per-tool flag — cheap to wire, so it's included. Bedrock's Converse `toolSpec` schema has no live-verified confirmation of accepting an analogous field, and this RFC's own smoke test already demonstrated AWS hard-rejects unrecognized fields with a real `ValidationException` rather than ignoring them — guessing at an unconfirmed field risked breaking every tool-bearing Bedrock request outright. Left unwired for Bedrock; a real future addition once verified, not an oversight.

### Fallback-chain capability gating (`gateway/internal/gateway/dataplane/fallback.go`, `dataplane.go`, `streaming.go`)

`attemptFallbackChain` gains a trailing `capabilityOK func(d Deployment) bool` closure parameter, checked in the same position and with the same skip-without-charging-backoff semantics as the existing `deploymentCapacityOK` check — a target that can't satisfy `req.ResponseFormat` is skipped entirely, never attempted. New helper `capabilityOKForRequest(dep Deployment, req adapter.ChatRequest) bool` in `dataplane.go`, wired at both real call sites (`runMissPath` and `streaming.go`'s fallback equivalent).

**Deliberately scoped to fallback hops only, not the first-attempt router pick.** Same-model deployments are presumed operator-configured as interchangeable; cross-model fallback is where capability-blindness actually bites. The first-attempt gap (a client explicitly targeting an unsupported Bedrock model with `ResponseFormat` set) is a named, accepted gap for this pass — `bedrock.go`'s `additionalModelRequestFields` helper just omits the field silently for an unsupported model rather than erroring, since closing that specific gap would mean either a request-time validation layer (a larger change) or degrading response quality expectations mid-request, neither scoped here.

### Cache-key fold (`gateway/internal/cache/key.go`)

`Key`/`NormalizedKey` both gain a trailing `responseFormatFingerprint string` parameter — the raw JSON marshal of `req.ResponseFormat` when non-nil, else `""`. Unlike `guardrailPolicyVersion` (always real, never empty), the fold is **conditional**: the `\x00response_format=...` segment is only appended when `responseFormatFingerprint != ""`, never emitted as an empty segment. This preserves an exact backward-compatibility guarantee — every existing request with no `ResponseFormat` produces the byte-identical key it produced before this parameter existed, not merely "a key that's stable going forward." A request with `ResponseFormat` set gets a genuinely different key than the same request without it, preventing a schema-conforming JSON response from ever being served to a caller that asked for (or didn't ask for) a differently-shaped output.

## Drawbacks

- Bedrock's per-model allowlist is a hardcoded substring list that will silently understate support if Anthropic extends the whitelist to new model families (e.g. a future Sonnet 6) without a corresponding Kelvran update — a real maintenance cost, mitigated only by this RFC's own doc comment naming exactly when/why it was derived and pointing at the live-verification method to re-run.
- No client-side JSON-Schema validation fallback exists for providers/models without native enforcement (deliberate — see Alternatives Considered) — a request against, say, an unsupported Bedrock model with `ResponseFormat` set simply gets no enforcement at all on the first attempt, silently. This is a disclosed v1 gap, not a silent promise-breaking one, but it is still a real correctness gap until either the allowlist grows or a future pass adds request-time rejection.

## Alternatives Considered

**A client-side JSON-Schema validation fallback dependency** (LiteLLM's own documented pattern for providers lacking native enforcement). Rejected for v1 — no JSON Schema validation library exists in `go.mod`/`go.sum` today, and adding one would break the "zero new dependencies" goal the CacheControl RFC established as a real verification target (confirmed clean: `go mod tidy` produced no diff for this RFC).

**Wiring `guardrail.Engine` to validate tool-call arguments against the schema**, since Kelvran's guardrails already inspect tool-call arguments for PII/injection. Named as a real, natural integration point by the round-3 research's own open questions, but deferred — it would require `guardrail.Engine` to gain a structural-JSON-Schema-conformance capability it doesn't have today, a second-RFC-sized change.

## Unresolved Questions

- Should Bedrock's allowlist be periodically re-verified against Anthropic's docs on some cadence, or only reactively when a user reports an unexpectedly-rejected model? Not decided here.
- Should the first-attempt gap (client targets an unsupported Bedrock model directly, not via fallback) eventually reject at request-validation time instead of silently omitting the field? Named as real future work, not decided here.

## Verification

`cd gateway && go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod tidy` — clean except the two pre-existing rootless-Docker integration-test failures (`cmd/gateway`'s Redis integration test, `internal/ratelimit/redislimiter`'s `TestMain`) this session has repeatedly confirmed unrelated to any change in this repo; `go mod tidy` produced an empty diff (no new dependency).

New/extended tests: per-adapter golden/regression fixtures proving `ResponseFormat` translates correctly into each provider's native shape, and a nil-`ResponseFormat` case proving byte-identical output to the pre-existing fixtures; `capabilities_test.go` proving Sonnet 5 is excluded and a Haiku-4.5-style Bedrock ID is included, and every non-Bedrock provider is unconditionally supported; `fallback_test.go::TestAttemptFallbackChainSkipsTargetLackingRequiredCapability`; `cache/key_test.go` cases proving two `ResponseFormat`-differing requests produce different keys and a nil-`ResponseFormat` request is byte-identical to the pre-existing key.

Sanity-checked-by-breaking, twice: (1) forced `capabilityOKForRequest` to always return `true`, confirmed the new fallback test failed for the exact predicted reason, restored; (2) dropped the `response_format` fold from `Key`'s `Fprintf` call (left `NormalizedKey`'s intact), confirmed the new key-collision test failed for the exact predicted reason, restored.
