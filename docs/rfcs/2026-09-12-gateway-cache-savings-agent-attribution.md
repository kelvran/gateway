- **Status**: accepted
- **Date**: 2026-09-12
- **Author(s)**: gateway team (via Round 7 requirements-traceability audit, savings follow-up)

## Summary

Add one additive field — `savings_usd` — to `GatewayDecisionEvent`, and extend the
existing `evals cost-report` command to also sum it per `agent_run_id`. This closes
the SAVINGS half of `PRD.md:41`'s success metric ("Cost savings attributable and
explainable down to the individual agent run, not just an aggregate dashboard
total"); the SPEND half was already closed by `docs/rfcs/2026-09-12-gateway-cost-
attribution-aggregation.md`'s `agent_run_id`/`cost_usd` fields.

## Motivation

`PRD.md`'s Success Metrics section states the requirement for both halves in one
sentence — cost *savings*, not just cost, must be attributable per agent run. The
Round 6 RFC (`2026-09-12-gateway-cost-attribution-aggregation.md`) closed this for
real SPEND: `GatewayDecisionEvent.agent_run_id`/`cost_usd` now co-occur on the one
contract built for durable, offline analysis, and `evals cost-report` sums `cost_usd`
by `agent_run_id`.

It did not close it for cache SAVINGS specifically. Today, cache savings exist only
as `kelvran.cache.savings_usd` (`gateway/internal/telemetry/telemetry.go:334-361`),
an aggregate OTel Counter dimensioned only by cache layer (`L1`/`L2`/`L3`) — deliberately
never by `agent_run_id`, for the same unbounded-cardinality reason the sibling RFC gave
for rejecting an `agent_run_id` metric dimension on cost. No per-agent-run savings
signal exists anywhere: not on `GatewayDecisionEvent`, not in any CLI, not in any
dashboard.

The data itself already exists at the right place and the right time: at
`dataplane.go`'s `finalize`, `finalize` already computes the exact notional-savings
figure (`cost.Float64()`, gated on `cacheInfo.Hit()`) and hands it to
`telemetry.RecordCacheSavings` — the identical construction site, a few lines later,
where `GatewayDecisionEvent.AgentRunId`/`CostUsd` are already populated from the
same-scoped `result` struct. This RFC does not add a new capture point; it names an
existing value on an existing contract, exactly like the Round 6 RFC did for
`cost_usd` itself.

(A related but distinct doc-vs-code gap surfaced during this investigation:
`cost_usd`'s own doc comment and the Round 6 RFC's text both asserted a cache hit's
`cost_usd` is `"0"` ("never billed"). Tracing `dataplane.go`'s `cost` computation
against the cache read/write path shows this was inaccurate — `resp.Usage` survives
the cache round-trip intact, so a cache hit's `cost` is the nonzero notional
would-have-cost, identical to the value this RFC now also exposes as `savings_usd`.
Fixed as a comment-only correction alongside this RFC, not a separate change.)

## Detailed Design

**Affected components:** `api/` (one additive proto field, non-breaking — confirmed
via `buf breaking`'s FILE-level rules, no `v1` version bump), `gateway/internal/
gateway/dataplane` (populate the new field at the existing construction site),
`evals/` (extend the existing `cost-report` command, no new command).

### `api/gatewayevents/v1/gatewayevents.proto`

```protobuf
  string agent_run_id = 12;
  string cost_usd = 13;
  string savings_usd = 14;
```

Mirrors `cost_usd`'s own established convention: Decimal-as-string, additive/non-
breaking per `buf breaking`. Diverges from `cost_usd`'s convention in exactly one
place, deliberately: `""` (not `"0"`) is the not-a-cache-hit sentinel, because "this
request was never a cache hit" and "this request had a cache hit worth exactly $0"
are genuinely different facts, and this field must not conflate them.

### `gateway/internal/gateway/dataplane/dataplane.go`

At the same `GatewayDecisionEvent{...}` construction site `finalize` already
populates `AgentRunId`/`CostUsd` from, add:

```go
var savingsUsd string
if cacheInfo.Hit() {
    savingsUsd = result.CostUSD
}

event := &gatewayeventsv1.GatewayDecisionEvent{
    // ... existing fields unchanged ...
    AgentRunId: result.AgentRunID,
    CostUsd:    result.CostUSD,
    SavingsUsd: savingsUsd,
}
```

No new computation — `result.CostUSD` is the exact same value `RecordCacheSavings`
already consumes (as a `float64`) a few lines above, gated by the identical
`cacheInfo.Hit()` check.

### `evals` — `cost-report` command

Extend the existing command's accumulation loop with a second `Decimal` running
total (`total_savings`), summing `event.savings_usd or "0"` alongside the existing
`event.cost_usd or "0"` sum, and print both in the same `click.echo` line. No new
flag, no new command — chosen over a separate `--show-savings` flag or a dedicated
`savings-report` command specifically to keep spend and savings reported together
for the same run, mirroring `PRD.md:40`'s own "must be reported together" precedent
for hit-rate + correctness.

### `evals/evals/ingestion/decode.py`

No change. `decode_gateway_decision_event` is a generic `Parse(raw,
GatewayDecisionEvent())` call — field-name-agnostic once `make gen-proto`
regenerates `gatewayevents_pb2.py`.

## Drawbacks

- A third field alongside `cost_usd` that, on a cache-hit row, will hold the exact
  same decimal string as `cost_usd` — a small, deliberate redundancy, justified for
  the same reason the Round 6 RFC justified `cost_usd` duplicating the OTel span:
  the span/counter are ephemeral/aggregate-only, `GatewayDecisionEvent` is the one
  contract built for durable, per-agent-run analysis.
- `savings_usd`'s `""`-vs-`"0"` convention deliberately diverges from `cost_usd`'s
  own `"0"`-is-real-not-absent convention on the same message — a caller reading
  both fields must know they follow different absent-value rules. Documented
  explicitly in the field's own comment to prevent silent misreading.

## Alternatives Considered

- **Redefine `cost_usd` to be `"0"` on a genuine cache hit, folding savings into it.**
  Rejected: changes an already-shipped field's semantics for existing consumers
  (`evals cost-report`'s own current sum), a behavioral break disguised as a comment
  fix, not a "genuinely additive" change per this project's own precedent for this
  kind of RFC.
- **Add a `cache_hit` bool field instead of a dedicated `savings_usd` value**, letting
  consumers derive savings from `cost_usd` + `cache_hit` themselves. Rejected: still
  requires every consumer to re-derive the same logic `RecordCacheSavings` already
  encodes once; a dedicated field is the minimum-viable "explainable" primitive
  `PRD.md:41` asks for, matching the Round 6 RFC's own reasoning for why `cost_usd`
  needed to exist as its own field rather than be re-derived from the span.
- **A dedicated `evals savings-report` command / `--show-savings` flag.** Rejected per
  `PRD.md:40`'s "reported together" precedent (see Detailed Design).
- **Do nothing, treat as `not_yet`.** Rejected: `PRD.md:41`'s success metric is
  explicitly unmet for savings today, the data already exists at the exact point
  needed, and the fix is a single additive field plus reusing already-existing
  aggregation code — the same low-risk, unblocked shape of gap the Round 6 RFC
  itself argued should not sit unactioned.

## Unresolved Questions

- Whether `evals ingest`'s `gateway_decision_event_to_eval_case_and_run` mapping
  should also surface `savings_usd` is out of scope here — that function has nothing
  to do with cost/savings attribution today (it maps into `EvalCase`/`Run` for drift-
  sample eval registration), matching the Round 6 RFC's own scoping decision to leave
  it untouched for `cost_usd`.
