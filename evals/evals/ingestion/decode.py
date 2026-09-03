"""Decodes a single api/gatewayevents/v1.GatewayDecisionEvent.

Per docs/rfcs/2026-09-03-api-gatewayevents-contract.md, this module's
entire job is proving `gateway`'s real generated Go bindings and `evals`'
real generated Python bindings agree on the wire format — no sampling,
no transport, no batching logic of any kind.
"""

from __future__ import annotations

from google.protobuf.json_format import Parse

from evals.contracts.gatewayevents.v1.gatewayevents_pb2 import GatewayDecisionEvent


def decode_gateway_decision_event(raw: str) -> GatewayDecisionEvent:
    """Decode raw (protojson-encoded, as gateway's logger emits it) into a
    real GatewayDecisionEvent message."""
    return Parse(raw, GatewayDecisionEvent())
