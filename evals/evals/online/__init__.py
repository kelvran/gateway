"""Online eval: which live `GatewayDecisionEvent`s from a continuous
production stream get selected for further review, out of what would
otherwise be every event — the real, industry-standard "online eval"
capability (Arize/LangSmith/Braintrust all ship this exact shape: a
configurable sampling rate for cost control, plus rule-based filters on
which traces get scored, applied to a SINGLE stream — never a shadow-
traffic/canary comparison, a genuinely separate capability this package
does not attempt).

`sampler.py` is deliberately narrower than a full LLM-judge "sample and
score" pipeline: `GatewayDecisionEvent` (see
`evals.ingestion.mapping`'s own doc comment) carries no prompt/
completion content at all, by design, per
docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md's own
Context section — `evals.judge.llm_judge.judge()`'s real signature
needs an `output`/`reference` string pair there is nothing here to
supply. Scoring a sampled event remains real, disclosed future work,
gated on either a genuinely different, content-carrying event source or
a narrower, deterministic scorer against the outcome-shaped fields this
event DOES carry — a distinct design decision, not made here. This
package ships the two pieces that ARE honestly buildable against real
data: `should_sample` (a uniform-random sampling-rate decision) and
`RuleFilter`/`passes_rule_filters` (mirrors `evals.auto_flag.FlagRule`'s
exact name+predicate shape, applied one stage earlier — against the raw
event, before any `EvalCase`/`Run` conversion exists at all).

Wired into `evals ingest` via its own optional `--sample-rate`/
`--filter-rules` flags, additive to that command's existing behavior,
never a replacement for it — omitting both reproduces `evals ingest`'s
original "ingest everything" behavior exactly.
"""
