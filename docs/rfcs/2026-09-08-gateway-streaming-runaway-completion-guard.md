# RFC: Mid-stream runaway-completion guard

## Status

Accepted, implemented 2026-09-08.

## Context

`evals/tests/fixtures/regression_corpus_cost_abuse.json`'s
`costabuse-streaming-budget-allow-not-rechecked-midstream` case (revision 1, an honest
FAIL) documents a real gap, independently re-confirmed against the live code before this
RFC: `gateway/internal/gateway/dataplane/dataplane.go`'s `checkRateLimit`/`budget.Reserve`
and `streaming.go`'s `HandleChatCompletionStream` call `limiter.ReserveTPM`/
`budget.Reserve` exactly ONCE, before a streamed request begins, and `finalize`'s
`ReconcileTPM`/`Reconcile` (run via `defer`, per
`docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md`) run exactly ONCE, after the
entire stream has finished. Between those two points, a single streaming request can run
for an arbitrarily long time and generate an arbitrarily large completion — a runaway or
maliciously-crafted request designed to produce a huge output — with zero recheck against
what was ever reserved for it.

This is distinct from the TOCTOU races the budget/ratelimit RFC above already closed: that
fix bounds N *concurrent* requests against each other (a shared reservation pool, raced by
multiple callers). This gap is about ONE already-approved request's own real cost vastly
exceeding its own reservation — no amount of concurrency-race hardening touches it, since
there's only ever one request involved.

### The constraint this fix must respect

`docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md` already established, and this RFC does not
revisit, that no tokenizer or token-count estimator exists anywhere in this codebase — real
usage is only known from a provider's own final usage frame, if one ever arrives (see
`finishStreamedResponse`'s existing `stream_missing_usage` warning for the already-accepted
"maybe never" case). A mid-stream guard therefore cannot count real tokens. The only thing
genuinely available as chunks arrive is accumulated output text, via
`streamaccumulator.go`'s `strings.Builder`-backed `streamAccumulator.add` — no token counts,
until (if ever) a final usage frame shows up.

## Design

### The proxy: accumulated output character length

`streamAccumulator` gains a `totalContentLen() int` method summing every choice's
`strings.Builder.Len()` — O(1) per call, since `Len()` is tracked incrementally, never
re-scanned. This is checked once per decoded chunk batch, in the same loop that already
calls `acc.add(c)`, in both `streamDeployment` (SSE-framed providers: openai, anthropic,
gemini, openaicompat) and `streamDeploymentBedrock` (binary `ConverseStream` framing) — the
two genuinely different decode loops `docs/rfcs/2026-09-04-bedrock-converse-stream.md`
already established need separate wiring for anything chunk-loop-scoped.

### The ceiling: a generous multiple of `req.MaxTokens`, or a fixed absolute floor

`streamrunaway.go`'s `streamRunawayCharsCeiling(maxTokens *int) int`:

- If the client set a positive `MaxTokens`, the ceiling is
  `MaxTokens * streamRunawayCharsPerToken(4) * streamRunawayMaxTokensMultiplier(10)`.
  `streamRunawayCharsPerToken = 4` is the commonly cited chars-per-token heuristic for
  English text (e.g. OpenAI's own public tokenizer guidance) — explicitly a PROXY, not a
  precise count, carrying the same caveat the TPM RFC already states for its own retrospective
  design. `streamRunawayMaxTokensMultiplier = 10` is deliberately generous rather than tight
  (2x/3x), for two independent reasons: (1) the chars-per-token proxy is itself imprecise —
  code-heavy or non-English content can legitimately run at a very different density in
  either direction, and a tight multiplier would false-positive on ordinary traffic shaped
  differently than the heuristic assumes; (2) a well-behaved upstream provider already
  enforces `MaxTokens` as its OWN hard cap — this guard is defense-in-depth against a
  misbehaving/malicious upstream or provider bug, not the primary enforcement mechanism for
  the ordinary case. 10x sits comfortably past any plausible combination of both slack
  sources while still bounding a genuinely runaway stream to a small, finite multiple of what
  was actually asked for.
- If the client set no `MaxTokens` at all (nil or `<= 0`), the ceiling falls back to
  `streamRunawayAbsoluteCharsCeiling = 750_000` characters. Sized comfortably above the
  largest legitimate SINGLE completion this gateway's configured providers can plausibly
  produce — a large generated-code-file response, not merely an "average" chat reply: the
  largest publicly documented single-completion output-token limits among providers this
  codebase has adapters for (openai, anthropic, gemini, bedrock, openaicompat) sit in the
  ~100K-128K output-token range as of this writing (e.g. Anthropic's extended-output mode,
  OpenAI's larger reasoning-output models). At 4 chars/token, 128,000 tokens is 512,000
  characters; 750,000 rounds further up from there, comfortably above even that ceiling.

### What happens when the ceiling is crossed: a clean, normal-looking truncation, not an error

Tripping the guard does three things, all inline in the read loop, right after the
`acc.add`/`sw.WriteChunk` pass that already delivered the crossing chunk to the client:

1. Logs one clear warning, `streaming_runaway_guard_triggered`, with `key_id`, `deployment`,
   `provider`, `model`, `accumulated_chars`, and `ceiling_chars` — never a generic error,
   since this is an intentional, working control tripping as designed, not a bug elsewhere.
2. Cancels `upstreamCtx` — a context.Context created as a CHILD of the request's own `ctx`
   specifically for this one upstream call (`upstreamCtx, cancelUpstream :=
   context.WithCancel(ctx)`, deferred unconditionally), never `ctx` itself. This is the one
   genuinely new piece of plumbing this RFC adds: previously `streamDeployment`/
   `streamDeploymentBedrock` passed the caller's own `ctx` straight into
   `p.upstreamStream(ctx, ...)`. Passing a narrower, purpose-scoped child context instead
   means the guard can cancel exactly the upstream connection/body `Read` — the same
   mechanism `NewHTTPUpstreamStreamCaller`'s own idle-timeout `cancel()` already uses to
   interrupt a stalled real HTTP body — without touching `ctx`, which `finishStreamedResponse`'s
   guardrail post-call check (below) and `finalize` (dataplane.go, via `defer` in the caller)
   both still need to run normally afterward.
3. Breaks the read loop — no further chunks are read from upstream.

Control then falls through to the SAME `finishStreamedResponse` call every normal
completion already uses: it builds the final (truncated) `ChatResponse` from whatever `acc`
accumulated, runs the existing audit-only post-call guardrail check, and writes the client's
normal done-sentinel (`sw.WriteDone()`) — so the client sees a truncated but well-formed,
valid stream, indistinguishable in shape from a provider that simply stopped sending chunks
on its own. `streamDeployment`/`streamDeploymentBedrock` return a nil error on this path,
deliberately: the request already legitimately started and passed its own budget/TPM
reservation, so a guard-triggered truncation is not classified as `OUTCOME_UPSTREAM_ERROR`,
and `streamDeploymentWithFallback`'s `err == nil` early-return means no fallback hop is
attempted for an already-serving, merely-truncated stream (mirroring the existing
"no further hop once a chunk reached the client" rule for a genuine failure).

`finalize` (dataplane.go) is unaffected and still runs exactly once via the caller's
existing `defer` — verified directly, not assumed: nothing about this change alters
`HandleChatCompletionStream`'s own control flow, only what `streamDeploymentWithFallback`'s
call chain does internally before returning to it. Cost/TPM reconciliation on this path
uses whatever `finalUsage` is known at truncation time — which, for a provider whose final
usage frame arrives only as the LAST event of a now-abandoned stream, is `nil`, hitting the
exact same, already-existing `stream_missing_usage` warning path a provider that never
sends usage at all already triggers. This is a deliberate non-goal, not a new gap: solving
"estimate a probably-more-honest partial cost from the character-length proxy" would require
treating the same proxy this RFC explicitly disclaims as precise for the guard decision as
also precise enough for real billing — a materially different, larger claim this RFC does
not make.

### `writeFakeStream` (the cache-hit synthesis path) is deliberately untouched

`writeFakeStream` replays an ALREADY-COMPLETE, already-bounded `ChatResponse` from a cache
hit — there is no live, open-ended upstream connection to guard against runaway growth,
because the size is already fixed and known before synthesis starts. If that response was
originally produced by a genuine cache-miss stream, it was already subject to this guard at
write time; re-applying a length check to a value that's already finished and already
capped would be redundant, not a real gap.

## Alternatives considered

**A real per-provider tokenizer, checked mid-stream** — rejected outright, per
`docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md`'s already-standing decision: no such
dependency exists in this codebase, and adding one now — for a guard that only needs a
coarse "is this obviously runaway" signal, not an exact count — would be a large, disproportionate
addition to solve a narrower problem than exact billing.

**Re-checking `budget.Tracker`/`ratelimit.KeyLimiter`'s live remaining headroom mid-stream**
(literally re-running the reservation check against current state, possibly against a
DIFFERENT concurrent request's spend on the same key) — considered, since this is closer to
the corpus case's own literal framing ("interrupted... once the caller's own budget cap is
crossed... by another concurrent request"). Rejected for this pass: it conflates two
genuinely separate concerns this RFC's own task framing explicitly separates — bounding
concurrent requests against each other (already solved by the TOCTOU reserve/reconcile
design) versus bounding ONE request's own unbounded growth. A live mid-stream budget
recheck would also need a new per-chunk read of shared, lock-protected budget state from
inside the hot streaming loop — a real, separate performance/locking design question this
RFC's narrower character-length-proxy guard avoids entirely by staying self-contained to
the one request's own accumulator.

**A fixed, small multiplier (2x-3x) instead of 10x** — rejected: verified by construction
that the 4-chars/token proxy is not precise enough to trust that tightly — code-heavy or
non-English completions can legitimately run at a meaningfully different density, and a
tight multiplier risks truncating a legitimate response, which is a real product regression,
not merely a missed edge case.

**Canceling the caller's own top-level `ctx` instead of a new child `upstreamCtx`** —
rejected: `ctx` is still needed, after the guard trips, for the unchanged post-call
guardrail check and for `finalize`'s telemetry/logging/reconciliation — canceling it would
risk those silently degrading (e.g. an OTel exporter skipping a canceled-context recording)
for a request that otherwise completed and needs to be accounted for normally.

## Verification

New tests in `gateway/internal/gateway/dataplane/streamrunaway_test.go`:
`TestStreamRunawayCharsCeiling` (unit-level, pins both ceiling branches against the named
constants); `TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletion` (SSE
path: a mock upstream willing to stream up to 1,000,000 50-char frames is cut off within a
handful of frames once a 200-char `MaxTokens`-derived ceiling is crossed, proven via the
mock's own `FramesServed` counter, the client body's actual content-character count, and —
load-bearing — the mock's own observed `context.Context` argument genuinely transitioning
to canceled, checked directly via a follow-up `Read` call after `HandleChatCompletionStream`
already returned); `TestHandleChatCompletionStreamRunawayGuardCutsOffExcessiveCompletionBedrock`
(the identical proof for `streamDeploymentBedrock`'s separate binary-framed decode loop, per
this task's own instruction not to assume the SSE fix's coverage carries over unchecked);
`TestHandleChatCompletionStreamRunawayGuardUnaffectedForOrdinaryStream` (an ordinary,
well-within-ceiling stream is delivered byte-for-byte identically, proving zero regression
for normal traffic). Sanity-checked by breaking: the ceiling-check condition was temporarily
short-circuited to `false` in both loops (one at a time) — both cutoff tests then failed for
the documented right reason (the bounded 5s outer test context expired, surfacing a real
`context deadline exceeded` error, instead of a clean nil-error truncation), never
vacuously; both reverted, confirmed via `git diff`/`grep` showing zero trace before
committing.

`evals/tests/fixtures/regression_corpus_cost_abuse.json`'s
`costabuse-streaming-budget-allow-not-rechecked-midstream` case bumped to revision 2, with
`output`/`reference` updated to describe the real, now-fixed guard behavior — re-verified
against the actual fixed code (this RFC + its tests), not edited to match a hoped-for
result.

Full chain: `cd gateway && go build ./... && go vet ./... && gofmt -l . && golangci-lint run
./... && go run github.com/fe3dback/go-arch-lint@v1.18.0 check && go test ./... -race` —
clean except the two pre-existing, environment-only rootless-Docker failures
(`TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`,
`internal/ratelimit/redislimiter`'s own `TestMain`), confirmed unrelated to this change.
