# RFC: Three CI-gate refinements for `evals report` — tier default, category gate, flaky exclusion

## Status

Accepted, implemented 2026-09-07.

## Context

`docs/rfcs/2026-09-05-evals-report-fail-under.md` shipped the `--fail-under` mechanism itself. `DECISIONS.md`'s 2026-09-06 Phase 1e entry wired it into `.github/workflows/ci.yml`, but **deliberately scoped as a dogfooding smoke test only** — it runs the deterministic scorer against the tiny, untiered `tests/fixtures/golden_example.json` fixture, with no tier filtering, no per-category gating, and no flaky-tolerance mechanism. `docs/upgrade-research/evals-2026-09-06.md`'s Finding 1 names all three of the following as cheap, concretely-adoptable follow-ons, each with real prior art (promptfoo's keyword-scoped critical-failure gate, DeepEval's `--official` regression-baseline convention and `flaky=True` escape hatch):

1. Default the CI gate to a specific tier (`regression`), not whatever fixture set happens to be passed with no filtering.
2. A category-scoped critical gate: cases tagged with a safety-relevant category can be held to their own, stricter (or looser) `--fail-under` bar, independent of the aggregate one.
3. A `flaky` tolerance flag so a known-noisy case runs and is reported, but never trips the gate on a false regression signal.

This RFC treats all three as one coherent design pass, because all three touch the same two things: `report_cmd`'s gate-computation code path, and the `EvalCase`/`Score` schema in `evals/models.py`. Splitting them into three uncoordinated patches would risk three different, incompatible answers to the same underlying question ("what subset of `Score`s does a given gate check run against, and using which counts?").

### What already exists (verified directly, not assumed)

- `EvalCase` (`evals/models.py`) already has `tier: EvalTier` (`Literal["golden", "regression", "drift_sample"]`) and `tags: list[str]` — both from the golden/regression/drift_sample promotion work (`docs/rfcs/2026-09-05-evals-golden-regression-promotion.md`). Neither needs to be invented.
- `promote_cmd` already has a `--tier` option (`click.Choice(["regression", "drift_sample"])`, deliberately excluding `"golden"` per `THREAT_MODEL.md`'s Evals Spoofing row). **`report_cmd` has no `--tier` option at all** — it only accepts `--successes`/`--total`, `--scores`, or `--traces`, with zero case-classification filtering in any of the three modes. This is the exact gap Finding 1 names.
- `Score` (`evals/models.py`) has no `tier`, `tags`, or `flaky` field — it only carries `eval_case_id`/`eval_case_revision` as its join key back to the originating `EvalCase`. `report_cmd`'s `--scores` mode never re-reads the originating `--suite` file, so today there is no way for it to know a given persisted `Score`'s case classification at all.
- `.github/workflows/ci.yml`'s `evals` job runs `evals run --suite tests/fixtures/golden_example.json --scores .ci-smoke-scores.jsonl` then `evals report --scores .ci-smoke-scores.jsonl --fail-under 0.1` — every case in `golden_example.json` is `tier="golden"`, untagged, non-flaky.

## Design

### 1. Denormalize `tier`, `tags`, and `flaky` onto `Score` at creation time, instead of joining back to a `--suite` file in `report_cmd`

**Decision:** add `tier: EvalTier | None = None`, `tags: list[str] = Field(default_factory=list)`, and `flaky: bool = False` to `Score`. `run_cmd` and `rollout_cmd` populate all three from the originating `EvalCase` (`case.tier`, `list(case.tags)`, `case.flaky`) at the exact point each `Score` is constructed — the `case` object is already in scope at every one of the four call sites (two in `run_cmd`, two in `rollout_cmd`'s `_score_and_record`).

**Alternative considered and rejected:** give `report_cmd` a new `--suite <path>` option and join `Score.eval_case_id`/`eval_case_revision` back against the suite file to look up tier/tags at report time. Rejected because: (a) it makes `report_cmd`'s three input modes asymmetric for no benefit — `--scores`/`--traces`/`--successes`+`--total` are deliberately mutually exclusive, self-contained inputs today, and bolting an optional fourth "context" file onto only one of them breaks that symmetry; (b) a `--scores` file can already outlive or diverge from the `--suite` file it was generated against (a case can be edited or removed from the suite after scoring), so a join-at-report-time design has a real staleness/silent-mismatch failure mode a denormalize-at-write-time design does not; (c) this codebase already has the identical precedent for exactly this call — `Run.cache_key`/`Score.score_cache_key` are computed once, at creation time, "regardless of whether caching is active," specifically so a later invocation can use them without redoing the original computation. Denormalizing `tier`/`tags`/`flaky` the same way is the same tradeoff, not a new one.

**Backward compatibility:** all three new fields are optional with defaults (`None`, `[]`, `False`), so a pre-existing persisted `Score` JSONL line (with none of the three fields) still `model_validate_json`s cleanly — the same proof pattern `test_old_shape_run_json_line_still_validates_with_new_fields_defaulted` already established for `Run`. A `Score` built from the raw `--successes`/`--total` report mode (no `EvalCase` ever exists in that path) is never affected — that mode still takes plain integers, no `Score` object is ever constructed.

**`flaky` lives on `EvalCase`, not `Run`** (the explicit design question this RFC's brief poses). Justification: flaky-ness, in DeepEval's own `LLMTestCase(flaky=True)` precedent this research cites, is a *declared, durable property of the test case itself* ("this case is known to be environmentally noisy"), set once by a human curator, not a property of one particular execution attempt. Putting it on `Run` would mean re-declaring it on every single trial of every rerun, and would give it a materially different lifetime than `tier`/`tags` (case-level, revision-scoped) for no real gain — nothing about *why* a case is flaky changes trial-to-trial. `EvalCase.flaky: bool = False` mirrors `tier` and `tags`'s own field shape and lifetime exactly: declared once at authoring/promotion time, immutable per `EvalCase`'s `frozen=True` convention, and denormalized onto every `Score` the case produces via the same mechanism as `tier`/`tags` above.

**`promote_cmd` copies `original_case.flaky` verbatim onto the new, promoted `EvalCase`** — the same "frozen at the exact point the Run used" precedent `task_spec`/`reference` already follow. No new `--flaky` CLI flag on `promote_cmd`: if a curator wants to mark a freshly-promoted case flaky (or un-flaky) independent of its origin, the resulting suite JSON file is already documented as "meant to stay small and human-reviewable" (`_append_cases_to_suite`'s own docstring) — hand-editing one boolean field there is simpler and safer than growing `promote_cmd`'s own option surface for a rare case, per this project's YAGNI convention.

### 2. Category tagging reuses `EvalCase.tags` — no new field, no required naming convention enforced in code

**Decision:** a "category" is just any string already present in `EvalCase.tags` (and therefore denormalized onto `Score.tags`, per §1). `report_cmd` gains a repeatable option:

```
--category-fail-under TAG:THRESHOLD
```

e.g. `--category-fail-under "category:safety:0.95"`. Each occurrence is parsed into a `(tag, threshold)` pair by `evals.cli._parse_category_fail_under`, using `str.rpartition(":")` (the threshold is always the rightmost `:`-delimited segment, so a tag itself may contain colons). Malformed entries (no colon, non-float threshold) and a tag repeated across more than one `--category-fail-under` occurrence both raise a `click.UsageError` immediately — never a silently-ambiguous "last one wins."

**Naming convention, not enforcement:** this RFC recommends (in CLI help text and the new CI fixture below) prefixing a category tag with `category:` (e.g. `category:safety`) purely so a human scanning `EvalCase.tags` can tell a category label apart from bookkeeping tags like `promoted-from:...`. The mechanism itself does not require or check for this prefix — any literal tag string is a valid `--category-fail-under` argument, mirroring promptfoo's own plain-substring keyword matching rather than inventing a schema-enforced taxonomy this project has no evidence it needs yet (YAGNI).

**Never blends `scorer_type`, even within a category — consistent with the pre-existing hard rule.** `report_cmd`'s docstring and its 2026-09-05 RFC both state, as a hard invariant, that `deterministic` and `llm_judge` scores are never averaged into one number. A category gate could have taken a shortcut and pooled every tagged `Score` regardless of `scorer_type` — rejected, because it would silently violate that invariant for exactly the class of case (a safety-critical category) where quietly blending two different measurement instruments together is least acceptable. Instead, category matching happens **inside** the existing `for scorer_type in sorted(...)` loop: for each `scorer_type` group already computed for the aggregate line, `report_cmd` additionally computes and prints a `scorer_type [category:TAG]` sub-line and (if the group has any eligible `Score`s) an independent gate check, for every configured category tag that has at least one match in that group.

**Category gate applies within whatever `--tier` filter is also active, never across the full untiered file.** This is the explicit interaction-semantics question this RFC's brief poses. Decision: `--tier`, when given, filters the working `Score` set once, up front; every downstream computation — the aggregate per-`scorer_type` line, the aggregate `--fail-under` gate, and every `--category-fail-under` gate — operates on that already-narrowed set, never on the full file. Justification: an operator invoking `evals report --scores out.jsonl --tier regression --category-fail-under category:safety:0.9` is asking one coherent question ("within the regression tier, does the aggregate clear the bar, and does the safety category clear its own bar") — silently letting the category gate reach outside the tier the rest of the command is scoped to would mean the printed aggregate line and the category gate line describe two different universes without saying so, which is a real, confusing footgun for no benefit. `--category-fail-under` may be used with or without `--tier`; it is not required to co-occur with `--fail-under` either — a category gate is independently useful even when no aggregate bar is set at all (see the RFC brief's own "in addition to" framing: addition implies the aggregate gate is optional context, not a hard co-requirement).

**A `--category-fail-under` tag with zero matches (within the active `--tier` scope, across every `scorer_type` group) is a hard `click.ClickException`, not a silent no-op.** Rationale: a typo'd or stale category tag would otherwise degrade into an always-passing gate with no signal that it's not actually checking anything — the same "explicit, never silent" discipline `_parse_judge_axes` already applies to an empty `--judge-axes` value.

**Scope: `--scores` mode only**, same as `--tier` (see §3 below for the shared reasoning) — both flags raise `click.UsageError` if given together with `--traces` or bare `--successes`/`--total`.

### 3. `--tier` on `report_cmd`, scoped to `--scores` mode only

**Decision:** add `--tier {golden,regression,drift_sample}` to `report_cmd`. When given, `evals.cli._filter_scores_by_tier` filters the loaded `Score` list down to `s.tier == tier` before any grouping happens. All three tiers are valid choices here (unlike `promote_cmd`'s deliberate exclusion of `golden` — that restriction is about *what a rollout can claim to promote itself into*, an entirely different concern than *what an operator can choose to report on*). If a suite were somehow scored entirely outside the given tier (or scored with `Score`s persisted before this RFC, so `tier=None` on every record), filtering yields an empty list — this is a distinct `click.ClickException` (`"{scores_path}: no Scores found for tier={tier!r}"`) from the pre-existing "no Scores found at all" case, so an operator can tell "the file is empty" apart from "the file has data, none of it in the tier you asked for."

**Scoped to `--scores` mode only, not `--traces` or raw `--successes`/`--total`.** `Span` has no case-classification field at all — it joins only to `Run.id`, and `Run` has no `tier` (adding one would mean plumbing tier through `Run` too, a real but separate, unrequested widening of scope beyond what this research finding names). Raw counts mode has no `EvalCase` in the picture whatsoever — there is nothing to filter. Both are a real, narrower, defensible scope limit, following this codebase's own "don't build the diagram-only box" discipline (the same discipline `evals/ARCHITECTURE.md` already applies to `Trace`/`Sandbox Pool`).

**CI wiring:** `.github/workflows/ci.yml`'s `evals` job now runs its smoke-test suite through `--tier regression` explicitly, and against a new fixture, `tests/fixtures/ci_gate_example.json`, built specifically to exercise all three mechanisms in one real run (not just accept the flags) — see Verification below for the exact numbers and why this fixture's specific composition is load-bearing, not arbitrary.

### 4. Flaky exclusion: excluded from every gate computation, included in every printed line

**Decision:** a `flaky=True` `Score` (denormalized from `EvalCase.flaky`, per §1) is dropped by `evals.cli._gate_eligible` before any Wilson-lower-bound gate check — both the aggregate `--fail-under` gate and every `--category-fail-under` gate, uniformly. It is never dropped from the **printed** report: the pass-rate/CI line for a `scorer_type` group (and for a `scorer_type [category:TAG]` sub-line) is computed over every `Score` in that group, flaky or not — exactly as today, when `flaky` didn't exist. When a group contains at least one flaky `Score`, its printed line gains a trailing ` (N flaky excluded from gate)` note, so an operator sees a real reason the printed denominator and the gate's own denominator differ, rather than being left to guess.

**Explicit answer to "does a flaky case still count toward a category gate?" — No, uniformly.** A flaky-tagged case is excluded from *every* gate computation it would otherwise be part of, aggregate or category, with no special case for the category path. There is exactly one exclusion rule, applied identically everywhere `Score`s feed a `--fail-under`/`--category-fail-under` decision — a flaky exception that applied to the aggregate gate but not the category gate (or vice versa) would be a second, harder-to-remember rule for no stated benefit, and nothing in DeepEval's own `flaky=True` precedent (a case-level, not gate-level, exemption) suggests the exclusion should be scoped any narrower than "every strict pass-rate gate this case would otherwise count toward."

**If every `Score` in a would-be gate group is flaky**, that specific gate check is skipped entirely (neither counted as an automatic pass nor a failure) — there is nothing left to compute a Wilson bound over (`wilson_interval` requires `total > 0`), and silently treating "no real signal" as "passing" would be worse than the alternative of simply not emitting that one gate check. This is a genuine, narrow edge case, called out explicitly rather than left to an accidental `ValueError`.

## Alternatives considered

**A new `--suite` join for tier/category, instead of denormalizing onto `Score`.** Rejected in §1 above — the write-time-denormalization precedent (`cache_key`, `score_cache_key`) already established in this codebase is a closer fit and avoids a staleness failure mode.

**Encoding `flaky` as a magic tag inside `EvalCase.tags` (e.g. `"flaky"`), instead of a dedicated field.** Rejected: `flaky` is a boolean gating property with a fixed, well-defined meaning to `report_cmd` itself, categorically different from a `tags` entry, which is an open-ended, operator-chosen label whose meaning to `report_cmd` is only ever "did the operator ask to gate on this string." Overloading one list with two different semantics (arbitrary category labels + one magic reserved string that changes gate behavior) is a real footgun (a category literally named `"flaky"` would silently change behavior) for no benefit over a dedicated, type-checked `bool` field. DeepEval's own precedent is a dedicated field (`LLMTestCase(flaky=True)`), not a tag string, which this design follows directly.

**A per-category gate that also blends `scorer_type`.** Rejected in §2 — would silently violate the pre-existing "never blend scorer_types" hard invariant for exactly the highest-stakes case (a safety category).

**A category gate scoped across every tier, ignoring `--tier`.** Rejected in §2 — would let the printed aggregate line and a category gate line silently describe two different universes in the same invocation.

**A new `evals.stats` entry point for a "per-category Wilson bound."** Considered and explicitly rejected: `evals.stats.wilson_interval` already takes plain `(successes, total)` integers and needs no change to serve a category group — `report_cmd` already computes `(successes, total)` for an arbitrary named group today (the pre-existing `gate_checks: list[tuple[str, int, int]]` mechanism, one entry per `scorer_type`), and a category group is just one more differently-filtered set of `Score`s reduced to the same `(successes, total)` shape before the identical `wilson_interval` call. Adding a new `stats.py` function would duplicate, not reuse, the existing math — and `stats.py` sits in `evals`' bottom import-linter layer specifically so it stays free of any `EvalCase`/`Score` (`evals.models`) dependency; a category-aware helper would either have to take raw counts anyway (making it a redundant thin wrapper) or import `evals.models` (which the `[importlinter:contract:layers]` contract in `.importlinter` already forbids — `evals.models` and `evals.stats` are declared as same-layer siblings, and the "layers" contract type keeps siblings independent of each other). `evals/evals/stats.py` is therefore **unchanged** by this RFC.

## Interaction semantics — summary table

| Question | Answer |
|---|---|
| Does `--tier` apply to `--traces` or raw counts mode? | No — `click.UsageError` if combined; `--scores` only. |
| Does `--category-fail-under` apply to `--traces` or raw counts mode? | No — same restriction, same reason. |
| Does a category gate apply only within the `--tier` filter, or across all tiers? | Only within the active `--tier` filter (or the whole file, untiered, if `--tier` is omitted). |
| Does a category gate blend `deterministic` and `llm_judge` scores together? | No — computed independently per `scorer_type`, exactly like the aggregate gate. |
| Is `--category-fail-under` required to co-occur with `--fail-under`? | No — independently useful; both are checked whenever given, neither requires the other. |
| Does a flaky case still count toward a category gate? | No — flaky exclusion is uniform across the aggregate gate and every category gate. |
| Is a flaky case's result still visible in the printed report? | Yes — every printed pass-rate/CI line includes flaky `Score`s; only the gate computation excludes them, with a `(N flaky excluded from gate)` note when it does. |
| Where does `flaky` live — `EvalCase` or `Run`? | `EvalCase` — a durable, case-level property, denormalized onto every `Score` the case produces, mirroring `tier`/`tags`. |
| Does `promote_cmd` carry `flaky` forward? | Yes, copied verbatim from the original case; no new CLI flag. |
| What happens if a category tag matches zero `Score`s? | `click.ClickException` — never a silent no-op. |
| What happens if every `Score` in a gate group is flaky? | That specific gate check is skipped (neither pass nor fail) — nothing to compute a bound over. |

## Verification

`evals/tests/test_models.py`: new field-default/backward-compat tests for `EvalCase.flaky` and `Score.tier`/`Score.tags`/`Score.flaky`.

`evals/tests/test_report_gate_helpers.py` (new): pure unit tests, no `CliRunner`, for `_filter_scores_by_tier`, `_gate_eligible`, and `_parse_category_fail_under` in isolation — malformed input, duplicate tags, tier=`None` no-op passthrough.

`evals/tests/test_cigate_integration.py` (new — `test_cli_integration.py` is already at 1500 lines, well past this project's own established 800-line split precedent that produced `test_promote_integration.py`): `CliRunner`-driven integration tests covering: `run_cmd`/`rollout_cmd` denormalizing `tier`/`tags`/`flaky` from `EvalCase` onto every persisted `Score`; `--tier regression` filtering out non-matching `Score`s from both the printed report and the gate; a category gate failing its own stricter threshold while the aggregate `--fail-under` still passes; a category gate passing on its own while a low aggregate pass rate is never even checked because `--fail-under` was omitted (proving independence in both directions); an unmatched category tag raising `click.ClickException`; a malformed `--category-fail-under` raising `click.UsageError`; a flaky case that fails but is excluded from both the aggregate and a category gate while still appearing in the printed PASS/FAIL line and the printed denominator.

`evals/tests/test_promote_integration.py`: one new test proving a promoted `EvalCase` inherits `flaky=True` from its original case.

**Real CI-equivalent run, not just unit assertions** (per this project's own established `--fail-under` proof precedent): new fixture `tests/fixtures/ci_gate_example.json` — 4 `tier="regression"` cases (2 plain PASS, 1 tagged `category:safety` and PASS, 1 `flaky=true` and deliberately WRONG) plus 1 `tier="golden"` case, also deliberately WRONG, that must be excluded entirely by `--tier regression`. Computed directly (not guessed) via `evals.stats.wilson_interval`:

- Without `--tier` filtering and without flaky exclusion (i.e. if either mechanism were silently broken): the deterministic group is 3 successes / 4 non-flaky-or-untiered-filtered trials → Wilson lower bound **0.3006**.
- With both mechanisms correctly applied (regression tier only, flaky case excluded from the gate): 3 successes / 3 eligible trials → Wilson lower bound **0.4385**.
- CI's `--fail-under 0.35` sits strictly between those two numbers — the same "load-bearing, not arbitrary" threshold-selection technique the original `--fail-under` RFC used for its own 8/10 point-estimate-vs-lower-bound proof. A regression in *either* the tier filter or the flaky exclusion collapses the eligible set from 3/3 to 3/4 and flips this specific CI step from green to red.
- The `category:safety`-tagged case is 1/1 → Wilson lower bound **0.2065**; CI's `--category-fail-under "category:safety:0.15"` sits below it with real margin.
- Manually run, both directions, before committing (mirroring the original RFC's own sanity-check-by-breaking discipline): the real fixture at the real CI thresholds exits 0; a deliberately-broken variant of each of the three mechanisms (tier filter, category gate, flaky exclusion) each independently produces a real, correctly-attributed nonzero exit, then every temporary change was reverted with `git diff` confirmed empty. Exact commands and output are recorded in this change's own session log entry rather than duplicated here.

`evals/ARCHITECTURE.md`'s `CI/CD Gate` paragraph is updated to describe `--tier`/`--category-fail-under`/flaky-exclusion, and its stale "not yet wired into this repo's own CI" clause (superseded by `DECISIONS.md`'s 2026-09-06 Phase 1e entry, which this pass discovered was never reflected back into `ARCHITECTURE.md`) is corrected in the same edit rather than left to recur a 7th time as this project's own documented "Doc-vs-code staleness" gotcha.
