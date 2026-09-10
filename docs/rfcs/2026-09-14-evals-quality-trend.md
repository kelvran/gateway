# RFC: Trended, side-by-side judge-quality history

## Status

Accepted, implementing 2026-09-14.

## Summary

Adds a new `TrendSnapshot` pydantic model and `append_trend_snapshots`/`load_trend_snapshots` (added directly inside `results_store.py`, reusing its existing generic JSONL helpers), an opt-in `--record-trend PATH` flag on both `report` and `audit-corpus`, and a new `evals trend show` read command — giving Kelvran a persisted history of judge-accuracy kappa, quote-grounding rate, audit-corpus defect rate, and cost, each trended as its own named series, never fused into a composite.

## Motivation

`docs/upgrade-research/evals-next-upgrade-round3-2026-09-11.md`'s Findings 2/5 confirmed every already-computed signal (kappa, quote-grounding rate, defect rate, cost) was thrown away after each command printed it once — real data, computed correctly, with nowhere to accumulate. Every surveyed 2026 platform (Langfuse/Braintrust/Arize) trends named score series side by side; none fuses them into a composite "quality score," which this codebase's own `evals.cli` module docstring already states as a governing principle ("never blended across `deterministic` and `llm_judge`") for the exact same reason: these are different measurements of different things, and averaging them would hide which one degraded.

## Detailed Design

### `TrendSnapshot` (`evals/evals/models.py`)

One snapshot always names exactly one `series` (`judge_accuracy_kappa` | `quote_grounding_rate` | `audit_corpus_defect_rate` | `cost_usd`). Exactly one of `rate_value`/`cost_usd_value` is the "active" field for that series — `cost_usd` uses `cost_usd_value` (a real `Decimal`, never cast to `float`, matching `Score.cost_usd`'s own established convention); every other series uses `rate_value`.

**The validator is deliberately narrower than a bare "exactly one of the two is non-None" check.** `rate_value` being `None` is itself a legitimate, meaningful value for its own active series — a `judge_accuracy_kappa` snapshot records `None` when Cohen's kappa is genuinely undefined (zero-variance verdicts, the same case `report_cmd` already names `kappa_str == "undefined"`), and a `quote_grounding_rate` snapshot records `None` when no `Score` in that run had a known `quote_grounded` value at all. A bare non-None check would incorrectly reject both real, spec-required cases. The actual invariant enforced: the field that does *not* belong to the snapshot's `series` must never carry a value — `cost_usd_value` must be `None` for any non-`cost_usd` series, and `rate_value` must be `None` for `cost_usd` (a real cost total is always computable at every real call site, so `cost_usd` has no "measured but undefined" case to make room for).

### Persistence (`evals/evals/results_store.py`)

`append_trend_snapshots`/`load_trend_snapshots` are added directly inside this file, one-line wrappers over the existing `_append_models`/`_load_models` generics — no new sibling module, no new import, and no `.importlinter` layer-3 sibling-independence question raised, since the functions live alongside the data they persist rather than importing across a layer boundary to reach it.

### Computation and CLI wiring (`evals/evals/cli.py`)

`--record-trend PATH` is a new, optional flag on both `report` and `audit_corpus`. Omitted (the default): zero trend code runs, the target file (if any) is left byte-unchanged — recording is opt-in, never automatic. When given, each command collects its own `TrendSnapshot`(s) during its existing loop (no second pass over the data) and appends once.

- **Kappa/quote-grounding**: collected inside `report_cmd`'s existing per-`scorer_type` loop, at the exact point that already computes `cohens_kappa`/`grounded`/`grounding_known`. `scorer_type` is real on every snapshot from this command — never blended across `deterministic`/`llm_judge`/`llm_judge_panel`, the same discipline `report_cmd`'s own pass-rate/CI lines already enforce.
- **Cost**: a new `_sum_known_costs(scores) -> (Decimal, int)` helper factored out of `_format_group_cost`'s own existing logic (that function now calls the helper instead of duplicating it). One `TrendSnapshot(series="cost_usd")` per `scorer_type` per invocation — matches `_format_group_cost`'s own existing granularity, which is already scoped per `scorer_type` group. Built against `Score.cost_usd` only; `Run.cost_usd` is confirmed dead in every real call path (sandbox rollout runs a Docker command, not a billed call; ingestion never sets it) and is not used here.
- **Defect rate**: one snapshot per `audit-corpus` invocation (`scorer_type=None` — audited per suite file, not per scorer), from the same `counts` dict the command's existing summary line already prints.
- **Recording happens before the `--fail-under`/`--category-fail-under` gate check**, not strictly "last thing before return" — a `report` run that fails its own gate still gets its trend history recorded, rather than silently losing that data point purely because the run happened to fail its quality bar. Covered by a dedicated test.

### `evals trend show` (new read command)

`evals trend show --path PATH [--series NAME]` loads via `load_trend_snapshots`, groups by series, sorts each group by `recorded_at`, and prints one block per series — **never averaged or combined across series into one line**. This is the read-side half of the "never fused" guarantee; a write-side model that never fuses would still be undermined by a display layer that quietly averages on the way out.

## Drawbacks

- `TrendSnapshot` files can grow unboundedly (JSONL append-only, matching every other `results_store.py`-backed file) — no rotation/pruning mechanism exists yet, the same disclosed limitation every other JSONL results file in this codebase already has.
- Cost-per-`scorer_type` granularity means a `report` invocation with 3 scorer types produces 3 separate cost snapshots, not one combined total — a deliberate mirror of `_format_group_cost`'s own existing behavior, not a new decision, but worth naming since a naive reader might expect one snapshot per command.

## Alternatives Considered

**A fused "eval quality score" combining kappa/grounding/defect-rate/cost into one number.** Rejected — explicitly named as a non-goal by the round-3 research (Finding 2: "do not build," runs against the grain of every surveyed platform) and by this codebase's own pre-existing module docstring.

**A new dedicated command (`evals trend record`) instead of a flag on existing commands.** Rejected — `report`/`audit-corpus` already compute the exact numbers at the exact right moment inside their own loops; a separate command would either duplicate that logic or call into these commands as a library, both worse than an opt-in flag at the point of computation.

## Unresolved Questions

- No rotation/retention policy for growing `TrendSnapshot` JSONL files — named as real future work, not decided here, matching every other unbounded-JSONL-file precedent in this codebase.

## Verification

`cd evals && uv run pytest tests/ && ruff check . && uvx --from import-linter lint-imports` — 387 passed, 11 skipped (pre-existing opt-in live/Docker tests, unrelated), ruff clean, all 3 import-linter contracts kept.

New tests: `test_models.py` (12 new `TrendSnapshot` tests — construction, both value fields, `None` as a legitimate value, all 3 validator-rejection cases, frozen, JSON round-trip preserving `Decimal`); `test_results_store.py` (6 new tests — missing-file→`[]`, round-trip, accumulate-not-overwrite, Decimal round-trip precision); new `test_trend_integration.py` (18 tests — full `CliRunner` integration covering snapshot content from both commands, the opt-in-only guarantee via untouched-file proofs, and `trend show`'s never-fused output with real string-content assertions, not just exit-code checks).

Sanity-checked-by-breaking, twice: (1) swapped the cost-trend call site to write `rate_value` instead of `cost_usd_value` — the model's own validator caught it immediately (`ValueError`), surfacing as failing integration tests before a bad value could even be persisted; reverted, re-ran green. (2) replaced `trend show`'s per-series loop with a single fused combined-average line — the never-fused integration test failed for the exact predicted reason; reverted, re-ran green.
