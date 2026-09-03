"""Golden-fixture round-trip test for evals.ingestion.decode.

Per docs/testing/TESTING.md §5's "golden-fixture round-trip test, where
evals decodes and gateway encodes the same checked-in set of
api/gatewayevents ... messages, confirming both languages' generated
bindings agree on the wire format" — this is that test, made real for
gatewayevents. tests/fixtures/gateway_decision_event.json was produced by
gateway's own real generated Go bindings
(google.golang.org/protobuf/encoding/protojson), not hand-authored here,
so this test genuinely proves cross-language agreement rather than
Python decoding a Python-authored guess at the wire format.
"""

from __future__ import annotations

from pathlib import Path

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent
from evals.ingestion.decode import decode_gateway_decision_event

_FIXTURE_PATH = Path("tests/fixtures/gateway_decision_event.json")


def test_decodes_gateway_encoded_fixture_exactly() -> None:
    raw = _FIXTURE_PATH.read_text()

    event = decode_gateway_decision_event(raw)

    assert event.trace_id == "4bf92f3577b34da6a3ce929d0e0e4736"
    assert event.span_id == "00f067aa0ba902b7"
    assert event.virtual_key_id == "team-fixture"
    assert event.requested_model == "gpt-4o"
    assert event.outcome == GatewayDecisionEvent.OUTCOME_BUDGET_EXCEEDED
    assert event.occurred_at.ToJsonString() == "2026-09-03T12:00:00Z"


def test_decode_rejects_malformed_json() -> None:
    import google.protobuf.json_format

    try:
        decode_gateway_decision_event("not valid json")
    except google.protobuf.json_format.ParseError:
        return
    raise AssertionError(
        "decode_gateway_decision_event accepted malformed input without raising"
    )
