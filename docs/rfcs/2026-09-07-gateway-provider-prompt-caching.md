# RFC: Provider-side opt-in prompt caching (Anthropic, Bedrock, OpenAI)

## Status

Accepted, implementing 2026-09-07.

## Summary

Adds a new opt-in `CacheControl` marker (`{TTL, Key}`) to the canonical `Message` and `ContentPart` types, and wires it through the three adapters whose providers document a real, currently-unreachable opt-in prompt/context-caching mechanism — Anthropic's per-content-block `cache_control`, Bedrock's `cachePoint` checkpoints, and OpenAI's request-level `prompt_cache_key` routing hint. Gemini is deliberately excluded: it has no request-level field for any caching mechanism at all (see "Why Gemini is excluded" below). Anthropic's native `System` field is restructured from a plain string to an array of typed blocks — the same shape Bedrock's `System` already has — so both providers can carry a caching marker on system content, not just on regular messages.

## Motivation

`docs/upgrade-research/gateway-provider-prompt-caching-2026-09-07.md` (Tier 1, "Kelvran's canonical schema has no field for any provider's opt-in caching signal") confirmed, by a repo-wide `grep` returning zero matches, that Kelvran's canonical `ChatRequest`/`Message`/`ContentPart` schema has no field for Anthropic's `cache_control`, Bedrock's `cachePoint`, or OpenAI's `prompt_cache_key` — meaning three of the four providers' opt-in caching mechanisms are not merely unexploited but **structurally unreachable** through Kelvran today, regardless of what the origin client wants, because the opt-in signal has nowhere to travel through the schema. That research pass's own Recommendation 3 named this exact gap as "a legitimate, evidence-backed candidate for a future roadmap item... [needing] its own RFC" rather than being built inline — this RFC is that follow-up.

The fully-automatic mechanisms (OpenAI's own implicit caching pre-GPT-5.6, Gemini's implicit caching, Bedrock's own implicit caching for Anthropic/Nova models) already work transparently on Kelvran's cache-miss traffic with zero code change, per that research's own confirmed byte-fidelity findings (`normalizeMessages` operates on a copy; `guardrails.Check` is read-only; `ToProvider` is byte-faithful for text) — this RFC does not touch or re-verify that path. It targets only the **opt-in** mechanisms that need an explicit marker somewhere in the request.

## Detailed Design

### The real granularity mismatch, and the call this RFC makes

Re-verifying each provider's own real mechanics directly (not just restating the research doc's framing) surfaces a distinction sharper than "fine-grained vs. coarse":

| Provider | What the marker attaches to | Mechanical shape |
|---|---|---|
| **Anthropic** | A specific content block, directly | `cache_control` is a *property of that block itself* (`{"type":"text","text":"...","cache_control":{"type":"ephemeral"}}`) — addressable per block, no separate marker object needed. |
| **Bedrock** | A position in a content array | `cachePoint` is a *separate, standalone block* inserted after the content to be cached (`[{"text":"..."},{"cachePoint":{"type":"default"}}]`) — a checkpoint boundary ("cache everything up to here"), not a property any single block owns. Real per AWS's own docs: checkpoints are evaluated in `tools → system → messages` order and can appear in any of the three sections, so this is genuinely closer to Anthropic's per-section granularity than "whole-request" — it is coarser only in the sense that a checkpoint marks a *boundary*, not an individually-addressable block. |
| **OpenAI** | Nothing in the content at all | `prompt_cache_key` is a top-level request field with no relationship to *which* content is cached — OpenAI's own caching is fully automatic; the key only ever influences *routing* (which warm machine a request lands on). There is no "mark this block" concept to collapse into at all — this is the genuinely coarse case. |

Given that, this RFC's real call is: **expose the marker at both `Message` and `ContentPart` granularity in the canonical schema (a superset of what any single provider needs), and let each adapter translate it into whatever mechanical shape that provider actually uses** — a block property for Anthropic, a standalone checkpoint block for Bedrock, or a plain existence-check-plus-value-extraction for OpenAI. This is the same superset-schema pattern `docs/rfcs/2026-09-06-gateway-multimodal-content.md` already established for `ContentPart` itself (canonical schema carries the union of every provider's real capability; adapters degrade gracefully, never silently). The alternative — modeling the marker as a single request-level or message-level-only boolean — was rejected because it would under-serve Anthropic's and Bedrock's real per-block/per-section addressability for no compensating simplicity benefit (the per-block plumbing this RFC adds is small; see Drawbacks).

**Message-level vs. ContentPart-level, and why both:** `Message.CacheControl` marks "cache everything this message becomes" (the common case — a large stable system prompt, a large tool result, an entire early conversation turn). `ContentPart.CacheControl` marks a single part independently of its parent message (e.g., a large attached PDF inside a message that also has short, non-cache-worthy lead-in text) — this composes for free with the multimodal RFC's own already-additive `Parts` field, at the cost of one more optional pointer field per part.

### Canonical schema (`gateway/internal/adapter/types.go`)

```go
// CacheControl is an opt-in marker requesting provider-side prompt
// caching for the message or content-part it's attached to, per
// docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md. A nil
// CacheControl (the default on every Message/ContentPart) is a silent
// no-op for every adapter -- it changes nothing about how the request
// is forwarded, matching this schema's existing optional-field
// convention (ContentPart/Parts's own additive, no-op-when-empty
// precedent from docs/rfcs/2026-09-06-gateway-multimodal-content.md).
type CacheControl struct {
	// TTL requests a non-default cache lifetime. Only Anthropic (direct,
	// and Bedrock's Anthropic-family cachePoint) reads this -- "" means
	// that provider's own default (5 minutes); "1h" opts into the
	// longer, more expensive lifetime both document. Every other
	// provider/model family ignores TTL as a no-op, never an error.
	TTL string `json:"ttl,omitempty"`
	// Key is a stable, caller-supplied identity (e.g. a session or
	// conversation ID) that only OpenAI's adapter reads, to set its
	// top-level prompt_cache_key routing hint. Ignored (a no-op) by
	// every other adapter.
	Key string `json:"key,omitempty"`
}
```

Added to `Message`: `CacheControl *CacheControl `json:"cache_control,omitempty"``.
Added to `ContentPart`: `CacheControl *CacheControl `json:"cache_control,omitempty"``.

Both are additive, pointer-typed (matching `ChatRequest.Temperature`/`MaxTokens`'s own existing optional-pointer convention), and land automatically in the client-facing wire format — the canonical schema doubles as Kelvran's own inbound API surface, per `gateway/ARCHITECTURE.md`.

### Anthropic (`internal/adapter/anthropic`)

`Request.System` is restructured from `string` to `[]SystemBlock`, where `SystemBlock{Type, Text, CacheControl *CacheControlWire}` mirrors Anthropic's real system-array wire shape. Critically, this is not just a type change: **each canonical `role:"system"` message becomes its own `SystemBlock`**, replacing today's `strings.Join(systemParts, "\n\n")` collapse into one block. This is what "restructuring to carry per-block cache_control" actually buys — without it, two system messages with different `CacheControl` settings would have no way to be marked independently, since they'd already be flattened into one opaque string before a marker could attach to either one specifically.

`ContentBlock` (already used for regular messages) gains `CacheControl *CacheControlWire`. `CacheControlWire{Type, TTL}` mirrors Anthropic's real `{"type":"ephemeral","ttl":"5m"|"1h"}` shape; a helper `cacheControlWire(*adapter.CacheControl) *CacheControlWire` converts the canonical marker (nil in → nil out, a silent no-op).

Wiring, per message role:
- **`system`**: its own `SystemBlock`, with `CacheControl` set from that message's own marker.
- **`tool`**: the single `tool_result` block it produces gets `CacheControl` set from the message's marker.
- **general (`user`/`assistant`)**: each `ContentPart` with its own `CacheControl` gets the marker on its own generated block (handled inside `contentPartToBlock`, which already receives the part). Separately, if the *message* itself carries `CacheControl` and the message's last generated block doesn't already carry one from a part-level marker, the marker attaches to that last block — "cache everything through this message," Anthropic's own idiomatic pattern of marking the last block of the region you want cached.

**Named scope limit, not silently dropped:** `ToolDef` (tool *definitions*, not tool calls) gets no `CacheControl` field this pass, even though Anthropic's own API supports `cache_control` on tool definitions too — `ToolDef` has no natural per-invocation identity the way `Message`/`ContentPart` do (a tool definition is request-scoped, not conversation-turn-scoped), and no research finding or task requirement named it. Real future work if a need appears; not built here. **Update, same day:** built as a same-day follow-up — see the "Addendum (2026-09-07): Tool-definition-level `cache_control`" section below.

### Bedrock (`internal/adapter/bedrock`)

`SystemContentBlock` and `ContentBlock` (both already union-style structs — the same pattern the multimodal RFC used to add `Image`/`Document`) each gain a `CachePoint *CachePoint` field. `CachePoint{Type string}` mirrors Converse's real `{"cachePoint":{"type":"default"}}` shape — "default" is the only value AWS documents.

Because a Bedrock `cachePoint` is a *standalone block*, not a property of the block it caches, wiring it means **appending** a `CachePoint` block immediately after the marked content, via a small helper (`appendCachePointIfNeeded`) that also guards against emitting two checkpoint blocks back-to-back (a part-level marker immediately followed by a message-level marker on the same last part would otherwise produce a meaningless empty-content second checkpoint). Wired for:
- **System messages**: one `SystemContentBlock{Text: ...}` per canonical system message (the same per-message restructuring Anthropic gets, since Bedrock's `System` was already array-shaped — this RFC does not change that shape, only what can follow each text block), plus a `CachePoint` block when that message's own marker is set.
- **`tool` messages**: a `CachePoint` block after the `toolResult` block, when the message's marker is set.
- **General messages/parts**: a `CachePoint` block after a part's own generated block when that part's marker is set, and/or after the whole message's last block when the message's own marker is set (same last-block semantics as Anthropic).

This covers system, tool-result, and regular message/part positions — not scoped down to "System only," even though that was this RFC's least-disruptive starting point (Bedrock's `System` was already array-based going in). Once `ContentBlock` needed the same `CachePoint` field anyway for symmetry with `SystemContentBlock`, extending the wiring to regular messages was a small marginal cost, and stopping at System-only would have meant a `CacheControl` marker on a non-system message either silently doing nothing or needing a typed error — both worse than just supporting the real capability the wire format and the existing union-block convention already make cheap.

### OpenAI (`internal/adapter/openai`)

`Request` gains one new top-level field: `PromptCacheKey string `json:"prompt_cache_key,omitempty"``.

A new helper, `findCacheKey(messages []adapter.Message) string`, scans every message (and, for multi-modal messages, every part) in order for the first non-empty `CacheControl.Key`, and returns it. **Kelvran never fabricates this value.** OpenAI's own documentation (per the prior research pass) describes `prompt_cache_key` as influencing *routing to a warm machine*, not *whether* caching happens — caching itself is already fully automatic. A hash of the request content would be actively counterproductive (every unique request would get its own key, defeating the "route repeat callers to the same machine" purpose); a random or Kelvran-generated value would be equally useless without being tied to real caller identity across requests. Since the canonical `ChatRequest` carries no tenant/session identity field itself, the only honest source for this value is the caller supplying one directly via `CacheControl.Key` — mirroring this codebase's existing "honest absence over fabricated placeholder" convention (e.g. `bedrock.go`'s `ChatResponse.ID` left empty rather than invented, since Converse has no native response-ID field).

**Named scope limit:** `openaicompat` (the adapter for self-hosted OpenAI-dialect runtimes — vLLM/TGI/Ollama) is explicitly **not** touched by this RFC, even though it shares OpenAI's wire shape closely enough that the multimodal RFC's `Content`-handling logic applies to both. `prompt_cache_key` is not a wire-format detail; it is a routing hint tied specifically to OpenAI's own multi-machine hosted infrastructure, which self-hosted runtimes have no equivalent of (a single self-hosted deployment has no fleet of machines to route between the way OpenAI's own API does). Wiring it into `openaicompat` would be adding a field with no real target semantics behind it — a named, deliberate exclusion, not an oversight.

### Why Gemini is excluded

Directly re-confirmed by reading `internal/adapter/gemini/gemini.go` in full for this RFC (not just citing the prior research pass): `Request`, `Content`, `Part`, `GenerationConfig` carry no field resembling a cache marker of any kind, and Gemini's own two real caching mechanisms (per the prior research pass's fetched vendor docs) both bypass the request schema entirely — **implicit caching is fully automatic with zero request field** ("enabled by default... nothing you need to do"), and **explicit caching (`CachedContent`) is a wholly separate, out-of-band API** (`client.caches.create()`, returning a `name` that a *later* `generate_content` call references) rather than a marker attached to the request that's being cached. There is no vendor-side opt-in mechanism shaped like Anthropic's/Bedrock's/OpenAI's for this RFC to target inside `ChatRequest`/`Message` at all — adding a `CacheControl`-consuming code path to `gemini.go` would have nothing real to wire it to. Gemini's adapter is therefore left **completely untouched** by this RFC; the new tests for this pass include a Gemini-specific test proving exactly that (see Verification).

### Adjacent fix: stale `ARCHITECTURE.md` claim

`gateway/ARCHITECTURE.md`'s Canonical Schema & Provider Adapters section claims "each adapter offers an `additionalModelRequestFields`-style escape hatch... so a new provider's quirk never requires touching the core pipeline" — the prior research pass confirmed this describes no code that exists anywhere in the repo (a repo-wide `grep` for `additionalModelRequestFields`/`AdditionalModelRequestFields` returns zero matches) and flagged it as a real instance of `AGENTS.md`'s own "doc-vs-code staleness" Gotcha, explicitly deferred to "whoever picks up the next implementation item." This RFC's implementation is already editing that exact section to document the new caching capability, so the stale sentence is corrected in the same edit rather than left for a fifth research pass to re-flag.

## Drawbacks

- **Per-adapter `ToProvider` complexity grows** — each of the three adapters now has extra branching (per-part marker checks, last-block/last-checkpoint bookkeeping) beyond the existing tool-call/system-placement logic. Mitigated by small, named helper functions (`cacheControlWire`, `appendCachePointIfNeeded`, `findCacheKey`) rather than inlining the logic into the main loop, and by comprehensive per-adapter unit tests (see Verification).
- **Anthropic's `System` type change is a breaking change to that adapter's own native `Request` type** (`string` → `[]SystemBlock`) — anything outside this package that read `anthropic.Request.System` as a string would break. Confirmed via a repo-wide reference check: the only consumer is `anthropic_test.go` itself (updated as part of this change); no other package or test touches this field.
- **A client can now send a `CacheControl.Key` string that Kelvran forwards verbatim to OpenAI as `prompt_cache_key`, with no server-side validation of its shape.** This is a routing hint, not a data-access or execution boundary — a malformed value has no worse effect than "cache locality doesn't improve," and OpenAI's own API is the one that would reject a genuinely malformed value. No new secret- or trust-boundary risk is introduced (unlike, say, guardrail bypass or cross-tenant cache key collision, `THREAT_MODEL.md`'s named highest-priority classes) since the value never participates in Kelvran's own cache key computation or tenant isolation at all.

## Alternatives Considered

**A single request-level boolean/string instead of per-message/per-part markers.** Rejected — this would under-serve Anthropic's and Bedrock's real per-block/per-section addressability (see the granularity table above) for a marginal simplicity gain, when the actual canonical-schema and per-adapter cost of the finer-grained version is small (one more optional pointer field on two already-small structs, plus small per-adapter helpers).

**Deriving OpenAI's `prompt_cache_key` from something Kelvran already has** (e.g. a hash of the virtual-key ID, `vk.ID`, which the prior research pass named as a plausible candidate value). Rejected for this pass: `vk.ID` lives in `internal/identity`, not on the canonical `ChatRequest` itself, so wiring it in would mean either threading tenant identity through the adapter interface (a larger, cross-cutting change touching the `Adapter` interface signature every implementation would need to update) or having the dataplane post-process the adapter's output (breaking the "adapters are pure functions" contract `types.go`'s own doc comment states). Named as a legitimate, smaller, complementary future extension distinct from this RFC's scope, not dismissed as a bad idea.

**Doing nothing / leaving the gap as documented-but-unaddressed.** Rejected — the prior research pass already established this is a real, evidence-backed capability gap (not a speculative one), and the concrete per-adapter wiring cost is small and well-scoped now that `ContentPart`'s own precedent (the multimodal RFC) already proved the superset-schema pattern works cleanly for this codebase's adapters.

## Unresolved Questions

- **Tool-definition-level `cache_control`** (Anthropic/Bedrock both support caching `tools[]` itself, not just messages/system) is named as real future work, not built here — see the Anthropic section's own scope note. **Update, same day:** built — see the Addendum below.
- **Whether Kelvran should ever auto-populate `CacheControl` on a client's behalf** (e.g. always marking the system message cacheable by default, rather than requiring every client to opt in explicitly) is a product-scope question this RFC does not answer — every marker in this design is caller-supplied and opt-in only, matching the "opt-in" framing throughout the prior research pass's own per-provider table (every one of the three mechanisms this RFC targets is itself documented by its own vendor as opt-in, not just Kelvran's choice to keep it opt-in).
- **Real measured impact** (cache-hit-rate or cost improvement from any of this) is not something this RFC or its tests measure — Kelvran has no production traffic yet, the same caveat the prior research pass's own Tier 3 section named repeatedly for this exact topic.

## Verification

`go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod tidy` (empty diff — no new dependency; this RFC adds zero new imports beyond the stdlib already in use).

New/updated unit tests per adapter: Anthropic and Bedrock each get tests proving (a) a message-level `CacheControl` on a system message produces a per-block/per-section marker in the native system representation, (b) a message-level marker on a regular message attaches to that message's last block/a trailing checkpoint, (c) a part-level marker on one `ContentPart` among several attaches to only that part's own block, (d) an unset `CacheControl` (the default) is a byte-identical no-op versus the pre-existing golden-fixture wire format. OpenAI gets tests proving `prompt_cache_key` is set from the first found `CacheControl.Key` and omitted entirely when no marker (or a marker with an empty `Key`) is present anywhere in the request. Gemini gets one new test proving a `ChatRequest` with `CacheControl` set on messages/parts round-trips through `ToProvider` with a native request byte-identical to the same request with no `CacheControl` at all — the concrete, load-bearing proof for "Gemini is unaffected," not just an absence of a Gemini-side code change. Existing golden-fixture regression tests for Anthropic (`request_anthropic_native.golden.json`) and Bedrock are updated only where the wire *shape* changed (Anthropic's `system` field: string → array of blocks) and re-verified byte-for-field identical otherwise, per this project's sanity-check-by-breaking discipline for any test whose correctness depends on new code.

## Addendum (2026-09-07): Tool-definition-level `cache_control`

This section is a same-day follow-up implementing the item this RFC's own "Unresolved Questions" and the Anthropic section's "Named scope limit" explicitly deferred: `ToolDef` (tool *definitions*, not tool calls) gains its own `CacheControl` marker, wired into Anthropic and Bedrock. Written as an addition to this RFC rather than a new document, per this RFC's own precedent of being the single home for "provider-side opt-in prompt caching" design decisions; the body above is left unmodified.

### Canonical schema

`ToolDef` (`gateway/internal/adapter/types.go`) gains `CacheControl *CacheControl`, the same marker type `Message`/`ContentPart` already use — no new type. Because `ToolDef` has a hand-written `MarshalJSON`/`UnmarshalJSON` pair (`toolDefWire`) rather than plain struct tags (it exists to keep `ArgumentsJSON`... no — `ParametersJSON`, a JSON-encoded string in the canonical Go type, presented as a parsed `parameters` object on the wire, matching `ToolCall`'s own already-established string-field/parsed-wire-shape convention), `toolDefWire` gains a matching `CacheControl *CacheControl `json:"cache_control,omitempty"`` field, and both `MarshalJSON`/`UnmarshalJSON` are updated to carry it through in both directions. Placed as a sibling of `type`/`function` in the wire shape (`{"type":"function","function":{...},"cache_control":{...}}`), not nested inside `function` — the same "additive marker sits alongside the provider-native shape, not inside it" convention `Message.CacheControl` already established (a sibling of `role`/`content`, not nested inside either).

### Anthropic: inline block property, confirmed against vendor docs

Directly re-fetched from `platform.claude.com/docs/en/build-with-claude/prompt-caching` for this addendum (not assumed from the messages/content-block precedent): Anthropic's `cache_control` on a tool definition is a **sibling key on the tool object itself** — `{"name":...,"description":...,"input_schema":...,"cache_control":{"type":"ephemeral"}}` — mechanically identical to `ContentBlock`'s own existing pattern, not a separate wrapper. Anthropic's docs further note caching works on *prefixes*, so marking the last tool in the array caches every tool defined before it too (in their own worked example, marking only `get_time`, the second of two tools, caches both) — this addendum does not build that "only the last one is needed" optimization into Kelvran's own logic; it simply attaches `cache_control` to whichever specific `Tool` the caller's `ToolDef.CacheControl` was set on, exactly mirroring `ContentPart`'s own per-item (not "trust the caller picked the optimal single spot") granularity. A caller who wants the documented last-tool-only idiom gets it for free by only setting `CacheControl` on their own last `ToolDef`; a caller who sets it on an earlier tool still gets a real, correctly-placed marker, not a silently-relocated one.

Wiring: `anthropic.Tool` (already a small, flat struct — `Name`/`Description`/`InputSchema`) gains `CacheControl *CacheControlWire `json:"cache_control,omitempty"``, reusing the exact same `CacheControlWire` type and `cacheControlWire()` helper `ContentBlock`/`SystemBlock` already use — no new wire type, since Anthropic's tool-level `cache_control` shape (`{"type":"ephemeral","ttl":...}`) is byte-identical to its block-level shape. `ToProvider`'s tool-building loop sets `CacheControl: cacheControlWire(t.CacheControl)` per tool, the same one-line pattern `contentPartToBlock` already uses.

### Bedrock: verified NOT identical to the content-block case — a standalone union member, not an inline field

This is the one place this addendum's design diverges from a naive "just copy the Anthropic pattern" approach, and the RFC's own instruction to verify rather than assume. Directly confirmed against AWS's live API reference (`docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Tool.html`, re-fetched for this addendum) and the Bedrock user guide's own "tools checkpoints" worked example (`docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html`): Converse's `toolConfig.tools[]` element (`Tool`) is documented as a **union type** with exactly three members — `cachePoint`, `systemTool`, `toolSpec` — "only one of the following members can be specified" per element. `cachePoint` is therefore **not** a property attached to a `toolSpec` object (unlike Anthropic's inline sibling-key shape above); it is its own standalone array element, sibling to `toolSpec` elements, exactly the same "standalone checkpoint block in an array" shape `ContentBlock.CachePoint`/`SystemContentBlock.CachePoint` already use for messages/system — not the Anthropic tool-def shape. AWS's own worked example confirms this concretely:

```json
"tools": [
  {"toolSpec": {"name": "top_song", "...": "..."}},
  {"cachePoint": {"type": "default"}}
]
```

Wiring: `bedrock.Tool` — currently `struct { ToolSpec ToolSpec }` with no `omitempty`, since every existing `Tool` value has always carried a real `ToolSpec` — is restructured to a union-style struct matching `ContentBlock`'s own convention: `ToolSpec *ToolSpec `json:"toolSpec,omitempty"`` (now a pointer, so a `cachePoint`-only element never marshals a spurious empty `"toolSpec":{}`) plus a new `CachePoint *CachePoint `json:"cachePoint,omitempty"``, reusing the existing `CachePoint` type. `systemTool` (Bedrock's own built-in tools, e.g. code execution) is not modeled — out of scope, since the canonical schema has no concept of a provider-native built-in tool and no research finding or task requirement named it. A new helper, `appendToolCachePointIfNeeded(tools []Tool, cc *adapter.CacheControl) []Tool`, mirrors `appendCachePointIfNeeded`'s exact shape (nil/empty no-op, back-to-back-checkpoint dedup guard) for the `tools[]` array specifically. `ToProvider`'s tool-building loop calls it once per tool, immediately after appending that tool's own `Tool{ToolSpec: ...}` element — the same "append right after the marked item" convention already used for content parts, not restricted to "only after the very last tool in the whole request" the way AWS's own convenience examples show (the same caller-controls-exact-placement reasoning as the Anthropic section above).

**Breaking change to `bedrock.Tool`, confirmed scoped:** changing `ToolSpec ToolSpec` to `ToolSpec *ToolSpec` is a breaking change to that field's Go type, mirroring this RFC's own precedent for Anthropic's `Request.System` (`string` → `[]SystemBlock`). Confirmed via a repo-wide reference check: the only non-test consumer is `bedrock.go` itself (updated as part of this change); `bedrock_test.go`/`regression_test.go` construct `Tool` values only through `ToProvider`'s own output, never by hand, so no test needs updating for the type change itself (only for the new behavior, per Verification below).

### Why OpenAI, openaicompat, and Gemini are still excluded — re-verified for tool definitions specifically, not assumed to carry over

- **OpenAI**: re-confirmed against `openai.go` — `findCacheKey`'s only signal is `CacheControl.Key`, and `prompt_cache_key` remains a top-level, whole-request routing hint with no per-block or per-tool addressing concept at all (unchanged from the RFC body's own finding for messages). A tool definition is not a special case of that finding; it is the same finding applied to a different block type. `openai.go`'s tool-building loop does not read `t.CacheControl` — a silent no-op automatically, with no code change needed, the same "the field simply isn't read" mechanism that already made `Message.CacheControl` a no-op for every provider besides its intended target.
- **openaicompat**: unchanged reasoning from the RFC body — `prompt_cache_key` is tied to OpenAI's own multi-machine hosted fleet, which self-hosted runtimes have no equivalent of. Its tool-building loop likewise never reads `t.CacheControl`.
- **Gemini**: re-confirmed by re-reading `gemini.go`'s tool-building loop (`ToProvider`, the `FunctionDeclaration` construction) for this addendum specifically, not by assuming the message-level finding transfers: it builds `FunctionDeclaration{Name, Description, Parameters}` with no field resembling a cache marker, and neither of Gemini's two real caching mechanisms (fully-automatic implicit; wholly out-of-band explicit `CachedContent`) has any per-tool-definition marker concept — the same structural absence the RFC body found for messages, now independently confirmed for tool definitions. The existing `TestToProviderCacheControlIsUnaffected` proof-of-non-effect test is extended to also include a `ToolDef` with `CacheControl` set, so the single existing test keeps being the one load-bearing proof for "Gemini is unaffected by this whole feature," across both this addendum and the original RFC's scope, rather than needing a second, narrower test.

### Verification

Same command as the RFC body: `go build ./... && go vet ./... && go test ./... -race && golangci-lint run ./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod tidy` (empty diff — no new dependency).

New/updated unit tests: Anthropic and Bedrock each get a test proving a `ToolDef.CacheControl` produces the correct native marker (Anthropic: an inline `cache_control` sibling key on that specific `Tool`; Bedrock: a standalone `{"cachePoint":{"type":"default"}}` element immediately after that specific tool's `{"toolSpec":{...}}` element), a test proving an unset `ToolDef.CacheControl` is a byte-identical no-op, and — the specific interference case this addendum's own task named — a test proving a request carrying **both** a cached `ToolDef` and a cached `Message` produces correct, independent markers for both in the same `ToProvider` output (not just one or the other). Gemini's existing `TestToProviderCacheControlIsUnaffected` gains a `ToolDef.CacheControl` case. Anthropic's/Bedrock's existing golden-fixture regression tests (`request_canonical.json` → `request_*_native.golden.json`) are left untouched on purpose — their fixture's one `ToolDef` carries no `CacheControl`, so `omitempty` means the new field must not appear in either golden file at all; this is itself sanity-checked (see LOGS.md's entry for this pass) by temporarily forcing the field to emit unconditionally and confirming the golden-fixture regression tests fail, then reverting.
