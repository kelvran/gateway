# RFC: A real CI/CD gate for `evals report` (`--fail-under`)

## Status

Accepted, implemented 2026-09-05.

## Context

`evals/ARCHITECTURE.md` marks its `CI/CD Gate` box "diagram-only" — a fresh backlog audit re-confirmed this directly: `report_cmd` computes a pass rate (and, for `--traces`, an OK-rate) but never exits non-zero based on it. No `--fail-under`, no `sys.exit(1)` path, and neither `.github/workflows/ci.yml` nor `Makefile` invoke `evals run|rollout|report` at all today. An operator wiring `evals` into a real CI pipeline has no way to make a build fail on a regressed pass rate without writing their own wrapper script to parse `report`'s text output.

## Design

### Gate on the Wilson **lower bound**, not the point estimate

`evals/ARCHITECTURE.md`'s own Data Model section states a real design principle: "no CI/CD gating decision runs without a prior power calculation." A full power calculation (sample-size-vs-effect-size-vs-power) is a genuinely separate, larger statistical undertaking — the same "bootstrap/Bayesian eval stats" placeholder this project has twice now deliberately left parked, since it needs real usage data to design against, not invented ahead of need.

This RFC ships a real, narrower, but still statistically-grounded gate instead of either building the full power-calculation machinery or ignoring the principle entirely: `--fail-under` compares against the already-computed **Wilson lower bound**, not the bare point estimate. This means the gate only passes when the operator can be `--confidence`-certain the true rate clears the bar — a small sample's wide interval makes the lower bound meaningfully more conservative than the point estimate, which is a real (if informal) proxy for "don't gate confidently on too little evidence" without inventing a whole new statistical subsystem. Concretely: 8/10 successes has a point estimate of 0.8 but a real Wilson lower bound of 0.4902 (at 95% confidence) — `--fail-under 0.6` correctly fails this case even though the point estimate alone would pass it.

### Scope: `report_cmd` only, all three modes, every group independently

`--fail-under FLOAT`, optional, default `None` — omitting it reproduces today's exact always-exit-0 behavior byte-for-byte. When given:

- **Raw `--successes`/`--total`**: one check against that single rate's lower bound.
- **`--scores` mode**: one check per `scorer_type` group, **independently** — a `deterministic` group passing does not offset an `llm_judge` group failing, matching this command's own pre-existing "never blend distinct scorer_types" rule exactly. Any one group failing fails the whole command.
- **`--traces` mode**: one check against the aggregate OK-rate's lower bound.

Every mode still prints its full report **before** any gate check runs — a gate failure never short-circuits or hides a passing group's own real numbers; the operator sees everything, then gets a clear `click.ClickException` naming exactly which group(s) failed and by how much.

Deliberately scoped to `report_cmd` only, not `run_cmd`/`rollout_cmd` (which also print a pass-rate line today) — the natural CI invocation shape is `evals run ... --scores out.jsonl && evals report --scores out.jsonl --fail-under 0.8`, so `report` is the one command that needs to own the go/no-go decision; widening `run`/`rollout` themselves would be a real but separate, unrequested feature.

## Alternatives considered

**A full power calculation before gating** — rejected for this pass; see "Gate on the Wilson lower bound" above. Named as the real, larger future work `evals/ARCHITECTURE.md`'s own stated principle points to, not silently declared solved by this narrower gate.

**Gating on the point estimate** — rejected: this is exactly the naive design the lower-bound choice improves on, and the one a less careful implementation might reach for first. Proven wrong by a dedicated test using 8/10's real numbers (point estimate 0.8, lower bound 0.4902) specifically because they diverge enough to matter at a real `--fail-under` value.

**Widening `run_cmd`/`rollout_cmd` with their own `--fail-under`** — rejected for this pass as unrequested scope beyond the audit's own specific finding about `report`; a real, defensible future addition if an operator's real CI workflow needs it.

## Verification

`evals/tests/test_cli_integration.py`: `test_report_fail_under_passes_when_lower_bound_clears_the_bar`, `test_report_fail_under_gates_on_the_lower_bound_not_the_point_estimate` (the load-bearing point-estimate-vs-lower-bound proof, using 8/10's real, already-pinned reference numbers), `test_report_fail_under_omitted_reproduces_exact_zero_exit_behavior`, `test_report_scores_fail_under_checks_each_scorer_type_group_independently` (reusing an existing fixture's real, already-verified Wilson bounds — deterministic 2/2 lower=0.3424, llm_judge 1/3 lower=0.0615 — to prove one group's failure fails the whole command without averaging), `test_report_traces_fail_under_checked_against_ok_rate`. All 5 pass; full suite 186 passed (up from 181), 11 skipped, zero regressions; `ruff check .`/`uvx ... lint-imports` both clean. Sanity-checked by temporarily changing the gate comparison from the Wilson lower bound to the bare point estimate (the exact rejected alternative above) and re-running: 3 of 5 new tests failed, each for the exact right reason (a case specifically designed to diverge between the two metrics), then restored.
