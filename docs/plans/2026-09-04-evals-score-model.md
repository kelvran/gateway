> **For agentic executors:** Task 1 (`Score` model) is independent and must land first. Task 2 (move+generalize `results_store.py`) depends on Task 1's `Score` model existing. Task 3 (`providers.py` rename) is independent of 1/2 and can land anytime before Task 4. Task 4 (CLI wiring) depends on 1, 2, and 3. Task 5 is docs/changelog/wrap-up.

---

**Goal:** A real `Score` model + JSONL persistence, wired into all four `evals` scoring call sites, closing the last unbuilt entity in `evals/ARCHITECTURE.md`'s Data Model sketch.

**Architecture:** New `Score`/`ScorerType` in `evals/evals/models.py`. `evals/evals/rollout/results_store.py` moves to `evals/evals/results_store.py`, generalized over any frozen pydantic model (`append_runs`/`load_runs` become thin wrappers; new `append_scores`/`load_scores`). `evals/evals/judge/providers.py`'s `_DEFAULT_JUDGE_MODEL` becomes public `DEFAULT_JUDGE_MODEL`. `evals/evals/cli.py`'s `_judge_output`/`_judge_case`/`_judge_run` widen their return type to `JudgeResult | None`; both `run_cmd` and `rollout_cmd` gain a required `--scores <path>` option and construct+persist a `Score` per case at all four scoring call sites.

**Spec:** `docs/rfcs/2026-09-04-evals-score-model.md` — the `run_id`-without-a-`Run` resolution, the exact `Score` field table, and the move-vs-leave-`results_store.py` decision all live there; this plan implements them verbatim.

**Global Constraints** (inherited from the spec):
- `Score.run_id` is `None` for `evals run`, never a fabricated `EvalCase.id` stand-in, never a synthetic `Run`.
- `judge()`/`llm_judge.py` are never modified — the widening happens only in `cli.py`.
- No `Score` row for a `JUDGE_ERROR` case or a non-`"completed"` `Run` — exactly mirrors the existing `continue`-before-counting control flow.
- `--scores` is required on both commands, mirroring `--results`'s existing required convention — a deliberate, named breaking CLI change, not an oversight.
- No `Score.id` primary key, no `rubric_axis` population, no skeptic-panel/`"human"` `scorer_type` value.

---

## Task 1 — `Score` model

**Files:**
- Modify: `evals/evals/models.py`
- Modify: `evals/tests/test_models.py`

**Steps:**
- [ ] Add `ScorerType = Literal["deterministic", "llm_judge"]` alongside `RunStatus`.
- [ ] Add `Score(BaseModel)` with `model_config = ConfigDict(frozen=True)`: `eval_case_id: str`, `eval_case_revision: int`, `run_id: str | None = None`, `scorer_id: str`, `scorer_type: ScorerType`, `value: bool`, `rationale: str | None = None`, `rubric_axis: str | None = None`, `bias_mitigations_applied: list[str] = Field(default_factory=list)`.
- [ ] Docstring names the `run_id`/`eval_case_id` design resolution and the permanently-`None` `rubric_axis`, pointer to the RFC, not a re-argued justification (mirroring `Run`'s own docstring style).
- [ ] Tests in `test_models.py`: construction with all fields; construction with only required fields (defaults apply — `run_id=None`, `rationale=None`, `rubric_axis=None`, `bias_mitigations_applied=[]`); frozen-instance mutation rejection; invalid `scorer_type` rejection.

**Verify:** `cd evals && uv run pytest tests/test_models.py -v`

## Task 2 — Move + generalize `results_store.py`

**Files:**
- Create: `evals/evals/results_store.py`
- Delete: `evals/evals/rollout/results_store.py`
- Modify: `evals/tests/test_results_store.py`

**Steps:**
- [ ] Create `evals/evals/results_store.py`: module docstring stating the move rationale (mechanism was never rollout-specific) and the corrected promptfoo/Inspect AI precedent claim (promptfoo uses an embedded SQLite DB, not flat files). `ModelT = TypeVar("ModelT", bound=BaseModel)`; private `_append_models(models: list[ModelT], path: Path) -> None` and `_load_models(model_cls: type[ModelT], path: Path) -> list[ModelT]`; public `append_runs`/`load_runs` (thin wrappers over `Run`, byte-identical behavior to today) and new `append_scores`/`load_scores` (over `Score`).
- [ ] Delete `evals/evals/rollout/results_store.py`.
- [ ] Update `test_results_store.py`'s import (`from evals.rollout.results_store import ...` → `from evals.results_store import ...`) — no assertion changes, since `Run` behavior is byte-identical. Add a parallel `Score` round-trip test (`_make_score` fixture mirroring the existing `_make_run` pattern): append 2 `Score`s, `load_scores` returns them equal; `load_scores` on a nonexistent path returns `[]`; appending twice accumulates rather than overwrites.

**Verify:** `cd evals && uv run pytest tests/test_results_store.py -v && grep -rn "rollout.results_store" evals/ tests/` (must return zero hits once Task 4 also updates `cli.py`'s import — acceptable to still show `cli.py`'s old import until Task 4 lands, per the dependency note above).

## Task 3 — Public `DEFAULT_JUDGE_MODEL`

**Files:**
- Modify: `evals/evals/judge/providers.py`
- Modify: `evals/tests/test_providers.py`

**Steps:**
- [ ] Rename `_DEFAULT_JUDGE_MODEL` → `DEFAULT_JUDGE_MODEL` (confirmed via grep: zero other references anywhere in the tree before this rename — a pure, zero-risk visibility change). Update its own doc comment if it references the old private name.
- [ ] Confirm `test_providers.py`'s existing tests (which reference `"claude-haiku-4-5-20251001"` as a literal in some assertions) still pass unmodified — no test changes should be needed since none imported the private constant by name.

**Verify:** `cd evals && uv run pytest tests/test_providers.py -v && ruff check evals/judge/providers.py`

## Task 4 — CLI wiring: `Score` construction + `--scores` at all four call sites

**Files:**
- Modify: `evals/evals/cli.py`
- Modify: `evals/tests/test_cli_integration.py`

**Steps:**
- [ ] Imports: `from evals.judge.llm_judge import judge, JudgeResult`; `from evals.judge.providers import DEFAULT_JUDGE_MODEL, make_anthropic_call_model`; `from evals.models import EvalCase, Run, Score`; `from evals.results_store import append_runs, append_scores` (replaces the old `evals.rollout.results_store` import).
- [ ] Widen `_judge_output`'s return type to `JudgeResult | None`: on success, `return` the full `judge()` result instead of `result.passed`; on the reference-missing branch, still raise `click.ClickException` unchanged; on a judge-call exception, still `return None` unchanged.
- [ ] Widen `_judge_case`/`_judge_run`'s return-type annotations to `JudgeResult | None` — bodies unchanged (they already just forward `_judge_output`'s return value).
- [ ] Add `_deterministic_scorer_id(case: EvalCase) -> str`: `"exact_match"` if `case.task_spec.get("match", "exact") == "exact"` else `"regex_match"`.
- [ ] `run_cmd`: add a required `--scores` `click.option` (`click.Path(dir_okay=False, path_type=Path)`, help text mirroring `rollout_cmd`'s `--results` help style) placed right after `--suite`. Body: accumulate `scores: list[Score] = []`; in the `llm_judge` branch, `judge_result = _judge_case(case, call_model)`; if `None`, echo `JUDGE_ERROR` and `continue` (no Score); else `passed = judge_result.passed` and append `Score(eval_case_id=case.id, eval_case_revision=case.revision, run_id=None, scorer_id=DEFAULT_JUDGE_MODEL, scorer_type="llm_judge", value=passed, rationale=judge_result.rationale, bias_mitigations_applied=judge_result.bias_mitigations_applied)`; in the deterministic branch, `passed = _score_case_deterministic(case)` and append `Score(eval_case_id=case.id, eval_case_revision=case.revision, run_id=None, scorer_id=_deterministic_scorer_id(case), scorer_type="deterministic", value=passed)`. After the loop, `append_scores(scores, scores_path)`, then the existing `format_report` echo — stdout ordering and content otherwise unchanged.
- [ ] `rollout_cmd`: add the same required `--scores` option right after `--results`. Body: same `scores: list[Score] = []` accumulation; the existing `if run.status != "completed": continue` branch stays exactly as-is (no Score); in the `llm_judge` branch, `judge_result = _judge_run(case, run, call_model)`, same `None`-check/`JUDGE_ERROR`/`continue`, else append `Score(..., run_id=run.id, scorer_id=DEFAULT_JUDGE_MODEL, scorer_type="llm_judge", value=judge_result.passed, rationale=judge_result.rationale, bias_mitigations_applied=judge_result.bias_mitigations_applied)`; deterministic branch appends `Score(..., run_id=run.id, scorer_id=_deterministic_scorer_id(case), scorer_type="deterministic", value=passed)`. After the loop, `append_scores(scores, scores_path)` (alongside the existing `append_runs(runs, results_path)` call, which stays at its current position before the scoring loop, unchanged).
- [ ] Update every existing test invocation in `test_cli_integration.py` to add `--scores <tmp_path>` (all `run`/`rollout` invocations, including the `--llm-judge` and Docker-gated ones) — a mechanical addition, no assertion changes for existing behavior.
- [ ] New tests in `test_cli_integration.py`: `evals run` (deterministic) persists one `Score` per case with the right `eval_case_id`/`scorer_id="exact_match"`/`run_id=None`/`value`; `evals run --llm-judge` persists a `Score` with `scorer_type="llm_judge"`, a non-empty `rationale`, and the real `bias_mitigations_applied` list (`["cot_forcing", "reference_guided_grading"]`) — the core load-bearing proof this RFC exists to deliver, since that data was previously discarded; a `JUDGE_ERROR` case produces zero `Score` rows for that case (persisted list length reflects only the cases that were actually judged); `evals rollout` (deterministic and `--llm-judge`) persists `Score`s with a real, non-`None` `run_id` matching the corresponding persisted `Run.id`; a non-`"completed"` `Run` produces zero `Score` rows for that case.

**Verify:** `cd evals && uv run pytest tests/test_cli_integration.py -v` (no real API key needed — llm-judge tests use the existing fake-`call_model` monkeypatch pattern).

## Task 5 — Docs, changelog, wrap-up

**Files:**
- Modify: `evals/ARCHITECTURE.md`
- Modify: `evals/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [ ] `evals/ARCHITECTURE.md`'s Data Model sketch: mark `Score` as real (v1-narrowed shape — `eval_case_id` added, `run_id` nullable, `scorer_type` narrowed to two real values, `rubric_axis` permanently `None`), pointer to the RFC. `Trace`/`Span` remain the only unbuilt entities.
- [ ] `evals/ARCHITECTURE.md`'s Package Layout: update the `rollout/` line's `results_store` reference (it moved) and add a one-line note at the top-level `evals/` listing for the new `results_store.py`.
- [ ] `evals/changelog/unreleased.md`: new `## Added` entry (the `Score` model, the four wired call sites, the required `--scores` flag) and a `## Changed` entry (the `results_store.py` move + the `providers.DEFAULT_JUDGE_MODEL` rename), at the same detail level as the two prior evals entries this session.
- [ ] `DECISIONS.md`: one new line at the true chronological end (re-check `tail` immediately before appending) naming the `run_id`/`eval_case_id` resolution, the deliberate divergence from promptfoo/Inspect AI/DeepEval/Braintrust's embed-on-run precedent (and why), and the `results_store.py` relocation.
- [ ] `docs/agents/LOGS.md`: new entry (Files touched / Intent-summary / Decisions made / Verification performed / Bugs found / Next steps).
- [ ] `STATUS.md`: update Current Phase / Last Completed Task / Next Action / Verification State.
- [ ] Full `make verify` from repo root — must pass clean before commit (`gateway` untouched by this pass).

**Verify:** `make verify` (root) passes end-to-end; `git diff` reviewed in full before committing.

## Scope Gate

This is architecturally scoped (new data model + a module relocation + a breaking CLI change across two commands), correctly warranting a plan file, not a one-line `DECISIONS.md` entry.
