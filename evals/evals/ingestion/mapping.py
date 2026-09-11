"""Maps one decoded `GatewayDecisionEvent` into the `EvalCase`+`Run` pair
`evals promote` already reads from every other source (`evals rollout`'s
own results-store output), per docs/rfcs/2026-09-07-evals-trace-ingestion-
object-storage.md's own "Unresolved Questions" (now resolved, per
DECISIONS.md): live-sampled production data feeds `evals promote` exactly
like any other run source — no separate review path, no separate
persistence shape.

The mapping is deliberately honest about what a `GatewayDecisionEvent`
does NOT carry. Per that RFC's own Context section: "No prompt/completion
content anywhere in this schema" — there is no real completion text for
`Run.stdout`, and no ground truth for `EvalCase.reference`; both are left
empty/`None` rather than fabricated. There is also no real per-request
wall-clock duration on the wire, so `Run.latency_ms` is `0.0` — not a
guess, the same "no genuine per-run timing exists" sentinel
`evals.rollout.scheduler.run_suite` already uses for a `status="skipped"`
or cache-hit `Run`, reused here rather than invented fresh.

This is exactly why the synthetic `EvalCase.tier` is always
`"drift_sample"`: per docs/rfcs/2026-09-05-evals-golden-regression-
promotion.md's own "Hard-requiring --scores always" rejection reasoning,
`drift_sample` is the one tier whose promotion doesn't need a pass/fail
judge verdict — the one honest fit for output that was never captured.
Nothing here stops a later `evals promote --tier regression` against one
of these `Run`s (the "no separate review path" policy means ingested data
is never special-cased), but that command's own `--scores` failing-verdict
precondition can never be honestly satisfied for it, since no `Score` was
or could be produced for content that was never captured — a real
constraint of the data, not a gap in this mapping.
"""

from __future__ import annotations

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent
from evals.models import EvalCase, Run


def gateway_decision_event_to_eval_case_and_run(
    event: GatewayDecisionEvent,
) -> tuple[EvalCase, Run]:
    """Build the `(EvalCase, Run)` pair for one decoded event.

    `id`/`eval_case_id` are derived from `trace_id`+`span_id` — real,
    spec-compliant OTel identifiers unique to the request this event
    describes (per the proto's own field comments), never a random UUID
    that would sever the join back to the source trace. Revision is
    always `1`: each ingested event is a fresh, standalone case, never a
    revision of anything already registered. Every ingest of the same
    underlying object produces the same `(EvalCase.id, Run.id)` again —
    deliberately not deduplicated here (`results_store.py`'s append-only
    files carry no dedup for any other source either); a caller re-
    ingesting overlapping S3/GCS prefixes gets a real duplicate line, the
    same tradeoff already accepted everywhere else in this module.
    """
    base_id = f"gatewayevents-{event.trace_id}-{event.span_id}"
    outcome_name = GatewayDecisionEvent.Outcome.Name(event.outcome)
    completed = event.outcome == GatewayDecisionEvent.Outcome.OUTCOME_OK

    case = EvalCase(
        id=base_id,
        revision=1,
        task_spec={
            "source": "gatewayevents_v1",
            "trace_id": event.trace_id,
            "span_id": event.span_id,
            "virtual_key_id": event.virtual_key_id,
            "requested_model": event.requested_model,
            "outcome": outcome_name,
            # occurred_at/rate_limit_fail_open/fallback_happened/
            # fallback_from_deployment/budget_spent_usd were previously
            # decoded from the wire and then silently discarded here --
            # `evals.auto_flag`'s own module docstring names this exact
            # gap as the reason a future rule can't use latency/cost/
            # fallback signals against already-ingested data.
            # occurred_at via ToJsonString(), not the raw Timestamp
            # message, since task_spec is a plain dict that must stay
            # JSON-serializable end to end (results_store.py's append-
            # only JSONL persistence).
            "occurred_at": event.occurred_at.ToJsonString(),
            "rate_limit_fail_open": event.rate_limit_fail_open,
            "fallback_happened": event.fallback_happened,
            "fallback_from_deployment": event.fallback_from_deployment,
            # budget_spent_usd is already a decimal-as-string on the wire
            # (this project's own established convention for exact
            # decimal values crossing a serialization boundary) -- passed
            # through verbatim, never parsed into a float here.
            "budget_spent_usd": event.budget_spent_usd,
        },
        reference=None,
        tier="drift_sample",
        tags=[f"gatewayevents-outcome:{outcome_name}"],
    )

    run = Run(
        id=base_id,
        eval_case_id=case.id,
        eval_case_revision=case.revision,
        harness_config={
            "source": "gatewayevents_v1",
            "trace_id": event.trace_id,
            "span_id": event.span_id,
        },
        status="completed" if completed else "error",
        latency_ms=0.0,
        stdout="",
        stderr=event.fallback_reason,
        error=None if completed else f"gateway outcome: {outcome_name}",
    )
    return case, run
