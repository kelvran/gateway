- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: project founder + Claude Code

## Summary

Build a `Score` model and JSONL persistence for `evals/` — the last remaining unbuilt entity in `evals/ARCHITECTURE.md`'s Data Model sketch — and wire it into all four scoring call sites (`evals run`/`evals rollout`, each with a deterministic and an `--llm-judge` path) so a scoring decision is a durable, queryable record instead of a `click.echo` line that vanishes after the process exits. This closes the single most acute instance of the gap: `judge()` already computes a full `rationale`+`bias_mitigations_applied` pair on every successful call, and `evals/evals/cli.py`'s `_judge_output` (line 115) throws both away, returning only a bare `bool`.

## Motivation

Confirmed directly against the live tree, not assumed: `evals/ARCHITECTURE.md`'s Data Model sketch names `Score { run_id, scorer_id, scorer_type, value, rationale, rubric_axis, bias_mitigations_applied }` and, as of the last two RFCs shipped this session, is the only one of the five sketched entities (`EvalCase`/`Run`/`Trace`/`Span`/`Score`) still fully unbuilt — `EvalCase` and `Run` are both real. `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`'s own Alternatives Considered #3 named this exact gap as "a clean, independently-scoped, zero-infra-dependent next feature... the natural next-next pick," deliberately not bundled into that RFC to keep its blast radius single-purpose.

Traced precisely where a score value exists today and what happens to it: in all four call sites (`run_cmd`/`rollout_cmd` × deterministic/`--llm-judge`), a scoring decision is computed, printed via `click.echo`, folded into the Wilson-CI counters, and then discarded. The most acute instance is `cli.py:115`, inside `_judge_output`: `judge()` returns a fully-populated `JudgeResult` (`passed`, `rationale`, `bias_mitigations_applied`), and the very next line does `return result.passed` — the reasoning text and bias-mitigation record that `THREAT_MODEL.md`'s own Tampering row cites as this project's judge-manipulation defense are computed and then thrown away, every single call. Separately, `Run`s are already persisted (`results_store.append_runs`, called *before* scoring even starts) but the `Run` model has no score-shaped field at all — even the surviving `bool` from a deterministic score never reaches any file, only stdout.

Grounded via a dynamic-workflow research pass (task `wfsu61qoc`, run `wf_2b122eb6-ca4`): a code-audit angle confirming the exact discard point and per-path data availability, a design-proposal angle that resolved the one genuinely hard design question (what does `run_id` mean when `evals run` never produces a `Run`?) with real reasoning rather than punting, and a precedent angle surveying promptfoo/Inspect AI/DeepEval/Braintrust's real score-record shapes. The synthesis independently re-verified every load-bearing citation directly against the live repo before trusting it.

## Detailed Design

### The decisive design question: what is `run_id` when there's no `Run`?

`evals run` never touches the Rollout Scheduler — `task_spec.output` is baked into the suite file, not produced by execution — so it never has a real `Run` object, only `evals rollout` does. Two tempting-but-wrong answers were explicitly considered and rejected:

1. **Let `Score.run_id` hold an `EvalCase.id` when there's no real `Run`.** Rejected: `run_id` is a typed pointer into the Results Store's `Run.id` namespace. Repointing it at a different entity's identity on the one path that lacks a `Run` is exactly the identity-spoofing failure class `THREAT_MODEL.md`'s Evals Spoofing row already names for this component ("a rollout claims to be a golden/regression-tier run when it isn't... corrupting the regression dataset"). A field's name is a promise to every future reader that joins on it; breaking that promise for one caller's convenience is worse than the field being legitimately absent.
2. **Fabricate a synthetic `Run` for `evals run` so a real `run_id` exists.** Rejected, more decisively: `Run.harness_config`/`latency_ms` are documented as the literal sandbox invocation that actually happened. `evals run` never runs a sandbox at all. Manufacturing a `Run` to satisfy a schema constraint is the exact "fabricated placeholder" pattern this codebase has already rejected once (`Run.cost_usd: None`, never `0.0`) — repeating it here would be the same mistake in a new place.

**Resolution**: `Score.run_id: str | None = None` — a real `Run.id` when one exists (`evals rollout`), `None` when it doesn't (`evals run`), mirroring `Run.cost_usd`'s own "`None` means genuinely not applicable, never a fabricated stand-in" convention exactly. A new field not in `ARCHITECTURE.md`'s original sketch, `eval_case_id: str`, becomes the join key both commands can *always* honestly supply — `case.id` is in scope at every `Score` construction site regardless of path. This is a deliberate, named departure from the bare sketch, the same class of departure `Run` itself already made (narrower `harness_config`, `cost_usd: None`) and explicitly justified rather than glossed over.

### A second, related divergence found by precedent research — named, not silently adopted

The precedent angle surveyed how promptfoo, Inspect AI, DeepEval, and Braintrust actually structure a score/judgment record, and found: **none of them use a separate, normalized `Score` entity joined by `(run_id, scorer_id)` foreign keys.** All four embed scores as a name-keyed map or list directly on the single per-run/per-sample record (promptfoo's `namedScores`/`gradingResult` JSON columns on one `eval_results` row; Inspect AI's `scores: dict[str, Score]` on one `EvalSampleSummary`; DeepEval's `metrics_data: list[MetricData]` on one `TestResult`; Braintrust's `scores: Mapping[str, float]` on one `EvalResult`). `evals/ARCHITECTURE.md`'s own `Score{run_id, scorer_id, ...}` sketch — a first-class entity with its own FK — has no direct precedent among any of them.

This RFC keeps the sketch's normalized-entity shape anyway, for a reason specific to Kelvran and absent in every framework studied: **all four surveyed frameworks always have exactly one atomic run/sample object to embed a score onto.** Kelvran does not — `evals run` and `evals rollout` are two different commands with two different atomic units (a bare `EvalCase` vs. a real `Run`), and embedding scores onto `Run` the way the precedent does would leave `evals run` with nowhere to attach a score at all, reopening the exact §-above design question in a different shape. A standalone `Score` entity, joined by the always-real `eval_case_id` and an optional `run_id`, is the one shape that honestly serves both commands. Named explicitly as a deliberate divergence from studied precedent, not "the way everyone does it" — the same discipline this project applies whenever research and Kelvran's actual shape disagree (Cache L2's narrowed normalization allowlist, Guardrails' NER exclusion).

One second-order finding also corrected here: `evals/evals/rollout/results_store.py`'s own docstring currently attributes "filesystem-only, no DB/MQ" to *both* Inspect AI and promptfoo — the precedent research found promptfoo's real mechanism is an embedded SQLite database (via `better-sqlite3`/Drizzle), not flat files; a serverless embedded DB, correctly "no separate service to stand up," but not "no DB" as the docstring currently implies. Corrected in the moved module (see below).

### `Score` model (`evals/evals/models.py`, new, alongside `EvalCase`/`Run`)

```python
ScorerType = Literal["deterministic", "llm_judge"]


class Score(BaseModel):
    model_config = ConfigDict(frozen=True)

    eval_case_id: str
    eval_case_revision: int
    run_id: str | None = None
    scorer_id: str
    scorer_type: ScorerType
    value: bool
    rationale: str | None = None
    rubric_axis: str | None = None
    bias_mitigations_applied: list[str] = Field(default_factory=list)
```

| Field | Populated with | Why |
|---|---|---|
| `eval_case_id`/`eval_case_revision` | `case.id`/`case.revision` | Not in the original sketch. The universal join key — always real in both commands, per the design question above. |
| `run_id` | `run.id` (rollout) / `None` (run) | See design question above. Never a fabricated stand-in. |
| `scorer_id` | `"exact_match"`/`"regex_match"` (deterministic, from the same `task_spec.get("match", "exact")` value already branched on); the pinned judge model id (`providers.DEFAULT_JUDGE_MODEL`) for `llm_judge` | Names *which* scorer ran, per `ARCHITECTURE.md`'s Harness-Transparency requirement ("never left as ambient config") — both values are already real and in-hand at the call site. |
| `scorer_type` | `Literal["deterministic", "llm_judge"]` | Narrowed from the sketch's `deterministic\|llm_judge\|skeptic_panel\|human` to only the two values this codebase actually produces — same discipline as `RunStatus`'s real-values-only `Literal`. |
| `value` | `bool` | Every scorer in this codebase (`exact_match`, `regex_match`, `JudgeResult.passed`) produces `bool` only; no partial credit exists to justify `float`. |
| `rationale` | `JudgeResult.rationale` (llm_judge) / `None` (deterministic) | Deterministic matching has no explanatory text — `None`, not a fabricated empty string. |
| `bias_mitigations_applied` | `JudgeResult.bias_mitigations_applied` (llm_judge) / `[]` (deterministic) | Bias mitigation is a judge-specific concept; `[]` matches `EvalCase.tags`'s existing empty-list-default convention. |
| `rubric_axis` | always `None` in v1 | `judge()` produces one holistic verdict, never a per-axis breakdown — nothing to populate this with. Left in the model rather than dropped, matching how `Trace`/`Span` stay named-but-unbuilt rather than vanishing from the doc. |

Deliberately excluded: a `Score.id` primary key. Nothing in this codebase addresses an individual `Score` by id yet (no dashboard, no comparison view) — adding one now would be a speculative field this project's YAGNI convention rejects. Named as considered-and-rejected, not a silent omission.

### Persistence: move + generalize `results_store.py`, don't duplicate it

`evals/evals/rollout/results_store.py` lives under `rollout/` and is typed to `Run` today — but `evals run` is not a rollout command (no sandbox, no scheduler), so importing from `evals.rollout.results_store` there would be a real layering violation in a repo that otherwise polices dependency direction explicitly. The mechanism itself was never `Run`-specific; only its first caller was. **Decision: move it to `evals/evals/results_store.py` (sibling to `models.py`), generalized over any frozen pydantic model, keeping `append_runs`/`load_runs` as thin, behavior-identical wrappers.** Since `kelvran-evals` has never published to PyPI (publish is still blocked on trademark clearance, per `STATUS.md`), there are no external consumers of the old import path to preserve compatibility for — a clean move, not a deprecation.

```python
# evals/evals/results_store.py — moved from evals/evals/rollout/results_store.py.
# The append/load mechanism was never rollout-specific; Run was just its
# first caller. evals run (no rollout, no sandbox) is a second, equally
# valid caller for Score, so a module namespaced under rollout/ was no
# longer an honest home for it.
#
# No new infrastructure — a flat, append-only JSONL file, matching Inspect
# AI's own real precedent (run and score data both live in one flat log
# file by default, no DB). promptfoo's own persistence is a serverless
# *embedded SQLite* database (via better-sqlite3/Drizzle), not flat
# files — a real, corrected distinction from this module's prior docstring,
# which had attributed "filesystem-only, no DB" to promptfoo as well.

ModelT = TypeVar("ModelT", bound=BaseModel)

def _append_models(models: list[ModelT], path: Path) -> None: ...
def _load_models(model_cls: type[ModelT], path: Path) -> list[ModelT]: ...

def append_runs(runs: list[Run], path: Path) -> None: ...
def load_runs(path: Path) -> list[Run]: ...
def append_scores(scores: list[Score], path: Path) -> None: ...
def load_scores(path: Path) -> list[Score]: ...
```

### `scorer_id` derivation, and one public-rename prerequisite

- **Deterministic**: a new, tiny `_deterministic_scorer_id(case: EvalCase) -> str` helper in `cli.py`, deriving from the exact `task_spec.get("match", "exact")` value `_score_output_deterministic` already branches on.
- **LLM-judge**: `evals/evals/judge/providers.py`'s `_DEFAULT_JUDGE_MODEL` constant is renamed to `DEFAULT_JUDGE_MODEL` (public) — confirmed via grep this is a zero-risk rename with no other references anywhere in the tree — so `cli.py` can cite the real pinned model id as `scorer_id`, per `ARCHITECTURE.md`'s harness-transparency requirement.

### CLI wiring (`evals/evals/cli.py`)

`_judge_output`/`_judge_case`/`_judge_run`'s return type widens from `bool | None` to `JudgeResult | None` — the only way `rationale`/`bias_mitigations_applied` survive past the point they're computed, with zero change to `judge()`/`llm_judge.py` itself and zero observable change to console output (callers now read `.passed` off the returned object instead of the bool directly; every existing `click.echo` call and its ordering is unchanged).

Both `run_cmd` and `rollout_cmd` gain a new, **required** `--scores <path>` option, placed identically to `rollout_cmd`'s existing `--results <path>` convention (a plain, caller-chosen append target, no default, no rotation policy). Both commands accumulate a `list[Score]` across their scoring loop and call `append_scores(scores, scores_path)` once after the loop — mirroring `append_runs`'s existing "batch write once" call site.

**The one rule, applied identically at all four call sites**: a `JUDGE_ERROR` case (the judge call itself failed) and a non-`"completed"` `Run` (timed out/errored — nothing was ever produced to score) both produce **no `Score` at all**, exactly mirroring the existing `continue`-before-counting control flow. There is no honest `bool` to record for either case.

### Drawbacks

- **Breaking CLI change**: `--scores` is required on both commands, mirroring `--results`'s existing required convention — every existing script or test invocation of `evals run`/`evals rollout` must add it. Chosen deliberately over an optional-with-a-default-path alternative, for consistency with the one precedent this repo already set (`--results`); accepted as the cost of that consistency, not glossed over.
- `_judge_output`/`_judge_case`/`_judge_run`'s return-type widening is a real internal signature change, though zero observable-behavior change, covered by the existing test suite's stdout assertions.
- A `JUDGE_ERROR` outcome is visible only in console output, never in the Score store — there is currently no way to later audit "how often did the judge itself fail" from the Scores file alone. Left open deliberately rather than widening `Score.value` to `bool | None`, since nothing today needs that query and it would blur "a judged verdict" with "an unjudged case" in the same field.
- The standalone `Score` entity is a deliberate divergence from every framework surveyed (see Detailed Design) — a real, if small, maintenance cost (one more file format to keep in sync with `Run`/`EvalCase`) accepted because Kelvran's two-command, two-atomic-unit shape doesn't fit the embed-on-run pattern those frameworks share.

### Alternatives Considered

1. **`run_id: str` required, with a synthetic `Run` fabricated for `evals run`** — rejected; fabricates required `Run` fields with no real sandbox invocation behind them.
2. **`Score.run_id` doubles as a generic "subject id"** (holds either a `Run.id` or an `EvalCase.id`) — rejected; breaks the field's single meaning and risks a silent mis-join if the two id-namespaces ever collide.
3. **Embed scores directly on `Run` as a dict/list** (matching promptfoo/Inspect AI/DeepEval/Braintrust's real precedent) — rejected for the reason named in Detailed Design: `evals run` has no `Run` to embed anything onto, and every framework studied always has exactly one atomic run/sample object, which Kelvran does not.
4. **A dedicated `evals/scores/store.py` module, separate from the moved/generalized `results_store.py`** — rejected; the JSONL append/load mechanism has nothing `Run`-specific about it, so a second near-duplicate file would violate this repo's own DRY convention for zero benefit.
5. **Track judge-call cost on `Score` now** (closing the `Run.cost_usd: None` gap the LLM-judge-wiring RFC's own Drawbacks named) — rejected for this pass; `providers.py`'s `call_model` closure doesn't currently surface token/cost data to thread through — a real follow-on, not bundled here.
6. **Leave `results_store.py` under `rollout/` and accept the layering smell for `evals run`'s import** — rejected; the move is a zero-risk, behavior-preserving refactor (no PyPI-published external consumers of the old path exist yet) that fixes a real, growing architectural smell before it calcifies further.

## Unresolved Questions

- Whether `rubric_axis` ever becomes real depends on a future multi-axis judge prompt in `judge()` — not designed here.
- Whether a `Score` row should ever exist for `JUDGE_ERROR`/non-completed-`Run` outcomes (via a `bool | None` widening of `value`) — no current consumer needs this; left open.
- A query/report command over persisted `Score`s (`evals report --scores ...` or similar) — no consumer needs this yet; `report_cmd` stays a bare-counts formatter, named as a real, deliberate gap.
- File-naming/retention convention for `--scores` is left to the operator, identical to `--results`'s existing no-rotation posture.

## Research Trail

Grounded via a dynamic-workflow research pass (task `wfsu61qoc` / run `wf_2b122eb6-ca4`): a code-audit angle (tracing all four scoring call chains, confirming the exact `cli.py:115` discard point and per-path data availability), a design-proposal angle (resolving the `run_id`-without-a-`Run` question decisively, with the exact model shape and CLI wiring), and a precedent angle (promptfoo/Inspect AI/DeepEval/Braintrust's real score-record shapes, pulled live from each project's source, not from memory). The synthesis independently re-verified every load-bearing citation against the live repo — the exact discard line, `Run`'s real field shape, `ARCHITECTURE.md`'s exact sketch wording, and the LLM-judge-wiring RFC's own "next-next pick" framing — before trusting any of it, and made the final call on the one item the design-proposal had explicitly left open (whether to relocate `results_store.py`).
