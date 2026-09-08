# RFC: Judge-panel reducer — build the real multi-judge majority vote

## Status

Accepted, implemented 2026-09-08. Companion to, and direct follow-on from,
`docs/rfcs/2026-09-07-evals-judge-panel-interface.md`.

**Revised 2026-09-08 (same day, immediately after initial implementation):
the panel's provider composition changed from Anthropic + OpenAI to two
AWS Bedrock-hosted Claude models (Sonnet 5 + Haiku 4.5), at the project
owner's explicit direction, for AWS-only operational simplicity — an
existing shared AWS credential already used elsewhere in this workspace
(Anvilry, daily-dose) can now cover this too, with no direct OpenAI key
needed at all.** This is a real, honest tradeoff, not a free swap: both
judges are now Claude models from the same vendor, sharing the same base
architecture and RLHF lineage, so the panel's bias-reduction premise —
the entire reason a second, disjoint judge helps — is weaker than the
originally-designed Anthropic+OpenAI pairing. This was surfaced to, and
knowingly accepted by, the project owner before implementation (a real
finding from this session's own research grounded the concern: a
cross-vendor Claude/GPT pair showed the *highest* pairwise agreement of
all pairs tested in one study, so same-vendor correlation risk is not a
theoretical worry). The reducer's algorithm, cache-key design, and CI-
scheduling decision below are all UNCHANGED by this revision — only the
two `call_model`s the panel is built from changed. See the "Real,
verified model ids and pricing" section (new, below) for what was
directly verified (not guessed) about the two Bedrock models actually
used, and the "Consequence: cross-mode cache reuse is now dormant"
section for one real, honestly-named side effect of this swap.

**Corrected 2026-09-08 (same day, once a real AWS credential was actually
available to test against): both model id constants were wrong.** The
bare `bedrock-runtime` model ids used above were never live-tested —
verified only against AWS documentation, which never states that these
two specific models reject on-demand invocation by bare id. A real
Converse call fails outright (`ValidationException: ... on-demand
throughput isn't supported ... Retry your request with the ID or ARN of
an inference profile`). Both constants now carry the `global.`-prefixed
cross-region inference profile id instead; see the "Real, verified model
ids and pricing" section's own correction note for the full account. A
real end-to-end `evals run --llm-judge-panel` invocation against a live
credential now succeeds — the first time this feature has actually run
against real Bedrock access, not just fakes.

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
(`"panel:anthropic.claude-sonnet-5+anthropic.claude-haiku-4-5-20251001-v1:0"`,
after the revision above) is used only as the final `Score.scorer_id`
display/identity value for the one combined `llm_judge_panel` Score row —
never as a cache-key input.

`_load_cached_panel_votes` (in `evals.cli`) pulls reusable votes from
both prior standalone `"llm_judge"` Scores and prior `"llm_judge_panel"`
Scores' own embedded `panel_votes`, filtering on `from_cache is False` in
both cases — mirroring `_load_cached_scores`'s own "never chain a hit off
a hit" discipline exactly, proven by
`test_run_with_llm_judge_panel_never_re_chains_a_cached_vote_off_another_cache_hit`.

### Consequence: cross-mode cache reuse was dormant, now reactivated

The cache-key design's cross-mode reuse property (a vote cached from a
*standalone* `--llm-judge` run reusable inside a *later*
`--llm-judge-panel` run for the same model id, and vice versa) was
architecturally correct and general, but had no real code path
exercising it as a direct consequence of the panel-composition revision
above: `--llm-judge` used direct-Anthropic-API's `DEFAULT_JUDGE_MODEL`
(`"claude-haiku-4-5-20251001"`), while `--llm-judge-panel` used two
Bedrock model ids — disjoint id strings, so no cache overlap between the
two modes existed.

**Reactivated 2026-09-08 (later still, same day, once a real AWS
credential existed to test against): `--llm-judge` moved to Bedrock too**,
specifically reusing `BEDROCK_HAIKU_4_5_MODEL_ID` — the exact same model
id as the panel's own Haiku panelist — so `ANTHROPIC_API_KEY` is no
longer needed anywhere in `evals`'s judge system at all (this was the
project owner's own explicit direction: the standalone judge should use
Bedrock too, for the same AWS-only operational simplicity as the panel).
Reusing the panel's own Haiku id, rather than a third, disjoint model,
was deliberate: it reactivates this exact cross-mode reuse property for
real.

**Verifying the "and vice versa" half of that property live for the
first time found a real, pre-existing bug**: `_load_cached_scores`
(`evals/evals/cli.py`), the lookup table backing standalone
`--llm-judge`'s own `--use-score-cache`, only ever read prior
`scorer_type == "llm_judge"` Scores — it never read a prior
`llm_judge_panel` Score's own embedded `panel_votes` at all. So while
`_load_cached_panel_votes` (the panel's own lookup) correctly read BOTH
directions already, `_load_cached_scores` only ever supported the
standalone-into-panel direction, never panel-into-standalone — the "and
vice versa" claim above was only ever half-implemented, undetected
because the cross-mode scenario itself was dormant until this pass.
Fixed by widening `_load_cached_scores` to also synthesize a matching
`llm_judge` `Score` from any `llm_judge_panel` Score's embedded
`panel_votes` (carrying the panel Score's own `bias_mitigations_applied`
forward, since that genuinely describes how the vote was produced).
Two new tests prove both directions for real:
`test_run_with_llm_judge_panel_reuses_a_prior_standalone_haiku_score` and
`test_run_with_llm_judge_reuses_a_prior_panel_haiku_vote` — the latter
failed before the fix (4 calls instead of the expected 2), confirmed via
sanity-check-by-breaking (the fix was disabled, the test failed for the
exact expected reason, then restored).

### Real, verified model ids and pricing (added by the panel-composition revision)

`BEDROCK_SONNET_5_MODEL_ID = "global.anthropic.claude-sonnet-5"` and
`BEDROCK_HAIKU_4_5_MODEL_ID = "global.anthropic.claude-haiku-4-5-20251001-v1:0"`
(`evals/evals/judge/providers.py`) — both verified 2026-09-08 directly
against AWS's own live Bedrock model-card pages' "Programmatic Access"
tables, never guessed from a plausible-looking naming pattern. Note the
real, asymmetric naming this verification surfaced: Sonnet 5's id has no
date suffix; Haiku 4.5's does — a detail that would have been wrong if
inferred from Haiku 4.5's own pattern alone.

**Correction, same day, once a real AWS credential was actually available
to test against:** the bare `bedrock-runtime` model id (no prefix) was
originally used here, on the documented-but-unverified assumption that it
was "the correct default for a single-region judge workload." A real
Converse call against the bare id fails outright on this account:
`ValidationException: Invocation of model ID ... with on-demand
throughput isn't supported. Retry your request with the ID or ARN of an
inference profile that contains this model.` Neither Sonnet 5 nor
Haiku 4.5 supports on-demand invocation by bare id at all — confirmed by
a real live call, not inferred from documentation prose (the model cards
describe the bare id as a valid "Model ID" without stating this
restriction anywhere obvious). Both constants now carry the `global.`-
prefixed cross-region inference profile id instead — chosen over a
`us.`/`eu.`/`au.`/`jp.` geo-scoped profile specifically so this never
needs to track whatever region the deployer's own `AWS_REGION` happens to
be set to; AWS's own docs confirm global cross-Region inference is
supported for on-demand model inference. Both new ids were re-verified
live (a real `evals run --llm-judge-panel` invocation against a live
credential, not just a raw provider call) before landing.

Both models are billed via AWS Marketplace as third-party models; a live
fetch of Bedrock's own pricing page did not surface a real, current
per-token rate for either in this pass (the page's real per-model rates
are rendered client-side and weren't captured by a static documentation
fetch). Rather than guess, `_BEDROCK_MODEL_PRICE_PER_MTOK_USD` is
deliberately empty for both — `_compute_bedrock_cost_usd` returns `None`
for both models today, mirroring `_compute_anthropic_cost_usd`/
`_compute_openai_cost_usd`'s own established "no price-table entry ->
genuinely unmeasured, never fabricated" convention exactly, not a new
gap unique to this provider. A real, live-verified price-table entry for
both models is a clean, additive future fix (add two lines to that
dict), named here as a known follow-up, not silently left unstated.

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
reasons, re-verified as still true after the panel-composition revision
above: (1) real, unbounded dollar cost against a live paid Bedrock
account on every push/PR update, vs. today's $0 deterministic smoke
test; (2) a live third-party (AWS Bedrock) availability dependency
directly gating PR mergeability, an unrelated coupling; (3) a directly
verified fact: neither `_AnthropicCallModel.__call__`,
`_OpenAICallModel.__call__`, nor `_BedrockCallModel.__call__`
(`evals/evals/judge/providers.py`) sets a `temperature` parameter — a
judge/panel verdict on the identical prompt is genuinely non-
deterministic run-to-run at each provider's own API default (Bedrock's
Converse API defaults are the model's own provider-side default, same
non-determinism risk as the direct Anthropic/OpenAI APIs — confirmed by
reading the same `providers.py` call site directly, not assumed to carry
over). Gating PR mergeability on a probabilistic call whose own corpus
is deliberately built to include ambiguous boundary cases (the Phase 4
judge-accuracy slice) would be a real, self-inflicted CI-flakiness
source. Deliberately not fixed in this pass: setting `temperature=0` on
the shared `providers.py` call sites would be a real behavioral change
affecting every existing `--llm-judge` caller too, out of scope here —
named as a separate future hardening candidate.

**Corrected 2026-09-08 (later still, same day): the workflow's own gate
was wrong, found by a real, live full-pipeline dry run against the
finished Phase 4 corpus.** The originally-planned `evals report
--fail-under 0.60 --category-fail-under "category:judge:0.60"` was
justified as "comfortably below the ~67% 'all obvious cases pass, zero
boundary cases' floor" — but that floor's own arithmetic was wrong: it
read "all obvious cases pass" as "all 16 obvious-bucket cases score
PASS," when half of that bucket (8 of 16) is deliberately
obviously-*incorrect* output a correctly-performing judge should FAIL,
not pass. A real dry run confirms this: `llm_judge` scored 0.4583 pass
rate, `llm_judge_panel` scored 0.4167 — both correctly classifying every
single "obvious" case, both far below 0.60, both would have failed the
gate on every single run regardless of judge quality. Fixed by removing
the gate entirely from this workflow rather than tuning the threshold
number down (which would only have weakened an already-wrong check,
not fixed it) — a meaningful gate for this corpus needs a real
accuracy-vs-designed-label metric (did the verdict match this corpus's
own intended correct/incorrect classification), which `evals report` has
no concept of today; named as separately-scoped future work, not
improvised here under time pressure. The workflow still runs both judge
legs and uploads the scores artifact — it's a real, recurring
data-collection job for Phase 6's disagreement-rate analysis, not (yet)
a pass/fail gate.

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
302 passed, 11 skipped; ruff clean; import-linter's layers contract still
kept (confirmed the new `evals.judge.llm_judge` → `evals.models` edge
does not violate it — 20 dependencies now, up from 19, all three
contracts still KEPT). New tests: `reduce_panel_votes` unit tests
(unanimous, 2-judge tie fail-closed, 4-judge even-split fail-closed,
3-judge majority, empty-list, bias mitigations); `judge()` panel-branch
tests including the concrete independent-refutation proof (neither
panelist's captured prompt contains the other's response text);
`PanelVote`/`Score` model tests including a full JSON round-trip; 8 new
`_BedrockCallModel`/`make_bedrock_call_model` unit tests (Converse
request/response wire shape, multi-block text joining, no-content-block
error, cost-is-None-when-unpriced, cost-rebinds-not-mutates, and that
constructing a Bedrock client never requires AWS credentials to be
present, mirroring the Anthropic/OpenAI providers' own "lazy credentials"
invariant); CLI integration tests covering real panel scoring against
fake Bedrock responses, disagreement fail-closed, mutual exclusivity
with `--llm-judge`, cost summation and its `None`-propagation, cache
reuse (per-case-granular reuse and the never-chain-a-hit-off-a-hit
property — the cross-mode standalone-to-panel reuse test was removed
after the panel-composition revision made its premise false; see
"Consequence: cross-mode cache reuse is now dormant" above), `rollout`
panel scoring, and `report --scores`'s separate-lines and quorum-tie-note
behavior. Sanity-checked-by-breaking twice: `reduce_panel_votes`'s
tie-break branch flipped to fail-open (confirmed the tie/disagreement
tests failed with the specific wrong-value assertion); `_BedrockCallModel`
's text-block extraction emptied (confirmed 5 of 6 dependent tests failed
with the exact expected "no text content block" error, the 6th — the
test proving that exact error — correctly still passed). Both restored
with zero `git diff` trace.

**Phase 4 DONE 2026-09-08, once real AWS Bedrock access existed:** the
24-case judge-accuracy corpus slice
(`tests/fixtures/regression_corpus_judge_accuracy.json`) is built and
live-verified — 8 designed obviously-correct, 8 obviously-incorrect, 8
boundary/ambiguous. Verified via 2 separate real, live runs against the
actual Bedrock panel (not fakes, not a single sample treated as ground
truth): 22 of 24 cases were stable across both runs; 2 genuinely were
not (`judgeacc-correct-units-2`, a unit-conversion-plus-rounding case
originally designed as obviously-correct; `judgeacc-boundary-numeric-3`,
a price-rounding case designed as boundary) — both reclassified to
`flaky: true` with both real runs' actual verdicts and rationale
recorded in their own `corpus_note`, not silently re-run until a
"clean" result appeared. Only 7 of the other 8 designed-boundary cases
turned out unanimous under live verification and were reclassified to
`flaky: false` accordingly (per this RFC's own Phase 4 spec: "reclassify
any that turn out unanimous") — a real, measured data point (not a
guess) supporting the same-vendor-correlation concern raised before this
panel's composition was decided: a same-vendor (both Claude) panel shows
less, and less *stable*, disagreement than the originally-designed
cross-vendor Anthropic+OpenAI pairing would have. The standalone
`--llm-judge` (direct Anthropic) leg could not be live-verified in this
same pass — no `ANTHROPIC_API_KEY` was available locally — named
honestly rather than fabricated; the corpus content itself doesn't
depend on which judge scores it, so this is a pure credential gap, not
a design gap.

**Corrected 2026-09-08 (later still, same day): the standalone
`--llm-judge` credential gap named above no longer exists.** Per the
project owner's direction, `--llm-judge` moved to Bedrock too (reusing
`BEDROCK_HAIKU_4_5_MODEL_ID`) — see "Consequence: cross-mode cache reuse
was dormant, now reactivated" above. `ANTHROPIC_API_KEY` is no longer
used anywhere in `evals`'s judge system; the 3 AWS secrets
(`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_REGION`) were added as
GitHub repo secrets the same day. A live, full-pipeline local dry run of
both judge legs against the finished Phase 4 corpus succeeded end-to-end
with only those 3 secrets — found and fixed the CI-gate bug documented
in "CI scheduling" above in the same pass.

**Not yet done, tracked separately (the implementation plan's remaining
Phase 6 item):** the real judge/panel disagreement-rate measurement
(closing the research's own open question about panel diversity) still
depends on the nightly workflow actually running once for real in CI
(not just dry-run locally) — trigger `workflow_dispatch` once now that
secrets exist, then pull that run's artifact. Phase 4's own multi-run
local sample (3 real runs total across this implementation: 2 panel, 1
standalone) is a real, honest starting data point, not a substitute for
that.
