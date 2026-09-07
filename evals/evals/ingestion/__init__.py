"""Consumes api/gatewayevents (and, eventually, api/otel) — nothing else.

`decode.py` is the original, deliberately minimal first pass from
docs/rfcs/2026-09-03-api-gatewayevents-contract.md: decode-only, no
sampling, no transport, no batching — proving gateway's and evals'
generated bindings agree on the wire format.

`object_store.py` is the real transport leg that RFC's own "Drawbacks"
section named as a stopgap, now resolved for the object-storage case per
docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md (and that
RFC's own 2026-09-07 addendum): it lists/reads gatewayevents_v1 objects
from either S3 (shipped there by docs/operations/vector-gatewayevents-
s3.yaml) or GCS (docs/operations/vector-gatewayevents-gcs.yaml), and
calls `decode.py` directly for the actual wire-format decoding — never
duplicating that logic. Wired into the CLI as `evals ingest --source
<s3://...|gs://...>` (see evals/evals/cli.py).

`mapping.py` closes that same RFC's own "Unresolved Questions" entry
("What happens downstream of `evals ingest`'s output file — feeding
`evals promote` unchanged, or a distinct review path for live-sampled
data"), per the project owner's decision recorded in DECISIONS.md: no
separate review path. `gateway_decision_event_to_eval_case_and_run` maps
one decoded event into the same `EvalCase`+`Run` shapes `results_store.py`
and `evals promote` already read from every other source — real fields
where the event genuinely carries them, honest empty/`None`/
`"drift_sample"`-tier defaults where it doesn't (a `GatewayDecisionEvent`
carries no prompt/completion content, so there is nothing to fabricate).
Wired into `evals ingest` via its own optional `--suite`/`--results`
flags, additive to `--out`'s existing raw decoded-event file, never a
replacement for it.
"""
