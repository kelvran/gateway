> **For agentic executors:** Task 1 (`Run` model) is independent and must land first — everything else depends on its shape. Task 2 (scheduler + results store) depends on Task 1. Task 3 (CLI wiring + shared scoring refactor) depends on Task 2. Task 4 is docs/changelog/wrap-up.

---

**Goal:** A real, working "case → sandboxed execution → scored, persisted `Run`" path in `evals/` — a `Run` model, a sequential Rollout Scheduler over the existing `run_in_sandbox()`, an append-only JSONL Results Store, and a new `evals rollout` CLI command scored with the existing deterministic scorer and Wilson CI.

**Architecture:** New `Run` model in `evals/evals/models.py` (alongside `EvalCase`, same frozen convention). New `evals/evals/rollout/scheduler.py` (`run_suite`) and `evals/evals/rollout/results_store.py` (`append_runs`/`load_runs`) modules, both consuming the existing, unmodified `evals/evals/rollout/sandbox.py`. New `evals rollout` CLI command in `evals/evals/cli.py`, reusing the existing `_load_cases`/`exact_match`/`regex_match`/`wilson_interval`/`format_report` via one small, behavior-preserving refactor (`_score_output_deterministic`) shared with the existing `run` command.

**Spec:** `docs/rfcs/2026-09-04-evals-rollout-scheduler.md` — the exact `Run` shape (including the deliberate `cost_usd: None`/no-`Trace`-field decisions), the scheduler's sequential-only contract, and every explicitly-cut scope item live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- Zero new dependencies — `evals/pyproject.toml` gains nothing (no `numpy`/`scipy`/`anthropic`/`openai`/DB driver).
- Sequential execution only — no `asyncio.gather`, no worker pool, no Ray.
- No `Trace`/`Span` field on `Run`, not even an empty placeholder.
- `cost_usd` defaults to `None`, never `0.0` — `None` means "not measured," not "measured as zero."
- `evals run`'s existing behavior, `task_spec` convention, and tests are unmodified — `rollout` is strictly additive.
- Do not touch `THREAT_MODEL.md`'s Evals STRIDE-table gap or wire a real LLM-judge SDK call in this pass — both are named, explicitly out of scope in the spec.

---

## Task 1 — `Run` model

**Files:**
- Modify: `evals/evals/models.py`
- Modify: `evals/tests/test_models.py`

**Steps:**
- [ ] Add `RunStatus = Literal["completed", "timed_out", "error"]` alongside the existing `EvalTier`.
- [ ] Add `Run(BaseModel)` with `model_config = ConfigDict(frozen=True)`: `id: str`, `eval_case_id: str`, `eval_case_revision: int`, `harness_config: dict`, `status: RunStatus`, `exit_code: int | None = None`, `stdout: str = ""`, `stderr: str = ""`, `latency_ms: float`, `cost_usd: float | None = None`, `error: str | None = None`.
- [ ] Docstring on `Run` states plainly (mirroring `EvalCase`'s own docstring style) that `cost_usd`/`Trace` are deliberately absent/`None` in v1 and why — pointer to the RFC, not a re-argued justification.
- [ ] Tests in `test_models.py`: construction with all fields, construction with only required fields (defaults apply), frozen-instance mutation rejection (mirrors `EvalCase`'s existing frozen test), and a test proving `cost_usd` defaults to `None` (not `0.0`) when omitted.

**Verify:** `cd evals && uv run pytest tests/test_models.py -v`

## Task 2 — Scheduler + Results Store

**Files:**
- Create: `evals/evals/rollout/scheduler.py`
- Create: `evals/evals/rollout/results_store.py`
- Create: `evals/tests/test_scheduler.py`
- Create: `evals/tests/test_results_store.py`

**Steps:**
- [ ] `scheduler.py`: module docstring stating the v1 contract plainly — sequential only, `task_spec` must contain `image`/`command` (optional `timeout_s`, default `DEFAULT_SANDBOX_TIMEOUT_S = 30`), one `Run` per `EvalCase`, never raises on a single case's failure.
- [ ] `run_suite(cases: list[EvalCase]) -> list[Run]`: for each case, build `harness_config = {"image": ..., "command": ..., "timeout_s": ...}` from `case.task_spec`; measure wall-clock via `time.monotonic()` around the `run_in_sandbox` call; on success, `status="timed_out"` if `result.timed_out` else `"completed"`, with `exit_code`/`stdout`/`stderr` copied from the `SandboxResult`; on any exception raised by `run_in_sandbox` itself (e.g. `FileNotFoundError` when the `docker` binary is missing), catch it, build a `Run` with `status="error"`, `error=str(exc)`, and continue to the next case rather than aborting the suite. Generate `id` via `uuid.uuid4().hex` (stdlib, no new dependency).
- [ ] `results_store.py`: `append_runs(runs: list[Run], path: Path) -> None` (opens in append mode, one `run.model_dump_json()` per line) and `load_runs(path: Path) -> list[Run]` (returns `[]` if the file doesn't exist yet; otherwise one `Run.model_validate_json(line)` per non-blank line).
- [ ] `test_scheduler.py`: monkeypatch `evals.rollout.scheduler.run_in_sandbox` (mirroring `test_sandbox_error_paths.py`'s monkeypatch pattern — no Docker needed for the default suite). Cases: a successful run produces `status="completed"` with the right `stdout`/`exit_code`; a `timed_out=True` `SandboxResult` produces `status="timed_out"`; a monkeypatched `run_in_sandbox` that raises `FileNotFoundError` produces `status="error"` with a non-empty `error` field, AND the suite continues to score the remaining cases (prove a two-case suite where case 1 errors still returns 2 `Run`s, not 1). One `RUN_DOCKER_TESTS=1`-gated real end-to-end test (mirroring `test_sandbox_integration.py`'s `pytestmark`) running a real `alpine:3.20` container through `run_suite`.
- [ ] `test_results_store.py`: append 2 `Run`s, `load_runs` returns them equal to the originals (round-trip); `load_runs` on a nonexistent path returns `[]`; appending twice then loading returns all 4 in order (proves append semantics, not overwrite).

**Verify:** `cd evals && uv run pytest tests/test_scheduler.py tests/test_results_store.py -v`

## Task 3 — CLI wiring + shared scoring refactor

**Files:**
- Modify: `evals/evals/cli.py`
- Modify: `evals/tests/test_cli_integration.py`
- Create: `evals/tests/fixtures/rollout_example.json`

**Steps:**
- [ ] Refactor: extract `_score_output_deterministic(output: str, reference: str | None, match_kind: str, pattern: str | None, case_id: str) -> bool` from the body of `_score_case_deterministic` (same exact_match/regex_match logic, same error messages referencing `case_id`) — `_score_case_deterministic` becomes a thin wrapper calling it with `case.task_spec.get("output")`/`case.reference`/`case.task_spec.get("match", "exact")`/`case.task_spec.get("pattern")`/`case.id`. Existing tests must pass unmodified — this is a pure refactor, not a behavior change.
- [ ] Add `_score_run_deterministic(case: EvalCase, run: Run) -> bool`: if `run.status != "completed"`, return `False` (a timed-out or errored run never passes); otherwise calls `_score_output_deterministic(run.stdout.strip(), case.reference, case.task_spec.get("match", "exact"), case.task_spec.get("pattern"), case.id)`.
- [ ] Add `rollout` command: `@main.command("rollout")`, options `--suite` (same `click.Path` shape as `run`), `--results` (required, `click.Path(dir_okay=False, path_type=Path)`, append target), `--confidence` (same default as `run`). Body: `_load_cases(suite_path)` → `asyncio.run(run_suite(cases))` → `append_runs(runs, results_path)` → for each `(case, run)` pair, `click.echo` a per-case line (`PASS`/`FAIL` if `status == "completed"`, else the literal status word e.g. `TIMED_OUT`/`ERROR`) → `click.echo(format_report(...))` at the end, identical shape to `run_cmd`.
- [ ] `--help` text on both `run` and `rollout` explicitly cross-references the other's `task_spec` convention (one line each), per the RFC's resolution of the two-conventions coexistence question.
- [ ] `rollout_example.json` fixture: 2 cases, `task_spec={"image": "alpine:3.20", "command": [...], "timeout_s": 30, "match": "exact"}`, one expected to pass (echo matching `reference`) and one expected to fail (echo not matching).
- [ ] New tests in `test_cli_integration.py`: a default-suite (no Docker) test monkeypatching `evals.rollout.scheduler.run_in_sandbox` to return scripted `SandboxResult`s keyed by command, asserting the printed PASS/FAIL lines, the Wilson-CI-bearing report line, AND that `--results <tmp_path>` now contains the right number of JSONL lines afterward (read back via `load_runs`). One `@pytest.mark.integration` + `@pytest.mark.skipif(RUN_DOCKER_TESTS != "1")`-decorated real end-to-end test using the real fixture against a real Docker daemon.

**Verify:** `cd evals && uv run pytest tests/ -v` (default suite, no Docker) → all non-integration tests pass, 0 regressions in the existing `run`/`report` tests; `cd evals && ruff check .` clean; separately, `RUN_DOCKER_TESTS=1 uv run pytest tests/ -v` against a real local Docker daemon.

## Task 4 — Docs, changelog, wrap-up

**Files:**
- Modify: `evals/ARCHITECTURE.md`
- Modify: `evals/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] `evals/ARCHITECTURE.md`'s Package Layout `rollout/` line: update from "sandboxed agent rollout orchestration" to name what's real (the sequential scheduler + results store) and what's still not (Sandbox Pool/concurrency, Task/Dataset Registry).
- [ ] `evals/ARCHITECTURE.md`'s Data Model sketch: mark `Run` as real (v1-narrowed shape, pointer to the RFC), `Trace`/`Span`/`Score` still unbuilt.
- [ ] `evals/ARCHITECTURE.md`'s Tech Stack table: correct the `Statistics` and `Judge SDKs` rows to distinguish what's real today (Wilson CI, stdlib-only) from the future target (bootstrap/Bayesian/pass@k via numpy/scipy/scikit-learn; a real Anthropic/OpenAI SDK call), per this RFC's Unresolved Questions resolution — mirroring how `gateway/ARCHITECTURE.md` already flags some Tech Stack rows as not-yet-built rather than implying they're installed.
- [ ] `evals/changelog/unreleased.md`: new `## Added` entry describing the `Run` model, scheduler, results store, and `rollout` command, at the same detail level as `gateway`'s Cache L3-lite/Guardrails entries — including the deliberate `cost_usd: None`/no-`Trace`-field decisions and the sequential-only scope.
- [ ] `DECISIONS.md`: one new line at the true chronological end (re-check `tail` immediately before appending) naming the build, the `cost_usd`/`Trace` narrowing decisions, and the explicitly-deferred `THREAT_MODEL.md` Evals STRIDE-table gap found but not fixed this pass.
- [ ] `docs/agents/LOGS.md`: new entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps).
- [ ] `STATUS.md`: update Current Phase / Last Completed Task / Next Action / Verification State — name the `THREAT_MODEL.md` Evals gap as a real open candidate for a future pass, the same way Guardrails itself was surfaced and later picked up.
- [ ] Full `make verify` from repo root — must pass clean before commit (both deployables; `gateway` is untouched by this pass, so its results must be identical to before).

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.

## Scope Gate

This is architecturally scoped (new data model + new module + new CLI surface), correctly warranting a plan file, not a one-line `DECISIONS.md` entry.
