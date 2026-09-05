- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: Claude (agentic session), grounded via a 4-angle dynamic-workflow research pass + independent synthesis re-verification

## Summary

Close `THREAT_MODEL.md`'s Evals Denial-of-Service gap ("a naive suite can cost tens of thousands of dollars per regression check... no tiered gating, no early-stopping rule, no prefix/result cache") with two independent, additive, off-by-default mechanisms in `evals/evals/rollout/scheduler.py`'s `run_suite`: an opt-in result cache, and a statistically-sound two-checkpoint early-stopping rule for repeated trials of the same `EvalCase`. Both reproduce today's exact behavior when unused.

## Motivation

`THREAT_MODEL.md:37` already names this as a live, uncorrected gap: `run_suite` is a single unconditional loop over every case, with no way to avoid re-paying for an unchanged case or to stop a repeated-trial group early once enough evidence has accumulated.

A grounding research pass (code-audit of the real scheduler/models/results-store/stats substrate, real precedent for sequential early-stopping methodology, real precedent for result-caching design, and an engineering design proposal) plus an independent synthesis re-verification found one critical correctness issue that changes this RFC's design decisively: **the early-stopping mechanism originally floated when this item was added to the backlog plan — "stop once the running Wilson CI excludes an operator-supplied baseline pass rate," rechecked after every trial — is not statistically sound.** This is the textbook optional-stopping / peeking problem (Armitage, McPherson & Rowe 1969; Evan Miller's "How Not to Run an A/B Test," 2010; Johari, Koomen, Pekelis & Walsh, KDD 2017 — the paper behind Optimizely's Stats Engine). A fixed-sample confidence interval like `wilson_interval` is only valid at one pre-committed sample size; recomputing it after every new trial and stopping at the first exclusion inflates the true false-positive rate well above the nominal level — Johari et al. measure 5-10x inflation at realistic sample sizes, and prove via the optional-stopping theorem that the running interval will eventually cross any fixed line with probability approaching 1 given enough trials, even when the case's true pass rate equals the baseline. An operator-supplied `min_trials` floor delays the first opportunity to false-trigger; it does not bound the inflation for any trial after the floor.

This RFC corrects course before any code was written: a two-checkpoint, Bonferroni-corrected design (check only at a pre-declared `min_trials` and `max_trials`, never continuously) that keeps the total false-positive probability bounded, reuses `wilson_interval` unmodified, and needs no new dependency.

## Detailed Design

### Scope

**In scope**: a result cache keyed on `(task_spec, harness_config)`; a two-checkpoint early-stopping rule for repeated trials of the same `(eval_case_id, eval_case_revision)`.

**Explicitly out of scope for this RFC** (see Alternatives Considered):
- Real SPRT / anytime-valid confidence sequences (mSPRT, Howard et al. 2021) — needs an operator-supplied alternative pass rate, a real scope addition this RFC doesn't ask for.
- More than 2 checkpoints, or a continuous per-trial check — each additional look needs its own alpha allocation (Pocock/O'Brien-Fleming-style spending), real statistical design work, not a parameter bump.
- `Score`-level caching (deduping paid `--llm-judge` calls) — this cache only removes sandbox-execution cost; caching a judge's LLM output carries a distinct correctness risk deserving its own review.
- Calibrated `min_trials`/`max_trials`/`baseline-pass-rate` defaults — this project has zero production traffic yet to calibrate against (mirrors Cache L3's real-embeddings deferral for the identical reason); the operator supplies all three explicitly, no defaults guessed.
- Non-sequential/parallel trial execution — `scheduler.py`'s own docstring already commits to sequential-only as the stated v1 contract.
- Caching `tier="drift_sample"` cases — that tier exists specifically to detect real-world drift over time; a cache hit would silently defeat its purpose, so it's hard-excluded in the mechanism itself, never left to `--use-cache`'s discretion.
- A pluggable early-stop strategy interface — v1 ships exactly one strategy (two-checkpoint Bonferroni); an abstract `EarlyStopFn`/strategy-factory interface is premature generality until a second strategy actually exists to justify it.

### Feature 1 — Result cache

New `evals/evals/rollout/cache.py`:

```python
def compute_run_cache_key(task_spec: dict, harness_config: dict) -> str:
    """SHA-256 hex digest over the canonical JSON of {task_spec, harness_config}."""
```

Canonical JSON: `json.dumps({"task_spec": task_spec, "harness_config": harness_config}, sort_keys=True, ensure_ascii=True, allow_nan=False, separators=(",", ":"))` — the real, documented baseline for stable hashing (per RFC 8785/JCS's own four correctness concerns: whitespace, key ordering, number formatting, NaN/Infinity rejection).

`Run` gains three additive fields (`cache_key: str | None = None`, `from_cache: bool = False`, `cache_source_run_id: str | None = None`) — verified safe against old JSONL data (an old-shape line validates cleanly against the widened model under the pinned pydantic version, all new fields resolving to their declared defaults).

`run_suite` gains `cached_runs: dict[str, Run] | None = None`. When `None` (the default), the cache-lookup branch is never entered — byte-for-byte identical to today. When provided, a hit requires `case.tier != "drift_sample"` (checked unconditionally, not left to caller discretion) and a cache-key match. **`run_suite` itself defensively re-checks `cached.status == "completed"` before treating a match as a real hit** — found necessary during implementation, not merely a CLI-layer convention: a test constructing `cached_runs` with an `error`/`timed_out` entry directly (bypassing the CLI's own filter) caught that the scheduler would otherwise have treated it as a real hit, fabricating a fake completed result from a transient infra failure. This is now a hard invariant at the lowest layer, matching this codebase's existing layered-defense convention (e.g. Guardrails' pre/post-call checks). Chaining a hit off another hit (`from_cache=True`) is *not* independently re-checked by `run_suite` — that filter is deliberately left to the CLI's `cached_runs` construction (`not r.from_cache`), a real, narrower, documented gap in the scheduler's own defense-in-depth, distinct from the `status` check above.

`cli.py`'s `rollout_cmd` gains `--use-cache` (off by default). When set, it builds `cached_runs` by reading back the existing `--results` file (`load_runs`, already generic, unmodified) and filtering to eligible entries — the cache *is* the results file; no new storage.

**Named limitation, not solved here**: the cache key does not include a harness/runner code version or Docker image digest — only the image *tag* already inside `harness_config`. A mutable tag rebuilt with different content would produce a silent stale hit. This is the operator's responsibility to avoid (write `task_spec.image` as a digest reference if reproducibility matters), named explicitly rather than solved with unrequested new fields.

### Feature 2 — Two-checkpoint early stopping

New function in `evals/evals/stats.py` (reusing `wilson_interval` unmodified):

```python
def two_checkpoint_early_stop(
    successes: int, trials_run: int, min_trials: int, max_trials: int,
    baseline_pass_rate: float, confidence: float = 0.95,
) -> bool:
    """Returns True only at trials_run == min_trials or == max_trials — never
    on any other trial count, by construction, so a caller invoking this
    after every trial gets the correct, alpha-controlled 2-checkpoint
    procedure without having to track which trial count is a real
    checkpoint. NOT the same as calling wilson_interval at every trial and
    stopping at first exclusion — that is the classical optional-stopping/
    peeking failure mode this function exists to avoid (see Motivation)."""
    if trials_run not in (min_trials, max_trials) or trials_run == 0:
        return False
    checkpoint_confidence = (
        confidence if min_trials == max_trials else 1 - (1 - confidence) / 2
    )
    lower, upper = wilson_interval(successes, trials_run, confidence=checkpoint_confidence)
    return not (lower <= baseline_pass_rate <= upper)
```

Bonferroni correction: splitting the false-positive budget `(1 - confidence)` evenly across the two checkpoints bounds the *combined* false-positive probability across both checks at `(1 - confidence)` (union bound) — simple, stdlib-only, matches this project's stated preference for boring mechanisms over a more powerful but more complex group-sequential boundary (O'Brien-Fleming), which is named as a future refinement if Bonferroni's conservatism costs too much power in practice. When `min_trials == max_trials` (a degenerate single checkpoint), no correction is applied.

`RunStatus` gains one additive `Literal` member, `"skipped"` (verified safe the same way as Feature 1's fields). `Run` gains `skip_reason: str | None = None`.

`run_suite` gains `early_stop: EarlyStopConfig | None = None` (a frozen dataclass co-located in `scheduler.py`, its only consumer — not a separate module, since v1 ships exactly one strategy):

```python
@dataclass(frozen=True)
class EarlyStopConfig:
    min_trials: int
    max_trials: int
    baseline_pass_rate: float
    score_fn: Callable[[EvalCase, Run], Awaitable[bool]]   # ASYNC — see below; must never raise; False for a non-"completed" Run
    confidence: float = 0.95
```

**`score_fn` must be async, not sync — a real bug caught by this RFC's own test suite before shipping, not a design choice made up front.** `run_suite` is already executing inside an event loop by the time it calls `score_fn` (the caller drives it via `asyncio.run(run_suite(...))`). The obvious-looking synchronous signature (`Callable[[EvalCase, Run], bool]`) works fine for the deterministic scorer, but `cli.py`'s `--llm-judge` path needs to await `judge(...)`; its existing helper (`_judge_output`) does that by internally calling `asyncio.run(judge(...))`, which raises `RuntimeError: asyncio.run() cannot be called from a running event loop` the moment it's invoked from inside `run_suite`'s own loop — and that error was being silently swallowed by `_judge_output`'s existing broad `except Exception: return None`, turning every early-stopped judged trial into a silent, wrong `JUDGE_ERROR` with the judge never actually called. A CLI-integration test asserting the real judge-call count caught this immediately. Fixed by making `score_fn` (and `run_suite`'s internal tally/stop bookkeeping) async, and giving `rollout_cmd` a single async-native `_score_and_record` closure (used for both the early-stop and non-early-stop paths, unifying `rollout_cmd`'s whole execution under one `asyncio.run(...)` call) that calls `judge(...)` directly via `await` rather than through the sync-wrapping `_judge_output` helper — which remains completely unchanged, still used by `evals run`'s fully-synchronous path (`_judge_case`). The old `_judge_run` helper (`rollout_cmd`'s prior sync-wrapping caller) became dead code once `_score_and_record` inlined the async call directly, and was removed rather than left unused.

When `early_stop` is `None` (the default), the group-tracking/skip branch is never entered — identical to today. When provided, `run_suite` tracks a running `(successes, trials_run)` tally per `(eval_case_id, eval_case_revision)` group, calling `early_stop.score_fn(case, run)` exactly once per real (non-skipped) trial — including cache hits, which still carry real, scoreable output — and checking `two_checkpoint_early_stop` after each. Once a group's decision fires, every later case in that group becomes a `status="skipped"` `Run` (never attempted, `latency_ms=0.0`) rather than being executed.

**A restructuring, not a pure addition, in one place**: today's `except Exception: ...; continue` on a sandbox-launch failure returns before any shared logic runs (there is none). For early-stopping to work correctly, an errored/timed-out trial must still count as one real trial in its group's tally — otherwise a group hitting a run of transient infra errors would never accumulate evidence and would never legitimately stop. `score_fn`'s contract (mirroring the existing `_score_run_deterministic`/`_judge_run` convention already in `cli.py`, which return `False` for any non-`"completed"` `Run` without being a new decision) makes this the *existing* behavior, extended, not a new judgment call.

**Two bugs a naive implementation of this feature would introduce, and how this design avoids them**: (1) *double-billing* — calling a billed `--llm-judge` scoring function once inside `run_suite` to decide when to stop, then again in `cli.py`'s existing post-hoc scoring loop, would double the exact cost this feature exists to reduce. Avoided by calling `score_fn` (`cli.py`'s `_score_and_record`) exactly once per real trial, either inside `run_suite` (early-stop path) or in `cli.py`'s post-hoc loop (non-early-stop path), never both. (2) *denominator drift* — naively deriving `total` from `len(scores)` would silently exclude `error`/`timed_out` runs (which never get a `Score`) from the reported denominator, diverging from today's behavior where they count in `total` with zero contribution to `successes`. Avoided by incrementing `total` inside `_score_and_record` for every non-`"skipped"` run, matching today exactly, and excluding only genuinely-never-attempted `"skipped"` runs.

`cli.py`'s `rollout_cmd` gains `--early-stop-min-trials`/`--early-stop-max-trials`/`--early-stop-baseline-pass-rate` (all `int`/`int`/`float`, `default=None`), validated together exactly like `report_cmd`'s existing `--successes`/`--total` pairing (`click.UsageError` if only some are given). Reuses the existing `--confidence` flag for the Bonferroni-corrected checkpoint confidence — no new flag needed.

## Drawbacks

- The Bonferroni correction is conservative — it costs some statistical power relative to a more sophisticated group-sequential boundary (O'Brien-Fleming), in exchange for being simple enough to verify by inspection and needing zero new dependencies.
- `run_suite`'s control flow genuinely changes shape (the exception path now also reaches the tally logic) — a real, if small, increase in the function's complexity, not a pure bolt-on.
- The cache's silent limitation (mutable image tags can produce stale hits) is a real, named, unaddressed risk for an operator who doesn't pin image digests.
- Skipped trials forfeit any chance to independently surface a later infra collapse within the same suite invocation — acceptable (that's the point of the feature), but a `"skipped"` `Run` must never be read as "verified healthy," only as "not attempted."

## Alternatives Considered

1. **The originally-floated "recheck the plain Wilson CI after every trial" design** — rejected: the classical optional-stopping/peeking failure mode this RFC's Motivation section documents in detail, not a defensible v1 slice.
2. **Real SPRT / anytime-valid confidence sequences** — the most statistically rigorous fix, researched in depth (Wald 1945; Howard, Ramdas, McAuliffe & Sekhon 2021) — rejected for this RFC because it needs a real scope addition (an operator-supplied alternative pass rate `p1`, not just a baseline) this backlog item never asked for; a defensible future RFC once that need is concretely felt. **Correction, 2026-09-05**: this rejection reason is accurate for classical Wald SPRT specifically, but not for the mixture-SPRT (mSPRT) variant — Robbins 1970, popularized by Johari, Koomen, Pekelis & Walsh (KDD 2017), the exact paper this RFC's own Motivation section already cites. mSPRT needs only the same `baseline_pass_rate` this RFC already asks for, plus a tunable-but-not-correctness-relevant mixing variance — see `docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md`, which implements it, replacing the two-checkpoint design below entirely.
3. **A pluggable `EarlyStopFn` strategy interface with a separate `early_stopping.py` module** — considered (this shape was in the initial research proposal) and rejected as premature generality: v1 ships exactly one strategy, and an abstraction with one implementation is speculative, not requested by anything real yet.
4. **Citing Optuna's median-stopping-rule pruner or Ray Tune's ASHA as precedent** — investigated and explicitly rejected as a category error: both are compute-allocation heuristics across *competing configurations* with no error-rate guarantee, not single-hypothesis tests against a fixed baseline.
5. **Do nothing** — the acknowledged status quo `THREAT_MODEL.md:37` already names as a live gap; rejected because a defensible, stdlib-only v1 slice exists and is worth shipping now.

## Unresolved Questions

- Should the cache key eventually include a harness/runner code version or image digest, closing the named stale-hit risk — deferred until a real incident or a real versioning scheme exists to hash against.
- Should the cache have a TTL (mirroring Inspect AI's default 1-week expiry), or is "the results JSONL file itself" an acceptable indefinite horizon for v1 — this RFC chooses the latter (the operator controls staleness by choosing which `--results` file to point at) given no production telemetry exists yet to calibrate a TTL against.
- ~~Whether Bonferroni's conservatism costs meaningful power in practice~~ — moot as of 2026-09-05: the two-checkpoint, Bonferroni-corrected design this question was about has been replaced entirely by a real mixture-SPRT test, per `docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md`, which has no Bonferroni correction to reconsider.
