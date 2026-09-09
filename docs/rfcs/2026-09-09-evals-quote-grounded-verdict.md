# RFC: Quote-grounded verdict — measurement-only

## Status

Accepted, implemented 2026-09-09.

## Context

`docs/upgrade-research/evals-next-upgrade-2026-09-09.md`'s Finding 3 cites two small open-source tools that independently converge on requiring a judge to cite a verbatim quote from the text it's judging: one discards a verdict whose quote can't be confirmed before tallying it. Per this codebase's own repeated "instrument first, act once there's real data" pattern (cache cross-instance telemetry, router-health filter primitives — both built as real, tested, but deliberately unwired measurement infrastructure ahead of any enforcement decision), this round adds quote-grounding as a recorded signal without changing any pass/fail logic. Enforcement (discarding or reweighting an ungrounded vote) is left as a deliberate, disclosed future decision once real data exists on how often grounding actually fails for this judge/panel.

## Design

Both prompt templates (`_JUDGE_PROMPT_TEMPLATE`/`_JUDGE_PROMPT_TEMPLATE_WITH_AXIS`) gain a required `QUOTE: <verbatim quote>` line, placed *before* `VERDICT` — consistent with the existing REASONING-before-VERDICT anti-post-hoc-rationalization rationale (forcing evidence before commitment). `_parse_judge_response`'s return type becomes a frozen `@dataclass`, `_ParsedJudgeResponse{passed, rationale, quote}`, mirroring `evals.judge.providers.JudgeCallCost`'s own small-value-object precedent — a bare 2- or 3-tuple would be a real, silent string/string positional-argument transposition hazard `rationale`/`quote` both invite. `_REASONING_PATTERN`'s regex terminates at whichever of `QUOTE:`/`VERDICT:` comes first, deliberately: a response with no `QUOTE:` line at all (an older-format response, a judge that ignores the new instruction, or a cached response from before this feature existed) still parses `REASONING` correctly up to `VERDICT:`, exactly as it always has — this is what keeps the addition backward-compatible rather than a breaking prompt-contract change. A new pure `_quote_is_grounded(quote, output, reference) -> bool` checks whether `quote` is a verbatim substring of either text; an empty quote (line omitted or blank) is never grounded.

`judge()`'s single-judge branch populates the new `JudgeResult.quote_grounded: bool | None`. Its panel branch populates the new `PanelVote.trigger_quote`/`PanelVote.quote_grounded` per panelist (added to `evals.models.PanelVote`, not `llm_judge.py`, per the existing layering contract). `evals.cli`'s `_judge_with_cache`/`_judge_panel_with_cache` thread the field through to `_JudgeOutcome` and on into `Score.quote_grounded` (also new): single-judge Scores copy the value directly; panel Scores use a new `_panel_quote_grounded` AND-aggregate across `panel_votes` (a single, printable panel-level signal, with full per-panelist detail still recoverable from `panel_votes` itself). `report_cmd`'s per-`scorer_type` line gains a non-gating `(quote_grounded: G/N)` note for `llm_judge`/`llm_judge_panel` groups — printed only when at least one Score in the group has a non-`None` value, never affecting `--fail-under`/`--category-fail-under`.

`BIAS_MITIGATIONS_APPLIED` (in `llm_judge.py`) is renamed from `_BIAS_MITIGATIONS_APPLIED` (dropping the leading underscore) so the Phase 6 position-swapped-debiasing helper can extend the list rather than hand-duplicating it — a pure rename, no behavior change, since the constant was previously only used within this same module.

## Alternatives considered

**Enforcing grounding now** (discarding an ungrounded panelist's vote before `reduce_panel_votes` tallies it) — the option explicitly deferred. For a 2-judge panel, discarding one vote leaves exactly one, which breaks the existing fail-closed-on-tie design (a panel of one has no tie to fail-closed on) — a real design question this round deliberately doesn't have to answer yet, since there's no data showing how often grounding actually fails.

**Requiring the quote field with a hard parse error if missing** — rejected in favor of mirroring `_REASONING_PATTERN`'s own existing lenient fallback exactly: a missing quote becomes an honest `quote_grounded=False`, not a parse failure that would make an otherwise-valid, already-shipped response format suddenly break.

## Verification

`evals/tests/test_llm_judge_prompt_golden.py`'s pinned golden prompt updated to include the `QUOTE:` line (a deliberate, expected diff — exactly what this test exists to catch). `evals/tests/test_llm_judge.py`: all 25 pre-existing tests pass completely unchanged (proving backward compatibility for responses without a `QUOTE:` line); 4 new tests — a real verbatim quote is grounded, a fabricated quote is not grounded but doesn't affect `passed`, a missing `QUOTE:` line still parses `REASONING` correctly and reports `quote_grounded=False`, and a panel populates `trigger_quote`/`quote_grounded` per panelist independently. Full `cd evals && uv run pytest tests/ && ruff check . && uvx --from import-linter lint-imports` clean (334 passed, 11 skipped — pre-existing, unrelated).
