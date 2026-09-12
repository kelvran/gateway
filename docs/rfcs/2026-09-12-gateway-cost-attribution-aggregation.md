- **Status**: accepted
- **Date**: 2026-09-12
- **Author(s)**: gateway team (via Round 6 requirements-traceability audit)

## Summary

Add two additive fields — `agent_run_id` and `cost_usd` — to `GatewayDecisionEvent` (the one contract built specifically for offline/cross-request analysis), and a new `evals cost-report` CLI command that sums cost by `agent_run_id` over ingested events. This closes PRD.md's own founding gap ("why did this agent run cost $4" has no answer beyond a raw total) with the minimum viable explanation capability, not a full dashboard.

## Motivation

PRD.md's Problem Statement names this as the first of three founding gaps Kelvran exists to close: "Every gateway surveyed... None of them understand a multi-step agent run — so 'why did this agent session cost $4' has no answer beyond a raw total." PRD.md's Success Metrics section restates it: "Cost savings attributable and explainable down to the individual agent run, not just an aggregate dashboard total."

A Round 6 requirements-traceability audit found this gap is still genuinely open, structurally: `agent_run_id` propagates correctly via OTel Baggage into the `kelvran.agent_run_id` span attribute, alongside `kelvran.cost.usd` — the two values genuinely co-occur on a per-request span. But that ephemeral span is the *only* place they co-occur:

- `GatewayDecisionEvent` (`api/gatewayevents/v1/gatewayevents.proto`) — the one contract built specifically so gateway telemetry can be shipped off-box and analyzed later (Vector → S3/GCS, decoded by `evals ingest`) — has neither an `agent_run_id` field nor a per-request cost field (only a cumulative pre-request `budget_spent_usd`).
- No OTel Metric is dimensioned by `agent_run_id` (correctly, for cardinality reasons — the effect is the same: no metric can answer the question either).
- `evals`' own ingestion pipeline — the only code anywhere that consumes `GatewayDecisionEvent` retrospectively — maps it into an `EvalCase`/`Run` pair for drift-sample eval registration; it has nothing to do with cost attribution.
- No dashboard, CLI, or admin-API route anywhere sums cost by `agent_run_id`. `docs/operations/TELEMETRY.md`'s own Dashboards section names the exact missing panel as its own litmus test ("if the dashboard can't answer that question, the feature isn't actually delivered yet") and discloses "no backend has been chosen yet."

An independent prior research pass (`docs/upgrade-research/gateway-cost-finops-round4-2026-09-11.md` Finding 2) already reached this same `build_now` verdict on 2026-09-11 and it was never actioned. `THREAT_MODEL.md`'s Gateway Repudiation row was corrected the same day as this RFC to stop overclaiming this as already solved.

## Detailed Design

**Affected components:** `api/` (additive proto fields, non-breaking — confirmed via `buf lint`/`buf breaking`, no version bump needed since only new field numbers are added), `gateway/internal/gateway/dataplane` (populate the two new fields at the existing construction site), `evals/` (a new CLI command reusing existing ingestion machinery).

### `api/gatewayevents/v1/gatewayevents.proto`

```protobuf
string agent_run_id = 12;
string cost_usd = 13;
```

Both mirror `budget_spent_usd`'s own established convention: Decimal-as-string for `cost_usd` (matching `docs/rfcs/2026-09-02-decimal-cost-accounting.md`), `""`/`"0"` are real, meaningful values (no baggage value propagated; a genuinely free cache hit), never a "field absent" sentinel.

### `gateway/internal/gateway/dataplane/dataplane.go`

At the existing `GatewayDecisionEvent{...}` construction site (`finalize`, immediately after `telemetry.ChatCompletionResult` is built a few lines above it), populate both new fields from values already in scope on that same `result` struct:

```go
event := &gatewayeventsv1.GatewayDecisionEvent{
    // ... existing fields unchanged ...
    AgentRunId: result.AgentRunID,
    CostUsd:    result.CostUSD,
}
```

No new capture point — `result.AgentRunID`/`result.CostUSD` are the exact same values already flowing into the OTel span (`telemetry.RecordChatCompletionResult`) a few lines above. Populated unconditionally, matching `result.CostUSD`'s own unconditional population (a cache hit's real $0 cost is itself a meaningful fact for later aggregation, not something to suppress).

### `evals` — `cost-report` command

A new `evals cost-report --agent-run-id <id>` command, reusing the existing S3/GCS ingestion machinery (`evals/evals/ingestion/object_store.py`) rather than building new fetch logic — lists/reads/decodes `GatewayDecisionEvent`s from the configured source (mirroring `evals ingest`'s own `--source s3://...` pattern), filters to the requested `agent_run_id`, and sums `cost_usd` (parsed as `Decimal`, matching this codebase's own cost-accounting convention). This is the minimum viable "explanation capability" PRD.md's own wording asks for ("attributable and explainable... not just an aggregate dashboard total") — not a dashboard, which this project has no need to build yet without real production traffic to size the actual UI against.

## Drawbacks

- Two more fields on a contract this project's own versioning rule freezes the moment it ships incompatibly — but these are additive, non-breaking (confirmed via `buf breaking`), so `v1` stays frozen as-is; no `v2` needed.
- `cost_usd` on `GatewayDecisionEvent` duplicates a value that (for a real, billable request) is already derivable from the OTel span — a small, deliberate redundancy, justified because the span is ephemeral (nothing durable retains it once the process's exporter discards it) while `GatewayDecisionEvent` is the one contract built for durable, offline analysis. This mirrors `budget_spent_usd`'s own existing precedent of carrying a value also present on the span.
- `evals cost-report` is a minimum-viable CLI, not a dashboard — an operator still has to run a command, not glance at a chart. Named explicitly as the intentionally minimal scope for this pass; a real dashboard remains future work, gated on real production traffic existing to size it against (the same trigger `docs/operations/TELEMETRY.md` already names for every other dashboard gap in this project).

## Alternatives Considered

- **Build a full Grafana dashboard panel now**, mirroring `grafana-cache-dashboard.json`'s precedent. Rejected for this pass: no real production traffic exists yet to validate what such a panel should even look like, and PRD.md's own wording explicitly asks for "attributable and explainable," not necessarily a dashboard — the CLI command satisfies the literal requirement at far lower risk/cost.
- **Add `agent_run_id` as an OTel Metric dimension.** Rejected: real cardinality risk (an unbounded number of distinct agent run IDs, unlike the existing bounded dimensions — layer, outcome, instance ID) that every other metric in this codebase deliberately avoids for the same reason.
- **Do nothing, treat as `not_yet`.** Rejected: an independent prior research pass already named this `build_now` with "no trigger needed beyond scheduling the work" on 2026-09-11, and it sat unactioned for a full day before this audit caught it — exactly the kind of confirmed, low-risk, unblocked gap this project's own backlog-audit discipline exists to close promptly.

## Unresolved Questions

- Should `evals cost-report` support grouping by something other than a single `--agent-run-id` (e.g. `--group-by agent_run_id` producing a full breakdown across every distinct ID seen)? Deferred — the single-ID lookup is the minimum viable version of "why did THIS agent run cost $X," and a full breakdown command can be added later without touching the contract again.
- Whether a real dashboard panel is ever built depends on real production traffic existing to size it against — genuinely undecided here, matching every other dashboard-shaped gap already deferred in this project for the identical reason.
