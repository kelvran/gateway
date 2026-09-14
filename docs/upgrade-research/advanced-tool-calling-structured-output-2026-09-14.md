# Advanced structured-output and tool-calling patterns across providers (2026-09-14)

## Scope and method

Research target: parallel tool calls, tool-choice forcing, streaming tool-call
delta shapes, the disclosed Bedrock first-attempt structured-output gap, and
JSON Schema dialect differences, across Anthropic, OpenAI, Gemini, and Bedrock
Converse — verified against each provider's own current (2026-09-14) docs,
not assumption, and grounded against Kelvran's real code
(`gateway/internal/adapter/*/`, `gateway/internal/streaming/`,
`THREAT_MODEL.md`) rather than its self-description.

Already real and shipped in Kelvran (not re-researched): cross-provider
`ResponseFormat`/JSON-schema normalization including the real Bedrock
`additionalProperties:false` auto-injection fix
(`bedrockEnsureAdditionalPropertiesFalse`, `gateway/internal/adapter/bedrock/bedrock.go:568`);
canonical tool-call round-tripping across all 5 adapters; reasoning-content
canonical schema with lossless capture/replay across all 4 providers; the
disclosed, accepted v1 scope limit where a Bedrock first-attempt (non-fallback)
call to a model outside the structured-output whitelist silently omits schema
enforcement with zero signal (`THREAT_MODEL.md` Gateway Elevation-of-Privilege
row, "New 2026-09-15" entry).

22 claims survived 3-vote adversarial verification; findings below merge
duplicates and add direct grounding against Kelvran's own code where the
research brief asked for it.

## Executive summary

Every major provider (Anthropic, OpenAI, Gemini, Bedrock Converse) now
supports parallel tool calls and Kelvran's canonical `Message.ToolCalls
[]ToolCall` schema already represents them losslessly — but Kelvran has
**zero** `tool_choice`/`ToolChoice`-equivalent field anywhere in
`gateway/internal/adapter/` today (confirmed by direct grep, zero hits),
despite all four providers exposing forcing semantics that differ enough
(Anthropic's `auto`/`any`/`tool`/`none` plus a nested
`disable_parallel_tool_use`; Bedrock's three-member `any`/`auto`/`tool` union
with `tool` restricted to Claude 3 and Nova; Gemini's four-mode
`auto`/`any`/`none`/`validated` plus `allowed_tools`) that a real
normalization gap exists and is entirely unbuilt. Streaming tool-call argument
delivery is incremental-by-default across Anthropic (buffered-then-streamed
unless `eager_input_streaming` is set), Gemini (`step.delta` fragments the
caller must aggregate), and OpenAI/Bedrock (index-keyed deltas) — and
Kelvran's own `streaming.ToolCallDelta` type is explicitly `Index`-keyed with
a real interleaved-parallel-tool-call test fixture
(`stream_two_tool_calls_interleaved.txt`), so this shape is already handled,
not a gap. The disclosed Bedrock first-attempt structured-output gap is a
genuinely minimal, well-scoped fix (extend the existing
`adapter.SupportsStructuredOutput` whitelist check from the fallback-hop
`capabilityOK` gate to the first-attempt router pick) rather than a structural
problem. Bedrock's JSON Schema dialect for structured outputs is a real,
checkable, narrower subset of Draft 2020-12 (no recursion, no external
`$ref`, no numeric/string-length constraints, `additionalProperties` must be
exactly `false`) that Kelvran's current per-adapter pass-through does not
normalize against OpenAI's fuller dialect (which supports `$ref`
self-reference/recursion and numeric constraints) — a real, checkable gap
distinct from the already-fixed `additionalProperties` issue.

## Findings

### Finding 1 — Parallel tool calls are universally supported by 2026; Kelvran's canonical schema already represents them losslessly

**Confidence: high** (4 primary sources, unanimous or 2-1 votes on each sub-claim; directly grounded against Kelvran's own code)

All four providers Kelvran adapts to support a model requesting more than one
tool call in a single turn:

- **Anthropic**: parallel tool use is on by default (turned off per-request
  via the nested `disable_parallel_tool_use` boolean inside `tool_choice` —
  see Finding 2). Anthropic also imposes a strict, checkable wire-format
  contract for the *results* side: every `tool_result` for a multi-`tool_use`
  turn must be returned together in a single subsequent user message, matched
  by `tool_use_id`, with every `tool_result` block preceding any text content
  — sending results as separate messages is documented to actively degrade
  Claude's future willingness to parallelize.
- **Gemini**: explicitly documents "parallel function calling" (multiple
  independent calls in one turn) as distinct from "compositional (sequential)
  function calling" (chained, dependent calls).
- **Bedrock Converse**: does not itself impose a single-tool-call limit (no
  contrary evidence found; Bedrock's `ToolChoice` union governs forcing mode,
  not call count — see Finding 2).
- **OpenAI**: not independently re-verified in this pass (already assumed
  shipped/normalized per the task's "already real" framing), but LiteLLM's
  own `litellm.supports_parallel_function_calling(model=...)` per-model
  capability check (591 models `true`, 51 explicitly `false`, e.g.
  `azure/o3`, `azure/gpt-5.1-chat-2025-11-13`, 3286 defaulting to
  unsupported) independently confirms parallel tool-call support is real but
  **non-uniform across models within a single provider**, not just across
  providers — a capability-check dimension Kelvran's own
  `adapter.SupportsStructuredOutput`-style per-model whitelist pattern
  (already used for Bedrock structured outputs) does not currently have an
  equivalent for parallel-tool-call support.

Grounding against Kelvran's real code: `gateway/internal/adapter/types.go:35`
defines `Message.ToolCalls []ToolCall` — a slice, not a single value — so the
canonical schema does **not** silently assume at most one tool call.
`gateway/internal/streaming/types.go` defines `ToolCallDelta` as explicitly
`Index`-keyed "so a [caller] can reassemble" multiple concurrent tool-call
streams, and a real fixture,
`gateway/internal/adapter/anthropic/testdata/stream_two_tool_calls_interleaved.txt`,
proves interleaved multi-tool-call streaming is exercised by tests today. **No
gap found** — parallel tool calls are a real, already-handled capability in
Kelvran's canonical schema and streaming layer, contrary to the research
brief's open question framing this as a possible gap.

A real, adjacent gap surfaced by research (not previously known): LiteLLM's
own `/responses` endpoint (open PR #38375, unmerged as of 2026-09-13) has a
confirmed normalization bug where Azure AI/Foundry models wrap multiple
parallel tool calls inside a synthetic `multi_tool_use.parallel` pseudo-
function with a nested `tool_uses` array and `functions.`-prefixed recipient
names — LiteLLM's existing Chat-Completions-shaped normalizer
(`_handle_invalid_parallel_tool_calls`) doesn't cover this Responses-API
shape, so the malformed structure passes through and crashes agent SDKs with
"Tool multi_tool_use.parallel not found." This is Azure-AI/Foundry-specific
(not a general Bedrock/Anthropic/Gemini/OpenAI-native-API failure mode) and
occurs only intermittently, when the model itself chooses to parallelize.
**`not_yet`** for Kelvran — Kelvran has no `/responses`-API-shaped endpoint
and no Azure AI/Foundry adapter today; this is a concrete gap class to watch
for if either is ever added, not a live gap in the current 5 adapters.

### Finding 2 — Kelvran has zero tool_choice-equivalent field; all four providers expose real, materially different forcing semantics

**Confidence: high** (5 primary sources, unanimous votes; directly grounded against Kelvran's own code with a definitive negative grep result)

Direct grep of Kelvran's real adapter code —
`grep -rn "ToolChoice\|tool_choice" gateway/internal/adapter/` — returns
**zero matches** across all five adapters (anthropic, bedrock, gemini, openai,
openaicompat). Kelvran's canonical `ChatRequest` has no `tool_choice`-
equivalent field, and no adapter's `ToProvider`/`FromProvider` handles tool-
choice forcing today. This is a real, previously-unnamed gap: a caller cannot
today force "must call this specific tool," "must call some tool," or "must
not call a tool" through Kelvran's gateway for any provider.

The gap is significant because the four providers' native semantics differ in
ways a single pass-through field could not safely normalize without explicit
mapping logic:

- **Anthropic**: four `tool_choice.type` modes — `auto` (default when tools
  given), `any` (must use one of the provided tools, unspecified which),
  `tool` (must use one specific named tool), `none` (default when no tools
  given; forbids tool use). A separate, *nested* boolean,
  `disable_parallel_tool_use`, inside the `tool_choice` object (not a
  top-level request field) additionally constrains `auto` to "at most one
  tool call" and `any`/`tool` to "exactly one tool call." Forced modes
  (`any`/`tool`) are not universally available: they error under manual
  extended thinking mode, and return a 400 on specific model variants
  (Claude Fable 5.1, Claude Mythos 5.1), where Anthropic's own documented
  workaround is `auto` + strict tool use or structured outputs instead of
  forcing.
- **Bedrock Converse**: `ToolChoice` is a strict union type — exactly one of
  `any` (AnyToolChoice, must request at least one tool), `auto`
  (AutoToolChoice, default), or `tool` (SpecificToolChoice, must request the
  named tool) may be set per request. Critically, `tool`/SpecificToolChoice is
  **not universally supported across Bedrock models** — AWS's own API
  reference restricts it to "Anthropic Claude 3 and Amazon Nova models" only,
  a cross-model gap *within* Bedrock itself, independent of the cross-
  provider differences.
- **Gemini**: four discrete modes via `tool_choice`/`allowed_tools` —
  `auto` (default), `any` (forced call), `none` (forbidden), and a fourth
  mode, `validated` (enforces schema adherence on the call), plus an
  `allowed_tools: {mode, tools: [...]}` sub-object to restrict which specific
  tools are eligible. This is a materially richer surface than either
  Anthropic's or Bedrock's three/four-mode taxonomies (no other provider
  reviewed exposes a `validated` mode or an explicit allow-list sub-object).
- **OpenAI**: not independently re-verified in this pass; treated as already
  covered by the "already real and shipped" scope note, but not confirmed to
  have a `tool_choice` normalization path in Kelvran given the zero-hit grep
  result above.

**`build_now`**: the fix is concretely scoped — add a canonical
`ToolChoice`-equivalent field to `adapter.ChatRequest` (e.g. an enum of
`auto`/`required`/`none`/`{tool: name}`, with an optional
`disable_parallel_tool_use`-style flag) and a small per-adapter mapping
function in each of the 5 `ToProvider` implementations, explicitly erroring
(not silently dropping) when a caller requests a mode/model combination a
provider doesn't support (mirroring the existing
`additionalModelRequestFieldsFor` whitelist-check pattern for Bedrock's
`tool`-mode Claude-3/Nova restriction, and Anthropic's extended-thinking/
Fable-5.1/Mythos-5.1 forced-mode restriction). This is real, validated
against current provider docs, and a natural extension of patterns Kelvran
already has (the `SupportsStructuredOutput` per-provider/per-model whitelist
shape) — not a speculative "might need this someday" item.

### Finding 3 — Streaming tool-call argument delivery is incremental by default (not atomic) across every provider reviewed; Kelvran's own streaming schema already handles this correctly

**Confidence: high** (4 primary sources, unanimous or 2-1 votes; directly grounded against Kelvran's own code)

- **Anthropic**: standard (non-eager) tool-use streaming already delivers
  tool input incrementally via `input_json_delta`/`partial_json` events on
  the wire, but the API buffers and validates each parameter value
  server-side before streaming it back — so nothing is visible to the client
  until Claude finishes generating that parameter, even though the wire
  format is delta-shaped. Setting the newer per-tool `eager_input_streaming:
  true` field (replacing the legacy `fine-grained-tool-streaming-2025-05-14`
  beta header) removes that server-side buffering/validation entirely, so
  fragments arrive as Claude generates them, with fewer mid-word breaks.
- **Gemini**: function-call arguments stream as a sequence of `step.delta`
  events carrying `partial_arguments` fragments; the caller **must**
  aggregate/concatenate these deltas itself before the tool call is complete
  and executable — Gemini gives no atomic "complete tool call" event on the
  stream.
- **Bedrock/OpenAI**: not independently re-verified with fresh primary-source
  fetches in this pass, but Kelvran's own canonical streaming schema (below)
  treats all four providers uniformly as index-keyed incremental deltas,
  consistent with the general industry pattern OpenAI's `tool_calls[].index`
  streaming shape established originally.

Grounding against Kelvran's real code:
`gateway/internal/streaming/types.go` defines a canonical `ToolCallDelta`
type explicitly documented as holding "incremental tool-call fragments, keyed
by Index so a [consumer] can reassemble" the complete call — i.e. Kelvran's
own streaming schema is built around the incremental/delta shape, not an
atomic one. Each adapter's own `stream.go` (`anthropic/stream.go`,
`bedrock/stream.go`, `gemini/stream.go`, `openai/stream.go`,
`openaicompat/stream.go`) references tool-call reassembly, and real
fixtures — `anthropic/testdata/stream_single_tool_call.txt`,
`stream_two_tool_calls_interleaved.txt`, `stream_thinking_and_tool_call.txt`,
`openai/testdata/stream_tool_call.txt`, `openaicompat/testdata/stream_tool_call.txt` —
exercise this. **No gap found**: the research brief's open question ("does
Kelvran's own streaming adapter code handle both shapes correctly today")
resolves to yes for the incremental/delta shape (which is what every provider
actually uses); no provider reviewed delivers tool-call arguments atomically
on the stream, so there is no second shape Kelvran needs to additionally
handle. The one live feature-completeness gap is narrower: Kelvran has no
equivalent of Anthropic's `eager_input_streaming` per-tool opt-in (i.e. no way
for a Kelvran caller to request Anthropic's zero-buffering eager mode
specifically) — **`not_yet`**, since this is a latency/UX optimization on top
of already-correct incremental delivery, not a correctness gap, and should
wait for a real caller complaining about time-to-first-fragment on large tool
arguments.

### Finding 4 — The disclosed Bedrock first-attempt structured-output gap has a concrete, minimal, well-scoped fix

**Confidence: high** (grounded directly against Kelvran's own code — `THREAT_MODEL.md`, `bedrock.go`, `fallback.go`)

`THREAT_MODEL.md`'s Gateway Elevation-of-Privilege row (entry dated
"New 2026-09-15") discloses the exact gap the research brief named: on a
Bedrock **first-attempt** (non-fallback) call to a model outside
`adapter.SupportsStructuredOutput`'s five-entry Bedrock whitelist,
`bedrock.additionalModelRequestFieldsFor` (`gateway/internal/adapter/bedrock/bedrock.go:520`)
returns `(nil, nil)` rather than an error — the request proceeds normally
with `AdditionalModelRequestFields` omitted, so Bedrock applies zero schema
enforcement, with zero error, zero log line, and zero response-side signal
(`adapter.ChatResponse` has no enforcement-status field). The function's own
doc comment states this is a *deliberate* v1 scope limit: the capability
check is intentionally applied only at the fallback-hop layer
(`attemptFallbackChain`'s `capabilityOK` gate in
`gateway/internal/gateway/dataplane/fallback.go:372`, which skips an
incapable fallback *target* before ever calling it), never at the
first-attempt router pick.

The concrete, minimal fix: apply the same `adapter.SupportsStructuredOutput`
check that already gates fallback-hop selection to the *first* deployment
pick as well — i.e. before dataplane's initial `callDeployment`/
`streamDeployment` call (not inside the adapter's `ToProvider`, which has no
visibility into "is this the first attempt or a fallback hop" and no
clean way to fail the *whole* request rather than just silently omitting a
field). Two structurally straightforward options, both consistent with
Kelvran's existing patterns:

1. **Router-level pre-check** (closest to the existing fallback-hop pattern):
   before the first `callDeployment` invocation, if `req.ResponseFormat != nil`
   and the initially-selected deployment's model fails
   `adapter.SupportsStructuredOutput`, either route around it to a capable
   deployment (reusing the same selection logic `attemptFallbackChain`
   already has) or return a real 4xx error naming the incapability — mirrors
   `capabilityOK`'s existing shape, just invoked one call earlier.
2. **Response-side signal** (narrower, lower-risk): leave routing unchanged,
   but have `additionalModelRequestFieldsFor` return a typed error (or a
   signal threaded through `adapter.ChatResponse`) when structured output was
   requested but the field was omitted, so the caller gets a real 4xx/5xx or
   an explicit "enforcement not applied" marker instead of a silent 200.

Both options are genuinely minimal — they extend an existing capability-check
function to one additional call site rather than requiring new
infrastructure — so **`build_now`** is warranted once a team decides between
"reroute" (option 1, better UX, matches existing fallback semantics) and
"error loudly" (option 2, simpler, but forces the caller to retry against a
different model themselves). `THREAT_MODEL.md` itself frames this as an
"accepted, tested v1 scope limit, not an oversight," so the trigger for
actually shipping the fix is a product decision to close it, not a technical
blocker — this is real and buildable today, not blocked on anything provider-
side.

### Finding 5 — Bedrock's structured-output JSON Schema dialect is a real, checkable, narrower subset of Draft 2020-12; Kelvran's pass-through does not normalize the gap beyond `additionalProperties`

**Confidence: high** (4 primary AWS sources, unanimous votes; cross-checked against Kelvran's own live-verified adapter comment)

Amazon Bedrock's structured-outputs feature (GA since Feb 2026, for
Anthropic Claude 4.5 models and "select open-weight models" — not all
Bedrock models, confirming the whitelist-based support model Kelvran's
`adapter.SupportsStructuredOutput` already assumes) validates submitted JSON
Schemas against a **restricted subset** of Draft 2020-12 and returns an
immediate 400 if the schema uses an unsupported feature. AWS's own current
docs (`docs.aws.amazon.com/bedrock/latest/userguide/structured-output.html`
and the companion ML blog) explicitly list as **not supported**:

- Recursive schemas
- External `$ref` references
- Numerical constraints (`minimum`, `maximum`, `multipleOf`)
- String constraints (`minLength`, `maxLength`)
- `additionalProperties` set to anything other than `false`

Kelvran's own code independently, empirically corroborates the last item:
`bedrockEnsureAdditionalPropertiesFalse`'s doc comment
(`gateway/internal/adapter/bedrock/bedrock.go:551`) states it was
"live-verified 2026-09-13 against a real Converse call" that Bedrock
"unconditionally rejects EVERY object-type schema node that omits
`additionalProperties`" — and Kelvran already fixed exactly that one item via
auto-injection. **The other four restrictions are not currently handled by
Kelvall's Bedrock adapter at all** — `additionalModelRequestFieldsFor` only
`json.Unmarshal`s the caller's schema and recursively fixes
`additionalProperties`; it does nothing about recursive schemas, external
`$ref`, or numeric/string-length constraints, so a caller who submits an
OpenAI-shaped schema using any of those features against Bedrock will get a
real AWS 400 with no Kelvran-side pre-validation or translation.

This is a genuine, checkable dialect gap versus OpenAI: OpenAI's own
Structured Outputs guide explicitly supports and documents JSON Schema
self-reference (`$ref: #`, `$ref: #/$defs/component`) for recursive schemas
(shown via a recursive UI-component-tree example), which Bedrock explicitly
forbids — so a schema that is valid and works against OpenAI can silently
(from the caller's perspective) 400 against Bedrock with no advance warning
from Kelvran. Separately, `THREAT_MODEL.md`'s own 2026-09-15 Denial-of-Service
row entry independently names a related, narrower issue: none of Kelvran's
five adapters bound `ResponseFormat.JSONSchema.Schema` by nesting depth or
structural complexity at all (each either passes the raw schema through
unparsed or unmarshal-only, erroring solely on malformed JSON) — a caller can
submit an arbitrarily deep/complex schema within the existing 32MiB
whole-body cap.

**`build_now`** (narrow): a pre-flight Bedrock-specific schema linter inside
`additionalModelRequestFieldsFor` that detects the four unsupported features
(external `$ref`, recursion, numeric constraints, string-length constraints)
and returns a clear, typed Kelvran-side error *before* sending to AWS —
turning an opaque AWS 400 into an actionable Kelvran error naming exactly
which schema feature is unsupported. This is concretely scoped (a schema-tree
walk, similar in shape to the existing `bedrockEnsureAdditionalPropertiesFalse`
recursive walker) and grounded in AWS's own documented, stable restriction
list. **`not_yet`** for a general cross-provider JSON-Schema-dialect
normalization layer (detecting *every* provider's dialect differences and
either erroring or auto-transforming schemas to fit) — that is real
"boil the ocean" scope (OpenAI/Anthropic/Gemini's non-strict tool-use paths
accept fuller-featured schemas than Bedrock's strict structured-output mode,
and no single canonical subset would serve all four without loss) and should
wait until a caller actually hits it in production, not be built speculatively.

### Finding 6 — Cross-model capability gaps exist even within a single provider, not just across providers; no known additional normalization concern beyond the above

**Confidence: medium** (evidence is real but narrower in scope than a full concrete gap)

Two real facts complicate any assumption that "provider supports X" implies
"every model from that provider supports X":

- Bedrock's `tool`/SpecificToolChoice forcing mode works only on Anthropic
  Claude 3 and Amazon Nova models — not universally across Bedrock.
- Anthropic's own forced modes (`any`/`tool`) 400 on specific model variants
  (Claude Fable 5.1, Claude Mythos 5.1) and error under manual extended
  thinking, with Anthropic's own documented workaround being `auto` + strict
  tool use/structured outputs.
- LiteLLM's per-model `supports_parallel_function_calling` capability map
  confirms parallel-tool-call support is genuinely non-uniform even within
  one provider's own model lineup (51 of ~3900+ tracked models explicitly
  `false`).

Kelvran's `adapter.SupportsStructuredOutput` already has the right *shape*
for this problem (a per-provider, per-model whitelist function) but today
only covers the structured-output capability, not tool-choice-forcing or
parallel-tool-call support. If Finding 2's `tool_choice` field is built, its
per-adapter mapping functions will need the same per-model whitelist
discipline (e.g. reject/reroute a `tool`-forcing request against a
non-Claude-3/non-Nova Bedrock model, or against Claude Fable 5.1/Mythos 5.1)
— this is a design requirement to fold into Finding 2's implementation, not a
separate standalone gap.

## Caveats

- OpenAI's own current tool-calling/parallel-call docs were not independently
  re-fetched in this pass (the task treated OpenAI-side structured-output
  normalization as "already real and shipped"); Finding 1 and Finding 2's
  OpenAI-specific claims lean on the existing shipped Kelvran code and
  LiteLLM's third-party capability map rather than a fresh primary-source
  fetch of OpenAI's own docs.
- The LiteLLM `multi_tool_use.parallel` Responses-API bug (Finding 1) is
  scoped to an *open, unmerged* PR (#38375) against a third-party project's
  Responses-API endpoint, which Kelvran does not implement — included as a
  concrete "gap class to watch for," not a live gap in Kelvran today.
- Gemini's `validated` `tool_choice` mode and its four-mode taxonomy reflect a
  2026 Gemini Interactions API update (Gemini 3.6/3.8-era models) that
  postdates older, more commonly-cited Gemini function-calling docs
  (legacy `function_calling_config` only exposed `AUTO`/`ANY`/`NONE`) — worth
  re-verifying if Kelvran's Gemini adapter targets an older API surface than
  the one these docs describe.
- Bedrock's exact JSON-Schema-subset restriction list (Finding 5) is dated to
  AWS's Feb 2026 GA announcement and userguide; AWS could expand this subset
  in a future release without Kelvran's adapter code or this document being
  updated to match — treat the "not supported" list as current-as-of-
  2026-09-14, not permanent.
- Two claims from the original research pass were refuted on verification and
  are excluded here: "Claude 4+ models make parallel tool calls by default
  without special configuration" (refuted 1-2 — parallel tool use requires no
  configuration to *enable*, but the refutation concerned an overreach in how
  "by default... without any special configuration" was framed against
  Anthropic's actual docs) and "Bedrock structured outputs is GA across nine
  model provider families" (refuted 1-2 — GA scope is Claude 4.5 + "select
  open-weight models," not nine families).

## Open questions

1. Should Kelvran's future `tool_choice`-equivalent field (Finding 2) error
   loudly on an unsupported mode/model combination (e.g. Bedrock `tool` mode
   against a non-Claude-3/non-Nova model, or Anthropic `any`/`tool` against
   Claude Fable 5.1/Mythos 5.1), or silently downgrade to `auto` the way
   `additionalModelRequestFieldsFor` silently omits an unsupported structured-
   output field today? The existing precedent argues for silent omission, but
   `THREAT_MODEL.md`'s own framing of that precedent as an "accepted, but
   named" gap suggests the team may want the *new* field to fail loudly
   rather than repeat the same silent-omission shape a third time.
2. Which of Finding 4's two fix shapes (reroute-around-incapable-first-pick
   vs. error/signal-on-omission) does the team prefer, and does closing this
   gap require also adding an enforcement-status field to
   `adapter.ChatResponse` (a real, disclosed absence per `THREAT_MODEL.md`)
   for any caller-visible signal to exist at all?
3. Does Kelvran's Gemini adapter target the older `function_calling_config`
   (`AUTO`/`ANY`/`NONE`) surface or the newer Interactions API
   (`auto`/`any`/`none`/`validated` + `allowed_tools`) — this determines
   whether Finding 2's Gemini mapping needs to support the fourth
   `validated` mode from day one.
4. Is there a real caller need today for Anthropic's `eager_input_streaming`
   (Finding 3) — i.e. has any Kelvran caller observed slow time-to-first-
   fragment on a large tool-call argument that this per-tool opt-in would
   fix — or should this remain `not_yet` until such a caller appears?
