"""Consumes api/gatewayevents (and, eventually, api/otel) — nothing else.

`decode.py` is the original, deliberately minimal first pass from
docs/rfcs/2026-09-03-api-gatewayevents-contract.md: decode-only, no
sampling, no transport, no batching — proving gateway's and evals'
generated bindings agree on the wire format.

`object_store.py` is the real transport leg that RFC's own "Drawbacks"
section named as a stopgap, now resolved for the object-storage case per
docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md: it lists/
reads gatewayevents_v1 objects from S3 (shipped there by
docs/operations/vector-gatewayevents-s3.yaml), and calls `decode.py`
directly for the actual wire-format decoding — never duplicating that
logic. Wired into the CLI as `evals ingest --source s3://...` (see
evals/evals/cli.py).
"""
