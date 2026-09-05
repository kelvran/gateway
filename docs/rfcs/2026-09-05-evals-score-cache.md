# RFC: Score-level (LLM-judge) result caching

## Status

Accepted, implemented 2026-09-05.

## Context

`THREAT_MODEL.md`'s Evals Denial-of-Service row (`docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`'s own scope boundary) names three real, distinct cost/DoS gaps left after Run-level result caching and two-checkpoint early-stopping shipped: per-tier calibrated trial-count defaults (blocked on production traffic that doesn't exist yet), real SPRT/anytime-valid sequential testing (a real statistical-design decision, not yet made), and `Score`-level (LLM-judge) caching. This RFC closes the third.

The gap is real and independent of Run-level caching: `--use-cache` (`docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`) skips re-*executing* a case's sandbox command, but every `evals run --llm-judge`/`evals rollout --llm-judge` invocation still re-*judges* every case from scratch, paying for a fresh Anthropic API call each time — including the common case of re-running the exact same suite for a report, or a `rollout` invocation where the sandbox output is deterministic (so Run-level caching is redundant) but the operator still wants a fresh Score row for tooling reasons.

## Design

### Cache key

`evals/judge/cache.py`'s `compute_score_cache_key(output, reference, scorer_id) -> str`: a SHA-256 hex digest over the canonical JSON of `{output, reference, scorer_id}`, mirroring `evals/rollout/cache.py`'s `compute_run_cache_key` exactly (RFC-8785-style canonicalization: `sort_keys`, ASCII-only, no whitespace, NaN/Infinity rejected).

Deliberately does **not** version the judge prompt template baked into `evals.judge.llm_judge.judge()` — the same deliberate omission `compute_run_cache_key` already makes for the harness/runner code version. A prompt change is a code change; invalidating stale cached verdicts after one is the operator's responsibility (point `--scores` at a fresh file), not solved with an unrequested prompt-hash field.

### `Score` model additions

`score_cache_key: str | None` and `from_cache: bool = False`, mirroring `Run.cache_key`/`Run.from_cache`'s own precedent exactly: `score_cache_key` is computed for every real `llm_judge` score regardless of whether `--use-score-cache` is active (so a *later* cached invocation can hit against an earlier, non-caching invocation's output) — always `None` for a `deterministic` score, since that scorer is already free and instant, with nothing worth caching. `from_cache=True` implies `cost_usd == Decimal("0")` (an exact, certain fact — no API call was made — the same reasoning already applied to a `deterministic` score's `cost_usd`).

Unlike `Run`, there is no `cache_source_score_id` — `Score` has no `id` field of its own for a hit to point back at.

### CLI wiring

`--use-score-cache` (off by default) on both `evals run` and `evals rollout`, mirroring `--use-cache`'s exact naming/opt-in convention. When set, `_load_cached_scores(scores_path)` builds a `dict[score_cache_key, Score]` from `--scores`, filtered to `scorer_type == "llm_judge"` and `not from_cache` — the same "never chain a hit off a hit" discipline `--use-cache`'s own `cached_runs` construction already applies to `Run.from_cache`.

A new shared `_judge_with_cache(output, reference, scorer_id, call_model, cached_scores)` async helper replaces the two previously-separate judge-invocation paths (`run_cmd`'s synchronous `_judge_output`/`_judge_case`, wrapped in `asyncio.run()`; `rollout_cmd`'s `_score_and_record`, which calls `judge()` directly via `await` since it may run inside `run_suite`'s own event loop during early-stopping). `_judge_with_cache` is itself a plain `async def`, awaited directly — never driving its own `asyncio.run()` — so it is safe to call from both contexts: `run_cmd` wraps it in `asyncio.run()` at its own call site (safe, since `run_cmd` never runs inside an event loop); `rollout_cmd` awaits it directly.

### Interaction with `--use-cache` (Run-level caching)

Independent mechanisms, deliberately: a `rollout` invocation with `--use-score-cache` but *not* `--use-cache` still re-executes every sandbox command for real, but skips the judge call once the resulting stdout matches a prior score's cached `(output, reference, scorer_id)` — useful when an operator wants fresh execution telemetry (timing, exit codes) without paying for re-judging deterministic output.

## Alternatives considered

**Cache on `(eval_case_id, eval_case_revision, scorer_id)` instead of `(output, reference, scorer_id)`** — rejected: this would cache a *case*, not a *judgment*, and would silently serve a stale verdict if a real rollout's sandbox output legitimately changed between invocations (e.g. a flaky command, or the model under test producing different output run to run). Caching on the actual judged content is the only version that stays honest under output non-determinism.

**Widen `judge()`/`llm_judge.py`'s own signature to accept a cache** — rejected, per this project's own established invariant (first stated in `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`, reaffirmed in `docs/rfcs/2026-09-04-evals-score-model.md`): `judge()`/`llm_judge.py` stay untouched, zero network-call awareness, fully testable without a key. Caching is a `cli.py`-level policy layered on top via the same `call_model` DI seam every other cross-cutting concern (cost tracking, provider swapping) already uses.

## Verification

`evals/tests/test_judge_cache.py` (5 unit tests on `compute_score_cache_key`); 3 new `test_cli_integration.py` tests proving zero new judge calls on a second `--use-score-cache` invocation for both `run` and `rollout`, and that omitting the flag re-judges every time. Sanity-checked by temporarily short-circuiting the cache lookup and confirming the 2 hit-detecting tests fail with the exact expected call counts, then restoring.
