# Evals Harness Architecture & Scaling — Round 4 (2026-09-11)

Scope: `evals/`'s eval-harness architecture/scalability lens — Rollout Scheduler, Sandbox Pool,
CI-cost scaling — distinct from judge-science or drift-monitoring. Grounded against
`evals/ARCHITECTURE.md`'s Rollout Scheduler section, `docs/rfcs/2026-09-07-evals-sandbox-pool-
deferred-decision.md` (the asyncio-vs-Ray decision record), a direct count of every
`regression_corpus_*.json` fixture on disk today, and real CI job-timing data pulled live via
`gh api` against this repo's own most recent GitHub Actions run — not estimated. Externally
verified against Inspect AI's current docs/changelog/source, terminal-bench/Harbor's actual
implementation code, Ray's own design-pattern docs, TerminalWorld's methodology appendix, and the
GitHub Actions job-limits reference, each claim adversarially 3-vote-verified before inclusion.

## Executive Summary

**The Ray-revisit trigger has NOT fired — but for a nuanced, evidence-backed reason worth
recording, not a simple "not yet."** The 2026-09-07 RFC's trigger is an explicit compound
condition: a **precondition** (real Docker-sandboxed `evals rollout` must be wired into the
PR-blocking CI gate) **AND** either of two sub-thresholds (CI wall-clock >5min, or regression-tier
case count >30). Directly counting every `regression_corpus_*.json` file on disk today gives
**137 real `tier: "regression"` cases across 6 files** (up from 3 at RFC-writing time, and up from
the 113-cases-across-5-files the CI gate itself already runs) — the case-count sub-threshold is
blown past by roughly 4.5x. But the precondition is still false: a direct grep of
`.github/workflows/ci.yml` shows zero occurrences of `evals rollout` or `run_in_sandbox` anywhere
in CI; the "Real regression corpus gate" step calls `evals run` exclusively — the non-sandboxed,
`task_spec.output`-baked-in, zero-Docker, zero-network code path. Real timing data pulled live from
this repo's most recent CI run (`gh api repos/kelvran/gateway/actions/jobs/103055214447`) confirms
why the case count alone is a misleading signal here: that 113-case regression-corpus-gate step
took **12 seconds** wall-clock (21:26:30Z→21:26:42Z), nowhere near the 5-minute threshold — because
it launches zero containers. So: **build verdict is "not yet"** for the Sandbox Pool /
asyncio-Semaphore upgrade the RFC already fully specified — but the corpus has quietly outgrown
the case-count number's own original calibration (which assumed Docker-timeout math that no longer
applies to this now-entirely-non-sandboxed CI reality), which is worth a lightweight note in
`DECISIONS.md`/the RFC rather than a silent drift. Separately, per-trial fresh-container isolation
(no pooling) is confirmed as the deliberate, still-correct 2026 industry-standard choice — Inspect
AI, Terminal-Bench/Harbor, and TerminalWorld's Harbor harness all do the identical thing at
production scale, so this is not a gap. Current CI cost at 137 cases is negligible (37s total for
the entire `evals` job) and not a real problem to fix today.

## Findings

### Finding 1 — The Ray-revisit trigger's case-count sub-threshold has fired numerically, but the compound trigger as a whole has NOT, because its precondition is still false
**Confidence: high** (direct repo inspection + live CI API data, cross-checked two ways)

`docs/rfcs/2026-09-07-evals-sandbox-pool-deferred-decision.md` defines the trigger as: **Precondition** (a real Docker-sandboxed `evals rollout` regression suite is wired into the PR-blocking `evals report --fail-under` CI gate) **must hold first**, and only then does **either** of (1) CI wall-clock >5min or (2) regression-tier case count >30 fire the revisit.

Direct, current counts (not estimated):

| Corpus file | Cases | Tier |
|---|---|---|
| `regression_corpus_cache_adversarial.json` | 17 | regression |
| `regression_corpus_cost_abuse.json` | 20 | regression |
| `regression_corpus_dogfood.json` | 35 | regression |
| `regression_corpus_guardrail.json` | 24 | regression |
| `regression_corpus_judge_accuracy.json` | 24 | regression |
| `regression_corpus_routing_chaos.json` | 17 | regression |
| **Total** | **137** | — |

`evals/tests/test_regression_corpus_fixtures.py` independently corroborates this shape (`assert len(_regression_corpus_files()) >= 6`), and `STATUS.md`'s own 2026-09-09 entry states the same "137 cases total" figure directly. **The case-count sub-threshold (30) is exceeded by ~4.5x.**

But the precondition is false. `.github/workflows/ci.yml`'s `evals` job has a step named "Real regression corpus gate (113 cases across 5 files)" (113/5, not 137/6, because it deliberately excludes `regression_corpus_judge_accuracy.json` — see Finding 4) that runs:
```
uv run evals run --suite tests/fixtures/regression_corpus_cache_adversarial.json --scores ...
uv run evals run --suite tests/fixtures/regression_corpus_cost_abuse.json --scores ...
... (3 more `evals run` calls)
uv run evals report --scores ... --tier regression --fail-under 0.78 ...
```
Every invocation is `evals run`, never `evals rollout`. A repo-wide grep (`grep -n "evals rollout\|run_in_sandbox\|docker run" .github/workflows/ci.yml`) returns zero matches. `evals run`'s own documented behavior (confirmed in the RFC itself and in `ARCHITECTURE.md`) is that `task_spec.output` is baked into the fixture file — it never calls `run_in_sandbox()`, never touches Docker, never makes a network call. This is why the CI comment can honestly claim "no live LLM calls, no network, no AWS credentials needed."

Real, live-pulled CI job timing confirms the practical consequence: the most recent successful `main` run (`gh run list` → run `34532157106`, job `103055214447`, fetched via `gh api repos/kelvran/gateway/actions/jobs/103055214447`) shows the "Real regression corpus gate (113 cases across 5 files)" step running `21:26:30Z`→`21:26:42Z` — **12 seconds** — and the entire `evals` job (`sync`, `ruff`, `import-linter`, pytest, CI-gate smoke test, and the regression-corpus gate combined) running `21:26:06Z`→`21:26:43Z` — **37 seconds** total. Both are two orders of magnitude under the 5-minute wall-clock threshold, and this is expected, not surprising: launching zero containers for 113 (or 137) cases costs essentially nothing.

**Verdict: NOT YET.** The compound trigger, as designed, has not fired. The RFC's own reasoning explicitly anticipated needing a precondition precisely to avoid a shallow "case count alone" false trigger — and that is exactly what would happen if this were read naively (137 > 30 "looks" fired). The correct, precondition-aware reading is that nothing has changed about whether a Sandbox Pool is needed, because the thing the case-count number was actually trying to measure (real sandboxed wall-clock cost) still doesn't exist in CI. **Action recommended, not urgent:** the 30-case number in the RFC was explicitly calibrated against `DEFAULT_SANDBOX_TIMEOUT_S = 30`-driven worst-case Docker-timeout math ("even in the fully degenerate case... 15 minutes") — that calibration assumed a sandboxed corpus, and the corpus that actually grew past 30 is a non-sandboxed one. This doesn't invalidate the RFC's design, but it's worth a one-line addendum in `DECISIONS.md` (not a re-litigation) noting that the case-count figure will need re-deriving against real sandboxed-corpus data once the precondition is ever wired, rather than reusing 30 unexamined.

### Finding 2 — If/when the precondition is met, the RFC's already-specified asyncio.Semaphore design is correct, not Ray — reaffirmed, not merely repeated, by six independent 2026 sources
**Confidence: high** (Inspect AI docs/changelog/source, Ray's own docs, Harbor's live implementation, all directly verified)

Since the trigger hasn't fired, no migration is due — but the RFC already fully specifies the shape a future implementer should build (a keyword-only `max_concurrent_sandboxes: int = 1` parameter on `run_suite`, an `asyncio.Semaphore` acquired around each real `run_in_sandbox()` call, pre-allocated `list[Run | None]` to preserve output order under concurrency, and a per-group `asyncio.Lock` to guard the mSPRT tally race). This round's external research reaffirms that design more strongly than the original RFC could, because it checked sources at genuinely larger scale than Kelvran's:

- **Inspect AI** (UK AISI's production eval framework) ships zero Ray/Dask/Celery/distributed-scheduler code anywhere — concurrency for tasks, samples, sandboxes, subprocesses, and model-API connections is uniformly a single-process, named-limit mechanism (`max_sandboxes`, `max_subprocesses`, `max_tasks`, `max_samples`, `max_connections`), several of which are even retunable **mid-flight** via a per-process AF_UNIX control server (`inspect ctl config --max-tasks`) — a materially more sophisticated version of the same "bounded local concurrency knob" pattern the RFC proposes, still with no distributed cluster anywhere.
- **Harbor** (the actual engine behind terminal-bench-2, a 2026 production terminal-agent benchmark) implements its `--n-concurrent` flag via literally `asyncio.Semaphore(n_concurrent)` in `src/harbor/trial/queue.py` — confirmed by reading the real source, not just the README — with zero `ray` dependency anywhere in its `pyproject.toml`. This is direct, current, same-domain (sandboxed-agent-trial orchestration) proof that bounded `asyncio.Semaphore` concurrency is what a 2026 production harness actually ships, not just what Kelvran's RFC guessed was sufficient.
- **Ray's own documentation** independently corroborates why this is the right call rather than a compromise: Ray's design-patterns docs explicitly warn that "parallelizing or distributing tasks usually comes with higher overhead than an ordinary function call," and that over-parallelizing fast-running work makes programs *slower* than the sequential version. Kelvran's `run_in_sandbox()` calls are I/O-bound (waiting on a Docker daemon subprocess), which is exactly the class of workload `asyncio.Semaphore` is designed for and Ray's own docs caution against reaching for reflexively.

Net: nothing in this round's research surfaces new evidence that Ray would be the right choice at any scale Kelvran is plausibly approaching. The RFC's rejection of Ray (multi-host distribution and actor supervision have "no purchase" on single-host, single-digit-to-low-double-digit concurrency) is, if anything, *underselling* how settled this is — Harbor's real implementation shows even a benchmark harness that *also* supports massively-parallel cloud-provider execution (Daytona, Modal, etc., via `--n-concurrent 100`) still bounds its local orchestration layer with a plain semaphore, layering cloud-scale-out on top rather than replacing the semaphore with a distributed scheduler.

### Finding 3 — Fresh-per-trial Docker containers (no pooling) is the deliberate, still-correct 2026 industry-standard isolation choice, not a gap
**Confidence: high** (4 independent primary sources: Inspect AI docs+source, Terminal-Bench, Harbor, TerminalWorld)

Kelvran's `sandbox.py` launches one fresh container per case (`docker run --rm --network=none --read-only --tmpfs=/tmp:rw,exec,nosuid,size=64m`, torn down via `--rm` immediately after) with zero pooling or reuse across cases or trials. This round checked whether any current production eval-harness design would flag that as a real cost/latency gap at scale, and the answer across every source checked is no:

- **Inspect AI**: "each sample gets its own sandbox instance... So samples do not interfere with each other's sandboxes" — confirmed both in docs and by reading the actual `sandboxenv_context()`/`init_sandbox_environments_sample()` source, which creates and tears down a fresh `SandboxEnvironment` for every individual `(sample, epoch)` pair. Only `task_init()`/`task_cleanup()` (image pulls) are amortized — a coarser, distinct optimization that doesn't touch per-trial container freshness.
- **Terminal-Bench / Harbor**: TerminalWorld's methodology appendix states verbatim that Harbor "provisions a fresh Docker container per task" and explicitly configures `environment.delete = true` ("prune containers after each trial to prevent resource accumulation") while caching only the built *image* (`environment.force_build = false`), not the container instance.
- Cost is managed via **concurrency caps**, not pooling, everywhere checked (Inspect's `max_sandboxes` defaulting to 2x processor count creates an effective global `max_samples` ceiling; Harbor's `--n-concurrent`) — the industry's answer to "containers are expensive" is "bound how many run at once," never "reuse the same one across trials."

Kelvran's isolation posture matches this pattern exactly. **Build verdict: not applicable / no gap.** This is a correct, already-aligned design choice, not deferred work.

### Finding 4 — Current CI cost at 137 cases is negligible; the real, distinct gap is a stale 113-vs-137 count in the CI comment, not a cost problem
**Confidence: high** (live CI timing data + direct diff between disk and CI-wired corpus)

At 137 real regression-tier cases on disk (113 of them wired into the CI gate), the entire `evals` CI job runs in 37 seconds. `--use-cache`/`--use-score-cache` appear nowhere in `.github/workflows/ci.yml` (confirmed by grep) — and correctly so: those flags exist to avoid redundant `run_in_sandbox()` calls or redundant LLM-judge calls, and CI's regression-corpus gate makes neither (it's pure `evals run` against baked-in outputs). There is no real CI-cost or wall-clock gap to flag at this corpus size, distinct from the Ray question in Finding 1.

The one real, concrete discrepancy this pass surfaced: the CI step's own comment says "113 cases across 5 files," while the disk corpus has grown to 137 cases across 6 files — the 24-case gap is entirely `regression_corpus_judge_accuracy.json`, which is **deliberately** excluded from the deterministic PR-blocking gate (it requires live AWS Bedrock judge calls, which would introduce real sampling noise, cost, and credential requirements the other 5 files' "zero live LLM calls, no network, no AWS credentials" property explicitly avoids) and is instead handled by a separate `.github/workflows/evals-judge-nightly.yml` workflow. This is correct architecture, not an oversight — but the CI comment's case-count number is now stale relative to the full on-disk corpus and worth a one-line correction next time that file is touched, to avoid a future reader assuming "113 across 5" is still the complete regression corpus.

### Finding 5 — Two smaller, real observations from this pass
**Confidence: medium** (doc-consistency observation + a durable statistical-foundation check, not new architecture findings)

- `evals/ARCHITECTURE.md`'s Tech Stack table still reads "Orchestration | `asyncio` at v1; Ray Core actors once concurrent-rollout count justifies it" — unchanged since before the 2026-09-07 RFC resolved exactly what "justifies it" means with a concrete compound trigger. This is a minor instance of this project's own already-named, recurring "doc-vs-code staleness" gotcha (`AGENTS.md`'s Gotchas section: "promoted here... recurred 6 times") — the Tech Stack row doesn't contradict the RFC, it's just vaguer than the RFC it predates and should eventually point at it rather than repeat the open-ended phrasing verbatim.
- GitHub Actions' real hard ceiling for any single CI job is 6 hours (confirmed live against `docs.github.com/en/actions/reference/limits`) — the actual forcing function that would eventually require sharding/matrix parallelism for a sequential Docker-sandboxed suite, entirely distinct from and far looser than the RFC's own deliberately tighter 5-minute/30-case thresholds (which are calibrated to PR-turnaround discipline, not to avoiding an outright CI failure). Worth naming for completeness; not remotely close to relevant at today's 37-second job time.
- The mSPRT early-stopping mechanism `evals` already ships rests on a well-established, mainstream statistical foundation (Johari/Pekelis/Walsh's always-valid-inference framework, contrasted correctly against the separate Bayesian-continuous-monitoring literature) — no red flags surfaced against the already-shipped design; this is a reinforcement of existing confidence, not a new build item.

## Caveats

- The RFC's trigger is a compound AND/OR condition; a shallow read of "case count > 30" alone, without checking the precondition, would have produced a false "trigger fired" conclusion. This report deliberately checked both halves independently (grep of CI YAML for `evals rollout`/`run_in_sandbox`, plus live `gh api` timing data) rather than inferring the precondition's status from the case count.
- CI timing was pulled from a single recent representative run (`34532157106`, a release-cut commit touching no evals code) — this is a fair sample for the "does this step even approach 5 minutes" question given the step's cost is dominated by fixed per-case work (no sandboxing) rather than by which commit triggered it, but it is one data point, not an aggregate over many runs.
- `regression_corpus_judge_accuracy.json`'s exclusion from the deterministic gate was independently verified as deliberate (separate nightly workflow exists, live-Bedrock-panel-verified corpus, no live-call flakiness in the PR-blocking path) — this was checked to rule out mistaking it for a second real gap alongside the Ray-trigger question, not because the research task asked about it directly.
- Kelvran's evals module lives in a workspace with some forward-dated `DECISIONS.md` entries (one dated 2026-09-15) relative to today's 2026-09-11 date context; this did not affect any finding in this report (all evidence cited is dated on or before 2026-09-11 and independently verified against live repo state / live CI data), but is noted for transparency since it was visible during research.

## Open Questions

1. When the precondition is eventually met (a real `evals rollout`-driven suite gets wired into the PR-blocking gate), should the 30-case sub-threshold be re-derived from scratch against real sandboxed timing data, or is there enough signal already (e.g. from the 2026-09-11 manual end-to-end Docker-sandbox rollout mentioned in `docs/agents/LOGS.md`) to propose a revised number now rather than waiting?
2. Is there an intentional roadmap item to promote `regression_corpus_judge_accuracy.json`-style live-judge cases into the PR-blocking gate at some point (with sampling-noise-aware thresholds), or is the nightly-workflow split meant to be permanent?
3. Given Harbor's real-world pattern of layering cloud-scale-out (`--n-concurrent 100` against remote sandbox providers) on top of a local `asyncio.Semaphore`, would Kelvran's own eventual Sandbox Pool ever want a similar two-layer design (local semaphore + optional remote-provider fan-out), or is that explicitly out of scope per `PRD.md`'s "massively parallel (10K+) eval rollouts" non-goal regardless of how it's implemented?
4. Should the CI step comment's "113 cases across 5 files" be corrected to "137 across 6" now (a small doc-accuracy fix) even though wiring the 6th file's cases into the *gate itself* remains correctly deferred to the nightly workflow?
