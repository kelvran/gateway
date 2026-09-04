> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Close `THREAT_MODEL.md`'s Evals DoS gap with an opt-in result cache and a statistically-sound two-checkpoint early-stopping rule in the Rollout Scheduler — both off by default, both backward-compatible.

**Architecture:** Two new small modules (`evals/evals/rollout/cache.py`, a new function in `evals/evals/stats.py`), additive fields on `Run`/`RunStatus` (`evals/evals/models.py`), a restructured-but-backward-compatible `run_suite` (`evals/evals/rollout/scheduler.py`), and new opt-in flags on `rollout_cmd` (`evals/evals/cli.py`).

**Tech Stack:** stdlib only — `hashlib`, `json`, `dataclasses`. No new dependency.

**Spec:** `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`.

**Global Constraints:**
- `run_suite(cases)` called with no new keyword arguments must be byte-for-byte identical in behavior to today.
- Every new `Run`/`RunStatus` field/member is additive with a default — an old JSONL `Run` line must still `model_validate_json` cleanly.
- No continuous per-trial CI re-check — the two-checkpoint rule only ever evaluates at exactly `min_trials`/`max_trials`.
- A `score_fn` call must never happen twice for the same real trial (double-billing risk for `--llm-judge`).

---

## Phase 1: Result cache

### Task 1: `compute_run_cache_key`

**Files:**
- Create: `evals/evals/rollout/cache.py`
- Test: `evals/tests/test_cache.py`

**Steps:**
- [x] Implement `compute_run_cache_key(task_spec: dict, harness_config: dict) -> str` — SHA-256 hex digest over `json.dumps({"task_spec": task_spec, "harness_config": harness_config}, sort_keys=True, ensure_ascii=True, allow_nan=False, separators=(",", ":"))`.
- [x] Test: stable across dict key-insertion order (build the same logical dict two different ways, assert identical hash).
- [x] Test: changes when any field in either `task_spec` or `harness_config` changes (no accidental collision for at least 3 distinct field changes).
- [x] `cd evals && ruff check . && uv run pytest tests/test_cache.py -v`.

### Task 2: `Run` model additions

**Files:**
- Modify: `evals/evals/models.py`
- Test: `evals/tests/test_models.py`

**Steps:**
- [x] Add `cache_key: str | None = None`, `from_cache: bool = False`, `cache_source_run_id: str | None = None` to `Run`. Extend `Run`'s docstring with the same "what None/False means" convention already used for `cost_usd`.
- [x] Test: an old-shape `Run` JSON line (missing all three fields) `model_validate_json`s cleanly with all three resolving to their declared defaults — a permanent regression fixture, not just a one-off check.
- [x] `cd evals && uv run pytest tests/test_models.py -v`.

### Task 3: `run_suite` cache-lookup integration

**Files:**
- Modify: `evals/evals/rollout/scheduler.py`
- Test: `evals/tests/test_scheduler.py`

**Steps:**
- [x] Add `cached_runs: dict[str, Run] | None = None` keyword-only param to `run_suite`. Compute `cache_key` unconditionally (cheap) for every case, even when `cached_runs is None`, so every produced `Run` carries it.
- [x] Cache-hit branch: only consulted when `cached_runs is not None` and `case.tier != "drift_sample"`. On hit, build a `Run` with `status="completed"`, the cached `stdout`/`stderr`/`exit_code`/`cost_usd`, `latency_ms=0.0`, `from_cache=True`, `cache_source_run_id=<hit's id>` — no `run_in_sandbox` call.
- [x] Test: `cached_runs=None` (the default) reproduces today's exact `Run` sequence for a suite with no other new params — a direct before/after equality assertion.
- [x] Test: a case with `tier="drift_sample"` never hits the cache even when a matching completed `Run` exists in `cached_runs`.
- [x] Test: a cache hit only fires against a prior `Run` with `status == "completed"` and `not from_cache` — explicit negative tests for `error`/`timed_out` priors and for chained-hit priors.
- [x] `cd evals && uv run pytest tests/test_scheduler.py -v`.

### Task 4: `--use-cache` CLI flag

**Files:**
- Modify: `evals/evals/cli.py`
- Test: `evals/tests/test_cli_integration.py`

**Steps:**
- [x] Add `--use-cache` flag (off by default) to `rollout_cmd`. When set, build `cached_runs` from `load_runs(results_path)` filtered to `status == "completed" and not from_cache and cache_key is not None`.
- [x] Test: a second `rollout` invocation with `--use-cache` against the same `--suite`/`--results` skips real sandbox execution for unchanged cases (assert the mocked `run_in_sandbox` call count doesn't increase) and the resulting `Run` has `from_cache=True`.
- [x] Test: without `--use-cache`, a second identical invocation still re-runs everything (today's exact behavior, unchanged).
- [x] `cd evals && uv run pytest tests/test_cli_integration.py -v`.

---

## Phase 2: Two-checkpoint early stopping

### Task 1: `two_checkpoint_early_stop`

**Files:**
- Modify: `evals/evals/stats.py`
- Test: `evals/tests/test_stats.py`, `evals/tests/test_stats_properties.py`

**Steps:**
- [x] Implement `two_checkpoint_early_stop(successes, trials_run, min_trials, max_trials, baseline_pass_rate, confidence=0.95) -> bool` per the RFC — returns `False` for every `trials_run` not exactly equal to `min_trials` or `max_trials`; applies the Bonferroni-corrected confidence (`1 - (1-confidence)/2`) unless `min_trials == max_trials`.
- [x] Property test (Hypothesis, extending `test_stats_properties.py`'s existing convention): never returns `True` for `trials_run` outside `{min_trials, max_trials}`, over arbitrary valid inputs.
- [x] Direct test: at `trials_run == min_trials` and `== max_trials`, the returned decision matches an independently-computed call to `wilson_interval` at the corrected confidence — cross-check against the real function, not a re-derivation.
- [x] **The regression test that proves the fix**: simulate repeated `Binomial(n=max_trials, p=baseline_pass_rate)` draws (the true null), run both checkpoints many times, assert the empirical stop-rate stays at or below the nominal `1 - confidence` — the concrete test that would have failed against the originally-floated naive "recheck every trial" design.
- [x] `cd evals && uv run pytest tests/test_stats.py tests/test_stats_properties.py -v`.

### Task 2: `run_suite` early-stopping integration

**Files:**
- Modify: `evals/evals/rollout/scheduler.py`
- Test: `evals/tests/test_scheduler.py`

**Steps:**
- [x] Add `RunStatus`'s `"skipped"` member and `Run.skip_reason: str | None = None` (`evals/evals/models.py`) — same backward-compat verification as Phase 1 Task 2.
- [x] Add `EarlyStopConfig` frozen dataclass (`min_trials`, `max_trials`, `baseline_pass_rate`, `score_fn: Callable[[EvalCase, Run], bool]`, `confidence: float = 0.95`) co-located in `scheduler.py`.
- [x] Add `early_stop: EarlyStopConfig | None = None` keyword-only param to `run_suite`. Track a per-`(eval_case_id, eval_case_revision)` running `(successes, trials_run)` tally; once `stopped_groups` contains a group, every later case in it becomes a `status="skipped"` `Run` (`latency_ms=0.0`, no sandbox call, no tally update).
- [x] Restructure the exception path so it also reaches the shared tally call before its `continue` (today's bare `continue` must not skip this).
- [x] Cache hits (Phase 1) also flow through the tally — a hit still has real, scoreable output.
- [x] Test: `early_stop=None` (the default) reproduces today's exact `Run` sequence — direct before/after equality.
- [x] Test: a group whose trials error out repeatedly still reaches a checkpoint decision (or exhausts `max_trials`) rather than hanging — direct test of the restructured error-path tally.
- [x] Test: `score_fn` is called exactly once per real (non-skipped) trial — never zero, never twice.
- [x] `cd evals && uv run pytest tests/test_scheduler.py -v`.

### Task 3: `--early-stop-*` CLI flags

**Files:**
- Modify: `evals/evals/cli.py`
- Test: `evals/tests/test_cli_integration.py`

**Steps:**
- [x] Add `--early-stop-min-trials`/`--early-stop-max-trials`/`--early-stop-baseline-pass-rate` (all `default=None`) to `rollout_cmd`; `click.UsageError` if only some are given, mirroring `report_cmd`'s existing `--successes`/`--total` pairing.
- [x] Unify scoring into one `_score_and_record(case, run) -> bool` closure (increments `successes`/`total`, appends the real `Score`, echoes PASS/FAIL/status) — used as `EarlyStopConfig.score_fn` when early-stopping is active, called directly in a post-hoc loop otherwise. Never called for a `"skipped"` `Run` either way.
- [x] A separate, lightweight post-`run_suite` loop echoes `SKIPPED` for any `status="skipped"` run — kept out of `_score_and_record` so skipped runs never affect `total`/`successes`.
- [x] Test: `--llm-judge` + early-stopping active — assert the mocked judge-call count equals the number of completed (non-cache-hit, non-skipped) runs, never double that (the double-billing regression test named in the RFC).
- [x] Test: `format_report`'s `total` denominator matches exactly between an early-stop-inactive run and an equivalent early-stop-active run with a stopping condition that never fires (the denominator-drift regression test named in the RFC).
- [x] `cd evals && uv run pytest tests/test_cli_integration.py -v`.

---

## Phase 3: Docs, verify, ship

### Task 1: Documentation

**Files:**
- Modify: `THREAT_MODEL.md` (Evals DoS row — correct from "Not built" to naming the real mechanism and what's still deferred)
- Modify: `evals/ARCHITECTURE.md` (Data Model / Rollout Lifecycle sections)
- Modify: `evals/changelog/unreleased.md`, `DECISIONS.md`, `docs/agents/LOGS.md`, `STATUS.md`

**Steps:**
- [x] `THREAT_MODEL.md`'s Evals DoS row: name the real two-checkpoint mechanism + result cache, and explicitly note per-tier calibrated thresholds/SPRT/Score-level caching remain future work.
- [x] `evals/ARCHITECTURE.md`: update to reflect the real, narrower `Run` shape and the real stats-engine addition.
- [x] Changelog + `DECISIONS.md` + `docs/agents/LOGS.md` + `STATUS.md`, per this project's established convention.

### Task 2: Full verification and ship

**Steps:**
- [x] `cd evals && uv run pytest tests/ -v` — full suite green, zero regressions to any pre-existing test's assertions.
- [x] `ruff check .` clean.
- [x] Root `make verify` clean.
- [x] `git add` the exact touched files; commit with a `feat(evals):` conventional-commit message.
- [x] Push; watch real CI to green.
- [x] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

Architecturally-scoped work (new stats primitive, restructured scheduler control flow, additive-but-real data-model changes) — correctly warranting this plan + `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`, not a one-line `DECISIONS.md` entry alone.
