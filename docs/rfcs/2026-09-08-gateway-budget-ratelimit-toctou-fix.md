# RFC: Closing the budget/TPM check-then-act (TOCTOU) race

## Status

Accepted, implemented 2026-09-08.

## Context

`docs/agents/LOGS.md`'s 2026-09-08 entry ("Research-driven corpus growth: 28→83 cases...") and
`evals/tests/fixtures/regression_corpus_cost_abuse.json`'s `costabuse-budget-allow-record-toctou-race`
and `costabuse-ratelimit-tpm-hasbalance-debit-toctou-race` cases document two 100%-reproducible
TOCTOU (check-then-act) races, independently re-verified by that session's orchestrator with a
fresh concurrent Go test before trusting the finding:

- `gateway/internal/budget.Tracker.Allow` (the check) and `Tracker.Record` (the debit) are called
  from two entirely separate points in `gateway/internal/gateway/dataplane/dataplane.go`: `Allow`
  inside `HandleChatCompletion`/`HandleChatCompletionStream`, before the cache lookup and the real
  upstream provider call; `Record` only inside `finalize`, after the upstream response has
  returned and cost is computed. Each of `Allow`/`Record` independently acquires and releases
  `Tracker.mu` — no lock, reservation, or other synchronization spans the two calls.
- `gateway/internal/ratelimit`'s TPM dimension has the identical shape: `KeyLimiter.AllowTPM` calls
  `TokenBucket.HasBalance` (a non-consuming check) from `checkRateLimit`, before the upstream call;
  `KeyLimiter.RecordTokens` calls `TokenBucket.Debit` only from `finalize`, after real usage is
  known.

Both gaps let N concurrent requests against a virtual key with exactly one request's worth of
headroom all pass the check before any single request's debit commits — verified at 20/20
allowed against a $1 cap, real spend $20, reproducible across 5 consecutive runs. The real-world
race window is the entire upstream LLM completion latency (hundreds of milliseconds to several
seconds), not an adversarially-synchronized single-packet burst — ordinary concurrent agentic
traffic exploits this by construction.

### The constraint this fix must respect

`docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md` explicitly rejected a pre-call reserve-then-
reconcile design for TPM specifically because **no tokenizer or token-count estimator exists
anywhere in this codebase** — real usage (`resp.Usage`) is only known after the upstream call
completes. That constraint has not changed. This RFC's fix must close the race without ever
needing an exact pre-call cost/token estimate, and must not simply serialize all traffic for a key
across the full upstream-call latency (that would trade a correctness bug for a real concurrency
regression, not an acceptable fix).

`docs/upgrade-research/evals-corpus-cost-abuse-patterns-2026-09-08.md`'s Finding 2 cites an
"atomic reserve-then-reconcile step" as the general shape a fix would take, and
`docs/upgrade-research/evals-corpus-cost-abuse-patterns-2026-09-08.md`'s scenario 10 references
LiteLLM-adjacent "over-estimated pre-call reservation" framing as inspiration — but neither
document specifies a concrete reservation-sizing mechanism that works without a pre-call cost
estimate. LiteLLM's own real reservation patterns found in this codebase's research history
(`docs/upgrade-research/gateway-pretoken-counting-2026-09-07.md`, `docs/decisions`'s prior
rejection of pre-call reservation) all assume either a tokenizer or a `max_tokens`-as-worst-case
estimate — exactly the "naive way" this task rules out. The design below is original to this RFC,
built specifically to avoid that assumption.

## Design

### Reserve-then-reconcile, with the reservation amount computed entirely from the key's own
### already-tracked spend/usage history — never from a per-request cost estimate

Both `budget.Tracker` and `ratelimit`'s TPM dimension get the same shape, one new pair of
methods each, added alongside (never replacing) the existing primitives:

- `budget.Tracker.Reserve(keyID, capUSD, resetInterval) (allowed bool, reserved bool, reservedUSD decimal.Decimal)`
- `budget.Tracker.Reconcile(keyID string, reservedUSD decimal.Decimal, realCost *decimal.Decimal, resetInterval time.Duration)`
- `ratelimit.TokenBucket.ReserveTPM() (allowed bool, reservedTokens float64)` /
  `KeyLimiter.ReserveTPM(keyID) (allowed bool, reserved bool, reservedTokens float64)`
- `ratelimit.TokenBucket.ReconcileTPM(reservedTokens float64, realTokens *float64)` /
  `KeyLimiter.ReconcileTPM(keyID string, reservedTokens float64, realTokens *float64)`

`Allow`/`Record`/`AllowTPM`/`RecordTokens`/`HasBalance`/`Debit` are **untouched** — every existing
test for them keeps passing unmodified, and every other caller (`checkBudgetWarnThreshold`'s
`SpentUSD` read, the boltstore persistence tests, the reset-window tests) is unaffected. Only
`dataplane.go`'s/`streaming.go`'s two request-pipeline call sites switch to the new pair.

**Reserve** replaces the old "read-then-return" `Allow` with an atomic "read-check-and-mutate,
all under one lock acquisition" — the textbook TOCTOU fix (compare-and-swap in spirit): the check
(`spent < cap`) and the provisional debit happen inside the *same* `t.mu.Lock()`/`b.mu.Lock()`
critical section, so a second concurrent caller can never observe the pre-reservation state the
first caller just checked against. This is exactly the pattern `ratelimit.TokenBucket.Allow`
(RPM) already uses today (refill, check, decrement, all under one lock) — the RFC's fix is to
extend that same already-correct technique to budget and TPM, not to invent a new one.

**Reservation size — the part that avoids needing a cost estimate:**

- If the key has at least one previously-reconciled real cost/token value (`billedCount > 0`),
  reserve that key's own running average (`spent / billedCount` for budget, `billedTokens /
  billedCount` for TPM) — a value derived entirely from the key's own history, no tokenizer, no
  `max_tokens`-as-worst-case guess.
- If the key has no history yet (`billedCount == 0` — a brand-new key, or one whose window/store
  just reset), reserve the full remaining headroom (`capUSD - spent`, or the bucket's own current
  balance for TPM). This is the maximally conservative choice given zero information: it can
  never let a cold-start key's concurrent burst overshoot its cap, because the reservation itself
  is bounded exactly at the point past which the key would already be rejected serially.

**Reconcile** undoes the reservation (subtracts exactly the `reservedUSD`/`reservedTokens` value
`Reserve` returned — an exact decimal/float inverse of what `Reserve` added, so sequential,
non-concurrent access is byte-identical in final state to the old `Allow`+`Record` pair, verified
directly: `spent_before + reserved − reserved + realCost == spent_before + realCost`, exactly,
regardless of what the reservation estimate happened to be) and, if `realCost`/`realTokens` is
non-nil, applies the real value and increments `billedCount` — this is the only point at which the
running average moves, and the only point that persists to `Store` (matching `Record`'s existing
persistence behavior exactly; the provisional reservation itself is never persisted, since a crash
before `Reconcile` loses it together with the rest of in-memory state, correctly reverting to the
last-durably-saved total — no real cost was ever billed for that in-flight request).

### Every reservation MUST be reconciled — the release/cleanup path

A request that reserves and then errors, times out, or is a non-billable cache hit/coalesced
follower before ever reaching the real-cost point **must** release its reservation, or budget/TPM
capacity leaks permanently (a key that errors on every request would otherwise monotonically
shrink its own remaining headroom to zero, forever). `finalize` — already the single "this request
just finished, however it finished" hook, already called via `defer` so it runs on every return
path including error and panic-unwind, per `gateway/ARCHITECTURE.md`'s "cost/observability
finalization always runs" principle — is where this happens: it now calls `budget.Reconcile`/
`limiter.ReconcileTPM` unconditionally whenever `vk != nil` and either a reservation was actually
made or a real, billable cost exists (the union of both reasons to reconcile; a request that
never reached the corresponding `Reserve` call at all, e.g. rejected earlier by auth/model-
allowed/rate-limit, correctly skips the call entirely — nothing to release). `realCost`/
`realTokens` stays `nil` (release-only) unless `err == nil && billable` — the exact same gate the
old `Record`/`RecordTokens` calls used, so the billable/cache-hit/coalesced-follower double-
counting invariant (`docs/rfcs/2026-09-05-gateway-cost-double-counting.md`) carries over
unchanged.

A process crash between `Reserve` and `Reconcile` is explicitly out of scope for the same reason a
crash is already out of scope for the rest of `budget.Tracker`'s in-memory state: the reservation
is transient, in-memory-only, never persisted — a crash loses it together with everything else,
reverting to the last durably-saved (pre-reservation) total on restart, which is correct (no real
cost was ever billed for the lost in-flight request).

### Interaction with the already-shipped per-key `ConcurrencyLimiter` (2026-09-07)

`ConcurrencyLimiter.Acquire`/`Release` are untouched and unaffected — they bound a genuinely
different dimension (how many of a key's requests may be simultaneously outstanding, checked
once per request and released once the whole request finishes) from what this RFC bounds (how
much of a key's $ budget/token allowance may be provisionally committed at once, released or
reconciled per-dimension as soon as each individual check's own decision is known, not held for
the whole request). The two controls are independent and compose exactly as the existing check
ordering already implies (rate-limit → concurrency → budget, per `checkRateLimit`'s and
`checkConcurrency`'s own doc comments) — a request can be admitted by one and rejected by the
other in either direction, and nothing about this RFC changes that ordering or either control's
existing behavior.

### RPM's `TokenBucket.Allow` is not touched

`Allow` (RPM) already holds its lock across check-and-decrement — it was never the shape this bug
targets, confirmed by the same corpus's sibling PASS case
(`costabuse-ratelimit-rpm-allow-atomic-no-race`). This RFC adds `ReserveTPM`/`ReconcileTPM` as new,
TPM-only methods on the same `TokenBucket` type (reusing it rather than a parallel type, per the
TPM RFC's own precedent) and two new TPM-only bookkeeping fields (`billedTokens`, `billedCount`);
`Allow`'s own fields and code path are unmodified.

### How long the lock is held, and why that duration is acceptable

The critical section inside `Reserve`/`Reconcile` (and `ReserveTPM`/`ReconcileTPM`) is bounded to:
one map lookup, one decimal/float comparison, one arithmetic operation, one map write — no I/O, no
network call, no channel operation, nothing that can block on anything other than the mutex
itself. This is the same bound `TokenBucket.Allow` (RPM) and `Tracker.Record` already accept today
— sub-microsecond in practice, many orders of magnitude below the race window this RFC closes
(the full upstream call, hundreds of milliseconds to seconds). The lock is never held across the
upstream call, across `finalize`'s telemetry/logging work, or across anything that can block —
only across the atomic check-and-provisional-mutate step itself, which is exactly design option
(b) from this task's own framing: a narrow per-key admission lock that serializes only the
check-and-provisional-debit step, never the request's own latency.

### A named, accepted, self-resolving trade-off: cold-start conservatism

A virtual key's very first-ever concurrent burst (before any real cost/usage has been reconciled
for it even once) is throttled to admit only as many concurrent requests as its remaining headroom
divided by... nothing yet known, which collapses to "reserve everything, admit one" per the sizing
rule above. For a key with a large cap and a workload whose real per-request cost is a tiny
fraction of that cap, this means the very first wave of truly simultaneous requests (arriving
before any of them has completed even once) is admitted one at a time rather than at full
concurrency — but only until the first one completes and reconciles, at which point every
subsequent burst uses the now-known average and regains full, realistic concurrency for the rest
of that key's life (or until its next rolling-window reset, which reruns the same one-time,
self-resolving learning phase). This is deliberately not "fixed" further in this pass: doing so
would require exactly the pre-call cost/token estimate this RFC is built to avoid needing.
Named explicitly, matching this codebase's established practice of documenting a real, bounded
edge rather than chasing it with an unbounded-scope fix (e.g. the guardrail corpus's case 13, the
budget-persistence RFC's own restart/reset-window boundary).

## Alternatives considered

**Hold `Tracker.mu`/the TPM bucket's `mu` across the entire request, from `Allow` through
`Record`** — rejected outright per this task's own constraint: this is exactly "serialize all
traffic for a key across the full upstream-call latency," a real concurrency regression, not a
narrower fix.

**Pre-call reserve using `req.MaxTokens`/a per-adapter tokenizer** — rejected for the same reason
`docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md` and
`DECISIONS.md`'s 2026-09-07 "Pre-call TPM reservation... dropped as a candidate" entry both already
rejected it: no tokenizer exists, `MaxTokens` is frequently unset, and `adapter.Adapter`'s own
"pure function, no network calls" invariant would be violated by any adapter-side estimation call
— unrelated to and not reopened by this RFC.

**A fixed, hand-picked reservation constant (e.g. "always reserve $0.01" or "always reserve 50
tokens") instead of history-derived sizing** — rejected: an arbitrary constant either fails to
close a tight race for an expensive workload (too small) or needlessly serializes a cheap
workload (too large), with no principled way to pick a value that works across Kelvran's actual
range of per-model prices. A key's own historical average is the one value that's automatically
calibrated to that specific key's real workload, with zero new configuration surface.

**Track a proportional (percentage-of-headroom) cold-start reservation instead of the full
remaining headroom** — considered and rejected: verified directly (by hand-tracing the corpus's
own reproduction scenario, cap exactly equal to one request's real cost) that any reservation
smaller than the full remaining headroom fails to bound admission to the required "at most one"
for that exact, adversarially-relevant shape — a fractional reservation only closes races where
the true per-request cost happens to be a large fraction of the cap, which isn't knowable in
advance without the estimate this RFC avoids requiring.

**A distributed (Redis-backed) reservation, extending `RedisBackend`** — out of scope, mirroring
the TPM RFC's own existing Redis-mode scope limit: budget has no Redis-backed mode at all today,
and TPM's Redis mode is already a deliberate no-op; this RFC changes neither.

## Verification

See the "Real before/after pass rate" note in the corresponding `docs/agents/LOGS.md` entry for
this work. New tests: `gateway/internal/budget`'s `TestConcurrentReserveReconcileBoundsAdmission...`
family and `gateway/internal/ratelimit`'s TPM equivalents, using the identical barrier-based
concurrent-goroutine technique (every goroutine passes `Reserve`/`ReserveTPM` before any goroutine
calls `Reconcile`/`ReconcileTPM`, honestly modeling the real upstream-call gap) the original
finding used — proving the fix bounds admission correctly, and, via a temporary revert-and-rerun
sanity check, that the same test fails (reproducing the original 20/20-allowed race) against the
pre-fix code. `evals/tests/fixtures/regression_corpus_cost_abuse.json`'s two TOCTOU cases bumped to
revision 2 with `output` updated to the real, re-verified fixed behavior.
