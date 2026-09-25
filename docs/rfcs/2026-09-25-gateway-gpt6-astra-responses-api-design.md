# RFC: GPT-6 Astra tool-calling / Responses API gap — design only, no code

## Status

Design-only, explicitly deferred pending its own named trigger (see Unresolved Questions). Mirrors
the exact "designed in full, deferred" precedent already established for the request-log store
(`docs/rfcs/2026-09-25-gateway-request-log-store-design.md`) and the MCP/A2A outbound-credential
design (`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md`): scope the "how" now,
without committing to the "when." The underlying gap this RFC scopes was named "Large effort...
confirmed real, not scheduled for this round" by its own motivating research
(`docs/upgrade-research/upstream-provider-api-changes-2026-09-24.md`, Finding 6's same-day
follow-up) — this RFC does not change that; it scopes the "how," not the "when."

## Date

2026-09-25

## Author(s)

Session agent (Claude Code), per two same-day investigations (adapter-architecture-fit and
severity/interim-mitigation) commissioned specifically to ground this RFC, plus the
2026-09-24 upgrade-research finding and its own same-day follow-up correction that first
surfaced the gap.

## Summary

For GPT-6 Astra, OpenAI supports tool/function calling only via the Responses API —
Chat-Completions tool users must migrate. Kelvran's `openai` adapter
(`gateway/internal/adapter/openai/openai.go`) is entirely Chat-Completions-shaped, with zero
Responses-API (`input[]`/`output[]`) request or response mapping anywhere in the package, and
`Deployment.BaseURL` (`gateway/internal/gateway/controlplane/config.go:56`) is a full, static,
operator-configured upstream URL with zero model-name-aware endpoint selection anywhere in
`router/` or `dataplane/`. If an operator configures a `gpt-6-astra` deployment the same way as
every other OpenAI model and a caller sends a tool-calling request against it, Kelvran marshals a
Chat-Completions-shaped payload and forwards it verbatim — there is no code path that could route
it to `/v1/responses` even in principle, and no clean client-side rejection either. This RFC
sketches — without implementing — both a cheap, independently-shippable interim rejection and the
full architectural shape of real Responses-API tool-calling support, and is explicit about which
parts of "real support" this repository has, in a sense, already started scoping under a different
name (see the Detailed Design section below).

## Motivation

`docs/upgrade-research/upstream-provider-api-changes-2026-09-24.md` Finding 6 first named this gap
(medium confidence, flagging its own caveat that the router/transport layer had not yet been
checked). That same document's own "Follow-up investigation (2026-09-24, same day)" closed the
caveat and confirmed the gap as real and structural, not hypothetical: `Deployment.BaseURL`
(`controlplane/config.go:56`) is a flat, static, operator-set string, and Kelvran's call chain POSTs
to that literal URL with zero model-name branching anywhere in `router/`/`dataplane/` (re-grepped
for `"responses"`/`"astra"` across both packages: no hits outside comments/tests). The same
document's own verdict: "Large effort... confirmed real, not scheduled for this round."

Two follow-up investigations, run specifically to ground this RFC, re-verified and sharpened that
finding against the live code (not merely re-cited it):

**What happens today, traced end-to-end.** `Pipeline.runMissPath`
(`gateway/internal/gateway/dataplane/dataplane.go:3319`) picks a deployment, then calls
`Pipeline.callDeployment` (`dataplane.go:3451-3487`), which looks up `p.adapters[dep.Provider]`
(`dataplane.go:3452`) — the `openai` adapter for any OpenAI-provider deployment, `gpt-6-astra`
included — and calls `a.ToProvider(upstreamReq)`. That call marshals `req.Tools`/`req.ToolChoice`
into the OpenAI Chat Completions wire shape (`Tool` at `openai.go:131`, `ToolChoiceWire` at
`openai.go:157-160`, the tools/tool_choice construction block at `openai.go:289-332`) — zero
`input`/`output` item types or any Responses-API shape exist anywhere in that 539-line file. The
resulting request is then POSTed via `NewHTTPUpstreamCaller` (`dataplane.go:4836-4850`) directly to
`dep.BaseURL` (`dataplane.go:4850`, `http.NewRequestWithContext(ctx, http.MethodPost, dep.BaseURL,
...)`), with the identical shape reachable through `streaming.go:333`'s twin first-pick call site
for streamed requests. `checkResponseFormatEnforceable` (`dataplane.go:311-315`) — the one
pre-upstream capability gate that already exists on both of those call sites — checks only
`req.ResponseFormat` against `capabilityOKForRequest`; nothing in that pre-flight chain inspects
`req.Tools`/`dep.Model` for this gap at all. Net effect, confirmed rather than assumed: neither a
clean Kelvran-originated error nor a predictable raw-upstream error — whatever OpenAI's Chat
Completions endpoint happens to return when a `gpt-6-astra` deployment's `BaseURL` receives a
Chat-Completions-shaped payload with `tools` set, forwarded near-verbatim to the caller, with no
Kelvran-originated explanation of *why*.

**No real operator demand signal exists yet.** A dedicated grep across `DECISIONS.md`,
`docs/decisions/*.md`, and `docs/agents/*.md` for `"gpt-6"`/`"astra"` (re-run directly for this RFC,
matching the investigation's own claim): zero hits anywhere outside the two
`docs/upgrade-research/*.md` documents already cited here and one incidental mention of a
*different* model (GPT-Live-1) delegating to GPT-6 Astra in
`docs/upgrade-research/gateway-realtime-multimodal-2026-09-13.md`. This mirrors, almost exactly,
the same real-demand-vs-research-only distinction this codebase's own MCP/A2A RFC already had to
draw for a different subsystem (`gateway/internal/mcp` has zero code either, per that RFC's own
Status line). This RFC does not resolve that absence — see Unresolved Questions — but it is the
central reason the interim mitigation and the full fix, below, are scoped as two independently
triggerable pieces of work rather than one.

**A closely related RFC already exists in this repository and must be read alongside this one.**
`docs/rfcs/2026-09-20-gateway-openai-responses-api-support.md` ("OpenAI Responses API support —
OpenAI-only passthrough") is also design-only and also unimplemented, but it was motivated by a
*different* gap (GPT-5.4's Chat-Completions-tool-calling-with-`reasoning_effort` restriction, not
GPT-6 Astra's Chat-Completions-tool-calling being unsupported at all) and it solves a *different*
integration point: a new, explicit `POST /v1/responses` route and a new `Pipeline.HandleResponse`
pipeline function that a client must proactively call using OpenAI's `client.responses.create()`
SDK surface. Neither of this RFC's two grounding investigations discovered or cited that RFC —
this section is this drafting pass's own addition, not attributed to either investigation. The
distinction matters concretely: that RFC's design, even fully implemented, does **not** close this
RFC's gap. A caller using the *existing* `/v1/chat/completions` route with `tools` set, routed by
Kelvran's own router to a `gpt-6-astra` deployment, has no way to know it should instead call a
different route — the gap this RFC scopes is about Kelvran *transparently* deciding, per
deployment/model, that a Chat-Completions-shaped inbound request must be translated to a
Responses-shaped outbound call, which is a strictly harder and different problem than exposing an
additional, explicitly-opted-into route. The two RFCs' wire-type sketches (see Detailed Design)
likely overlap enough to share code if both are ever implemented; their pipeline integration points
do not overlap at all. This RFC treats that reconciliation as open, not resolved here (Unresolved
Questions).

## Detailed Design

### (1) What happens today, with zero code changes

Covered in full in Motivation above — restated briefly here because it is the RFC's own baseline,
not a drawback of any specific proposal: a `gpt-6-astra` deployment configured like any other
OpenAI deployment, receiving a `/v1/chat/completions` request with `tools` set, is silently
forwarded Chat-Completions-shaped to whatever `BaseURL` the operator configured, with no Kelvran
code path capable of routing it to `/v1/responses` and no dedicated client-side rejection. The
caller sees whatever OpenAI's own Chat Completions endpoint returns for that malformed-for-this-
model request, forwarded near-verbatim.

### (2) Interim mitigation — Small effort, independently shippable, not the full fix

`checkResponseFormatEnforceable` (`dataplane.go:311-315`) is the exact shape to mirror: a pure,
pre-upstream-call guard returning a sentinel error, called from both real first-pick sites
(`runMissPath` at `dataplane.go:3360`, `streaming.go:333`) before any network call happens. A
`gpt-6-astra` tool-calling gate follows the same substring-match convention every other per-model
capability check in `gateway/internal/adapter/capabilities.go` already uses (e.g.
`bedrockStructuredOutputModelSubstrings`/`SupportsStructuredOutput`, `capabilities.go:24-63`;
`anthropicForcedToolChoiceUnsupportedModelSubstrings`/`AnthropicModelRejectsForcedToolChoice`,
`capabilities.go:96-129`) — matched by `strings.Contains` against a real model ID's
region/version-prefixed, date/version-suffixed shape, not exact equality, for the identical reason
those existing lists document.

Concretely: a new `OpenAIModelRequiresResponsesAPIForTools(model string) bool` in
`capabilities.go`, matching `"gpt-6-astra"` by substring; a new check in the same shape as
`checkResponseFormatEnforceable` — `if len(req.Tools) > 0 &&
adapter.OpenAIModelRequiresResponsesAPIForTools(dep.Model) { return
fmt.Errorf("%w: model %s", adapter.ErrResponsesAPIRequiredForTools, dep.Model) }` — wired at the
same two call sites (`runMissPath:3360`, `streaming.go:333`), immediately alongside the existing
`checkResponseFormatEnforceable` call.

**A real gap in the precedent itself, not silently inherited.** `adapter.ErrStructuredOutputUnsupported`
— the exact sentinel `checkResponseFormatEnforceable` returns — is not special-cased in
`writeErrorResponse` (`gateway/cmd/gateway/main.go:1749-1808`); every explicitly-handled sentinel in
that function (`ErrNotAnEmbeddingDeployment`, `ErrGuardrailBlocked`,
`ErrPromptAndMessagesBothSet`/etc.) maps to 400 with a documented "this is a client request-shape
mistake, never an upstream failure" rationale, but `ErrStructuredOutputUnsupported` has no such case
and falls through to the function's own `http.StatusBadGateway` (502) default — confirmed directly
against `main.go`, not assumed. A new `ErrResponsesAPIRequiredForTools` gate mirroring
`checkResponseFormatEnforceable`'s shape would be "clean" in the sense that matters most (it fires
pre-upstream, is Kelvran-originated, and carries an honest message body) but would inherit that same
502-by-default status code unless a future implementation explicitly adds its own `case
errors.Is(err, adapter.ErrResponsesAPIRequiredForTools): status =
http.StatusBadRequest` to `writeErrorResponse` — the same "client asked for something this
deployment cannot do" shape as `ErrNotAnEmbeddingDeployment`, not a backend failure. This RFC
recommends making that choice explicitly rather than accepting the 502 fallthrough by default; see
Unresolved Questions for where the decision itself is left open.

This mitigation ships a clean, Kelvran-originated 4xx in place of an unpredictable, near-verbatim
upstream error — it does not add any tool-calling capability for `gpt-6-astra`. It is real,
independently valuable, Small effort, and has zero dependency on whether the full fix (below) is
ever built or on real operator demand ever materializing (see Alternatives Considered).

### (3) Architectural design for real Responses-API tool-calling support

**(3a) New Adapter implementation vs. extension of the existing one.** `adapter.Adapter`
(`gateway/internal/adapter/types.go:551-563`, `ToProvider(ChatRequest) (any, error)` /
`FromProvider(any) (ChatResponse, error)` / `Name() string`) is, by its own signature, provider-
shape-agnostic — one implementation could in principle emit either wire shape. The real constraint
is not that interface; it is the dispatch layer around it, which is keyed by the single string
`dep.Provider` in exactly four places: `p.adapters[dep.Provider]` (`dataplane.go:3452`),
`responseUnmarshalers[dep.Provider]` (`dataplane.go:4883`), `embeddingResponseUnmarshalers[dep.Provider]`
(`dataplane.go:4952`), and `streamUpstreamURL`'s provider `switch` (`dataplane.go:4977-4999`) — each
holding exactly one adapter/unmarshaler/URL-rule per provider string. A new, separate implementation
under its own package and its own new `Provider` registry value (e.g. `"openai-responses"`) is the
lower-blast-radius choice: it adds one new key to each of those four maps/switches (a mechanical,
additive change, mirroring how Gemini and Bedrock each already occupy their own single key with
their own single wire shape) rather than teaching all four of them to become model-aware for the
`"openai"` key specifically, which would be a deeper, cross-cutting change to `dataplane.go`'s core
routing contract touching every provider, not just OpenAI's.

**(3b) New wire types and their concrete structural differences from Chat Completions.** Responses
replaces `messages[]` with an `input[]` array of typed items — not uniformly role+content turns;
system guidance commonly rides a top-level `instructions` field instead. It replaces
`choices[].message` with an `output[]` array of typed items (`message`, `function_call`,
`reasoning`, …) plus a top-level `status`/`incomplete_details`, rather than a per-choice
`finish_reason`. A tool call surfaces as a top-level `function_call` output item
(`call_id`/`name`/`arguments`), not nested inside `message.tool_calls[]` the way
`openai.go`'s existing `Message.ToolCalls` does today; a tool result goes back as a
`function_call_output` input item keyed by `call_id`, not a `role:"tool"` message with
`tool_call_id`. Streaming uses a distinct SSE event taxonomy (`response.output_text.delta`,
`response.function_call_arguments.delta`, …) — a genuinely separate implementation from
`internal/adapter/openai/stream.go`'s existing `chat.completion.chunk`-shaped `nativeStreamChunk`
decoder (`stream.go:19-38`), not a delta on it. There is also a stateful dimension
(`previous_response_id`) with no Chat Completions or canonical-schema analog today. These field
names are stated from established knowledge, not live-fetched in this pass — re-verify against
OpenAI's current documentation before implementation.

This is not a green field: `docs/rfcs/2026-09-20-gateway-openai-responses-api-support.md` already
sketches essentially this same wire-type shape for its own, differently-motivated route —
`ResponseRequest`/`ResponseResult`/a third `ResponseAdapter` interface, a discriminated-union
output-item decoder via a custom `UnmarshalJSON`, and an explicit choice to reuse
`adapter.ReasoningBlock`'s existing `Redacted`/`Data`/`Signature` fields for encrypted reasoning
content rather than inventing a second opaque-blob shape. A future implementation of *this* RFC's
tool-calling-transparency requirement should very likely reuse that existing sketch's types rather
than re-deriving them, but the two RFCs' entry points differ (see Motivation): that RFC's
`HandleResponse` pipeline function serves an explicit, client-opted-in route; this RFC's gap needs
the *existing* `HandleChatCompletion`/`runMissPath` pipeline to detect, at dispatch time, that a
specific deployment requires the Responses shape and internally translate `ChatRequest` ->
`ResponseRequest` -> upstream call -> `ResponseResult` -> `ChatResponse`, transparently, without the
caller ever knowing. Whether that translation step lives inside the new `openai-responses` adapter's
own `ToProvider`/`FromProvider` (translating at the adapter seam, keeping `dataplane.go` itself
provider-shape-agnostic) or as an explicit pre/post step in `callDeployment` is not decided here.

**(3c) Per-request API-shape decision mechanism: an explicit Deployment-level signal, not
model-substring auto-detection.** The existing substring convention
(`bedrockStructuredOutputModelSubstrings`, etc., see (2) above) exists to gate a boolean capability
*within one fixed wire shape and one fixed dispatch-table entry* — it has never been used to select
between two entirely different dispatch tables (adapter, unmarshaler, streaming-URL rule)
simultaneously. Auto-detecting `"gpt-6-astra"` in `dep.Model` and rerouting all four dispatch
points based on that string match would still require teaching every one of them to consult model
name — the same blast radius as adding an explicit new `Provider` value, per (3a) — but with an
added, silent-behavior-change risk if OpenAI ever ships another Responses-only model under a
different name later. A named, operator-visible signal (either a new `Provider` value on the
`Deployment`, e.g. `"openai-responses"`, or a new explicit boolean/enum field like
`RequiresResponsesAPI`) is safer and matches this codebase's existing pattern of explicit,
operator-configured `Deployment` fields (`AllowInsecureHTTP`, `SessionTokenEnv`, etc., all in
`controlplane/config.go`) over implicit inference from a model-name string. Note this is a third,
distinct mechanism from the existing 2026-09-20 RFC's own "client explicitly calls a different
route" mechanism — that mechanism does not solve this RFC's transparency requirement at all (see
Motivation), so it is not an alternative to (3c), just a different problem the sibling RFC already
answers for its own scope.

**Effort: Large**, matching both grounding investigations' independent conclusion. Concretely:
new union-typed wire structs (following existing `ToolChoiceWire`/discriminated-union idioms — not
novel design, but real, new code); a full new `Adapter` implementation (~500+ LOC, mirroring
`openai.go`'s own size); a genuinely separate streaming implementation (a parallel file to
`stream.go`, not a delta on it); threading a new dispatch key through all four provider-keyed
maps/switches in `dataplane.go` plus `setUpstreamAuthHeaders` and fallback error classification; a
`controlplane/config.go` schema addition for the new signal in (3c); and this repository's own
heavy golden-fixture test culture (`testdata/*.golden.json`) demanding a full parallel fixture set.
It stays "Large," not "XL," because it is purely additive — the existing Chat-Completions-shaped
`openai` package is untouched — and v1 scope can reasonably exclude stateful conversations
(`previous_response_id`) and async tool calls.

## Drawbacks

**Scope overlap with an existing, also-unimplemented RFC is a real coordination cost, not just a
citation nicety.** Two design-only RFCs now exist in this repository sketching overlapping
Responses-API wire types for different reasons. If both are ever picked up for implementation
without an explicit reconciliation pass first, the more likely failure mode is not "wasted effort"
so much as two independently-evolved, subtly incompatible `ResponseRequest`/`ResponseResult`-shaped
types landing in the same package at different times — exactly the kind of drift this repository's
own "many small files, high cohesion" convention is supposed to prevent, but doesn't automatically
prevent across two separate RFCs written eleven days apart by different investigation passes.

**The full fix is a large, cross-cutting change for a gap with no confirmed real-world trigger.**
Threading a new dispatch key through four provider-keyed maps/switches, `setUpstreamAuthHeaders`,
and fallback error classification (per (3a)/(3c) above) touches core `dataplane.go` routing code
that every other provider's requests also flow through — a real regression-risk surface for
providers that have nothing to do with GPT-6 Astra, in service of a gap with, per Motivation, zero
corroborating operator/support signal in this codebase's own history today.

**A genuinely new streaming implementation, not a delta, doubles the maintenance surface for
OpenAI specifically.** `stream.go`'s existing SSE decoder and a future Responses-shaped decoder
would need to coexist and both stay correct against OpenAI's own evolving streaming behavior,
including error-frame handling (`stream.go`'s own documented mid-stream `data:
{"error":...}` handling) — a second place that class of bug can now recur.

**The interim mitigation, if shipped without deciding its own status-code question, quietly
inherits a wrong-shaped error.** As noted in (2), the natural sentinel-error shape to mirror
(`checkResponseFormatEnforceable`'s own pattern) falls through to 502 by default unless a future
implementation explicitly adds a 400 case — an easy step to skip, given that the existing precedent
sentinel (`ErrStructuredOutputUnsupported`) itself already has this exact gap today, unnoticed until
this RFC's own investigation found it.

**Real prompt/tool-call content flows through a new code path with no independent security review
yet.** A new adapter handling `function_call`/`function_call_output` items is a new place a
malformed or adversarial tool-call payload could be mishandled; this RFC does not attempt a threat-
model pass for the new wire shape, and a future implementation should not skip one just because the
existing `openai` adapter has already been through this codebase's own security-review cycles.

## Alternatives Considered

**Do nothing — this is a research-finding-only gap with no real demand signal.** The honest
default absent this RFC. Per Motivation, a dedicated grep across this codebase's own decision/agent
logs found zero corroborating operator or support signal for `gpt-6-astra` tool-calling specifically
— the only mentions anywhere are the two upgrade-research documents that motivated this RFC and one
incidental cross-reference to a different model. Choosing this alternative costs nothing today and
risks nothing new; it only continues today's status quo (per (1) above) for as long as no real
caller configures a `gpt-6-astra` deployment with tools.

**Ship the interim rejection only, defer full support.** The alternative this RFC's own two
grounding investigations converge on as the pragmatic middle ground: (2)'s Small-effort,
Kelvran-originated 4xx replaces today's unpredictable raw-upstream-error behavior at low cost and
with zero dependency on demand ever materializing, while (3)'s Large-effort full fix stays exactly
as deferred as "do nothing" until a real trigger fires. This does not resolve the underlying gap —
tool calling on `gpt-6-astra` still does not work after shipping only this half — it only makes the
failure mode honest and clean instead of an opaque forwarded upstream error.

**Build full Responses-API tool-calling support now.** Rejected for this round on the same grounds
the original research finding already used: Large effort, zero confirmed demand signal, and (per
this RFC's own Motivation section) a real, unresolved scope-overlap question with an existing
sibling RFC that a future implementer would have to resolve first — building now would mean either
resolving that reconciliation under time pressure or knowingly building on top of an unreconciled
design gap.

**Fold this RFC into, or treat it as amending, the existing 2026-09-20 Responses-API RFC.**
Considered and rejected for this drafting pass specifically because the two RFCs' triggers, and more
importantly their pipeline integration points, are genuinely different (see Motivation) — merging
them now would either force a premature decision on the reconciliation question this RFC
deliberately leaves open (Unresolved Questions), or produce one RFC trying to serve two
non-overlapping call paths (an explicit new route vs. transparent existing-route rerouting) under
one Detailed Design section. Recording the relationship explicitly, as this RFC does, without
merging the documents, keeps each RFC's own trigger condition independently falsifiable.

## Unresolved Questions

**The hardest open design question, named by the adapter-architecture-fit investigation and not
resolved here: does Kelvran preserve its current "exactly one wire shape per provider string"
invariant, or does this gap justify breaking it for the first time?** The four provider-keyed
dispatch points in `dataplane.go` (`p.adapters[dep.Provider]`, `responseUnmarshalers`,
`embeddingResponseUnmarshalers`, `streamUpstreamURL`'s switch) have, to date, never needed
model-aware selection *within* one provider string — Gemini and Bedrock each get exactly one
adapter/unmarshaler/URL-rule per their own single key. A new `"openai-responses"` `Provider` value
(per (3a)/(3c)) preserves that invariant, at the cost of a new operator-visible config concept
(a deployment must be registered under a different `Provider` string than plain `"openai"` to use
tool calling on this one model). Model-aware dispatch inside the existing `"openai"` key would break
the invariant for the first time, touching every one of those four dispatch points even for
providers that never need it, in exchange for a config surface that looks more like "one deployment,
one model" to an operator. This is a pure Kelvran-architecture call — no amount of further OpenAI-
API research resolves it, and it determines whether a future implementation is a one-new-package
change or a change to `dataplane.go`'s core routing contract. Not resolved here.

**Whether real operator demand for GPT-6 Astra tool calling exists at all — reported honestly as
unresolved, not decided.** Per Motivation, a dedicated grep across `DECISIONS.md`,
`docs/decisions/*.md`, and `docs/agents/*.md` found zero hits for `"gpt-6"`/`"astra"` outside the two
upgrade-research documents that already motivated this RFC. This RFC does not resolve that absence
one way or the other — it is not proof demand will never exist, only that none has been observed in
this codebase's own history as of this RFC's date. Whether or when to build (3) should depend on
this question changing, not on this RFC's own architectural sketch being complete.

**Reconciliation with `docs/rfcs/2026-09-20-gateway-openai-responses-api-support.md`, named but not
decided.** Both RFCs sketch overlapping Responses-API wire types for different, non-overlapping
pipeline entry points (see Motivation and Drawbacks). Whether a future implementation should: (a)
implement 2026-09-20's route-based design first and have this RFC's transparent-rerouting design
extend its types later, (b) implement this RFC's gap first (it is the one with a confirmed,
structural "this literally cannot work today" finding, vs. 2026-09-20's narrower reasoning_effort
combination gap) and have that RFC reuse this one's types, or (c) treat them as one combined
implementation effort once either is triggered — is not decided here, and should be resolved
explicitly before either RFC moves past design-only status.

**The interim mitigation's own status-code decision, named but not decided.** Per (2), whether a
future `ErrResponsesAPIRequiredForTools` gate gets its own explicit 400 case in `writeErrorResponse`
(matching `ErrNotAnEmbeddingDeployment`'s precedent) or inherits the 502 default the way
`ErrStructuredOutputUnsupported` currently does — this RFC recommends the former but does not mandate
it, and notes the existing precedent sentinel has the identical undecided gap today, unfixed.

**Whether the full fix should include stateful conversations (`previous_response_id`) in v1.**
Per (3b), the stateful dimension has no Chat Completions or canonical-schema analog today and adds
real scope (a new kind of cross-request state Kelvran does not otherwise track for chat completions).
This RFC's Effort estimate in (3) assumes v1 can reasonably exclude it, mirroring the sibling
2026-09-20 RFC's own treatment of `previous_response_id` as a disclosed, accepted limitation rather
than a blocking requirement — but a future implementer triggered by a real `gpt-6-astra` demand
signal (see above) may find that signal specifically requires stateful tool-calling conversations,
which would change this estimate.

## Verification

None — this RFC is design-only, per its own Status line and the scope of both grounding
investigations. No code changes accompany it; a future implementation pass would need its own new
adapter package (or extension, per the Unresolved Questions above), tests mirroring
`internal/adapter/openai`'s existing golden-fixture and unit-test conventions plus new coverage for
the new dispatch-table entries and the interim mitigation's own capability gate, and its own RFC
status update from "Design-only" to "Accepted, implemented" — split, if (2) and (3) end up shipping
on different timelines as this RFC recommends, into two separate status transitions rather than one.
