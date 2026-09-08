# RFC: Judge-panel reducer — build the real multi-judge majority vote

## Status

Accepted, implemented 2026-09-08. Companion to, and direct follow-on from,
`docs/rfcs/2026-09-07-evals-judge-panel-interface.md`.

## Context

The interface RFC widened `judge()`'s `call_model` parameter to accept
`CallModel | list[CallModel]` and `Score.scorer_type` to include
`"llm_judge_panel"`, but deliberately left the panel itself unbuilt —
`judge()` raised `NotImplementedError` for any `call_model` list longer
than one. That RFC's own stated reason: at the time, Kelvran's
regression-tier corpus was a 5-case hand-built smoke fixture, not a real
curated dataset, and a fancier scoring mechanism layered on top of a case
set that thin wasn't worth building yet.

Since then, the regression corpus grew to 113 real, individually-verified
cases across 5 files, landing in the sizing research's own recommended
N≈100-150 target. A dedicated 2026-09-08 follow-up research pass
(`docs/upgrade-research/evals-judge-panel-reducer-2026-09-07.md`, its
revised sections) confirmed that specific blocker is genuinely resolved —
but found a sharper one in its place: 100% of those 113 cases use
deterministic exact-match scoring; zero exercise the judge path at all.
So building the reducer alone would have nothing real to act on. This RFC
covers both: the reducer itself, and the CLI/caching wiring that makes it
usable — the judge-accuracy corpus slice and its CI wiring that give the
reducer real cases to run against are tracked separately (Phases 4-6 of
the approved implementation plan), gated on live provider API keys this
implementation pass did not have.

The reducer's core algorithm (strict majority, fail-closed tie-break,
independent refutation) is fully specified by the research's own "Concrete
reducer design for Kelvran" section — this RFC implements that design and
resolves two questions the research explicitly left open.

## Design

### The reducer: `PanelVote`/`PanelVerdict`/`reduce_panel_votes`

`reduce_panel_votes(votes: list[PanelVote]) -> PanelVerdict` in
`evals/evals/judge/llm_judge.py` implements Inspect AI's `majority_score`
shape exactly: `count * 2 > len(votes)` for a strict majority PASS or
FAIL; **fail-closed** (`passed=False, quorum_reached=False`) on no
majority — the guaranteed outcome of every 1-1 split on Kelvran's v1
2-judge panel, and possible on any even split of a larger one. This
mirrors the existing guardrails fail-closed convention
(`docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md`) directly: when
a verdict mechanism can't produce a confident answer, withhold a pass,
don't grant one. A judge panel that can't agree has no analogous second,
independent control the way `budget.Tracker` backstops the rate
limiter's own fail-open default — there is no equivalent safety net here,
so fail-open was rejected.

`judge()`'s real panel branch (replacing the `NotImplementedError`) calls
every panelist with the IDENTICAL prompt via `asyncio.gather` —
independent refutation, no panelist ever sees another's response or
verdict — then reduces. `judge()` itself has no way to know which real
model id each opaque `call_model` wraps, so its own `PanelVote.scorer_id`
values are positional placeholders (`"panelist_0"`, ...); `evals.cli`,
which built the panel from named provider factories and does know each
real id, remaps them before persisting.

### `PanelVote` lives in `evals.models`, not `evals.judge.llm_judge`

The research's own illustrative sketch placed `PanelVote` in
`llm_judge.py`. Verified directly against `.importlinter`'s layers
contract before implementing:

```
[importlinter:contract:layers]
layers =
    evals.cli
    evals.rollout.scheduler | evals.ingestion.*
    evals.tracing | ... | evals.judge.deterministic | evals.judge.llm_judge | evals.judge.providers
    evals.models | evals.stats
```

`evals.judge.llm_judge` sits ABOVE `evals.models` — a lower layer may
never import from a higher one. Since `Score` (in `models.py`) needs to
embed `panel_votes: list[PanelVote]`, `PanelVote` must live in
`evals.models`; `llm_judge.py` imports it downward instead — the first
time that module has ever imported from `evals.models`. Verified this
doesn't create a real circular-dependency risk: `evals.models` imports
nothing from `evals.judge.*` anywhere, so the new edge is one-directional.

### Cache-key design: per-panelist granular, never a composite key

Each panelist's cache key is computed exactly as if it were a standalone
single judge — `scorer_id` is that panelist's own real model id (e.g.
`"claude-haiku-4-5-20251001"`), never a composite string. This is
deliberate, not an oversight: it means (1) a panelist's cached vote from
a *standalone* `--llm-judge` run is transparently reusable inside a
*later* `--llm-judge-panel` run for that same model id, and vice versa,
since both are keyed identically — proven by
`test_run_with_llm_judge_panel_use_score_cache_reuses_a_prior_standalone_score`;
and (2) a future 3rd judge added to the panel reuses the 2 existing
panelists' cached votes untouched, since nothing about their own cache
keys depends on panel membership or size.

The panel's REDUCED verdict is never itself cached — `reduce_panel_votes`
re-runs on every invocation over whatever mix of cache-hit and
freshly-called votes results, per case. The composite string
(`"panel:claude-haiku-4-5-20251001+gpt-4o-mini"`) is used only as the
final `Score.scorer_id` display/identity value for the one combined
`llm_judge_panel` Score row — never as a cache-key input.

`_load_cached_panel_votes` (in `evals.cli`) pulls reusable votes from
both prior standalone `"llm_judge"` Scores and prior `"llm_judge_panel"`
Scores' own embedded `panel_votes`, filtering on `from_cache is False` in
both cases — mirroring `_load_cached_scores`'s own "never chain a hit off
a hit" discipline exactly, proven by
`test_run_with_llm_judge_panel_never_re_chains_a_cached_vote_off_another_cache_hit`.

### Cost accounting: sum across panelists, `None` if any is unknown

`Score.cost_usd` for a panel score is the `Decimal` SUM of every
panelist's real per-call cost, computed via the existing
`_last_judge_call_cost_usd` helper reused per-panelist (no new cost
helper needed beyond a small summation function). If ANY fresh
panelist's cost is unmeasured (`None` — an unpriced model, or a test fake
with no cost side-channel), the WHOLE panel's `cost_usd` is `None`, never
a silently-understated partial sum — generalizing `Score.cost_usd`'s own
"genuinely unmeasured, never a fabricated stand-in" convention from one
judge to any judge in the panel. An all-cache-hit panel score is the
exact, certain `Decimal("0")`.

### CI scheduling: nightly + `workflow_dispatch`, never per-push

A new, separate `.github/workflows/evals-judge-nightly.yml` — never wired
into the existing per-push/PR `ci.yml` `evals` job. Three concrete
reasons: (1) real, unbounded dollar cost against two live paid providers
on every push/PR update, vs. today's $0 deterministic smoke test; (2) a
live third-party availability dependency (Anthropic AND OpenAI both)
directly gating PR mergeability, an unrelated coupling; (3) a directly
verified fact: neither `_AnthropicCallModel.__call__` nor
`_OpenAICallModel.__call__` (`evals/evals/judge/providers.py`) sets a
`temperature` parameter — a judge/panel verdict on the identical prompt
is genuinely non-deterministic run-to-run at each provider's own API
default. Gating PR mergeability on a probabilistic call whose own corpus
is deliberately built to include ambiguous boundary cases (the Phase 4
judge-accuracy slice) would be a real, self-inflicted CI-flakiness
source. Deliberately not fixed in this pass: setting `temperature=0` on
the shared `providers.py` call sites would be a real behavioral change
affecting every existing `--llm-judge` caller too, out of scope here —
named as a separate future hardening candidate.

### Report visibility: quorum ties get their own note

`report_cmd` needs zero code changes for correctness — it already groups
purely by `s.scorer_type`, so `"llm_judge_panel"` gets its own report
line and gate check with zero changes. One small, optional addition on
top: a `" ({N} quorum-tie, fail-closed)"` note on the `llm_judge_panel`
line, mirroring the existing `"({N} flaky excluded from gate)"` pattern —
answers the research's own open question about surfacing disagreement-
driven fail-closed cases distinctly from genuine unanimous failures.

## Alternatives considered and rejected

- **A composite panel-level cache key** (e.g. hashing both model ids
  together) — rejected: loses both cross-mode reuse properties above for
  no benefit; per-panelist keying is strictly more general.
- **Caching the reduced verdict itself** — rejected: the reduction is pure,
  cheap, deterministic-given-its-inputs computation; caching it would add
  staleness risk (a reduction policy change wouldn't retroactively apply
  to old cached verdicts) for zero real cost savings over just re-running
  `reduce_panel_votes` on cached votes every time.
- **Wiring judge/panel scoring into the existing per-push `ci.yml`** —
  rejected per the CI-scheduling reasoning above.
- **Folding the existing 113-case deterministic corpus into the same new
  nightly workflow** — rejected: that corpus has its own separate,
  unresolved CI-gate calibration question (several cases are deliberate,
  documented FAIL cases) unrelated to anything in this RFC; bundling them
  risks masking or false-alarming on that separate, pending work.

## Verification

`cd evals && uv run pytest tests/ && ruff check . && uvx --with-editable . --from import-linter lint-imports` —
294 passed, 11 skipped; ruff clean; import-linter's layers contract still
kept (confirmed the new `evals.judge.llm_judge` → `evals.models` edge
does not violate it — 20 dependencies now, up from 19, all three
contracts still KEPT). New tests: `reduce_panel_votes` unit tests
(unanimous, 2-judge tie fail-closed, 4-judge even-split fail-closed,
3-judge majority, empty-list, bias mitigations); `judge()` panel-branch
tests including the concrete independent-refutation proof (neither
panelist's captured prompt contains the other's response text);
`PanelVote`/`Score` model tests including a full JSON round-trip; 12 new
CLI integration tests covering real panel scoring, disagreement fail-
closed, mutual exclusivity with `--llm-judge`, cost summation and its
`None`-propagation, cache reuse (including cross-mode reuse and the
never-chain-a-hit-off-a-hit property), `rollout` panel scoring, and
`report --scores`'s separate-lines and quorum-tie-note behavior.
Sanity-checked-by-breaking: temporarily flipped `reduce_panel_votes`'s
tie-break branch to fail-open, confirmed the tie/disagreement tests
failed with the specific wrong-value assertion, restored with zero
`git diff` trace.

**Not yet done, tracked separately (the implementation plan's Phases
4-6):** the judge-accuracy corpus slice (`regression_corpus_judge_
accuracy.json`) requires live-verified `(output, reference)` verdicts
against real Anthropic and OpenAI API keys, which this implementation
pass did not have available — cannot be fabricated without violating
this codebase's own "verify, don't guess" discipline. The CI workflow
YAML above is written and validates, but will fail loudly on its first
real tick until `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` are added as GitHub
repo secrets (a human Settings-page action) and the corpus slice exists.
The real Anthropic+OpenAI disagreement-rate measurement (closing the
research's own open question about panel diversity) depends on both of
those and cannot happen until they do.
