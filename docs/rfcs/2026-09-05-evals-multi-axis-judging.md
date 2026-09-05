# RFC: Multi-axis LLM-judge scoring

## Status

Accepted, implemented 2026-09-05.

## Context

`evals.judge.llm_judge.judge()` produces exactly one holistic PASS/FAIL verdict per case — a candidate output either "matches the reference" or it doesn't, with no way to distinguish *why* it failed (wrong facts vs. an unsafe response vs. bad tone, for example). This RFC was scoped via an explicit brainstorming check-in (`AskUserQuestion`) rather than a unilateral default, since it's a genuine product-shape decision, not a bug fix: the call-structure options were (a) one LLM call per axis, N calls at N× the judge cost, or (b) one call asking the judge to return a structured multi-axis JSON verdict, 1× the cost but coupling every axis's verdict to a single response the judge might get partially wrong or malform. **Given (a): one call per axis** — deliberately paying N× the judge cost for N independently-scoped, independently-failable verdicts, rather than betting the whole axis set on one JSON-shaped response.

## Design

### `llm_judge.py`: additive, not rewritten

`build_judge_prompt(output, reference, axis=None)` and `judge(output, reference, call_model, axis=None)` both gain an optional `axis: str | None = None` parameter. `axis=None` (the default) dispatches to the original, byte-for-byte-unchanged `_JUDGE_PROMPT_TEMPLATE` — every existing caller that never passes `axis` sees no behavior change whatsoever, proven by `test_build_judge_prompt_with_no_axis_is_byte_identical_to_before`. A given `axis` dispatches to a sibling `_JUDGE_PROMPT_TEMPLATE_WITH_AXIS`, which scopes the CoT-forcing, reference-guided prompt to that one named dimension ("Grade specifically on this dimension: {axis}... do not let other dimensions influence this verdict").

`JudgeResult` itself gains no `axis` field. The caller already knows which axis it asked for (it passed `axis` into `judge()`) and is responsible for recording it — duplicating it back onto the result would be a second source of truth for the same fact.

This is the first change to `llm_judge.py`'s own scoring logic since it shipped — `judge()`/`build_judge_prompt()` were kept deliberately untouched across the provider-wiring, Score-model, and Score-cache RFCs, each of which had its own reason to leave this module alone (provider swapping and caching are `call_model`/`cli.py`-level concerns, not prompt-construction concerns). Axis-scoping is different: it's genuinely this module's own responsibility, since it changes what the judge is asked to grade, not how the call is made or memoized.

### Cache key: `axis` included, but only when given

`evals.judge.cache.compute_score_cache_key(output, reference, scorer_id, axis=None)` — a fourth optional field, included in the canonical JSON dict **only when not `None`**. This is a real backward-compatibility guarantee, not just an intention: omitting `axis` reproduces the exact original 3-field canonical JSON byte-for-byte, so every `score_cache_key` computed before this parameter existed still matches today — proven by `test_omitting_axis_matches_the_original_pre_axis_key_exactly`, which independently recomputes the pre-axis hash by hand and asserts equality. An earlier draft unconditionally serialized `"axis": axis` (as `null` when absent) — that would have silently changed the hash, and therefore invalidated every pre-existing `--scores` cache file, for every caller that never uses axes at all. Caught before shipping by re-checking the draft against its own backward-compatibility docstring claim, not by a failing test.

Two different axes over the identical `(output, reference, scorer_id)` never collide (`test_different_axes_on_the_same_output_never_collide`) — a cached "correctness" verdict can never be mistakenly served for a "safety" query.

### CLI: one call per configured axis, AND-semantics for the case verdict

`--judge-axes` (comma-separated, e.g. `correctness,safety`) on both `run` and `rollout`, parsed by `_parse_judge_axes` into `list[str] | None`. Omitting the flag (`None`) reproduces exactly today's single-holistic-verdict behavior — this is not a new required decision for existing callers.

`_judge_all_axes(output, reference, call_model, cached_scores, axes)` is the one shared per-axis loop, called from both `run_cmd` (via `_judge_case`, wrapped in `asyncio.run()`) and `rollout_cmd`'s `_score_and_record` (awaited directly, since it may run inside `run_suite`'s own already-running event loop during early-stopping — the same nested-event-loop hazard the Score-cache RFC already found and fixed for the single-axis path). `axes=None` degenerates to exactly one `_judge_with_cache` call, unchanged.

A case's overall pass/fail (the printed summary line, the `successes` counter, and `report`'s pass-rate) is **AND across every configured axis** — a case passes only if it passes on every axis. A response that's factually correct but unsafe is not a passing response. One `Score` row is persisted per axis, each with its own `rubric_axis` field and its own independently-cacheable `score_cache_key`.

If **any** axis's judge call fails (a malformed response, an SDK error), the **whole case** becomes `JUDGE_ERROR` — no partial per-axis `Score`s are persisted for that case. This mirrors the pre-existing single-axis all-or-nothing behavior rather than inventing a new, more complex partial-failure state; proven by `test_run_with_judge_axes_any_axis_error_marks_the_whole_case_judge_error`.

### `Score.rubric_axis`

Already existed as a field (added ahead of this feature, always `None` in v1 per its own docstring) — this RFC is what actually populates it. Docstring corrected to describe the real, now-shipped behavior instead of "stays `None` always in v1."

## Alternatives considered

**Single call, structured multi-axis JSON response** — the recommended default, rejected by explicit user choice via the brainstorming check-in. 1× the judge cost, but couples every axis's verdict to one response: a judge that gets one axis subtly wrong, or returns malformed JSON for one axis, risks corrupting or losing every other axis's verdict in the same call. One-call-per-axis pays N× the cost for verdicts that fail independently — a real, deliberate trade-off, not an oversight.

**Add an `axis` field to `JudgeResult`** — rejected: the caller already knows which axis it requested (it's the same value it passed into `judge()`); echoing it back would be a redundant second source of truth that could theoretically disagree with the caller's own record.

## Verification

`evals/tests/test_llm_judge.py` (4 new tests: byte-identical no-axis prompt, axis-scoped prompt content, axis-scoped `judge()` call, two axes disagreeing on the same output). `evals/tests/test_judge_cache.py` (3 new tests: backward-compatible no-axis key, axis changes the key, two axes never collide). `evals/tests/test_cli_integration.py` (6 new tests: `run`/`rollout` each get an AND-semantics test with a deliberately axis-order-sensitive fixture — first axis PASS, later axis FAIL — so a buggy "just use the first axis" implementation would be caught, not just a case where every axis happens to fail; an any-axis-error-aborts-the-case test; a `--use-score-cache` test proving the cache discriminates per-axis through the full CLI path, not just at the unit level).

Sanity-checked by temporarily replacing both `run_cmd`'s and `rollout_cmd`'s `passed = all(o.passed for o in outcomes)` with `passed = outcomes[0].passed` and confirming the two AND-semantics tests failed with the exact wrong verdict — the first attempt at this sanity check used fixture data where the first axis already failed, which happened to pass under the broken implementation too (an accidental false-negative in the check itself, caught and fixed by re-ordering the fixture responses so the first axis passes and a later axis fails) — then restored.
