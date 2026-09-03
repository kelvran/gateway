- **Status**: accepted
- **Date**: 2026-09-03
- **Author(s)**: project founder + Claude Code

## Summary

Add three fields to `api/gatewayevents/v1`'s `GatewayDecisionEvent` that `docs/rfcs/2026-09-03-api-gatewayevents-contract.md` explicitly named and deferred as "real, useful, not yet built": a **rate-limit fail-open flag**, **fallback-routing detail** (did a fallback happen, from which deployment, why), and **budget-spend-at-decision-time**. Unlike that RFC's other two deferred items — a real transport, and `api/otel/`'s consumption path — these three are deferred for exactly one reason (they need real code restructuring, not just "add one emission call") and that reason no longer applies: this RFC does the restructuring. This is a purely additive proto change (fields 7–11 on an already-shipped, tagged message) — no breaking change, no `UPGRADE.md` entry.

## Motivation

The original contract's own "Alternatives Considered" (§2) named this precisely: *"none of those are honestly obtainable from `finalize` without real code restructuring beyond 'add one emission call,' and inventing the fields ahead of a real producer is the exact trap this RFC's own research flagged."* That trap doesn't apply here — this pass builds the producer first, then the fields. Each field closes a real, named observability gap:

- **Rate-limit fail-open**: `checkRateLimit`'s fail-open branch (`docs/rfcs/2026-09-03-distributed-rate-limiting.md`'s own "second, independent control" argument for why fail-open is safe) is currently invisible outside a single log line. Making it a structured, queryable field is what makes that argument auditable in production, not just asserted in a doc.
- **Fallback-routing detail**: today, `finalize` only ever sees the *last* deployment tried — a fallback silently discards the original deployment and the error that triggered it. Recording this is the only way to see "how often is deployment X's failure actually being masked by a successful fallback to Y."
- **Budget-spend-at-decision-time**: today, a `BUDGET_EXCEEDED` rejection has no attached number — you can't tell "how far over" without cross-referencing separate logs. This field makes the exact spend at the moment of rejection (or acceptance) part of the same structured event.

## Detailed Design

### Grounding

Grounded via a dynamic-workflow research pass (3 parallel angles — proto evolution/field-shape conventions, Go patterns for threading decision-context without breaking existing signatures, and a full read of the real current `dataplane.go`/`streaming.go`/`budget.go` code — plus a synthesis). Every line number and code shape the research cited was independently re-verified directly against the live tree before being relied on here (see Research Trail).

### Proto: five new fields, additive only

```protobuf
// api/gatewayevents/v1/gatewayevents.proto — appended after `Outcome outcome = 6;`

// True iff the rate limiter's backend errored for this request and the
// request was allowed through anyway (fail-open), per
// docs/rfcs/2026-09-03-distributed-rate-limiting.md's "Fail-open, not
// fail-closed" section. false covers BOTH "rate limiter ran and did not
// fail open" and "rate limiter never ran for this request" (an Outcome of
// OUTCOME_AUTH_FAILED/OUTCOME_MODEL_NOT_ALLOWED means the request never
// reached the rate-limit check at all) — those two are intentionally not
// distinguished; cross-reference Outcome for that.
bool rate_limit_fail_open = 7;

// fallback_happened, fallback_from_deployment, and fallback_reason
// together describe whether this request fell back to a second
// deployment. Kelvran's fallback logic attempts at most ONE fallback per
// request, never a chain (gateway/ARCHITECTURE.md's router step) — a
// fixed 3-field record, not a repeated/list shape.
bool fallback_happened = 8;
// Name of the deployment first tried and abandoned. "" when
// fallback_happened is false. The deployment ULTIMATELY used is not
// repeated here — it's already derivable from the OTel span's
// DeploymentName/Provider attributes, joinable via trace_id/span_id, per
// this message's own "never re-encodes anything already real on that
// span" rule.
string fallback_from_deployment = 9;
// The first attempt's error (err.Error()). "" when fallback_happened is
// false. Free text, not an enum — unlike Outcome, these are arbitrary
// upstream/provider failures with no existing sentinel-error taxonomy.
string fallback_reason = 10;

// Cumulative USD spend for virtual_key_id at the moment the budget check
// decided whether to allow this request — i.e. BEFORE this request's own
// cost (if any) is added. Decimal-as-string, matching
// docs/rfcs/2026-09-02-decimal-cost-accounting.md's convention. "0" is a
// real, meaningful value (a key that has genuinely spent nothing yet),
// not a "field absent" sentinel — only meaningful when the budget check
// actually ran; cross-reference Outcome (OUTCOME_AUTH_FAILED/
// OUTCOME_MODEL_NOT_ALLOWED/OUTCOME_RATE_LIMITED all precede the budget
// check and never populate this field).
string budget_spent_usd = 11;
```

**Why plain `bool`, not `optional bool`, for `rate_limit_fail_open`** — a deliberate departure from protobuf.dev's own general recommendation ("we recommend always adding the `optional` label... unless you have a specific reason not to"). The specific reason: `logRequest`'s existing `protojson.Marshal(event)` call (verified directly — no `MarshalOptions{EmitUnpopulated: true}` anywhere in this codebase) already omits an unset plain-`bool` field from the JSON output exactly like an absent field would be; `optional` only matters if something needs to distinguish "explicitly false" from "absent," and nothing here ever will — the original contract RFC's own framing of this field already treated "false/absent" as one interchangeable category. Not a stylistic call; a specific, checked reason.

**Why a flat `bool` + two `string`s for fallback, not a nested/repeated message** — real precedent is split (OpenTelemetry and LiteLLM both use flat scalars/counts; Kong's balancer log uses a real repeated per-attempt structure), but Kelvran's own code settles it: the fallback loop in both `HandleChatCompletion` and `streamDeploymentWithFallback` structurally attempts at most one fallback — there is no loop to unroll into a repeated field. A richer shape would be over-provisioned for a value that is structurally binary.

### Go: widen in place, no new struct where none is needed

**`checkRateLimit`** — widen its return in place, not via a new sibling function:

```go
func (p *Pipeline) checkRateLimit(ctx context.Context, vk *identity.VirtualKey) (ok bool, failedOpen bool) {
    allowed, err := p.limiter.Allow(ctx, vk.ID)
    if err != nil {
        p.logger.Warn("ratelimit_backend_unavailable", "key_id", vk.ID, "error", err.Error())
        return true, true
    }
    return allowed, false
}
```

This departs from the general Go-ecosystem guidance (favoring a decision-record struct returned via a new sibling function, to protect existing callers from a breaking signature change) for a checked, specific reason: `checkRateLimit` has exactly two call sites (`dataplane.go:438`, `streaming.go:73`), both private `Pipeline` methods this same change already edits — there is no external caller to protect. Kelvran already has an in-repo precedent for exactly this shape: `nextDeployment` (`dataplane.go:542`) returns `(Deployment, bool)`, consumed as `var found bool; dep, found = p.nextDeployment(...)`. Widening `checkRateLimit` to `(bool, bool)` and consuming it the identical way matches an existing convention in this same file — more consistent with Kelvran's own style than introducing a new struct type that has zero precedent anywhere in this codebase.

**Fallback detail** — one small, purpose-built struct, used identically on both the buffered and streaming paths (this *does* cross a function boundary in the streaming case, and 3 cohesive values are enough to justify bundling them):

```go
// fallbackInfo captures whether a request fell back to a second
// deployment, and if so which one and why — captured at the one point in
// the fallback block where the original dep/err are still available,
// before they're overwritten.
type fallbackInfo struct {
    happened bool
    from     string // Deployment.Name first tried and abandoned.
    reason   string // err.Error() from the first attempt.
}
```

`HandleChatCompletion`'s inline fallback block, capturing `fallbackInfo` before `dep`/`err` are overwritten, and only inside the branch where a fallback is actually attempted (not merely wherever `err != nil`):

```go
resp, err = p.callDeployment(ctx, dep, req)
if err != nil {
    if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
        fallback = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
        dep = fallbackDep
        resp, err = p.callDeployment(ctx, dep, req)
    }
}
```

`streamDeploymentWithFallback` widens its return list by exactly one value (the struct):

```go
func (p *Pipeline) streamDeploymentWithFallback(ctx context.Context, dep Deployment, req adapter.ChatRequest, sw *streaming.Writer) (adapter.ChatResponse, Deployment, fallbackInfo, error) {
    var firstChunkSent bool
    var fb fallbackInfo

    resp, err := p.streamDeployment(ctx, dep, req, sw, &firstChunkSent)
    if err != nil && !firstChunkSent {
        if fallbackDep, hasFallback := p.nextDeployment(req.Model); hasFallback && fallbackDep.Name != dep.Name {
            fb = fallbackInfo{happened: true, from: dep.Name, reason: err.Error()}
            dep = fallbackDep
            resp, err = p.streamDeployment(ctx, dep, req, sw, &firstChunkSent)
        }
    }
    return resp, dep, fb, err
}
```

When `firstChunkSent` is already true, the inner branch never runs, so `fb` stays its zero value even though `err != nil` — correctly reporting *no* fallback for the "already streamed, can't retry" case, distinct from "errored and no fallback was attempted."

**Var-block widening** (both `HandleChatCompletion` and `HandleChatCompletionStream`), the existing, already-established mechanism (this is the same mechanism `cacheHit`/`vk`/`dep` already use to reach the deferred `finalize` call on every return path, including early returns):

```go
var (
    cacheHit              bool
    vk                    *identity.VirtualKey
    dep                   Deployment
    rateLimitFailedOpen   bool
    fallback              fallbackInfo
    budgetSpentAtDecision decimal.Decimal
)
```

**`finalize`** widens from 8 to 11 parameters — plain widening, not a wrapper struct. `finalize` has exactly two call sites, both edited by this same change; the same "no external caller to protect" reasoning as `checkRateLimit` applies. (If `finalize`'s parameter list keeps growing past this pass, bundling it into one struct is a reasonable *future* refactor — explicitly out of scope now, per YAGNI: nothing today requires it.)

### Budget-spend-at-decision-time: exact semantics

Captured at the `budget.Allow` call site, **immediately before** calling `Allow` — before this request's own cost is later computed/recorded in `finalize`, and **unconditionally**, including on the `BUDGET_EXCEEDED` rejection path (that's precisely the case where "how close to the cap was this key" is most valuable):

```go
budgetSpentAtDecision = p.budget.SpentUSD(vk.ID)
if !p.budget.Allow(vk.ID, vk.BudgetUSD) {
    err = ErrBudgetExceeded
    return
}
```

A naive implementation might instead call the new getter *inside* `finalize` (it already has `p.budget` and `vk` in scope there, "with zero new arguments") — but that captures spend *at finalize time* (after cache lookup, deployment selection, and a full upstream call), not spend *at decision time*. Under concurrent requests for the same key those are different numbers. The capture must happen at the `Allow` call site, threaded through the var block exactly like `dep`/`cacheHit` already are — not convenience-plumbed into `finalize`.

New `budget.Tracker` method, following `Allow`'s own locking idiom exactly:

```go
// SpentUSD returns keyID's cumulative recorded spend so far. Never touches
// the store — like Allow, a pure in-memory read. A never-recorded key
// correctly returns the zero-value decimal.Decimal{} (renders as "0").
func (t *Tracker) SpentUSD(keyID string) decimal.Decimal {
    t.mu.Lock()
    defer t.mu.Unlock()
    return t.spent[keyID]
}
```

Accepted, deliberately-not-fixed limitation: `SpentUSD` and `Allow` are two separate lock acquisitions, not one atomic read — under concurrent requests for the *same* key, another goroutine's `Record` could land in the gap between them. This is fine: it's an observability field, not part of the enforcement decision itself (`Allow`'s own correctness is untouched), and adding a combined atomic accessor purely to close a race that only affects an audit-log number is unneeded complexity for what it buys.

### Non-breaking, confirmed against `buf`'s real rule taxonomy

`api/buf.yaml` is configured `breaking: use: [FILE]` — the strictest category buf defines. Every rule in every category (`FILE`, `PACKAGE`, `WIRE`, `WIRE_JSON`) targets either deletion of an existing element or enforced sameness of an already-existing field (`FIELD_NO_DELETE`, `FIELD_SAME_TYPE`, `FIELD_SAME_NAME`, etc.) — none targets *addition*. Appending five new fields at five new, previously-unused field numbers (7–11) does not delete or renumber fields 1–6 or touch the `Outcome` enum, so no rule in any category fires. No `UPGRADE.md` entry needed. The one real (non-`buf breaking`) caveat: old code emitting the old 6-field message, read by new code, simply sees zero-value defaults for the 5 new fields — correctly interpreted as "not populated," never as false data, given the doc comments above.

## Drawbacks

- `fallback_reason` is free text (an arbitrary upstream error string), not an enum — a real, accepted inconsistency with `Outcome`'s structured-enum precedent, justified because upstream/provider failures have no existing sentinel-error taxonomy to classify into (inventing one now would be scope creep beyond what this RFC needs).
- `SpentUSD`+`Allow`'s two-lock-acquisition gap (above) means the recorded `budget_spent_usd` can, in a narrow concurrent-request-to-the-same-key race, be stale by one in-flight `Record` relative to what `Allow` itself compared against. Accepted as an observability-only imprecision, not an enforcement bug.
- `finalize`'s parameter list grows to 11 — a real, if minor, readability cost, deferred rather than addressed now (see "future refactor" note above).

## Alternatives Considered

1. **`optional bool rate_limit_fail_open`** — rejected for the specific, checked reason in Detailed Design (protojson's existing zero-`EmitUnpopulated` behavior already makes false/absent indistinguishable in the one place this event is serialized; nothing needs to tell them apart).
2. **A repeated `FallbackAttempt` message (deployment ID, error, latency) per attempt, Kong-balancer-style** — rejected: over-provisioned for a value that is structurally at-most-one-attempt in this codebase today; would also require restructuring the fallback loop into a real loop, which it currently isn't.
3. **A decision-record struct returned via a new sibling function for `checkRateLimit`** (the general Go-ecosystem recommendation for API compatibility) — rejected: no external caller exists to protect; widening the existing two call sites in place matches an already-established in-repo convention (`nextDeployment`) more closely than introducing a new pattern would.
4. **Threading fail-open/fallback state via `context.WithValue`** — rejected: the later telemetry-building step in the same method has a genuine data dependency on this state (not decorative/best-effort), which real, current Go guidance treats as disqualifying for context values; it also imposes concurrency-safety obligations (per `context.Value`'s own documented contract) that buy nothing here, since this never crosses a goroutine boundary.
5. **Calling the new `budget.SpentUSD` getter inside `finalize`** instead of at the `Allow` call site — rejected: captures the wrong moment (spend at finalize-time, not decision-time), silently wrong under concurrent load. Named explicitly as the most likely naive-implementation mistake.

## Unresolved Questions

- `finalize`'s parameter list will need a real refactor (likely a struct) if it grows again — not addressed now, per YAGNI.
- Whether `fallback_reason`'s free-text shape should eventually gain a coarse classification (network timeout vs. 4xx vs. 5xx, etc.) once real fallback volume exists to justify the added taxonomy — deferred, same as the original contract RFC's own posture on inventing categories ahead of real data.
- Producer-side latency cost of the two new decimal-to-string conversions and the additional `SpentUSD` lock acquisition per request — unmeasured, expected to be small (comparable to work `logRequest` already does), not benchmarked.

## Research Trail

Grounded via a dynamic-workflow research pass (3 parallel angles: proto evolution/field-shape conventions — cited directly against buf's and protobuf.dev's own docs, cross-checked against OpenTelemetry, LiteLLM, and Kong's real production schemas; Go patterns for threading decision-context without breaking signatures — cited against the Go team's own compatibility guidance, a real Kubernetes admission-webhook precedent, and current (2026) guidance on `context.Value` anti-patterns; and a full, line-numbered read of Kelvran's own current `dataplane.go`/`streaming.go`/`budget.go` — plus a synthesis). Every line number, function signature, and code shape the synthesis relied on was independently re-verified directly against the live tree before being written into this RFC, not merely trusted from the research pass's report. The synthesis explicitly departed from each individual research angle's own general-case recommendation in three places (plain `bool` over `optional bool`; flat fallback fields over a richer repeated shape; in-place signature widening over a new sibling function) — each departure grounded in a fact specific to Kelvran's own real code (its existing `protojson.Marshal` call, its at-most-one-fallback control flow, and its existing `nextDeployment`-style convention, respectively), not a preference asserted independently of it.
