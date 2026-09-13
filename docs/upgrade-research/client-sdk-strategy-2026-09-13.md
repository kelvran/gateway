# Client SDK Strategy for Kelvran (2026-09-13)

## Research Question

Should Kelvran ship a first-party client SDK? Ground truth: Kelvran has **zero** client/SDK
directory anywhere in the repo today (only `gateway/`, `evals/`, `api/`) — every caller
integrates via raw HTTP against the OpenAI-Chat-Completions-compatible wire format that
`gateway/internal/adapter/openai.go` / `openaicompat.go` implement. A separate project
(Deep-Research) already calls Kelvran successfully via its own unmodified `PlannerClient`/
`AgentToolClient` using plain `httpx`, with no Kelvran-provided client at all. This report
compares 2026 production practice across OpenAI's official SDKs, Anthropic's SDK, LiteLLM's
proxy + SDK, Vercel AI SDK/Gateway, and peer LLM gateways, and tags every finding
`build_now` (validated, concrete, low-risk, no new code) or `not_yet` (needs a trigger).

## Executive Summary

Every peer LLM gateway examined (LiteLLM Proxy, Vercel AI Gateway) is built and documented
around the "point your existing OpenAI/Anthropic SDK's `base_url` at us" pattern as the
**primary, most-promoted** integration path, not a bespoke client SDK — and where a
gateway-specific SDK exists at all (Vercel's AI SDK, LiteLLM's own Python SDK), the vendor's
own docs either demote it to a secondary option or explicitly warn against using it against
a running proxy ("duplicate logic... unexpected errors"). Kelvran's current zero-SDK,
OpenAI-wire-compatible posture (already proven working end-to-end by Deep-Research's
unmodified `httpx` clients) is therefore *already* the dominant production pattern, not a
gap — the only concrete, low-risk near-term action is to document this "bring your own
OpenAI/Anthropic SDK, override `base_url`" path explicitly (a `build_now`, zero-new-code
guide). Shipping a genuine first-party, multi-language Kelvran SDK is a real but expensive
undertaking — Cloudflare's own case study puts hand-maintained multi-language SDK
coordination at "a minimum of 4 pull requests" per change and in-house generator tooling at
6-9 months for a *single* language — so it should stay `not_yet` until a concrete trigger
(e.g. real external callers in multiple languages explicitly asking for typed clients, or a
Kelvran-specific extension — like `ReasoningBlocks` — that the OpenAI/Anthropic wire format
genuinely cannot express) actually materializes.

## Findings

### Finding 1 — Peer gateways document "point your existing SDK's base_url at us" as the primary integration path, not a proprietary client
**Confidence: high** (multiple primary sources, unanimous votes)

LiteLLM Proxy's own docs state plainly: *"LiteLLM Proxy is OpenAI-Compatible, it works with
any project that calls OpenAI. Just change the `base_url`, `api_key` and `model`."* The
`/chat/completions` walkthrough on the same page demonstrates this with OpenAI Python,
OpenAI JS, the Anthropic Python SDK, the Mistral Python SDK, and Langchain — all redirected
via `base_url`/`api_base`/`endpoint` — and defers LiteLLM's own SDK to a separate page rather
than showing it inline as the primary example. Vercel AI Gateway follows the identical shape
at the vendor level: its docs explicitly state *"Using the OpenAI SDK? The OpenAI Responses
API and Chat Completions API both work by changing your base URL,"* and the site's own
summary line is *"drop-in compatible APIs that let you switch by changing a base URL. No
code rewrites required."* Kelvran's OpenAI-Chat-Completions-compatible wire format already
puts it in this same "drop-in" category with zero additional code required from callers.

### Finding 2 — Vendors ship a first-party SDK *and* the base_url-drop-in path simultaneously; they are not mutually exclusive strategies
**Confidence: high** (primary source, unanimous vote)

Vercel is the clearest example that these two strategies coexist rather than compete: its AI
Gateway docs label the AI SDK as the "recommended" default for *new* projects (because it
normalizes provider differences, streaming, tool calls, and reasoning across providers) while
simultaneously documenting, with equal prominence, the zero-code OpenAI-SDK-`base_url` path
for existing integrations. This means Kelvran does not face a binary choice — it can ship the
zero-code compatibility guide now and evaluate a typed SDK independently, later, without one
precluding the other.

### Finding 3 — Even where a gateway ships its own SDK, the vendor actively steers users away from using it to call the gateway/proxy itself
**Confidence: high** (primary source, unanimous vote)

LiteLLM's own quick-start docs show a tab for calling the running proxy with LiteLLM's native
Python SDK (`litellm.completion(..., base_url=...)`) but attach this explicit warning
directly above it: *"This is not recommended. There is duplicate logic as the proxy also uses
the sdk, which might lead to unexpected errors."* Independent corroboration: a real-world
GitHub issue (`run-llama/llama_index#16365`) cites this exact warning as its reason for using
an OpenAI-compatible client instead of LiteLLM's own SDK against the proxy. This is a direct,
vendor-acknowledged argument against Kelvran building a client that duplicates logic already
implemented server-side in the gateway — the risk is not hypothetical, LiteLLM's own users
have hit it.

### Finding 4 — Official OpenAI/Anthropic SDKs are engineered to be pointed at alternate deployments via `base_url`, and Kelvran's existing wire compatibility already benefits from this for free
**Confidence: high** (primary source code, one 2-1 split vote, rest unanimous)

The Anthropic Python SDK resolves `base_url` with an explicit precedence order — constructor
kwarg > `ANTHROPIC_BASE_URL` env var > profile config > hardcoded default
(`https://api.anthropic.com`) — meaning any caller can redirect 100% of traffic to Kelvran
with one kwarg or one env var, no code changes, no Kelvran-authored library. This is the same
mechanism openai-python exposes and that every gateway examined documents as the standard
integration path. Kelvran's `gateway/internal/adapter/openai.go` / `openaicompat.go` already
target this exact door.

### Finding 5 — Streaming ergonomics in official SDKs use typed async iterators, not callbacks, and reasoning/thinking content has an established two-event wire pattern Kelvran's adapter already parallels
**Confidence: high** (primary sources, unanimous votes)

anthropic-sdk-python exposes streaming via two distinct classes, `Stream` (sync,
`__iter__`/`__next__`) for the sync client and `AsyncStream` (`__aiter__`/`__anext__`) for the
async client — true async-iterator ergonomics, not event-emitter callbacks, set as the
default stream class per client type. Separately, Anthropic's Messages API streams extended
thinking through two dedicated SSE delta kinds — `thinking_delta` for reasoning text and a
`signature_delta` sent immediately before `content_block_stop` for integrity verification —
rather than folding reasoning into generic text deltas. This is the closest documented
industry analog to Kelvran's own `adapter.Message.ReasoningBlocks` (confirmed at
`gateway/internal/adapter/types.go:65-79`, an ordered slice of `ReasoningBlock` on the
assistant turn). openai-python's chat-completions streaming helper independently confirms the
typed-event pattern industry-wide: it wraps SSE in an async context manager yielding named
event types (`ContentDeltaEvent`, `FunctionToolCallArgumentsDeltaEvent`, etc.) rather than raw
JSON chunks. If Kelvran ever ships a client, this typed-event / async-iterator shape — not
callbacks — is the validated 2026 convention to follow.

### Finding 6 — Structured output in official SDKs is a thin client-side wrapper over the plain JSON-schema wire param, not a new wire protocol
**Confidence: high** (primary source, unanimous vote)

openai-python's `.parse()` method accepts a Pydantic model directly as `response_format`,
auto-converts it to a JSON schema for the wire call, and returns a `ParsedChatCompletion`
whose `message.parsed` field is generically typed back to the original Pydantic model. This
confirms that "typed structured output" is achievable entirely as client-side sugar over
Kelvran's existing JSON-schema wire format — it does not require Kelvran to invent or extend
its wire protocol, only (if ever built) a thin per-language wrapper.

### Finding 7 — Provider-agnostic abstractions (Vercel AI SDK) exist specifically to solve vendor lock-in from divergent provider APIs — a different problem than what a Kelvran-specific SDK would solve
**Confidence: high** (primary source, unanimous votes)

The Vercel AI SDK's own docs frame its core design goal explicitly: *"Each provider typically
has its own unique method for interfacing with their models, complicating the process of
switching providers and increasing the risk of vendor lock-in. To solve these challenges, AI
SDK Core offers a standardized approach..."* This is a meaningfully different problem from
"Kelvran needs its own SDK" — Kelvran's OpenAI-wire-compatible gateway already lets callers
use exactly this class of abstraction (Vercel AI SDK, LangChain, etc.) for free, since those
tools already know how to redirect `base_url` at an OpenAI-compatible endpoint. Kelvran
inherits multi-provider-abstraction benefits by being wire-compatible, without writing an
abstraction layer itself. (Note: a related, more speculative claim about the AI SDK's
provider-registry `providerId:modelId` pattern did not survive verification and is not relied
upon here.)

### Finding 8 — Reasoning/thinking effort is exposed in modern SDKs as a stable, provider-agnostic enum, offering a template for how Kelvran could someday expose `ReasoningBlocks`-adjacent controls without inventing new wire shapes
**Confidence: medium** (primary source, one 2-1 split vote)

The Vercel AI SDK's provider abstraction maps a stable enum
(`'provider-default' | 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh'`) onto each
provider's native reasoning config via two named strategies ("effort mapping" to a
provider-specific string, "budget mapping" to an absolute token budget) — rather than
exposing each provider's raw reasoning parameter shape to callers. This is a useful reference
pattern *if* Kelvran ever builds a typed client wrapping `ReasoningBlocks`, but it is a
forward-looking design note, not something Kelvran needs today (raw JSON passthrough already
carries this information).

### Finding 9 — Official SDKs bake in production-grade retry/backoff and typed error hierarchies that a hand-rolled `httpx` caller (like Deep-Research's current integration) does not get for free
**Confidence: high** (primary source code, unanimous votes)

anthropic-sdk-python retries automatically on 408/409/429/5xx with exponential backoff plus
jitter, honoring a server-supplied `Retry-After` header when present; anthropic-sdk-go
matches this precisely in its own runtime (2 retries by default, 0.5s base doubling to an 8s
cap, with 0-25% jitter subtracted). Both SDKs also map HTTP status codes to a typed exception
hierarchy rooted in a base error class (`RateLimitError` for 429, `OverloadedError` for 529,
`AuthenticationError` for 401, etc.), giving callers distinct catchable types instead of one
generic error. This is real, validated behavior that Kelvran's raw-HTTP callers (Deep-Research
included) do not currently get — it is the strongest concrete argument *for* eventually
building a typed client, but it is retry/error-handling logic that could equally be
documented as a wrapper recipe around `httpx`/`requests` retry libraries without Kelvran
writing or maintaining a full SDK.

### Finding 10 — Multi-language first-party SDK maintenance has a documented, multiplicative cost that peer infra companies have moved away from hand-rolling
**Confidence: high** (primary source, first-person case study, unanimous votes)

Cloudflare's own engineering blog reports that its hand-maintained per-language SDK process
required coordinating "a minimum of 4 pull requests" for a single API change across
languages, and that building in-house generator tooling (instead of buying) was estimated at
"at least 6-9 months away from a single high quality SDK," with recurring cost for every
additional language after that. Cloudflare ultimately bought a generation engine (Stainless —
the same engine that generates OpenAI's own official Python/TS SDKs) rather than building
in-house. This is the clearest quantified evidence that a genuine multi-language first-party
Kelvran SDK is a real, non-trivial infrastructure investment, not a weekend project — it
should not be started speculatively.

## build_now vs not_yet Classification

| # | Item | Classification | Why |
|---|------|----------------|-----|
| 1 | Document "point openai-python / openai-node / Anthropic SDK's `base_url` at Kelvran" as the official integration guide | **build_now** | Zero new code; matches the dominant, vendor-validated pattern (Finding 1, 4); Deep-Research already proves the wire format works with a raw HTTP client, so an official SDK-drop-in doc is strictly additive documentation |
| 2 | Document that Kelvran's `ReasoningBlocks` are additive JSON fields riding on top of the OpenAI-compatible response, alongside how Anthropic's own SDKs surface `thinking_delta`/`signature_delta` as the nearest prior art | **build_now** | Zero new code; clarifies an already-shipped adapter feature (`gateway/internal/adapter/types.go`) for callers without requiring a client library |
| 3 | Publish a short "recommended retry/backoff settings" doc (mirroring Anthropic SDKs' 408/409/429/5xx + exponential backoff + jitter defaults) for `httpx`/`requests`/`node-fetch` callers | **build_now** | Zero new code; addresses the one concrete gap Finding 9 surfaces, without committing to SDK maintenance |
| 4 | Ship a genuine first-party, typed, multi-language Kelvran client SDK (Python/TS/Go) | **not_yet** | No validated trigger yet — no known external caller has asked for one; Deep-Research already integrates successfully via plain `httpx`; Finding 10's Cloudflare case study shows real multi-language SDK maintenance costs (min. 4 PRs/change, 6-9 months in-house per language) that are unjustified without demand |
| 5 | Build a typed client-side wrapper exposing `.parse()`-style structured output over Kelvran's JSON schema wire param | **not_yet** | Technically cheap per Finding 6, but still new code to maintain; defer until either #4 is triggered or a specific caller reports friction with raw JSON schema construction |
| 6 | Adopt a provider-agnostic reasoning-effort enum (`none`/`low`/`medium`/`high`) as a Kelvran-native abstraction over `ReasoningBlocks` | **not_yet** | Only relevant if/when #4 is triggered; today's raw passthrough already carries this data; premature to design an abstraction with no client consuming it |

## Caveats

- Several claims in this research rest on **secondary sources (DeepWiki)** for
  anthropic-sdk-python's and anthropic-sdk-go's retry/error internals; each was independently
  re-verified against the primary GitHub source during adversarial voting, so confidence
  remains high, but DeepWiki itself is a derivative/auto-generated wiki and should not be
  treated as authoritative on its own in future research.
- The Vercel AI SDK provider-abstraction findings (Findings 7, 8) draw partly on a specific
  commit hash of an internal architecture doc (`vercel/ai@08cdf6ae/architecture/provider-abstraction.md`)
  rather than stable, versioned public API docs — this is pre-1.0/internal-design-doc material
  and could change without notice; treat the "effort mapping / budget mapping" terminology as
  illustrative, not a citable public contract.
- Several plausible-sounding claims about Vercel AI SDK's handling of LiteLLM-specific
  reasoning delta fields, and about the AI SDK's provider registry, were explicitly
  **refuted** during adversarial verification (0-3 or 1-2 votes) and are excluded from the
  findings above — do not resurface them without re-verifying against current source.
- This report did not independently test Kelvran's own OpenAI-compatibility surface
  (`gateway/internal/adapter/openai.go`/`openaicompat.go`) against a live official SDK
  (e.g., pointing `openai.OpenAI(base_url=...)` at a running Kelvran instance) — the
  "build_now" documentation recommendation assumes wire compatibility based on the adapter's
  stated purpose, not a fresh end-to-end verification run in this session.
- Time-sensitivity: findings about Vercel AI Gateway/SDK reflect the `v5`/current generation
  as of the pages' last-updated metadata (within days of 2026-09-13); LiteLLM findings
  reflect the current live docs as fetched during verification. Fast-moving areas (reasoning
  enum naming, AI SDK version numbers) should be re-checked if this report is consulted more
  than a few months from now.

## Open Questions

1. Has any real external caller (beyond Deep-Research's own `httpx`-based `PlannerClient`/
   `AgentToolClient`) actually requested a typed Kelvran client, in any language? This is the
   concrete trigger the `not_yet` items are waiting on.
2. Does Kelvran's `ReasoningBlocks` field ever need to express something the OpenAI-compatible
   wire format or Anthropic's `thinking_delta`/`signature_delta` pattern cannot represent
   (e.g., multi-provider-normalized reasoning across OpenAI/Anthropic/Bedrock in one
   response)? If so, that specific gap — not general SDK demand — would be the strongest
   argument for Kelvran-specific typed handling.
3. Should the "point your SDK's `base_url` at Kelvran" doc (build_now item #1) include
   worked examples for the Anthropic SDK's Bedrock/Vertex-style dedicated-subclass pattern,
   given Kelvran's own Bedrock pilot integration (per `docs/upgrade-research` history)?
4. If demand for a first-party SDK ever materializes, should Kelvran build/maintain it
   in-house or evaluate a schema-driven generator (Stainless or equivalent, per Cloudflare's
   case study) from day one rather than repeating Cloudflare's multi-year hand-rolled phase?
