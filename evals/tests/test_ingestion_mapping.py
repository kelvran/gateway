"""Unit tests for evals.ingestion.mapping.gateway_decision_event_to_eval_case_and_run.

No dedicated test existed for this function before this pass -- prior
coverage was only indirect, via test_ingest_promote_integration.py's
full-CLI round trip against a static JSONL fixture. These tests exercise
the mapping directly, with a real, protobuf-constructed
`GatewayDecisionEvent` (never a hand-built dict standing in for one).
"""

from __future__ import annotations

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent
from evals.ingestion.mapping import gateway_decision_event_to_eval_case_and_run


def test_persists_occurred_at_rate_limit_fallback_and_budget_fields() -> None:
    # Real, distinguishable, non-zero-value fields -- proves each one is
    # threaded through individually, not just "some dict got populated."
    event = GatewayDecisionEvent()
    event.trace_id = "4bf92f3577b34da6a3ce929d0e0e4736"
    event.span_id = "00f067aa0ba902b7"
    event.occurred_at.FromJsonString("2026-09-11T08:00:00Z")
    event.virtual_key_id = "team-fixture"
    event.requested_model = "gpt-4o"
    event.outcome = GatewayDecisionEvent.OUTCOME_OK
    event.rate_limit_fail_open = True
    event.fallback_happened = True
    event.fallback_from_deployment = "primary-deployment"
    event.fallback_reason = "upstream 503"
    event.budget_spent_usd = "0.0042"

    case, run = gateway_decision_event_to_eval_case_and_run(event)

    assert case.task_spec["occurred_at"] == "2026-09-11T08:00:00Z"
    assert case.task_spec["rate_limit_fail_open"] is True
    assert case.task_spec["fallback_happened"] is True
    assert case.task_spec["fallback_from_deployment"] == "primary-deployment"
    assert case.task_spec["budget_spent_usd"] == "0.0042"
    # Pre-existing fields must be unaffected by this addition.
    assert run.stderr == "upstream 503"
    assert run.status == "completed"


def test_persists_zero_value_defaults_when_event_leaves_them_unset() -> None:
    # An event that never sets any of the 5 fields (the common case for a
    # non-fallback, non-rate-limited, non-billed request) must still
    # persist their real protobuf zero values, never omit the keys or
    # substitute a fabricated placeholder.
    event = GatewayDecisionEvent()
    event.trace_id = "t"
    event.span_id = "s"
    event.virtual_key_id = "vk"
    event.requested_model = "gpt-4o"
    event.outcome = GatewayDecisionEvent.OUTCOME_OK

    case, _run = gateway_decision_event_to_eval_case_and_run(event)

    assert case.task_spec["occurred_at"] == "1970-01-01T00:00:00Z"
    assert case.task_spec["rate_limit_fail_open"] is False
    assert case.task_spec["fallback_happened"] is False
    assert case.task_spec["fallback_from_deployment"] == ""
    assert case.task_spec["budget_spent_usd"] == ""


def test_task_spec_stays_json_serializable() -> None:
    # The whole point of ToJsonString() over the raw Timestamp message:
    # task_spec must round-trip through json.dumps cleanly, matching
    # results_store.py's append-only JSONL persistence contract.
    import json

    event = GatewayDecisionEvent()
    event.trace_id = "t"
    event.span_id = "s"
    event.occurred_at.FromJsonString("2026-09-11T08:00:00Z")
    event.virtual_key_id = "vk"
    event.requested_model = "gpt-4o"
    event.outcome = GatewayDecisionEvent.OUTCOME_OK

    case, _run = gateway_decision_event_to_eval_case_and_run(event)

    json.dumps(case.task_spec)  # raises if any value isn't JSON-serializable
