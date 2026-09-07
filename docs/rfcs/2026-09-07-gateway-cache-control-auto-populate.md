# RFC: `CacheControl` auto-populate for system prompts (Anthropic, Bedrock)

## Status

Accepted, implementing 2026-09-07. Amends
`docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`, which shipped
`CacheControl` as strictly caller-supplied, opt-in-only, and explicitly left
"whether Kelvran should ever auto-populate `CacheControl` on a client's
behalf" as an Unresolved Question. This RFC resolves that question.

## Summary

The project owner has decided the policy: Kelvran auto-marks a request's
system prompt as cacheable by default, without requiring the caller to set
`CacheControl` explicitly — for Anthropic and Bedrock only (the two
adapters whose provider caching mechanism attaches to a specific message,
per the original RFC's granularity table; OpenAI's `prompt_cache_key` has
no "which content is cached" concept for auto-population to target at
all, and Gemini has no request-level caching field of any kind). This RFC
designs the concrete mechanism: an exact heuristic (system messages only,
never user/assistant), a per-deployment opt-out defaulting to auto-populate
ON, and a precedence rule (explicit caller intent always wins).

## Motivation

`docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md` shipped
`CacheControl` as a marker every caller must set explicitly on every
request that wants it. In practice, the highest-value, lowest-risk cache
target — the system prompt — is exactly the content most agents send
unchanged on every single turn of a multi-turn conversation (the same
system prompt, over and over, only the user/assistant turns actually
change). Requiring every caller to remember to opt in defeats a real
default of "prompt caching should just work" the way it does for OpenAI's
and Gemini's own fully-automatic mechanisms — the original RFC's own
"fully-automatic mechanisms... already work transparently... with zero
code change" framing for those two providers has no Anthropic/Bedrock
analog today, purely because Kelvran requires an explicit marker where
those two vendors require none at all upstream.

`docs/agents/LOGS.md`'s 2026-09-07 "tool-definition-level cache_control"
entry named this exact follow-on as still open, alongside "real production
cache-hit-rate measurement" — this RFC addresses the first; the second
stays out of scope, unchanged, since no production traffic exists yet to
measure against either way.

## Detailed Design

### (a) The exact heuristic: system messages only, never arbitrary user/assistant content

Auto-populate applies to **exactly one thing**: a canonical `role:"system"`
message whose own `CacheControl` field is completely unset (`nil`). It
never applies to:

- **User or assistant messages** — even though `Message.CacheControl`
  exists on every role, auto-populate only ever reads/writes it for
  `role:"system"`. A user's first turn, an assistant's tool call, a tool
  result — none of these are ever auto-marked, regardless of size or
  position in the conversation. Reasoning: a system prompt's whole
  *purpose* is to be sent unchanged on every call in a conversation (or
  across many conversations, for a fixed agent persona) — it is the one
  message type whose repetition is a structural guarantee of the schema
  itself (canonical `Message.Role == "system"`), not a guess about
  caller behavior. A user/assistant message has no equivalent structural
  guarantee: an agent's tool-result payload, retrieved document, or
  conversation history is exactly as likely to be unique per call as
  repeated, and Kelvran has no way to distinguish the two from the
  canonical schema alone. Auto-marking those would be a heuristic on
  much shakier ground — attaching a cache breakpoint to content that
  usually differs call-to-call wastes one of Anthropic's scarce 4
  breakpoints-per-request for no benefit, and (for Bedrock/Anthropic
  both) writing to a cache costs more than a cache-miss inference call
  does per Anthropic's own documented pricing, so a wrong guess isn't
  free.
- **Content parts** (`ContentPart.CacheControl`) — same reasoning,
  narrower: a part lives inside a user/assistant message already
  excluded above.
- **Tool definitions** (`ToolDef.CacheControl`) — tool definitions are
  request-scoped, not conversation-turn-scoped (per the original RFC's
  own "no natural per-invocation identity" reasoning for why `ToolDef`
  needed its own addendum at all). A future RFC could reconsider
  auto-populating the last tool definition (tools are also typically
  stable across a conversation), but that is a genuinely separate
  decision with its own tradeoffs (which tool is "last"? does auto-
  marking interact with `appendToolCachePointIfNeeded`'s Bedrock union-
  type restructuring?) and is not decided here — named future work, not
  silently folded into this pass.
- **Multiple system messages** — the canonical schema allows more than
  one `role:"system"` message (each becomes its own independent
  `SystemBlock`/`SystemContentBlock`, per the original RFC). Auto-
  populate treats every system message independently: each one whose own
  `CacheControl` is unset gets the same default marker, matching the
  already-shipped explicit-marker mechanism's own per-message
  independence (no flattening, no "only the last one" special-casing).
  This mirrors the original RFC's own explicit choice not to build a
  "mark only the last tool" optimization for `ToolDef.CacheControl` even
  though Anthropic's docs describe that as the idiomatic pattern — the
  caller (or, here, auto-populate) marks what it marks, one item at a
  time, and Anthropic's own prefix-caching semantics make marking
  earlier blocks in addition to the last one a harmless (not double-
  billed, not incorrect) redundancy, never a correctness bug. A request
  with enough system messages to exceed Anthropic's 4-breakpoint limit is
  a pre-existing possible outcome of the explicit-marker mechanism too
  (a caller could already hit it); this RFC does not add new handling
  for it.
- **The default marker's TTL** is the zero value (`&adapter.CacheControl{}`,
  empty `TTL`) — each provider's own default cache lifetime (Anthropic:
  5 minutes; Bedrock: no TTL concept at all, `cachePoint` has none to
  set). This is deliberately the *cheapest, most conservative* opt-in
  shape, not the longer/more-expensive 1-hour TTL — auto-populate is a
  default applied without the caller ever having asked, so it should
  never silently commit them to the more expensive tier.

### (b) Per-deployment opt-out, defaulting to auto-populate ON

Following the existing per-deployment boolean-flag convention in
`gateway/internal/gateway/controlplane/config.go` (e.g.
`HealthProbeConfig`/`AdminConfig`/`CacheConfig`'s own "zero value means
the pre-existing/safe default, an explicit config value changes it"
pattern), a new field:

```go
// DeploymentConfig, gateway/internal/gateway/controlplane/config.go
DisableCacheControlAutoPopulate bool
```

parsed from a new, optional `disable_cache_control_auto_populate: true`
YAML key (via a new `getBool` helper, mirroring `getString`/`getFloat`/
`getInt`'s existing shape). **False is the default** — every deployment
configured before this field existed, and every deployment that never
sets the key at all, keeps auto-populate ON. This is the opposite
polarity from an opt-IN flag deliberately: the policy decision already
made is "auto-populate by default," so the zero value (an unset boolean
in Go, and a YAML key simply not present) must be the ON state, matching
this file's own established "zero value preserves existing/intended
behavior" discipline throughout (`HealthProbeConfig.IntervalSeconds <= 0`
disables probing entirely — the *feature* defaults off; here the
*feature* defaults on, so the *opt-out* boolean's zero value is `false`,
not `true` — same discipline, opposite feature-default, correctly
mirrored field polarity).

The field is threaded to `dataplane.Deployment` (same name, same
semantics, no config-vs-runtime renaming) and bridged in
`cmd/gateway/main.go`'s existing `DeploymentConfig` → `dataplane.Deployment`
construction loop, alongside every other per-deployment field
(`FallbackChains`, `Region`, etc.).

**Why per-deployment, not per-virtual-key or per-request:** a virtual
key's traffic can span multiple deployments/providers with different
caching economics; a deployment is the level at which "this upstream
provider/model genuinely benefits from system-prompt caching" is actually
known (an operator configuring a `claude-opus-primary` deployment knows
whether that model/workload benefits from caching; a virtual key's
config has no provider-specific knowledge at all). A per-request override
was considered and rejected: the caller already has the fully-general
per-request opt-in mechanism (`Message.CacheControl` set explicitly on
their own system message) — a caller who genuinely wants no caching on
one specific call already has no need for an auto-populate opt-out,
because explicit caller intent already wins outright (see (c) below).
The opt-out exists for an *operator* who runs a deployment where system-
prompt caching is undesirable for every caller uniformly (e.g., a
deployment whose system prompt genuinely changes every call, where a
cache write is pure wasted cost) — that is an operator-level policy
decision, not a per-call client one.

### (c) Precedence: explicit caller intent always wins; auto-populate only fills the gap

```go
// anthropic.go / bedrock.go, package-private, one copy per adapter
// (adapters do not import each other, per gateway/ARCHITECTURE.md's
// dependency rules — this is a deliberately duplicated ~6-line helper,
// not shared code)
func effectiveSystemCacheControl(explicit *adapter.CacheControl, autoDisabled bool) *adapter.CacheControl {
	if explicit != nil {
		return explicit
	}
	if autoDisabled {
		return nil
	}
	return defaultCacheControl // &adapter.CacheControl{} — provider's own default TTL
}
```

Three cases, in order:

1. **Caller set `CacheControl` explicitly on the system message** (any
   non-nil value, including `&adapter.CacheControl{}` with every field at
   its own zero value) → that exact marker is used, verbatim, regardless
   of the deployment's auto-populate setting. "Completely unset" means
   Go `nil` — a caller who explicitly constructs an empty-but-non-nil
   `&adapter.CacheControl{}` has still made an explicit choice (per the
   original RFC's own established meaning of "unset" for every other
   `CacheControl` call site in this codebase — nil, not zero-valued,
   is the no-op sentinel).
2. **Caller left it `nil`, deployment has NOT opted out** (the default) →
   auto-populate fills in the provider's own default marker.
3. **Caller left it `nil`, deployment HAS opted out** → no marker at all,
   byte-identical to this whole feature not existing (the pre-existing,
   caller-explicit-only behavior every deployment had before this RFC).

This precedence rule is why auto-populate cannot be implemented as a
request-preprocessing step in `dataplane` that mutates
`Message.CacheControl` before adapters ever see it: the *adapter* is the
only place that already distinguishes "explicit marker present" from
"marker absent" per message, and doing the auto-populate decision
anywhere else would mean re-deriving that same distinction a second time,
outside the one place it already exists correctly.

### Where the deployment's opt-out reaches the adapter: `ChatRequest`, not the `Adapter` interface

`adapter.Adapter.ToProvider(ChatRequest) (any, error)` takes exactly one
parameter, and per `types.go`'s own doc comment, adapters "must be pure
functions: no network calls, no hidden state beyond what's passed in."
The deployment's opt-out is deployment-scoped, not message-scoped — it
has nowhere else to live except a new field on `ChatRequest` itself,
mirroring the original RFC's own rejected alternative for OpenAI's
`prompt_cache_key`-from-`vk.ID` idea ("would need either threading tenant
identity through the `Adapter` interface... or a dataplane post-
processing step, breaking the pure-function contract"). A new field on
the one parameter the interface already has avoids both:

```go
// adapter.ChatRequest, gateway/internal/adapter/types.go
DisableCacheControlAutoPopulate bool `json:"-"`
```

`json:"-"` is deliberate and load-bearing, not incidental: `ChatRequest`
doubles as Kelvran's own client-facing wire format (`chatCompletionsHandler`
unmarshals the request body directly into `adapter.ChatRequest`) — this
field must never be caller-controlled, since it expresses an *operator's*
deployment-level policy, not a per-call client choice (see (b) above for
why a per-request override was rejected on the merits, independent of
this wire-format concern). `json:"-"` makes the field structurally
unreachable from a request body, not merely undocumented.

`dataplane.Pipeline` sets it immediately before calling `ToProvider`, at
all three existing call sites that already do the identical "copy the
request, override one field from `dep`" step for `Model`:

```go
// internal/gateway/dataplane/dataplane.go (callDeployment) and
// streaming.go (streamDeployment, streamDeploymentBedrock) — same
// pre-existing pattern, one more line each
upstreamReq := req
upstreamReq.Model = dep.UpstreamModel
upstreamReq.DisableCacheControlAutoPopulate = dep.DisableCacheControlAutoPopulate
```

This is the exact same mechanism `Model`/`UpstreamModel` already
establishes as precedent (a per-deployment override applied to a copy of
the canonical request, immediately before `ToProvider`), not a new
pattern invented for this feature.

### Anthropic and Bedrock wiring

Both adapters' `ToProvider` already have exactly one `case "system":`
branch in their per-message switch (see `anthropic.go`/`bedrock.go`).
That is the only call site touched:

```go
// anthropic.go
case "system":
	systemBlocks = append(systemBlocks, SystemBlock{
		Type:         "text",
		Text:         m.Content,
		CacheControl: cacheControlWire(effectiveSystemCacheControl(m.CacheControl, req.DisableCacheControlAutoPopulate)),
	})
	continue
```

```go
// bedrock.go
case "system":
	systemBlocks = append(systemBlocks, SystemContentBlock{Text: m.Content})
	systemBlocks = appendSystemCachePointIfNeeded(systemBlocks, effectiveSystemCacheControl(m.CacheControl, req.DisableCacheControlAutoPopulate))
	continue
```

`appendSystemCachePointIfNeeded`/`cacheControlWire` are the pre-existing
helpers from the original RFC — neither needs a single change; only the
*value* passed to them (now `effectiveSystemCacheControl(...)` instead of
the bare `m.CacheControl`) changes. Every other call site in both
adapters (`tool`, general user/assistant messages, `ToolDef`) is
untouched — auto-populate is reachable from exactly one branch in each
file, matching heuristic (a) above precisely.

### OpenAI, openaicompat, Gemini: unaffected, re-confirmed rather than assumed

- **OpenAI**: `findCacheKey` only ever reads `CacheControl.Key`, never a
  `CacheControl`'s mere presence, and this RFC adds no `Key` value
  anywhere — `openai.go`'s `ToProvider` is untouched. The new
  `ChatRequest.DisableCacheControlAutoPopulate` field is never read by
  `openai.go` at all, matching the original RFC's own "the field simply
  isn't read" no-op mechanism that already made `CacheControl` a no-op
  for every non-target adapter.
- **openaicompat**: same reasoning — never touched by the original RFC,
  never touched by this one.
- **Gemini**: `gemini.go` has no field resembling a cache marker of any
  kind (re-confirmed by the original RFC's own full read-through); this
  RFC adds a new `ChatRequest` field Gemini's `ToProvider` never reads,
  the identical no-op mechanism.

## Drawbacks

- **A caller who never wanted caching, and never knew this feature
  existed, gets it anyway by default on Anthropic/Bedrock deployments.**
  Mitigated by the heuristic being deliberately narrow (system prompt
  only) and the marker being the cheapest tier (default TTL, not the
  extended one) — the caller-visible behavior change is "an upstream
  provider bills a marginally different cache-write/cache-read cost
  breakdown for the same conversation," never a change to response
  content, ordering, or correctness. A caller who genuinely does not
  want this can already override it per-call (set `CacheControl` to
  something in their own request... but there is no per-call way to
  force it to *no* caching, since `nil` is what already triggers
  auto-populate). This is a real, named residual gap: **there is no
  per-request "definitely do not cache this" signal** — only a per-
  deployment operator-level opt-out. Accepted because the original RFC's
  `CacheControl` schema has no "explicitly opt out" sentinel distinct
  from "unset" (a `nil` pointer already means "no preference," which is
  exactly what this RFC repurposes into "use the default"); adding one
  (e.g., a sentinel struct meaning "never cache this") is a bigger
  schema change than this RFC's scope, named as possible future work.
- **Two near-duplicate `effectiveSystemCacheControl` helpers** (Anthropic,
  Bedrock) — accepted, matching this codebase's own established
  precedent of small, deliberately duplicated per-adapter helpers rather
  than a shared cross-adapter dependency (adapters do not import each
  other).
- **Golden-fixture regression tests change value, not just shape.**
  Unlike the original RFC's golden-fixture updates (a wire-format *shape*
  change: Anthropic's `system` string → array), this RFC's default
  behavior change means the existing fixture's unmarked system message
  now genuinely gets a `cache_control`/`cachePoint` marker in the
  checked-in golden JSON — a real behavioral pin, not a cosmetic one.
  Reviewed carefully rather than treated as routine.

## Alternatives Considered

**Auto-populate as a `dataplane`-level request-mutation step, applied
uniformly before every adapter's `ToProvider`.** Rejected — this would
need `dataplane` to know Anthropic/Bedrock-specific caching semantics
(exactly the kind of provider-specific logic `gateway/ARCHITECTURE.md`'s
dependency rules keep out of `dataplane`, which is provider-agnostic
except for the one `dep.Provider == "bedrock"` streaming-decoder-choice
special case that already exists for an unrelated reason). Per-adapter
implementation (this RFC's actual design) keeps every caching decision
inside the two adapters that already own Anthropic's/Bedrock's own real
mechanics, matching the original RFC's own architecture exactly.

**A per-request opt-out** (e.g., a magic `CacheControl` sentinel value
meaning "auto-populate must never apply here"). Rejected for this pass —
no such sentinel exists in the current schema, and inventing one is a
bigger design surface (does it also suppress a caller's OWN future
explicit opt-in on a later turn of the same conversation? does it apply
per-message or per-request?) than this RFC's scope. Named as a real,
possible future extension in Drawbacks above, not silently declined.

**Auto-populating tool definitions too** (the last `ToolDef` in a
request, mirroring Anthropic's own "mark the last tool, it caches the
prefix" idiom). Rejected for this pass — genuinely separate design
questions (which tool counts as "last"; Bedrock's union-type restructuring
for `ToolDef.CacheControl` is structurally different from its message-
level shape) that the project owner's stated policy ("auto-mark the
system prompt") does not name. Left as real, named future work.

**Doing nothing / leaving `CacheControl` caller-explicit-only forever.**
Rejected — this is the exact question the original RFC's own Unresolved
Questions section left open, and the project owner has now made the
policy call.

## Unresolved Questions

- Whether a genuine per-request "never cache this" override is ever
  needed — no evidence of demand exists yet (no production traffic).
- Whether tool-definition-level auto-populate should ever be built,
  and if so, exactly how "last tool" is determined per adapter.
- Real measured cache-hit-rate/cost impact — unmeasurable today, same
  caveat every prior prompt-caching RFC in this repo has already named
  repeatedly, for the same reason (no production traffic exists yet).

## Verification

From `gateway/`: `go build ./... && go vet ./... && go test ./... -race
&& golangci-lint run ./... && go run
github.com/fe3dback/go-arch-lint@v1.18.0 check && gofmt -l . && go mod
tidy` (empty diff — no new dependency; stdlib only).

New/updated unit tests: Anthropic and Bedrock each get tests proving (a)
a system message with `CacheControl` left unset gets the default marker
auto-populated when the deployment has not opted out; (b) an explicit,
caller-supplied marker on a system message is never overridden or
replaced by the default marker, regardless of the deployment's
auto-populate setting; (c) the deployment-level opt-out
(`DisableCacheControlAutoPopulate: true` on the canonical request, as set
by `dataplane`) genuinely suppresses auto-population, producing the exact
pre-existing "caller-explicit-only" output; (d) auto-populate never
applies to a user/assistant message, a content part, or a tool
definition, even when the deployment has not opted out. Two pre-existing
tests whose own assertions predate this RFC and directly encoded "an
unmarked system message carries no marker" (`TestToProviderUnsetCacheControlIsNoOp`
and the system-message-independence proof in both adapters) are updated
to explicitly set the new opt-out field, isolating their original,
still-valid claims from this RFC's new default behavior — the change is
narrated in each test's own updated doc comment, not silently patched.
Existing golden-fixture regression tests
(`request_anthropic_native.golden.json`, `request_bedrock_native.golden.json`)
are updated to include the now-real default marker on their fixture's
previously-unmarked system message, per this project's sanity-check-by-
breaking discipline (verified by temporarily reverting the fixture and
confirming the regression test fails with a real wire-format mismatch,
then re-applying). OpenAI's existing `TestToProviderUnsetCacheControlOmitsPromptCacheKey`
and Gemini's existing `TestToProviderCacheControlIsUnaffected` are each
extended (not duplicated into new standalone tests) to also prove the new
`DisableCacheControlAutoPopulate` field has zero effect on their own
output. `controlplane/config_test.go` gains two tests proving the new
`disable_cache_control_auto_populate` YAML key defaults to `false`
(auto-populate ON) when absent and parses `true` correctly when present,
matching this file's own established "genuinely optional, mirror-image"
test convention. One new `internal/gateway/dataplane` test proves the
full config→`Deployment`→`ToProvider` wiring end-to-end through
`callDeployment` (the buffered path) with the real `anthropic` adapter
registered — the two streaming call sites (`streamDeployment`,
`streamDeploymentBedrock`) carry the byte-identical one-line wiring,
verified by direct code review rather than a second, third dataplane-level
test, a named scope reduction.
