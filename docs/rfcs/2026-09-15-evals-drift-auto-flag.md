# RFC: Drift-sample auto-flag rules feeding `evals promote`

## Status

Accepted, implementing 2026-09-15.

## Summary

Adds `evals/evals/auto_flag.py` (a new, flat, third-layer module) and a new `evals flag-candidates --suite PATH --results PATH [--out PATH]` command: a rule-based pass over already-ingested `drift_sample`-tier `EvalCase`s that surfaces promotion candidates and prints the exact `evals promote` command to run for each — never a new, automated promotion path. v1 ships exactly one rule, matching on `EvalCase.task_spec["outcome"]` for four gateway-failure outcomes.

## Motivation

`docs/upgrade-research/evals-next-upgrade-round3-2026-09-11.md`'s Findings 3/4 modeled this on Arize AX's real, shipped "auto-add rules" pattern — with an explicit caveat carried into this design: any such mechanism must retain a durable, human-labeled anchor corpus, not just a one-time review gate, per model-collapse literature's finding that even 10% real-data retention only reduces, never eliminates, self-referential-data degradation. This RFC does not build that anchor-corpus mechanism; it only ensures the funnel it feeds (`evals promote`) stays human-triggered, so that constraint remains structurally enforceable downstream.

## Detailed Design

### Why `task_spec["outcome"]`, never `Run.status`

`evals.ingestion.mapping.gateway_decision_event_to_eval_case_and_run` maps `GatewayDecisionEvent.Outcome`'s 9 non-OK enum values into a single `Run.status = "error"` string — real information is lost at ingestion time. Only `EvalCase.task_spec["outcome"]` (the enum name itself, e.g. `"OUTCOME_UPSTREAM_ERROR"`) survives with enough resolution for a rule to distinguish *which* failure occurred. `auto_flag.py` exists specifically to read that field instead of the lossier one.

### v1 scope: outcome-based rules only — `mapping.py` is untouched

`GatewayDecisionEvent` fields decoded during ingestion but discarded before persistence (`occurred_at`, `rate_limit_fail_open`, `fallback_happened`, `fallback_from_deployment`, `budget_spent_usd`) are simply not available on already-ingested data today; no latency or per-request-cost field exists on the proto at all. Extending `mapping.py` to additively persist them is named as a real, low-risk, explicitly-deferred follow-up (a few new `task_spec` keys, additive, no schema break) — deliberately not bundled into this phase, since it would only help newly-ingested data going forward (no retroactive benefit) and conflates two independently-revertible changes for no v1 benefit.

### `evals/evals/auto_flag.py` — layer placement

New sibling on the existing layer-3 `|`-joined bullet, alongside `evals.audit_corpus`. Imports only `evals.models` — never `evals.results_store` or `evals.audit_corpus` — mirroring `audit_corpus.py`'s own documented reasoning for sibling-independence, which `.importlinter`'s `layers` contract enforces mechanically, not just by convention. `FlagRule{name, flagged_outcomes: frozenset[str]}.matches(case)` checks `case.task_spec.get("outcome") in flagged_outcomes`. `DEFAULT_RULES` ships one rule (`gateway-failure-outcome`) flagging `OUTCOME_UPSTREAM_ERROR`/`OUTCOME_RATE_LIMITED`/`OUTCOME_GUARDRAIL_BLOCKED`/`OUTCOME_DEPLOYMENT_CAPACITY`. `flag_candidates(cases, runs, rules=DEFAULT_RULES)` re-checks `tier == "drift_sample"` internally even though the CLI caller pre-filters — never trusts the caller alone. A case matched by two rules produces one `FlagCandidate` naming both rules, never two separate candidates.

### `evals flag-candidates` — report-only, by construction and by test

Loads `--suite`/`--results` the same way `promote_cmd` does; filters to `drift_sample`; prints each candidate plus the literal, copy-pasteable `evals promote --suite ... --results ... --run-id <id> --tier drift_sample --output <path>` command to run next. Never calls any suite-mutation helper, never constructs an `EvalCase`. Never exits non-zero based on findings — even a fixture where every case matches still exits 0, mirroring `audit_corpus_cmd`'s identical report-only posture; only a real tool/IO error, or zero `drift_sample` cases present in `--suite` at all, is a hard `click.ClickException` (mirroring the existing `f"{path}: no <Thing> found"` convention already used by `report_cmd`/`trend_show_cmd`).

**The "never a new promotion path" guarantee is machine-checked, not merely a docstring claim**: `test_flag_candidates_never_mutates_the_suite_file` reads the `--suite` file's bytes before and after invoking the command and asserts equality.

## Drawbacks

- One rule, four outcomes — a narrow v1. Real future work (once the named `mapping.py` follow-up lands) could add latency/cost/fallback-aware rules; not built here.
- No anchor-corpus retention mechanism is enforced by this command itself — it relies entirely on `evals promote` staying human-triggered downstream, which this RFC verifies structurally (the byte-identical-suite-file test) but does not itself guarantee against some future change to `promote_cmd`.

## Alternatives Considered

**Adding a batch/rule option directly to `promote_cmd`.** Rejected — `promote_cmd` takes exactly one `--run-id` per invocation with zero rule logic of its own; blurring an automated-candidate funnel into the promotion command itself contradicts the hard "never a new promotion path" constraint the research explicitly named.

**Extending `mapping.py` in this same phase** to also persist `fallback_happened`/`budget_spent_usd`/etc. Rejected for v1 — see the scope-decision section above; named as a deliberate, low-risk, separate follow-up.

## Unresolved Questions

- When (if ever) should the `mapping.py` follow-up land, adding fallback/budget-aware rules on top of this module's existing `FlagRule` shape? Not decided here.

## Verification

`cd evals && uv run pytest tests/ && ruff check . && uvx --from import-linter lint-imports` — 397 passed, 11 skipped (pre-existing, unrelated), ruff clean, all 3 import-linter contracts kept.

New tests: `test_auto_flag.py` (matching outcome flags; `OUTCOME_OK` doesn't; a non-`drift_sample` case is never flagged even with a matching outcome; a case with no corresponding `Run` is skipped, not a crash; two simultaneously-firing rules produce one candidate naming both). New `test_flag_candidates_integration.py` (expected candidate lines + summary count; `--out` JSON shape; zero `drift_sample` cases → `ClickException`; every-case-matches still exits 0; the suite file is byte-identical before and after running the command).

Sanity-checked-by-breaking, twice: (1) temporarily added `import evals.results_store` inside `auto_flag.py` — `lint-imports` reported a real layer-contract violation (`evals.auto_flag is not allowed to import evals.results_store`); reverted, confirmed clean again. (2) temporarily made `FlagRule.matches` always return `True` — the `OUTCOME_OK`-produces-no-candidate test failed with a real assertion (1 candidate instead of 0); restored, confirmed passing again.
