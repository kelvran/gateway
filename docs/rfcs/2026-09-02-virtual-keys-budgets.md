- **Status**: accepted
- **Date**: 2026-09-02
- **Author(s)**: project founder + Claude Code

## Summary

Replace the gateway's single static virtual key with real multi-tenant identity: multiple statically-configured virtual keys, each with its own per-key rate limit, per-key USD budget, and optional per-key allowed-model list. Because this is the first time more than one tenant can exist, this RFC also closes a latent cross-tenant cache-leakage gap: `cache.Key()` gains a tenant dimension in the same pass, so it is never true — not even for one commit — that Kelvran supports multiple tenants without tenant-isolated caching.

## Motivation

`PRD.md`'s v1 scope explicitly commits to "virtual API keys; per-key budget/rate limits," and `gateway/ARCHITECTURE.md`'s Data Model already describes virtual keys resolving to "team/workspace/budget/rpm-tpm/allowed-model records." None of this exists yet — `internal/identity` implements exactly one configured key, checked against a single global `ratelimit.TokenBucket`, with no budget concept and no per-key model restriction at all. This is also a `THREAT_MODEL.md` commitment already made and not yet delivered: its Spoofing mitigation row states "Virtual keys are opaque, unguessable, tenant-scoped tokens validated against `internal/identity` on every request," and its LLM10 Unbounded Consumption row cites "Rate limiting, budgets... (Gateway)" as an existing control. Today there is nothing tenant-scoped about the one key that exists, and there is no budget enforcement at all.

The reason this can't be scoped as "just add more keys" is `internal/cache/key.go`: `Key(model, serializedMessages, temperature, maxTokens)` has no tenant dimension. With exactly one implicit tenant this was never exploitable, but the moment a second virtual key exists, two different tenants asking the same question would silently share one cache entry — exactly the **cross-tenant cache leakage** class `THREAT_MODEL.md`'s Cache STRIDE table already names as a real, published attack (KeyPooling: "exploitable in 5 of 5 tested production-representative gateways") and already commits to defending against ("Tenant namespace baked into the vector-index partition itself ... and enforced at every hop"). Shipping virtual keys without also namespacing the cache would ship that exact vulnerability, not just leave a documented gap.

## Detailed Design

### Why hashes, not env-var names

Every existing secret in this codebase (provider API keys, the old single gateway key) follows the same pattern: config holds the *name* of an environment variable, never the value, because the value is a third party's credential Kelvran must protect on someone else's behalf. Virtual keys are different — they're a credential **Kelvran itself issues** to its own callers. The config doesn't need to protect the raw bearer token at all; it only needs to verify a presented token matches one it issued. So virtual keys are configured by a **SHA-256 hash of the actual secret**, not an env-var indirection:

```yaml
virtual_keys:
  team-alpha:
    key_hash: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a1"  # sha256(actual secret)
    budget_usd: 100.0                    # 0 or absent = unlimited
    rate_limit:
      burst: 20
      refill_per_second: 10
    allowed_models: ["gpt-4o"]           # absent/empty = all configured models allowed
```

A SHA-256 hash of a sufficiently high-entropy random secret (the operator generates the raw key themselves, e.g. `openssl rand -hex 32`) is not invertible and is not itself sensitive — this is the same reasoning that makes committing a bcrypt password hash safe when committing the password would not be. This means `config.example.yaml` can now ship **real example hashes of made-up example keys**, which is strictly more useful than another `<env var name>` placeholder.

This is a breaking config-format change to the gateway's top-level `api_key_env` field (removed entirely, replaced by `virtual_keys`). There is no tagged release and no deployed instance yet (`STATUS.md`: "Current Version: Unreleased"), so this is the cheapest possible moment to make this change — waiting would mean a real migration later instead of a config-file edit now. No `UPGRADE.md` entry is needed for the same reason (that file documents breaking changes *between released versions*; there is no prior release this breaks).

### `internal/identity` — new data model

```go
type VirtualKey struct {
    ID              string              // config key name, e.g. "team-alpha" — never the secret itself
    KeyHash         string              // hex-encoded sha256 of the actual secret, from config
    BudgetUSD       float64             // 0 = unlimited
    AllowedModels   map[string]struct{} // empty/nil = all configured models allowed
    RateLimitBurst  float64
    RateLimitRefill float64
}

type Verifier struct { /* unexported: map[string]*VirtualKey keyed by hex key hash */ }

func NewVerifier(keys []VirtualKey) (*Verifier, error)

// Verify now resolves and RETURNS the matched key, not just pass/fail —
// every downstream check (rate limit, budget, allowed-models, cache
// namespace, structured logging) needs to know WHICH key this is.
func (v *Verifier) Verify(authorizationHeader string) (*VirtualKey, error)
```

`Verify` extracts the bearer token, computes its SHA-256 hash, and compares against every configured key's hash using `crypto/subtle.ConstantTimeCompare` (looping over all configured keys unconditionally, never short-circuiting on the first match attempt) — preserving the existing timing-side-channel defense from the single-key implementation, now across N keys instead of one. `ErrMissingHeader` and `ErrInvalidKey` are unchanged; a third, `ErrDuplicateKeyHash`, is returned by `NewVerifier` if two configured keys hash to the same value (a config error, not a runtime one).

### New `internal/budget` package

A new, single-responsibility package — mirroring `internal/ratelimit`'s own shape and its own explicit "single-instance, in-memory" scope note:

```go
package budget

// Tracker enforces a per-key cumulative USD spending cap, held in memory
// for the process lifetime. Not persisted: a restart resets every key's
// spend to zero. This is a documented Phase 2 gap (a real control-plane
// store doesn't exist yet — see gateway/ARCHITECTURE.md's /internal/admin
// entry), not a silently accepted one.
type Tracker struct { /* unexported: mutex + map[string]float64 */ }

func NewTracker() *Tracker

// Allow reports whether keyID has remaining budget under capUSD, given
// its cumulative spend so far. capUSD <= 0 means unlimited (always true).
func (t *Tracker) Allow(keyID string, capUSD float64) bool

// Record adds costUSD to keyID's cumulative spend. Called once per
// request, after cost is known — including on requests that produced
// partial/zero usage, matching gateway/ARCHITECTURE.md's existing "cost/
// observability finalization always runs" principle.
func (t *Tracker) Record(keyID string, costUSD float64)
```

### `internal/cache` — tenant-namespaced keys

```go
// Key gains a leading tenantID parameter. Every existing caller updates;
// there is no migration path for old cache entries (Cache has no
// persistence across process restarts yet — inprocess is the only active
// adapter — so there is nothing to migrate).
func Key(tenantID, model string, serializedMessages string, temperature *float64, maxTokens *int) string
```

`tenantID` is `VirtualKey.ID`. This closes the cross-tenant leakage gap described above by construction, not by convention: two different keys asking byte-for-byte identical questions now always hash to different cache entries.

### Dataplane wiring (`HandleChatCompletion` and `HandleChatCompletionStream`)

Both entry points change identically:

1. `vk, err := p.verifier.Verify(authHeader)` — was previously a bare `error`; now resolves the calling tenant.
2. **Allowed-models check** (new, first — cheapest, no side effects yet): if `vk.AllowedModels` is non-empty and doesn't contain `req.Model`, return a new typed `ErrModelNotAllowed`.
3. **Per-key rate limit**: `Pipeline` now holds `map[string]*ratelimit.TokenBucket`, one per configured `VirtualKey.ID`, built once at `NewPipeline` time from each key's `RateLimitBurst`/`RateLimitRefill` — replacing the single global `*ratelimit.TokenBucket` field. `ErrRateLimited` is unchanged as a sentinel; it now means "this key's bucket is empty," not "the gateway's one global bucket is empty."
4. **Budget check**: `if !p.budget.Allow(vk.ID, vk.BudgetUSD) { return ErrBudgetExceeded }` (new sentinel).
5. **Cache key fabrication**: `cache.Key(vk.ID, req.Model, ...)` instead of `cache.Key(req.Model, ...)`.
6. **On completion** (inside `logRequest`, which already runs via `defer` on every path per the existing "always finalize" pattern): after computing `cost`, call `p.budget.Record(vk.ID, cost)`, and add `"virtual_key_id", vk.ID` to the structured log line.

`cmd/gateway/main.go`'s `writeErrorResponse` gains two new cases: `ErrBudgetExceeded` and `ErrModelNotAllowed` both map to HTTP **429** and **403** respectively — 429 for budget, matching OpenAI's own API precedent of returning 429 with an `insufficient_quota`-style error for billing/budget failures (not just literal request-rate failures), which matters here because Kelvran's canonical schema explicitly targets OpenAI-SDK client compatibility; 403 for a valid-but-unauthorized-for-this-model key, matching the existing REST convention that a resource exists and the caller is authenticated but not entitled to it.

### What is explicitly deferred (stated up front, not discovered later)

- **No live/no-restart key provisioning** — still static YAML + process restart. `/internal/admin`'s "declarative config, live no-restart mutation" (per `gateway/ARCHITECTURE.md`) remains unbuilt.
- **No persistent budget/usage tracking** — in-memory only, resets on restart. A real control-plane store (Postgres, per `gateway/ARCHITECTURE.md`'s Tech Stack) is Phase 2.
- **Rolling-window budget reset** (e.g. "monthly" budgets) is real as of 2026-09-05: a positive `budget_reset_interval_seconds` on a virtual key turns `budget_usd` into a real rolling window instead of a lifetime-of-the-process cap — checked lazily on each `Allow`/`SpentUSD`/`Record` call (mirroring `internal/ratelimit.TokenBucket`'s own lazy-refill design), never a background scheduler/ticker. The reset window's own boundary (not the cumulative spend total, which was already durable per the budget-persistence RFC) is intentionally not itself persisted — a restart resets a key's reset-window clock to the restart moment, extending that one window by at most the configured interval, a narrow and self-limiting gap named explicitly in `internal/budget`'s own doc comments, not hidden.
- **No hierarchical scope resolution** (org → team → user → key → session, per `DESIGN.md`'s Open Design Questions) — this ships flat, key-level scope only. The hierarchy stays an implementation detail for a later pass, exactly as `DESIGN.md` already flagged it.
- **Rate limiting is still single-instance, in-memory** — now per-key instead of one global bucket, but still not the Redis-backed distributed limiter flagged as a candidate when the streaming RFC was written. That candidate is deferred a second time, deliberately, not by oversight — `DECISIONS.md` records this explicitly so it doesn't read as a dropped thread.
- **No key-generation CLI** — the operator runs `openssl rand -hex 32` (or equivalent) once per key and hashes the result themselves (`sha256sum`, or any standard tool) to produce `key_hash`. A bespoke Kelvran keygen command would be maintenance for something two off-the-shelf commands already do.

## Drawbacks

- Breaking change to `cache.Key()`'s signature and to the gateway's YAML config format. Accepted because there is no released version and no deployed instance to migrate — see "Why hashes, not env-var names" above.
- In-memory-only budgets mean a gateway restart silently resets every key's spend to zero. This is a real limitation, not a rounding error, for any operator relying on budgets as a hard financial control across restarts — explicitly flagged rather than glossed over, and the honest reason persistent tracking isn't built yet (no control-plane store exists) is stated plainly above.
- Looping over every configured key on every request in `Verify` is O(n) in the number of virtual keys, rather than O(1) via a hash-indexed lookup keyed by the presented token's own hash directly. This RFC accepts O(n) deliberately: a true O(1) lookup (`map[hash]*VirtualKey` indexed by the *computed* hash of the presented token, not a linear scan) is actually both simpler AND faster, and does not weaken the timing defense (comparing digests via direct map lookup leaks nothing time-wise beyond "is this exact hash present," since a cryptographic hash's output is already uniformly distributed and unrelated to prefix-matching auth tokens). This is captured as a concrete implementation choice in the plan, not left as a real O(n) cost in the shipped code — noted here only because it means "loop + `ConstantTimeCompare` per key" from earlier drafts of this design is superseded by a map lookup on the hash, which is simpler, not a tradeoff.

## Alternatives Considered

1. **Keep cache un-namespaced and rely on "don't build cross-tenant deployments yet" as a policy** — rejected: this is exactly the gap `THREAT_MODEL.md` already promises is closed, and policy-only mitigations for a named, published attack class are not an acceptable substitute for a structural fix that costs one extra function parameter.
2. **Env-var-per-key instead of hashes** (mirroring provider API keys' pattern) — rejected: would require one environment variable per tenant, doesn't scale past a handful of keys, and treats Kelvran-issued credentials as if they were third-party secrets, which they aren't.
3. **429 for budget-exceeded vs. 402 Payment Required** — 402 is semantically closer to "you must pay to continue," but real OpenAI-SDK-compatible client tooling has no special handling for 402 and does have retry/backoff handling for 429; since Kelvran's canonical schema explicitly targets OpenAI-SDK compatibility, interoperability won over semantic precision here.
4. **Build the full hierarchical org→team→user→key→session scope now** — rejected as premature: nothing in the current codebase has more than one level of scope to resolve yet (there is no "team" or "org" concept anywhere), and `DESIGN.md` already deferred this exact question to "implementation detail settled during Gateway's Phase 1 build" — this RFC is that Phase 1 build, and flat key-level scope is the right size for it.

## Unresolved Questions

- Whether `ErrBudgetExceeded` should eventually distinguish "hard cap, request rejected outright" from "soft cap, request allowed but flagged" — left as hard-cap-only for v1; no evidence yet that a soft-budget mode is needed.
- Whether per-key rate limits should support RPM/TPM (requests-per-minute / tokens-per-minute) as two separate dimensions rather than one token-bucket-of-requests — `gateway/ARCHITECTURE.md`'s Data Model language ("rpm-tpm") implies both eventually; this RFC ships request-rate limiting only (mirroring the existing single-bucket limiter's own scope), with TPM left as a follow-up once there's a concrete need to shape traffic by token volume rather than request count.
