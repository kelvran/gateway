# RFC: Same-vendor prompt-debiasing (position-swapped), opt-in

## Status

Accepted, implemented 2026-09-09.

## Context

`docs/upgrade-research/evals-next-upgrade-2026-09-09.md` cites a single 2026 preprint's "Combined Budget" technique — a merged CoT+rubric prompt, called twice with the candidate/reference position swapped — as producing a real accuracy gain specifically for Claude judges. Position (which side of the prompt the candidate output appears on) is an axis that doesn't exist in Kelvran's current judge prompt at all today. Since the effect size is a single unreplicated finding and this doubles judge-call cost/latency, it must be opt-in and off by default — mirroring `--use-score-cache`/`--judge-axes`/`--llm-judge-panel`'s own established "expensive new technique = opt-in" pattern.

Two real gotchas surfaced during design, not in the original research: `evals/judge/providers.py`'s `_AnthropicCallModel`/`_BedrockCallModel` expose per-call cost via a shared, *rebound* (not per-call-stored) `last_call_cost` attribute, explicitly documented as safe only because callers use it sequentially. Calling the same `call_model` instance twice *concurrently* (e.g. both position variants in one `asyncio.gather`) races on this attribute — confirmed directly by a deliberate sanity-check-by-breaking pass (see Verification below), which reproduced exactly the predicted silent cost-under/over-count. Separately, `compute_score_cache_key` deliberately excludes the prompt template from its hash — correct for every existing caller, but a debiased and non-debiased verdict for the identical `(output, reference, scorer_id, axis)` would otherwise collide onto the same cache key.

## Design

`judge()`/`llm_judge.py`'s core stays untouched. A new prompt-builder, `build_debiased_judge_prompt(output, reference, position: Literal["reference_first", "candidate_first"], axis=None)`, is built on top of Phase 5's `QUOTE:` contract via four new templates (`_DEBIASED_JUDGE_PROMPT_TEMPLATE_REFERENCE_FIRST`/`_CANDIDATE_FIRST`, plus `_WITH_AXIS` variants).

The orchestration lives in `evals.cli`, mirroring `_judge_panel_with_cache`'s own existing precedent of bypassing `judge()`'s dichotomy when the caching/orchestration need doesn't fit it. A new `_debiased_judge_verdict(output, reference, call_model, axis=None) -> _DebiasedVerdict | None` helper makes the two position-swapped calls to the same `call_model` **sequentially** — never concurrently — reading `last_call_cost` immediately after each call, before the next call can rebind it, then summing both via the existing `_sum_panel_cost`. The two verdicts combine fail-closed on disagreement (`passed_a == passed_b`, else `False`, mirroring `reduce_panel_votes`'s own fail-closed-tie precedent), with the rationale marking a disagreement explicitly (`"[POSITION DISAGREEMENT — fail-closed]"`) so it's visible in `evals report`/`--scores`, never silently swallowed. Quote-grounding (Phase 5) is AND-combined across both calls.

`_judge_with_cache` gains `debias: bool = False`, dispatching to `_debiased_judge_verdict` instead of `judge()` when set, and tagging the resulting `_JudgeOutcome.bias_mitigations_applied` with `"position_swap_debiasing"` appended to `BIAS_MITIGATIONS_APPLIED`. `_judge_panel_with_cache` gains the same parameter: with `debias=True`, `judge()`'s own built-in panel branch is bypassed entirely — each panelist's own inner work becomes 2 sequential calls (via `_debiased_judge_verdict`) instead of 1, but cross-panelist concurrency is unchanged (every panelist's 2-call chain still runs concurrently with every other panelist's, via `asyncio.gather` across panelists, never across one panelist's own two calls). A 2-judge panel with debiasing on therefore makes 4 total calls: 2 concurrent chains of 2 sequential calls each. `_judge_all_axes`/`_judge_case` thread `debias` straight through without altering the one-call-per-axis structure above them.

`judge/cache.py`'s `compute_score_cache_key` gains an optional `debias: bool = False` parameter, included in the hash only when `True` — mirrors `axis`'s own exact backward-compatible pattern, so every pre-existing key still matches byte-for-byte.

New opt-in `--judge-debias` flag on both `run_cmd` and `rollout_cmd`, threaded through to `_judge_all_axes`. `providers.py`'s `_AnthropicCallModel` docstring gets a doc-comment-only update cross-referencing this new caller's dependence on the sequential-only `last_call_cost` invariant — two call sites now depend on it, not one. No signature change.

## Alternatives considered

**Running the two position-swapped calls concurrently via `asyncio.gather`** — rejected, and deliberately proven wrong: temporarily switching `_debiased_judge_verdict` to `asyncio.gather` and reading `last_call_cost` after both completed caused `cost_a`/`cost_b` to both read the same final rebind, producing `0.004` (`0.002 + 0.002`) instead of the correct `0.003` (`0.001 + 0.002`) in `test_run_with_judge_debias_makes_two_sequential_calls_and_sums_both_costs`. Confirmed the exact predicted failure mode, then reverted.

**Widening `providers.py`'s `call_model` contract to return cost per-call instead of via the `last_call_cost` side channel** — out of scope this round: no other caller needs this yet, and it would touch `judge()`'s own signature contract that this RFC's design explicitly keeps untouched.

**Extending `judge()` itself to accept a `debias` flag** — rejected in favor of keeping `judge()`/`llm_judge.py`'s core untouched, exactly as `_judge_panel_with_cache` already established for panel orchestration: `evals.cli` owns the caching/orchestration decision of *how many calls* and *in what order*, `llm_judge.py` owns only prompt-building and response-parsing.

## Verification

`evals/tests/test_judge_cache.py`: 3 new tests (`debias` changes the key, omitting it reproduces the pre-existing key byte-for-byte, `debias`+`axis` compose without collision) — 11/11 passing. `evals/tests/test_cli_integration.py`: 2 new tests — `test_run_with_judge_debias_makes_two_sequential_calls_and_sums_both_costs` (a scripted fake proving call order `[1, 2]` and `cost_usd == Decimal("0.003")`, the sum of both calls, not just the last) and `test_run_with_judge_debias_fails_closed_on_position_disagreement` (a scripted fake returning PASS then FAIL, proving `value is False` with the `[POSITION DISAGREEMENT — fail-closed]` marker present in the persisted rationale). Sanity-check-by-breaking: temporarily made the two calls concurrent via `asyncio.gather` and confirmed the cost-summing test failed for the exact predicted reason (see Alternatives above), then restored the sequential implementation and re-confirmed the full suite green. Full `cd evals && uv run pytest tests/ && ruff check . && uvx --from import-linter lint-imports` clean (339 passed, 11 skipped — pre-existing, unrelated).
