# Request-Lifecycle Reliability Guarantees — Upgrade Research (2026-09-15)

Scope: what Kelvran guarantees (or doesn't) when a client's request to the gateway
doesn't complete cleanly — timeouts, client-side retries, network partitions
mid-stream, client disconnects mid-stream, and the resulting exactly-once-billing/
safe-retry/request-deduplication picture, given Kelvran's real per-key USD budget
enforcement. Grounded first against the live code: `gateway/internal/gateway/dataplane/dataplane.go`,
`streaming.go`, `streamrunaway.go`, `fallback.go`, `retry_after.go`, `streamaccumulator.go`,
and their test files; `gateway/internal/budget/budget.go`; `gateway/cmd/gateway/main.go`;
`docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md`,
`docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`,
`docs/rfcs/2026-09-08-gateway-streaming-runaway-completion-guard.md`. Externally
verified against Stripe's own idempotency design/API reference, OpenAI's and
Anthropic's own SDK source and docs, AWS Bedrock's and Google's Gemini API
references, a real, recently-shipped LiteLLM fix for the identical bug class (and
its own immediate follow-up correctness bug), a real production AI-gateway's
published billing-decision table, a practitioner's measured production-incident
data, and the Go language specification — each claim checked against a primary
source, not training-data memory, before inclusion here.

## Executive Summary

Kelvran's dataplane already has more request-lifecycle engineering than a first
glance suggests: a TOCTOU-safe `Reserve`/`Reconcile` budget-and-TPM reservation
pair that survives concurrent requests on the same key
(`docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`), a mid-stream
runaway-completion guard and a mid-stream reservation top-up that keep a single,
already-admitted streaming request from silently outrunning its own reservation
(`streamrunaway.go`), an idle-timeout streaming reader that aborts a genuinely
stalled upstream connection, and a client-facing `Retry-After` backoff signal on
capacity rejections (`retry_after.go`). None of that is what's missing.

What's missing is a coherent answer to "what happens to the money and the
observability trail when a request doesn't end cleanly" — and the answer today is,
concretely, **it evaporates**. Three converging, independently-verified findings:
(1) a stream that ends any way other than receiving the provider's own terminal
`usage` frame is billed at exactly $0, regardless of how much real, already-
provider-charged output was generated and delivered before the cutoff — and this
has at least three distinct, real triggers, one of which is Kelvran's own code
deliberately causing the condition that then under-bills it; (2) Kelvran's
client-facing API has no `Idempotency-Key` (or equivalent) contract at all — its
only retry-safety net is an accidental, content-addressed cache with real,
deliberately-documented holes for exactly the retry scenarios that matter most,
and zero coalescing on the streaming path; (3) Kelvran itself, as a client of its
own upstream providers, never uses the `Idempotency-Key` primitive that two of its
five provider integrations (OpenAI, Anthropic) already support specifically to
make a caller's own retries safe — so Kelvran's own well-built fallback/retry
machinery is not provably safe against being double-charged by its own providers.
All three are genuinely buildable today: none of them is gated on the "no real
production-traffic-volume floor yet" trigger that correctly defers most of
Kelvran's other named reliability gaps (fleet-wide retry budgets, priority
shedding, passive health signals — see
`docs/upgrade-research/gateway-reliability-resilience-round4-2026-09-11.md`, whose
territory this report deliberately stays out of). These are pure correctness/
safety properties whose value doesn't depend on traffic scale.

## Findings

### Finding 1 — A stream that ends any way other than a clean terminal usage frame is billed at $0, even when real output was already generated and paid for
**Confidence: high**

**Verdict: BUILD NOW.**

`HandleChatCompletionStream` reserves a provisional budget/TPM debit
(`budget.Reserve`) before the stream starts, then relies entirely on
`finishStreamedResponse` — reached only via a clean provider EOF/done — to supply
the real cost that `finalize`'s `budget.Reconcile` call applies in place of that
reservation. `finishStreamedResponse` builds its response from `streamAccumulator`
(`acc`) and `finalUsage`, and `finalUsage` is populated only from the provider's
own terminal `usage` frame — confirmed directly: Kelvran's own OpenAI/openaicompat
adapters explicitly set `stream_options.include_usage: true`
(`internal/adapter/openai/openai.go`, `internal/adapter/openaicompat/openaicompat.go`),
and that frame is documented, cross-provider, to arrive only in the LAST SSE event
before `[DONE]`.

Three distinct, real triggers all bypass `finishStreamedResponse` entirely,
discarding `acc` and never billing anything:

1. **Client disconnect.** `streamDeployment`'s read loop
   (`for { ev, readErr := reader.Next(); ... }`) treats any non-`io.EOF` `readErr`
   as a hard failure: `return adapter.ChatResponse{}, fmt.Errorf(...)`. A mid-
   stream client disconnect cancels `ctx` (`cmd/gateway/main.go` derives it from
   `r.Context()`, which Go's own `net/http` cancels on client disconnect), which
   cascades to `upstreamCtx` and aborts the in-flight upstream read — producing
   exactly this non-EOF error path, discarding every chunk `acc` already
   accumulated and every dollar of upstream cost already incurred. Confirmed live
   by `TestHandleChatCompletionStreamNoFallbackAfterFirstByte`
   (`streaming_test.go`), which proves a real, already-partially-delivered
   response returns a non-nil error — and asserts nothing about cost, because
   nothing about cost survives this path. A full-repo grep of the dataplane test
   suite for "disconnect"/"Canceled" returns zero hits: this scenario has no
   test coverage of its billing outcome at all.
2. **Gateway graceful-shutdown force-exit.** `cmd/gateway/main.go`'s own
   `gracefulShutdownTimeout` constant (30 seconds) is explicitly documented as
   bounding "how long an in-flight request (most realistically a long streaming
   SSE/eventstream response) gets to finish after a SIGTERM/SIGINT before the
   process force-exits regardless." Once that elapses and `main()` returns, the
   Go language spec is unambiguous about what happens to any still-running
   request-handling goroutine: "When the function main returns, the program
   exits. It does not wait for other (non-main) goroutines to complete."
   (go.dev/ref/spec#Program_execution). That goroutine's own deferred
   `attachRetryAfter`/`finalize` calls — the ONLY code path that ever writes a
   structured log line, ends the OTel span, or reconciles the budget reservation
   — simply never run. This is *worse* than the client-disconnect case: there,
   at least a zero-cost, billable=false log line gets written; here, nothing
   does, for a routine, scheduled, entirely-within-Kelvran's-control event (every
   deploy), not a rare crash.
3. **Kelvran's own mid-stream safety guards.** The runaway-completion-ceiling
   trip (`streamRunawayCharsCeiling`) and the mid-stream reservation top-up's
   exhausted-headroom trip (`checkMidStreamReservationTopup`) are both,
   deliberately, non-error, graceful truncations — they call `cancelUpstream()`
   and `break`, which DOES reach `finishStreamedResponse`. But because every
   provider's `usage` frame arrives only in the final SSE event, and these
   guards exist specifically to cut a stream off before it reaches that final
   event, `finalUsage` is, by construction, almost always still nil when
   `finishStreamedResponse` runs — triggering the existing `stream_missing_usage`
   warning and a bare zero-usage response. The exact scenario these two guards
   were built to catch — an unusually long, unusually expensive completion — is
   therefore also the scenario Kelvran is now guaranteed, by its own design, to
   under-bill for every time it fires.

This is not a hypothetical or Kelvran-specific oversight — it is a well-
documented, actively-being-fixed bug class across the LLM-gateway industry:

- **LiteLLM**, a named competitor already covered in Kelvran's own
  `gateway-competitor-gaps-2026-09-07.md`/`gateway-hierarchical-budgets-2026-09-07.md`
  research, shipped two real 2026 fixes for precisely this root cause: PR #30630
  ("bill partial usage when a streaming request is cancelled") and PR #33736
  ("bill partial streamed spend when the client disconnects mid-stream"),
  describing the identical mechanism in near-identical language: "the tokens
  already produced upstream are billed by the provider... but never recorded on
  the proxy... An authenticated caller can stream to 99% completion, disconnect
  before `[DONE]`, and pay nothing." Both fixes work by assembling a response
  from already-collected chunks (LiteLLM's `stream_chunk_builder`, functionally
  the same role Kelvran's own `acc`/`streamAccumulator` already plays) and
  billing that partial result instead of discarding it, with an explicit dedup
  guard against double-billing a stream that also completes normally around the
  same time.
- A real production AI-gateway product, **Aivene**, publishes an explicit
  billing-decision table (`docs.aivene.com/billing/what-gets-charged`) with the
  row "Client cancels mid-stream: Partial – tokens generated so far" charged,
  distinct from "First-chunk or chunk-stall timeout: No" charge — i.e., the
  industry-converged correct target behavior is neither "always bill the full
  estimate" nor "always waive everything," but exactly the graceful partial-
  credit middle Kelvran's own `acc` already has the data to support.
- A real, dated practitioner writeup (Gemini Lab, "Idempotency Key Design for
  the Gemini API," five months/six sites of production data) measured
  mid-stream disconnection as the SINGLE LARGEST trigger — 11 of 28 measured
  incidents, 39% — of exactly this class of lost-billing/duplicate-generation
  risk, direct evidence this is a routinely-recurring failure mode in real LLM
  traffic, not an edge case.
- A well-funded, major competitor, **Cloudflare AI Gateway**, has multiple
  currently-open GitHub issues (cloudflare/ai#470, plus a community-reported
  "streaming input/output does not get counted" report) confirming that
  accurate cost/token accounting specifically on the non-happy-path streaming
  case is a widely-recurring, industry-wide gap even among mature, well-resourced
  products — genuinely hard to get fully right, but a real, high-value, fixable
  gap nonetheless.
- A follow-up LiteLLM correctness bug, #37992 ("Client-disconnect partial
  billing treats explicit zero usage as missing and charges estimated tokens"),
  is an important cautionary data point for HOW to build this fix, not a reason
  not to: a naive "usage missing → fall back to a local estimate" branch can
  silently promote that estimate into an invoice, over-billing when the provider
  actually reported real, non-zero usage that a buggy "or"-style fallback
  treated as absent. Kelvran's own Go types (a `*adapter.Usage` pointer, nil
  until a real frame arrives) already distinguish "no frame arrived" from "a
  frame arrived reporting zero" more cleanly than LiteLLM's original Python
  implementation did — a real, structural advantage worth preserving explicitly
  in the fix rather than accidentally regressing.

**Buildable fix:** reach `finishStreamedResponse` (or an equivalent) from all
three trigger paths above instead of discarding `acc`. When `finalUsage` is
genuinely nil, estimate completion tokens from `acc.totalContentLen()` using the
SAME, already-proven, already-precedented `streamRunawayCharsPerToken` (4
chars/token) proxy `streamrunaway.go` already uses for the runaway guard and
reservation top-up — but only when `firstChunkSent` is true (mirroring Aivene's
own "chunk-stall timeout: not charged" rule, and the general "waive when the
user received nothing of value" principle), and mark the resulting charge
explicitly as an estimate (never conflated with a real provider-reported total —
the exact distinction LiteLLM's #37992 got wrong on its first attempt). For the
graceful-shutdown trigger specifically, the more robust fix is proactive: have
the shutdown sequence cancel in-flight streaming request contexts with enough of
`gracefulShutdownTimeout`'s own 30-second budget still remaining for their normal
defer-based `finalize` path to run and record a partial estimate, rather than
relying on the hard process exit to not race it.

### Finding 2 — No Idempotency-Key (or equivalent) contract exists on Kelvran's client-facing API; the only retry-safety net today is an accidental, holed cache
**Confidence: high**

**Verdict: BUILD NOW.**

A full-repo grep (`grep -rniE "idempoten" --include="*.go"`, excluding tests)
returns exactly one hit anywhere in `gateway/`'s production Go source — an
unrelated comment about `context.CancelFunc`'s own idempotency in
`idleTimeoutReader.Read`'s doc comment (`dataplane.go`) — and zero hits in
`docs/rfcs/` or `DECISIONS.md`. Kelvran has never designed, built, or even
explicitly deferred a request-level idempotency mechanism for its client-facing
API.

What exists instead is a purely incidental substitute: Kelvran's own
content-addressed L1 (exact)/L2 (normalized)/L3 (lexical near-duplicate) cache,
keyed deterministically on `(tenant, model, messages, temperature, max_tokens,
guardrail policy version, response_format, prompt fingerprint)`
(`cache.Key`/`cache.NormalizedKey`), plus `runMissPath`'s `singleflight.Group`
coalescing of concurrent identical L1-key misses. Together these mean a
byte-identical retry of a request whose first attempt already succeeded and was
cached is often served for free rather than double-billed — a real, if
accidental, property. But it has deliberate, already-documented holes for
exactly the retry scenarios that matter most:

- A response that hit `finish_reason == "length"` (truncated at `max_tokens`) is
  DELIBERATELY never written to any cache layer (`responseWasTruncated`'s own
  doc comment; proven by `TestHandleChatCompletionTruncatedResponseNeverCached`
  and its streaming sibling `TestHandleChatCompletionStreamTruncatedResponseNeverCached`)
  — meaning a client that times out waiting for a long completion and retries
  identically, if the first attempt genuinely hit its own token cap (a very
  common outcome for long-form/agentic generation), incurs a full second real
  charge every time, indefinitely, never once benefiting from the cache.
- A post-call guardrail Block-tier verdict is likewise deliberately never cached
  (`TestHandleChatCompletionPostCallBlockedResponseNeverCached`) — a retry of a
  blocked request re-executes the full paid generation again before being
  blocked again.
- The streaming path has NO coalescing at all — `streaming.go`'s own doc comment
  states this explicitly: "The streaming path has no singleflight coalescing
  (unlike runMissPath's buffered miss path) — every completed stream is its own
  real, unshared upstream call, so it's always billable." Two genuinely
  concurrent, byte-identical streaming requests (the textbook "client times out,
  opens a second connection while the first is still actively streaming"
  pattern) are guaranteed to trigger two full, independently-billed upstream
  calls, with no dedup mechanism of any kind.
- Even where the cache does apply, `p.cache.Put`'s own error is silently
  swallowed (`_ = p.cache.Put(ctx, l1Key, encoded, p.cacheTTL)`) — a transient
  cache-backend write failure at exactly the wrong moment produces the identical
  "retry re-bills" outcome by pure chance, invisibly.

This is precisely the gap Stripe's own `Idempotency-Key` design exists to close
(`docs.stripe.com/api/idempotent_requests`; `stripe.com/blog/idempotency`,
"Designing robust and predictable APIs with idempotency," 2017, still Stripe's
current documented mechanism as of the 2026-03-25 API version) — and, notably,
two of Kelvran's own five upstream providers, OpenAI and Anthropic, have
independently converged on the identical mechanic for their own APIs: a
client-supplied `Idempotency-Key` header, a server-side cache of the terminal
result, a hard rejection when the same key is reused with different parameters
(Stripe's `idempotency_error`), and — per Stripe's own documented mechanics — a
`409 Conflict` on a genuinely concurrent in-flight duplicate rather than letting
it silently double-execute. Three independent companies (Stripe, OpenAI,
Anthropic) converge on the same ~24-hour result-cache TTL as their default,
confirmed directly against each one's own current docs/SDK source
(`openai-python`'s and `anthropic-sdk-python`'s `_base_client.py` both generate
and reuse an idempotency key across a request's own retries; Anthropic's docs
state a 24-hour dedup window explicitly).

**Buildable fix:** an opt-in, client-supplied `Idempotency-Key` request header on
`/v1/chat/completions` (buffered and streaming), backed by a short-TTL store of
`(virtual_key_id, idempotency_key) -> (request fingerprint, terminal result)`,
mirroring Stripe's own concurrency-lock (a second concurrent request with the
same key gets `409` instead of double-executing) and parameter-fingerprint-
mismatch rejection. This single mechanism closes both the truncated/blocked-
response retry-rebilling gap AND the streaming-concurrent-duplicate gap at once,
entirely independent of the cache layer's own hard-gates — which exist for
unrelated correctness/security reasons (`THREAT_MODEL.md`'s cache-poisoning row)
and must never be weakened to work around this (`AGENTS.md`'s explicit "never
weaken the cache hard-gate" rule).

### Finding 3 — Kelvran never sends an Idempotency-Key to its own upstream providers on automatic retries/fallback hops, even though two of its five providers already support one for exactly this purpose
**Confidence: medium-high**

**Verdict: split — BUILD NOW for OpenAI/Anthropic hops; NOT YET for Gemini/Bedrock (no upstream primitive exists to build against).**

Kelvran's own error-classified, multi-hop fallback-chain mechanism
(`docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md`,
`fallback.go`) treats a plain network-level failure — a timeout, a connection
reset, anything that isn't a structured `UpstreamHTTPError` — as
`FallbackClassGeneric` and retries it, by design: `fallback.go`'s own doc comment
states "errors from ToProvider/FromProvider... or a network-level failure carry
no equivalent structured data and always classify as FallbackClassGeneric." This
is exactly the AMBIGUOUS-outcome case Stripe's own idempotency post names
directly: "On a response failure (i.e. the operation executed successfully, but
the client couldn't get the result)... the server simply replies with a cached
result of the successful operation." Kelvran, as a *client* of OpenAI/Anthropic/
Bedrock/Gemini, has exactly this problem on its own buffered (non-streaming)
upstream call path (`NewHTTPUpstreamCaller`): a network partition on the
response READ — after the provider already computed, and would bill for, the
full completion — is, in Kelvran's own error handling, indistinguishable from a
partition before the provider ever started; both classify identically as
`FallbackClassGeneric` and both trigger an automatic retry/fallback to (often) a
different deployment.

Verified directly: `NewHTTPUpstreamCaller`, `NewHTTPUpstreamStreamCaller`, and
`setUpstreamAuthHeaders` (`dataplane.go`) never set an outbound `Idempotency-Key`
header on any provider call, for any provider. OpenAI's and Anthropic's own
SDKs both implement exactly this primitive already, confirmed directly against
each SDK's real, current source: `openai-python`'s `_base_client.py` generates
`input_options.idempotency_key` once per logical request and reuses it across
every retry of that same request; `anthropic-sdk-python`'s `_base_client.py` does
the identical thing under the identical field name — a mature, multi-year-stable
primitive on two of Kelvran's five provider integrations, not a hypothetical.

By contrast, AWS Bedrock's `Converse`/`ConverseStream` API — confirmed directly
against AWS's own current API reference and the full boto3/botocore parameter
lists for both operations — exposes no client-token/idempotency-shaped field
anywhere in its request schema, genuinely absent, unlike the `ClientToken`/
`clientRequestToken` pattern common elsewhere in AWS's own APIs. Google's Gemini
API has the same absence: Google's own official guidance (Vertex AI's "Retry
strategy" doc) frames retry safety purely in terms of server-side STATE ("While
`generateContent` is not strictly idempotent due to the stochastic nature of
generative models, it is generally safe to retry for transient errors as it does
not modify server-side state"), never mentioning the billing dimension at all —
and an independent practitioner (the same Gemini Lab writeup cited in Finding 1)
confirms the gap from the other side explicitly: "Gemini does not offer an
Idempotency-Key header, which means clients have to enforce request uniqueness
on their own," reporting mid-stream-disconnection-triggered retries as their
single largest (39%) resulting cost-duplication category.

**Buildable fix, honestly scoped:** thread a stable, per-logical-client-request
`Idempotency-Key` — deterministically derived from the same content-addressed
`l1Key` Kelvran already computes for caching, mirroring the "derive the foreign
idempotency key deterministically from your own row's key" pattern Stripe's own
Rocket Rides reference implementation documents — into every outbound call to an
OpenAI or Anthropic deployment. This is small, well-scoped, and has a real
upstream contract to build against today. Doing the same for Gemini or Bedrock
is genuinely `NOT YET` — not gated on Kelvran's own traffic volume (unlike most
`NOT YET` verdicts elsewhere in Kelvran's own resilience research), but because
no upstream primitive exists at all; the honest fix there is different in kind
(e.g. treating a response-path network failure more conservatively for those two
providers specifically, rather than retrying every ambiguous `FallbackClassGeneric`
error identically regardless of whether the original attempt might have already
completed server-side).

**Scope limit, stated plainly:** even the OpenAI/Anthropic half of this fix only
closes the gap for a retry that lands on the SAME deployment/provider that made
the original, ambiguous-outcome call — an `Idempotency-Key` sent to a *different*
deployment (a different provider, or a different model) that never saw the
original attempt is meaningless. Kelvran's own fallback chains frequently retry
to a different deployment after a first-hop failure, which narrows this fix's
real-world value to same-deployment retries specifically — see Open Questions.

## Open Questions

- Has Kelvran's real (Bedrock) production pilot ever actually observed a client
  mid-stream disconnect, and what fraction of real traffic does it represent?
  Unlike most of Kelvran's other deferred reliability gaps, Finding 1's fix
  doesn't need a traffic-volume floor to be worth building (it's valuable even
  at n=1 — every disconnected stream today loses real money with zero
  visibility), but a real number would still sharpen how urgently to prioritize
  it against the rest of the backlog.
- Do Kelvran's own `fallback_chains` configs, in practice, ever list the SAME
  deployment as a retry target for a `FallbackClassGeneric` failure (making
  Finding 3's OpenAI/Anthropic fix immediately valuable for observed traffic
  shapes), or does every real config always route a fallback hop to a genuinely
  different deployment (making the fix a smaller, forward-looking safety net
  rather than a fix for anything actually happening today)? No config was
  reviewed for this pass — this is a real, checkable question, not
  rhetorical.
- What TTL/store is right for Finding 2's Idempotency-Key cache, given Kelvran's
  own existing cache-layer TTLs (L1 defaults to 5 minutes) are far shorter than
  Stripe/OpenAI/Anthropic's converged ~24-hour default? The two mechanisms serve
  genuinely different purposes (one answers "is this answer still fresh enough
  to reuse," the other answers "did I already execute this specific client
  operation") and may reasonably need different lifetimes — this needs an
  explicit decision, not an accidental inheritance of the response-cache's own
  TTL.
- Is estimating a disconnected/truncated stream's cost from
  `acc.totalContentLen()`'s existing character-per-token proxy (Finding 1's
  proposed fix) accurate enough to trust as a real charge against a hard per-key
  USD cap, or should it be surfaced only as an observability/FinOps-
  reconciliation signal, explicitly flagged as an estimate rather than ever
  silently subtracted from budget headroom? This is a real product-policy
  decision about Kelvran's own billing philosophy, not a pure engineering
  question, and deserves an explicit answer before implementation rather than
  an implicit one baked into the first PR.

## Caveats

Several external sources here (Aivene, BUZZ AI Gateway, Gemini Lab, "The
Cancellation Tax") are independent, dated practitioner/blog writeups rather than
vendor-neutral standards bodies — treated as real, plausible, internally-
consistent production evidence (and cross-checked against each other and against
the two directly-verifiable LiteLLM GitHub PRs/issue, which are primary,
linkable source-code/issue-tracker evidence), never as authoritative
specifications on their own. LiteLLM's own fix (PR #33736) shipped very recently
and had its own immediate correctness follow-up bug (#37992) — a caution that
this exact class of fix is genuinely easy to get subtly wrong, not a reason to
avoid building it. The Bedrock/Gemini "no idempotency primitive today" claim in
Finding 3 is grounded in each provider's own current, live API reference,
checked directly rather than from training-data memory — this is exactly the
kind of "provider capability" claim with a real shelf life and should be
re-verified before any spec built on it ships, in case either provider adds one
later. This report deliberately stayed out of the fleet-wide retry-budget/
priority-shedding/passive-health-signal territory
`gateway-reliability-resilience-round4-2026-09-11.md` already covers in depth —
that report is about Kelvran's own retries to its fallback deployments at the
infrastructure/circuit-breaker level; this report is about the client-to-Kelvran
request contract and Kelvran-as-a-client-of-its-own-providers, a related but
distinct layer.

## Sources

- Stripe: https://docs.stripe.com/api/idempotent_requests
- Stripe: https://stripe.com/blog/idempotency ("Designing robust and predictable APIs with idempotency")
- Stripe idempotency deep-dive: https://sujeet.pro/articles/stripe-idempotency-reliability
- OpenAI Python SDK source: https://github.com/openai/openai-python/blob/main/src/openai/_base_client.py
- OpenAI idempotency guide: https://theneuralbase.com/openai/learn/advanced/request-idempotency/
- Anthropic SDK source: https://github.com/anthropics/anthropic-sdk-python/blob/main/src/anthropic/_base_client.py
- Anthropic SDK README (retries): https://github.com/anthropics/anthropic-sdk-python/blob/main/README.md
- Anthropic idempotency/dedup guide: https://theneuralbase.com/anthropic-api/learn/advanced/request-deduplication/
- Anthropic Batch API idempotency key usage: https://kindatechnical.com/claude-ai/retries-with-exponential-backoff-and-jitter.html
- LiteLLM PR #33736: https://github.com/BerriAI/litellm/pull/33736
- LiteLLM PR #30630: https://github.com/BerriAI/litellm/pull/30630
- LiteLLM Issue #37992: https://github.com/BerriAI/litellm/issues/37992
- Aivene billing policy: https://docs.aivene.com/billing/what-gets-charged
- "Billed for Nothing: How Streaming Disconnects Turn Estimates Into Charges": https://buzzai.cc/blog/streaming-disconnect-phantom-billing
- "The Cancellation Tax: Your Inference Bill After the User Hits Stop": https://tianpan.co/blog/2026-04-23-cancellation-tax-streaming-abort-billing
- "Idempotency Key Design for the Gemini API": https://gemilab.net/en/articles/gemini-api/gemini-api-idempotency-key-design-multisite-production
- Cloudflare AI Gateway streaming cost bug: https://github.com/cloudflare/ai/issues/470
- Cloudflare AI Gateway community report (streaming input/output not counted): https://www.answeroverflow.com/m/1413614354439344189
- AWS Bedrock Converse API reference: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html
- AWS Bedrock Converse/ConverseStream boto3/botocore reference: https://docs.aws.amazon.com/boto3/latest/reference/services/bedrock-runtime/client/converse.html, https://docs.aws.amazon.com/botocore/latest/reference/services/bedrock-runtime/client/converse_stream.html
- Google Cloud Vertex AI retry/idempotency guidance: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/retry-strategy
- Gemini API troubleshooting/retry guide: https://ai.google.dev/gemini-api/docs/troubleshooting
- Go language specification, Program execution: https://go.dev/ref/spec#Program_execution
- `gateway/internal/gateway/dataplane/dataplane.go` (Reserve/Reconcile, runMissPath, finalize, NewHTTPUpstreamCaller/NewHTTPUpstreamStreamCaller)
- `gateway/internal/gateway/dataplane/streaming.go` (HandleChatCompletionStream, streamDeployment(Bedrock), finishStreamedResponse)
- `gateway/internal/gateway/dataplane/streamrunaway.go` (streamRunawayCharsCeiling, checkMidStreamReservationTopup)
- `gateway/internal/gateway/dataplane/fallback.go` (FallbackClassGeneric classification)
- `gateway/internal/gateway/dataplane/retry_after.go`
- `gateway/internal/gateway/dataplane/streaming_test.go` (`TestHandleChatCompletionStreamNoFallbackAfterFirstByte`, `TestHandleChatCompletionStreamTruncatedResponseNeverCached`)
- `gateway/internal/budget/budget.go` (Reserve/Reconcile/IncreaseReservation)
- `gateway/cmd/gateway/main.go` (gracefulShutdownTimeout, r.Context() wiring)
- `docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md`
- `docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`
- `docs/rfcs/2026-09-08-gateway-streaming-runaway-completion-guard.md`
- `docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md`
- `docs/upgrade-research/gateway-reliability-resilience-round4-2026-09-11.md` (adjacent territory, deliberately not re-covered)
- `AGENTS.md` (cache hard-gate rule)
- `DECISIONS.md` (confirmed: no prior idempotency decision exists)
