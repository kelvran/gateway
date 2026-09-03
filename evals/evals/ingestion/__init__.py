"""Consumes api/gatewayevents (and, eventually, api/otel) — nothing else.

Per docs/rfcs/2026-09-03-api-gatewayevents-contract.md, this is a
deliberately minimal first pass: decode-only, no sampling, no transport,
no batching. Live production-trace ingestion from a running gateway is
explicitly deferred to a future pass, once the transport question
(queue vs. periodic file export vs. push endpoint) is actually decided —
see that RFC's Unresolved Questions.
"""
