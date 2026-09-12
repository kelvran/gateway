# evals — Architecture

Python service. Runs continuous, statistically rigorous evaluation of the agents/models routed through `gateway`. Never sits in the request path — it calls `gateway`'s API for offline rollouts and samples `gateway`'s production telemetry for online regression detection. For the whole-system view, see the root `ARCHITECTURE.md`.

## Package Layout

```
evals/
  pyproject.toml
  uv.lock                    — own workspace manager; zero cross-awareness with the Go side
  .importlinter               — real as of 2026-09-05, per gateway's own equivalent
                                 `go-arch-lint` wiring: 1 layers contract (cli >
                                 {rollout.scheduler, ingestion.decode, ingestion.object_store,
                                 ingestion.mapping} > {tracing, results_store, rollout.cache,
                                 rollout.sandbox, judge.*, audit_corpus, auto_flag,
                                 field_swap_lint, corpus_staleness, trend_alert} > {models,
                                 stats}, built from the real, verified import graph, not
                                 assumed) plus 2 independence contracts (judge's 3 scoring
                                 strategies never depend on each other; rollout's cache and
                                 sandbox stay decoupled). **Corrected 2026-09-12**: this
                                 paraphrase previously omitted `ingestion.mapping` from layer 2
                                 entirely, and listed layer 3 as only `judge.*`, silently
                                 dropping
                                 `audit_corpus`/`auto_flag`/`field_swap_lint`/`corpus_staleness`/`trend_alert`
                                 — all 6 are real modules the live `.importlinter` file already
                                 lists that this prose paraphrase never caught up to; see this
                                 file's own Package Layout entries below for each.
                                 `evals/contracts/` (generated, no `__init__.py`, PEP 420 namespace
                                 package) is deliberately left undeclared — no layering constraint
                                 makes sense for generated code nobody hand-edits. Wired into
                                 `.github/workflows/ci.yml`'s `evals` job and `make lint-evals` via
                                 `uvx --from import-linter lint-imports`, matching the existing
                                 `uvx ruff check .` convention rather than adding a permanent
                                 `pyproject.toml` dependency for a CI-time-only static checker
  evals/
    contracts/                — GENERATED Python stubs from api/*.proto — no source dependency, ever.
                                 Real for gatewayevents/v1; empty for otel/ (deliberately not built —
                                 see api/README.md)
    ingestion/                 — `decode.py`: real, v1, per docs/rfcs/2026-09-03-api-gatewayevents-
                                 contract.md — decodes a checked-in gatewayevents fixture via the
                                 generated bindings, the golden-fixture round-trip test
                                 docs/testing/TESTING.md §5 promised. `object_store.py`: real, per
                                 docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md — lists/
                                 reads gatewayevents_v1 objects from either S3 (shipped there by
                                 docs/operations/vector-gatewayevents-s3.yaml, a Vector DaemonSet
                                 tailing gateway pods' stdout, never touching gateway's request-serving
                                 path) or GCS (docs/operations/vector-gatewayevents-gcs.yaml, the same
                                 shipper pattern via Vector's `gcp_cloud_storage` sink — added
                                 2026-09-07 as that RFC's own addendum records, no longer the
                                 not-yet-built follow-on its original Alternatives Considered section
                                 named), calling `decode.py` directly for the actual wire-format decode
                                 rather than duplicating it. `parse_object_storage_uri` dispatches on
                                 the source URI's scheme (`s3://`/`gs://`); `list_object_keys`/
                                 `iter_object_lines` each route internally to a `boto3`- or
                                 `google-cloud-storage`-backed helper, so `evals ingest --source
                                 <s3://...|gs://...> --out <path>` is the one CLI entry point for
                                 both clouds. `mapping.py`: real, per
                                 `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md`'s
                                 own resolved Unresolved Questions entry (per `DECISIONS.md`) —
                                 `gateway_decision_event_to_eval_case_and_run` maps one decoded
                                 `GatewayDecisionEvent` into the `EvalCase`+`Run` pair `evals
                                 promote` already reads from any other source; called per
                                 decoded event by `evals ingest`'s own `--suite`/`--results`
                                 options. Honest about what the schema can't supply:
                                 `Run.stdout`/`EvalCase.reference` stay empty/`None`,
                                 `Run.latency_ms` is `0.0` (the same no-real-timing sentinel
                                 `rollout/scheduler.py` already uses), and `EvalCase.tier` is
                                 always `"drift_sample"` — the one promotion tier that never
                                 requires a pass/fail judge `Score`, per
                                 `docs/rfcs/2026-09-05-evals-golden-regression-promotion.md`'s
                                 own "Hard-requiring --scores always" rejection
    results_store.py           — real, v1, per docs/rfcs/2026-09-04-evals-score-model.md: append-only
                                 JSONL persistence, generic over any frozen pydantic model this package
                                 produces (`Run`, `Score`, `Span`). Moved here from `rollout/` — the
                                 mechanism was never rollout-specific, only its first caller (`Run`) was;
                                 `evals run` (no rollout, no sandbox) is `Score`'s other, equally valid caller
    tracing.py                  — real, v1, per docs/rfcs/2026-09-04-evals-trace-span-model.md: a
                                 self-contained, no-exporter OTel Python SDK span capture for real
                                 sandbox executions. Holds its own local `TracerProvider` — never the
                                 process-wide `trace.set_tracer_provider()` singleton
    rollout/                   — real, v1, per docs/rfcs/2026-09-04-evals-rollout-scheduler.md:
                                 `sandbox.py`'s Docker-sandboxed execution wrapper and `scheduler.py`'s
                                 sequential Rollout Scheduler (one EvalCase -> one run_in_sandbox() call
                                 -> one Run, no concurrency/pool this pass). Sandbox Pool (concurrency)
                                 and a Task/Dataset Registry remain unbuilt — diagram-only, per that
                                 RFC's own scope boundary
    judge/                     — real, v1+panel: `deterministic.py` (exact/regex matching),
                                 `llm_judge.py` (single-judge scoring per docs/rfcs/2026-09-04-evals-
                                 llm-judge-provider-wiring.md, PLUS a real multi-judge panel reducer
                                 per docs/rfcs/2026-09-08-evals-judge-panel-reducer.md), `providers.py`
                                 (Anthropic + OpenAI + Bedrock call_model wiring — Bedrock added
                                 2026-09-08, backing --llm-judge-panel's 2-judge Sonnet-5+Haiku-4.5
                                 pair), `cache.py` (score-level result caching, per
                                 docs/rfcs/2026-09-05-evals-score-cache.md). **Corrected 2026-09-05**:
                                 this entry previously claimed bootstrap resampling/Bayesian model
                                 comparison/pass@k reliability live here — none of that exists anywhere
                                 in the repo (confirmed: zero matches for bootstrap/bayesian/pass@k
                                 outside tests), and this package has never contained any statistics
                                 code at all. The real, shipped statistics (`wilson_interval`,
                                 `mixture_sprt_early_stop`) live in a separate top-level `stats.py`
                                 module (not listed in this tree, matching its own existing convention
                                 of also omitting `models.py`/`cli.py`), imported directly by `cli.py`
                                 and by `rollout/scheduler.py`, never by anything under `judge/`.
                                 Bootstrap resampling/Bayesian model comparison remain unbuilt, per the
                                 Tech Stack table's own already-correct "not installed, not built" note
    audit_corpus.py            — real, per `docs/rfcs/2026-09-11-evals-audit-corpus.md`:
                                 `audit_case`/`build_audit_prompt`/`parse_audit_response` — a
                                 static, report-only LLM audit of a regression-corpus case's
                                 OWN DESIGN (ambiguous task framing, wrong/unverifiable ground
                                 truth, an environment/tooling conflict), never a judge verdict
                                 on a candidate output. Deliberately never calls
                                 `evals.judge.llm_judge.judge()` — auditing the corpus with the
                                 same mechanism that will later judge cases drawn from it would
                                 make the audit's own blind spots identical to the thing being
                                 audited. Wired via `evals audit-corpus --suite PATH [...]
                                 --out PATH [--judge-model] [--record-trend]`; never a
                                 `--fail-under`-style gate. Flat, third-`.importlinter`-layer,
                                 sibling-independent of `judge.*` on that same `|`-joined layer
                                 bullet
    auto_flag.py               — real, per `docs/rfcs/2026-09-15-evals-drift-auto-flag.md`:
                                 `FlagRule`/`flag_candidates` — a rule-based pass over
                                 already-ingested `drift_sample`-tier `EvalCase`s (v1 ships one
                                 rule, `gateway-failure-outcome`, matching four gateway-failure
                                 `task_spec["outcome"]` values), surfacing promotion CANDIDATES
                                 for the still-fully-human-triggered `evals promote` — never
                                 constructs an `EvalCase`, never promotes anything itself.
                                 Reads `task_spec["outcome"]`, never the lossier `Run.status`,
                                 since `ingestion/mapping.py` collapses all nine non-OK
                                 `GatewayDecisionEvent.Outcome` values into one
                                 `Run.status="error"` string. Wired via `evals flag-candidates
                                 --suite PATH --results PATH [--out]`. Flat, third-layer,
                                 sibling to `evals.audit_corpus`
    field_swap_lint.py         — real, per `DECISIONS.md`'s 2026-09-11 Round-4 Phase 3 entry:
                                 `find_reference_swaps` detects the exact guardrail-13/18 bug
                                 class — two DISTINCT `EvalCase`s in one file whose `reference`
                                 values were transposed with each other — via a pure crosswise
                                 value-swap signature, deliberately never a
                                 task_spec-similarity heuristic. CI-gating, not its own CLI
                                 command: wired as a real pytest assertion in
                                 `test_regression_corpus_fixtures.py`. Flat, third-layer,
                                 sibling to `evals.audit_corpus`/`evals.auto_flag`
    corpus_staleness.py        — real, per `DECISIONS.md`'s 2026-09-11 Round-4 Phase 3 entry:
                                 `check_case_staleness`/`parse_citations` — report-only; flags
                                 a `task_spec["verified_against"]` citation whose cited file
                                 was touched (`git log`) more recently than the corpus case's
                                 own line (`git log -L`), a possible-staleness signal only,
                                 never a hard failure, for the same false-positive reasons
                                 `audit_corpus` stays report-only. Wired via `evals
                                 check-corpus-staleness --suite PATH [...] [--repo-root]
                                 [--out]`. Flat, third-layer, sibling to
                                 `evals.audit_corpus`/`evals.auto_flag`
    trend_alert.py             — real, per
                                 `docs/upgrade-research/evals-continuous-monitoring-2026-09-11.md`
                                 Finding 3 (`DECISIONS.md`'s 2026-09-12 Phase 2,
                                 v2-upgrade-research plan):
                                 `TrendAlertRule`/`TrendAlert`/`check_trend_alerts` —
                                 static-threshold-only alerting (an operator-supplied numeric
                                 bound vs. the mean of the most recent `window` snapshots for a
                                 series), deliberately never a statistical/DDM-style drift
                                 detector, matching every 2026 platform surveyed. **Corrected
                                 2026-09-12, per `DECISIONS.md`'s Round 6 Phase 3**: triggered
                                 alerts also carry a pooled Wilson 95% CI via
                                 `evals.stats.wilson_interval` (`None` for `cost_usd`, which
                                 has no success/total concept), closing a literal `PRD.md`
                                 "never a bare percentage" violation. Wired via `evals trend
                                 alert --path PATH --threshold
                                 SERIES:DIRECTION:VALUE[:SEVERITY] [--window] [--out]
                                 [--notify-webhook]`. Flat, third-layer, sibling to
                                 `evals.audit_corpus`/`evals.auto_flag`/`evals.field_swap_lint`/`evals.corpus_staleness`
```

**Dependency direction rules:**

```
evals    → api/otel, api/gatewayevents        (versioned, exported contracts only)
evals    ✗→ apps/gateway/internal/*            (no source dependency on Go internals — different
                                                 language, different lifecycle)
gateway/cache ✗→ evals                         (never, under any circumstance)
```

## Rollout Lifecycle

```
[Task/Dataset Registry] → [Rollout Scheduler] → [Sandbox Pool] → [Trace Collector]
        ↑                                                              │
        │ promote failing trace                                       ▼
        │                                                     [Scorer Service]
        │                                             (deterministic → LLM-judge → skeptic panel*)
        │                                                              │
        │                                                              ▼
[Golden/Regression Dataset] ←──────────────────────────────── [Stats Engine: CI / power / pass^k]
        │                                                              │
        ▼                                                              ▼
   [CI/CD Gate] ← blocks/allows deploy                     [Results Store + Dashboard]
        ▲                                                              │
        │                                                              ▼
[Online Eval Service: shadow / canary / drift] ←── production traffic sample (from gateway)
```

**Real today**: `Rollout Scheduler` (sequential — no `Sandbox Pool` yet) and a flat-file `Results Store` (no Dashboard), per `docs/rfcs/2026-09-04-evals-rollout-scheduler.md`. `Scorer Service` is real for both `deterministic` and single-provider `llm_judge` (Anthropic + OpenAI, per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md` — `cli.py` still wires only Anthropic by default, no skeptic-panel wiring), with every judged verdict now persisted as a real `Score` (`docs/rfcs/2026-09-04-evals-score-model.md`), not just printed. The Rollout Scheduler also has real, opt-in cost/DoS mitigation: a Run-level result cache (`rollout --use-cache`, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`) and a real mixture-SPRT (anytime-valid) early-stopping rule (`--early-stop-*`, per `docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md` — upgraded 2026-09-05 from that first RFC's original two-checkpoint, Bonferroni-corrected design) for repeated trials — both off by default. A second, independent opt-in cache exists one layer up, at the judge call itself: `--use-score-cache` (both `evals run`/`evals rollout`), per `docs/rfcs/2026-09-05-evals-score-cache.md`, skips re-calling the LLM judge for a case whose `(output, reference, scorer_id)` already has a real `Score` on file. `llm_judge`'s single holistic verdict can also be split into independent per-axis verdicts via `--judge-axes` (one real judge call per configured axis, AND-combined into the case-level pass/fail), per `docs/rfcs/2026-09-05-evals-multi-axis-judging.md`; omitting the flag reproduces the original single-verdict behavior exactly. The `Stats Engine` gained `mixture_sprt_early_stop` alongside `wilson_interval`; no power calculation or bootstrap resampling beyond that. **`CI/CD Gate` is now partially real, per `docs/rfcs/2026-09-05-evals-report-fail-under.md`**: `evals report --fail-under <rate>` exits non-zero when a reported group's Wilson lower bound (not the bare point estimate — a deliberately narrower, already-CI-backed check, not the full power calculation this section's own principle above names as the aspiration) falls below the threshold. **Corrected 2026-09-07: the "not yet wired into this repo's own CI" clause immediately below this correction was itself stale** — `DECISIONS.md`'s 2026-09-06 Phase 1e entry already wired a dogfooding-smoke-test invocation into `.github/workflows/ci.yml`'s `evals` job, but that fact was never reflected back into this file, a real instance of this project's own documented "Doc-vs-code staleness" gotcha, caught while implementing the three refinements described next. Per `docs/rfcs/2026-09-07-evals-cigate-refinements.md`, `report_cmd` gained: (1) `--tier {golden,regression,drift_sample}`, filtering to `Score`s whose denormalized tier matches, scoped to `--scores` mode only — CI's own smoke-test step now runs with `--tier regression` explicitly, against a purpose-built fixture (`tests/fixtures/ci_gate_example.json`) rather than the untiered `golden_example.json`; (2) repeatable `--category-fail-under TAG:THRESHOLD`, an independent, per-`scorer_type` gate scoped to `Score`s whose denormalized `tags` contains `TAG`, checked in addition to (never blended with, never required alongside) the aggregate `--fail-under`; (3) `EvalCase.flaky`/denormalized `Score.flaky` — a flaky-tagged case still runs and is still printed, but is excluded from every gate computation (aggregate and category, uniformly) it would otherwise count toward. `tier`/`tags`/`flaky` are captured on `Score` at the exact moment `run_cmd`/`rollout_cmd` construct it (mirroring `cache_key`/`score_cache_key`'s own "compute once, at write time" precedent), not joined back to a `--suite` file at report time. `evals/evals/stats.py` is unchanged — every new gate reuses the existing `wilson_interval(successes, total)` call directly, never a new per-category entry point, since `stats.py` sits in this package's bottom import-linter layer specifically to stay free of any `evals.models` dependency. **`Golden/Regression Dataset` promotion is now partially real, per `docs/rfcs/2026-09-05-evals-golden-regression-promotion.md`**: `evals promote --run-id <id> --tier {regression,drift_sample}` joins a `Run` with its `Score` and its source `EvalCase` (frozen at the exact revision the Run used) into a new, distinct `EvalCase` appended to a target suite file — `--tier golden` is not a valid choice at all (Click's own `Choice` type excludes it), and `--tier regression` requires at least one matching failing `Score` when `--scores` is given. Still manual (a human runs the command; nothing auto-promotes), and there's no "production trace → regression dataset" pipeline this diagram's arrow implies — only the promotion mechanism itself. **Corrected 2026-09-07**: `Trace Collector` is no longer purely diagram-only — `evals ingest --source s3://...` (per `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md`) is a real, tested, but narrow slice of it: listing/decoding a sampled `gatewayevents_v1` stream that a separate Vector shipper (`docs/operations/vector-gatewayevents-s3.yaml`) already delivered to S3. **Addendum, same day:** `evals ingest --source gs://...` is now the same real slice against GCS instead, shipped there by the sibling `docs/operations/vector-gatewayevents-gcs.yaml` config — one command, one code path, dispatching on the `--source` scheme. Both carry only `GatewayDecisionEvent`'s narrow outcome/trace-ID schema, never full span/trace content. **Corrected 2026-09-07 (later still, same day): the "neither cloud feeds `evals promote`" claim above is now false — that RFC's own "Unresolved Questions" entry on this exact point is resolved.** The project owner decided the policy: live-sampled production data feeds `evals promote` the same as any other run source, with no separate review path (`DECISIONS.md`). `evals ingest`'s own new `--suite`/`--results` options (additive; omitting both reproduces the original decode-only behavior exactly) map each successfully-decoded event, via `evals.ingestion.mapping.gateway_decision_event_to_eval_case_and_run`, into the same `EvalCase`+`Run` shapes `evals promote` already reads from a real `rollout`. The mapping is honest about what a `GatewayDecisionEvent` cannot supply — no prompt/completion content exists anywhere in that schema, so the synthetic `Run.stdout` is `""` and `EvalCase.reference` is `None`, never fabricated — which is exactly why the synthetic `EvalCase.tier` is always `"drift_sample"`: the one promotion tier that never requires a pass/fail judge `Score` (per the promotion RFC's own "Hard-requiring --scores always" rejection). `--tier regression` still runs against an ingested `Run` (no special-casing, per the "no separate review path" policy) but can never honestly clear that tier's own failing-`Score` precondition, since no `Score` was or could be produced for output that was never captured. `Task/Dataset Registry` and `Online Eval Service` remain fully diagram-only. **Added 2026-09-07, per `docs/rfcs/2026-09-07-evals-regression-corpus-conventions.md`'s Phase 1:** `tests/fixtures/regression_corpus_dogfood.json` is a new, separate `tier: regression` fixture of 7 hand-authored `EvalCase`s mined directly from `docs/agents/LOGS.md` (`category:*` + `provenance:dogfood` tagged) — a dogfood-provenance seed corpus documenting already-fixed bugs for corpus-building and cross-harness visibility, not a comprehensive suite, and deliberately not wired into `ci_gate_example.json` or CI (that wiring is Phase 3, still gated on reaching a meaningful case count). **Added 2026-09-07 (Phase 2, same RFC):** `tests/fixtures/regression_corpus_guardrail.json` is a hand-curated, human-reviewed batch of 13 `category:guardrail`/`provenance:hand-written` cases targeting prompt-injection and cross-tenant/cache-poisoning-adjacent attack patterns against `gateway/internal/guardrail`'s real detectors — each case's expected verdict was verified directly against the live classifier code, including three honestly-named cases where it currently does not catch an attack it arguably should. Also not wired into any CI gate or `evals report` invocation yet, for the same Phase 3 reason. **Corrected 2026-09-08, per `docs/rfcs/2026-09-08-evals-judge-panel-reducer.md`: the "no skeptic-panel wiring" clause earlier in this paragraph is now stale.** `cli.py` wires a real 2-judge panel via `--llm-judge-panel`, majority-reduced — **revised same day**: both judges are Claude models (Sonnet 5 + Haiku 4.5) served via AWS Bedrock, not the originally-implemented Anthropic-direct + OpenAI pairing, an explicit, accepted same-vendor-correlation tradeoff for AWS-only operational simplicity (see that RFC's own honest accounting). A judge-accuracy corpus slice to exercise this panel against real cases, and a scheduled CI workflow to run it, are both specified but not yet built — see that RFC's own "Not yet done" section for exactly what's gated on live AWS Bedrock access this implementation pass didn't have. **Corrected 2026-09-08 (later still, same day, once a real AWS credential existed to test against): three more clauses above are now stale.** (1) `cli.py` no longer wires only Anthropic by default for standalone `--llm-judge` — it now wires the exact same Bedrock Haiku 4.5 model id as the panel's own Haiku panelist (a deliberate choice: it reactivates the cache-key design's cross-mode reuse property, which had no real code path while the two modes used disjoint providers — a real, pre-existing gap in `_load_cached_scores` this exact reactivation exposed and fixed, since that function only ever read prior `llm_judge` Scores, never a prior `llm_judge_panel` Score's own embedded `panel_votes`). No `ANTHROPIC_API_KEY` is used by `evals` for judging at all anymore. (2) The judge-accuracy corpus (Phase 4) is now built and live-verified — `tests/fixtures/regression_corpus_judge_accuracy.json`, 24 cases — against real Bedrock access. (3) A real, live full-pipeline dry run of the nightly workflow surfaced a genuine design flaw in its own `--fail-under 0.60`/`--category-fail-under "category:judge:0.60"` gate: this corpus is deliberately half designed-to-FAIL, so a correctly-performing judge's raw pass_rate lands around 40-55%, not near the ~67% floor that gate's own justification assumed — fixed by removing that gate from the nightly workflow entirely (a real accuracy-vs-designed-label metric would be needed for a meaningful gate here, separately-scoped future work) rather than tuning the threshold number, which would have just weakened an already-wrong check. **Corrected 2026-09-12: this section never caught up with 6 real corpus-governance/continuous-monitoring CLI commands that shipped alongside the modules described in the Package Layout above — another real instance of this project's own documented "Doc-vs-code staleness" gotcha (`AGENTS.md`'s Gotchas entry).** **Added 2026-09-11, per `docs/rfcs/2026-09-11-evals-audit-corpus.md`:** `evals audit-corpus --suite PATH [--suite PATH ...] --out PATH [--judge-model MODEL_ID] [--record-trend PATH]` runs a real LLM audit pass over one or more regression-corpus suite files for the CORPUS's own design defects (ambiguous task framing, wrong/unverifiable ground truth, environment/tooling conflicts) — never a judge verdict on a candidate output, and never wired to any `--fail-under`-style gate; findings are for a human to review. **Added 2026-09-11, per `DECISIONS.md`'s 2026-09-11 Round-4 Phase 3 entry:** `evals check-corpus-staleness --suite PATH [--suite PATH ...] [--repo-root PATH] [--out PATH]` flags a `task_spec["verified_against"]` citation whose cited file was touched (via `git log`) more recently than the corpus case's own line (via `git log -L`) — a possible-staleness signal only, never a hard failure, for the same false-positive reasons `audit-corpus` is report-only. **Added 2026-09-12, per `docs/rfcs/2026-09-14-evals-quality-trend.md` and `DECISIONS.md`'s 2026-09-12 Phase 2 (v2-upgrade-research plan) entry:** persisted `TrendSnapshot` history (`--record-trend PATH`, opt-in on both `report` and `audit-corpus`) can be read back via `evals trend show --path PATH [--series NAME]` (prints each of `judge_accuracy_kappa`/`quote_grounding_rate`/`audit_corpus_defect_rate`/`judge_panel_tie_rate`/`cost_usd`'s own history, never averaged or fused across series) and alerted on via `evals trend alert --path PATH --threshold SERIES:DIRECTION:VALUE[:SEVERITY] [--window N] [--out PATH] [--notify-webhook URL]` — a static, operator-supplied threshold against the mean of the most recent `--window` snapshots (default 5), never a statistical/DDM-style drift detector, matching every 2026 platform surveyed for this feature. **Corrected 2026-09-12, per `DECISIONS.md`'s Round 6 Phase 3:** `trend alert`'s triggered alerts now also carry a pooled Wilson 95% CI, closing a literal `PRD.md` "never a bare percentage" violation the bare `window_mean` line originally shipped with. **Added 2026-09-15, per `docs/rfcs/2026-09-15-evals-drift-auto-flag.md`:** `evals flag-candidates --suite PATH --results PATH [--out PATH]` is a rule-based pass over already-ingested `drift_sample`-tier `EvalCase`s (v1 ships one rule, matching four gateway-failure `task_spec["outcome"]` values) that prints promotion CANDIDATES plus the exact `evals promote` command to run for each — never a new, automated promotion path; `evals promote` stays the only real promotion mechanism. **Added 2026-09-12, per `DECISIONS.md`'s Round 6 Phase 6 entry and its new `docs/rfcs/2026-09-12-gateway-cost-attribution-aggregation.md`:** `evals cost-report --source <s3://...|gs://...> --agent-run-id ID` sums `cost_usd` (parsed as `Decimal`) across ingested `gatewayevents_v1` objects, scoped to the one requested `agent_run_id` — the minimum-viable "why did this agent run cost $X" answer, reusing `ingest`'s own list/read/decode primitives rather than building new fetch logic; `agent_run_id`/`cost_usd` only exist on `GatewayDecisionEvent` from that same Round 6 change onward (additive proto fields 12/13).

\* Skeptic-panel adversarial verification is a v2 feature per `PRD.md`'s scope note — v1 ships a single LLM-judge with bias mitigations (CoT-forcing, reference-guided grading, judge-model ≠ policy-model). **Corrected 2026-09-05**: PRD.md's v1 line describes this as "the interface for a multi-judge skeptic panel designed in from the start," but the real code is a much thinner seam than that phrasing implies — `judge()` (`evals/judge/llm_judge.py`) takes exactly one `call_model: Callable[[str], Awaitable[str]]` parameter (no list, no aggregation, no voting), and `Score.scorer_type` (`evals/models.py`) is a closed `Literal["deterministic", "llm_judge"]`. Adding a real panel later means widening that `Literal` and building new multi-judge aggregation/voting logic from scratch — an additive *feature*, yes, but not a drop-in against an already-designed multi-judge *interface*; no such interface exists yet.

Two decisions drive this shape, both taken from how the highest-scale operators run this pattern in practice: **generation is decoupled from evaluation** (a rollout produces a trace and terminates; scoring is a separate, resumable job that reads the trace, so a pod eviction never invalidates already-collected data), and **the trace store is structurally distinct from the dataset store but joinable by ID** (a production trace that reveals a bug is promoted directly into the regression dataset, not re-typed by hand).

### Regression Corpus: the "GAP" convention

Most `regression_corpus_*.json` cases have `reference == task_spec["output"]` — the case documents a fixed, verified-correct behavior, and `reference`/`output` matching is exactly what proves it. A real minority of cases deliberately break that equality: `task_spec["output"]` holds the honest, currently-verified-real system behavior — a defect — while `reference` holds the ideal/target behavior the system *should* eventually exhibit. This is not an authoring mistake; it's how this corpus tracks a known, currently-unfixed system defect as a permanent regression case without lying about what the system actually does today. Confirmed present, as of 2026-09-11, in 13 cases across three files: `regression_corpus_cache_adversarial.json` (6), `regression_corpus_cost_abuse.json` (4), `regression_corpus_routing_chaos.json` (3) — a case in each carries the word "gap" (case-insensitive) in its `task_spec["rationale"]`/`corpus_note` *and* has `reference != task_spec["output"]`.

**Two lookalikes this convention is easy to confuse with, named explicitly to avoid mis-scoping future tooling**: (1) `regression_corpus_guardrail.json`'s own `*-gap-*`/`*-scope-gap-*`-ID'd cases (e.g. `regcorpus-guardrail-13-scope-gap-polite-cross-tenant-data-request`) have `reference == task_spec["output"]` — both pin down the SAME real, currently-accepted-permanent classifier limitation (a miss or false positive Kelvran has decided not to fix yet); there is no ideal/target value being tracked, so these are not this convention, despite "gap" appearing in the case ID. (2) `regression_corpus_judge_accuracy.json` has 24 cases where `reference != task_spec["output"]`, but that divergence is a property of how a judge-accuracy corpus is inherently shaped (the case is a judge-scoring exercise, not a system-defect tracker) — unrelated to this convention entirely.

**The rule, made explicit here for the first time** (the underlying pattern was found organically across separately-authored files during a 2026-09-15 triage swarm, per `DECISIONS.md`, but never previously written down as a named convention): when a case documents a real, currently-unfixed defect, `reference` must hold the ideal/target behavior and must **never** be edited to match `output` just to make the case pass — doing so would silently launder a real, tracked gap into a false "resolved" state, exactly the mistake that same triage swarm caught itself making and reverted (`adversarial-cache-document-drift-volatility-keyword-gap`, `adversarial-cache-hit-miss-timing-side-channel`).

**There is no standardized, machine-checkable marker for this convention today — only prose, and inconsistent prose at that.** The field itself is named `rationale` in some cases and `corpus_note` in others (a real, minor naming drift, named here rather than silently perpetuated). The wording that signals "this is the GAP convention, not a defect" varies case to case: "REAL, VERIFIED GAP -- not fixed in this pass (corpus authoring only)"; "A real, currently-not-built gap, honestly named rather than fixed here"; "Honestly scoped out, per this task's own explicit instruction"; even "REVISION 2 -- upgraded from revision 1's verified_real_gap to verified_fixed" (a case that started as this convention and was later closed, leaving the history in the rationale rather than deleting it). The one loose common thread is that the word "gap" tends to appear somewhere in the prose — not a strict prefix, not a schema-enforced field, and not currently required by anything: nothing in `EvalCase` (`evals/models.py`) or any test enforces a marker, or even the divergence itself being intentional, on a case whose `reference`/`output` differ. **This is itself a real, disclosed gap in the convention's own discoverability** — a future auditor (human or LLM, including `evals audit-corpus`, whose own prompt-building has no awareness of this convention today) has no reliable structural signal to distinguish "this divergence is the GAP convention, working as intended" from "this divergence is itself a bug" — exactly the ambiguity any future corpus-linting tool (a field-swap detector, a citation-staleness checker) must account for explicitly, not silently mis-flag.

## Data Model (sketch)

```
EvalCase { id, revision, task_spec, reference | null, tier: golden|regression|drift_sample, tags }
Run      { id, eval_case_id, eval_case_revision, agent_version, model_id,
           harness_config: {scaffold_version, tool_budget, retry_policy, step_budget, sandbox_tier},
           sandbox_id, status, cost_usd, latency_ms, token_usage {input, output} }
Trace    { trace_id, run_id, spans: [Span] }
Span     { span_id, parent_span_id, gen_ai.operation.name, input, output (opt-in, masked),
           start_time, end_time, attributes }
Score    { run_id, scorer_id, scorer_type: deterministic|llm_judge|llm_judge_panel,
           value, rationale, rubric_axis, bias_mitigations_applied: [...],
           panel_votes, quorum_reached }
```

**`EvalCase`, `Run`, `Score`, and (as of `docs/rfcs/2026-09-04-evals-trace-span-model.md`) `Span` are real** (`evals/models.py`); only `Trace` remains unbuilt. `Run`'s real v1 shape (`docs/rfcs/2026-09-04-evals-rollout-scheduler.md`) is deliberately narrower than the sketch above: `harness_config` carries only `{image, command, timeout_s}` — the literal sandbox invocation the Rollout Scheduler actually makes — not the full `scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set sketched for a future pluggable multi-step agent harness that doesn't exist yet; `cost_usd` defaults to `None` (not `0.0`), since v1's sandbox-only harness makes no billed call. `Run` also carries `cache_key`/`from_cache`/`cache_source_run_id` and a `status="skipped"`/`skip_reason` pair, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md` — all additive, all optional, an old JSONL `Run` line without them still loads cleanly via pydantic's declared defaults, verified directly rather than assumed.

`Span`'s real v1 shape also diverges from the sketch above, deliberately: no `Trace{spans:[Span]}` wrapper — today's harness makes exactly one `run_in_sandbox()` call per `Run`, so `Span.run_id` joins directly to `Run.id`, the same "don't build the diagram-only box" discipline already applied to `Task/Dataset Registry`/`Sandbox Pool`; no `gen_ai.operation.name` — confirmed, via direct inspection of the OTel semantic-conventions registry, to be an LLM/model-inference-only namespace, not applicable to a container-sandbox execution; real (if experimental/incubating) `process.*`/`container.*`-sourced fields (`process_command_args`, `process_exit_code`, `container_image_name`, and — added 2026-09-04 alongside a real `sandbox.py` timeout-enforcement bug fix — `container_id`, the stable `container.id` semantic convention, captured via `docker run --cidfile`) replace the sketch's generic `input`/`output`/`attributes`. `process_pid` remains deliberately excluded even now that a PID is technically obtainable: the local `docker run` CLI process's PID is not the containerized command's own process, and attaching it as `process.pid` would be a real, honest-sounding-but-wrong value. `span_id`/`trace_id` are real, spec-compliant OTel IDs generated via a locally-held `TracerProvider` in `evals/tracing.py` — no OTLP exporter is wired; the SDK is used purely as a correct ID/timestamp/status generator, resolving `THREAT_MODEL.md`'s "full audit logging tied to the trace" gap entirely inside `evals`, never blocked on `api/otel`'s still-undecided transport (a doc-vs-code framing correction — that gap was never actually about consuming gateway's own spans).

`Score`'s real v1 shape (`docs/rfcs/2026-09-04-evals-score-model.md`) also diverges from the sketch, deliberately: it gains `eval_case_id`/`eval_case_revision` (not in the sketch) as the one join key both `evals run` (no `Run` — `task_spec.output` is baked into the suite file, never executed) and `evals rollout` (a real `Run`) can always honestly supply; `run_id` is nullable — `None`, never a fabricated `EvalCase.id` stand-in, when no `Run` exists — mirroring `Run.cost_usd`'s own "`None` means not applicable" convention; `scorer_type` is narrowed to `deterministic|llm_judge|llm_judge_panel`, dropping the sketch's `human` (still `v2`-scoped per `PRD.md`) and renaming the sketch's `skeptic_panel` to `llm_judge_panel` — added interface-only 2026-09-07, per `docs/rfcs/2026-09-07-evals-judge-panel-interface.md`. **Corrected 2026-09-08, per `docs/rfcs/2026-09-08-evals-judge-panel-reducer.md`: the panel is now real** — `evals.cli`'s `--llm-judge-panel` flag constructs a 2-judge panel (Claude Sonnet 5 + Claude Haiku 4.5, both via AWS Bedrock — revised the same day from an originally-implemented Anthropic-direct + OpenAI pairing, an explicit same-vendor-correlation tradeoff accepted for AWS-only operational simplicity), majority-reduced via `evals.judge.llm_judge.reduce_panel_votes` (strict majority, fail-closed on a tie). `Score` gained two more fields for this, both `None` for every other `scorer_type`: `panel_votes` (the full per-judge audit trail, a `list[PanelVote]`) and `quorum_reached` (`False` on a fail-closed tie, distinguishing it from a genuine unanimous FAIL). The judge-accuracy corpus slice that would give this panel real cases to run against, and its own scheduled CI wiring, remain not yet built — gated on live provider API keys, tracked separately in that RFC's own "Not yet done" section; `rubric_axis` is `None` for a holistic verdict, or a real axis name (e.g. `"correctness"`, `"safety"`) when `--judge-axes` was given (`docs/rfcs/2026-09-05-evals-multi-axis-judging.md`) — never populated for `deterministic`. This is also a deliberate divergence from every real framework surveyed while grounding that RFC (promptfoo/Inspect AI/DeepEval/Braintrust all embed scores directly on their one run/sample record rather than as a separate normalized entity) — Kelvran's `evals run`/`evals rollout` split means there is no single atomic run/sample object every score can attach to, so a standalone `Score` joined by the always-real `eval_case_id` is the one shape that honestly serves both commands. `Score.cost_usd` is real judge-call cost, `Decimal` (not `float`, unlike `Run.cost_usd`) since `evals report --scores` sums it across a group — the exact revisit trigger that RFC named for adopting Decimal — and is `Decimal("0")` for a `deterministic` score (an exact, certain fact) vs. `None` only for an unpriced judge model, deliberately not the same convention as `Run.cost_usd`'s `None`. **Added 2026-09-07, per `docs/rfcs/2026-09-07-evals-cigate-refinements.md`**: `Score` also carries `tier: EvalTier | None`, `tags: list[str]`, and `flaky: bool` — verbatim, denormalized copies of the originating `EvalCase`'s own fields of the same name (`EvalCase` itself also gains `flaky: bool = False`, a durable case-level declaration, not a `Run`-level one), captured at the exact moment `run_cmd`/`rollout_cmd` construct the `Score`, never joined back to a `--suite` file later. `evals report`'s `--tier`/`--category-fail-under` read these fields to scope a gate to a specific tier or to a `tags`-matched category, and its flaky-exclusion logic reads `flaky` to drop a known-noisy case from a gate computation while still printing its real result.

`harness_config` is a **required** field on `Run`, not optional metadata — harness swaps alone have been shown to move scores 10+ points in comparable systems, so no cross-run comparison is allowed to surface without this visible.

## Harness-Transparency Design

"Transparent" means operationally: model, scaffold version, tool budget, retry policy, step budget, and sandbox tier are recorded on every `Run` and surfaced in every comparison view — never inferred, never left as ambient config. No score is emitted without a confidence interval; no CI/CD gating decision runs without a prior power calculation.

## Sandbox Tiering

Docker (v1, CI/moderate-risk workloads) → gVisor → Firecracker microVM (customer-facing/code-exec workloads, added once real need is proven) — consumed via Kubernetes `runtimeClass` or a managed sandbox provider, never reimplemented. Isolation is enforced *between concurrent trial sandboxes*, not just sandbox-vs-host — package-registry/dependency proxies get the same scrutiny as the sandbox boundary itself.

## Tech Stack

| Concern | Choice |
|---|---|
| Language/runtime | Python 3.12+ |
| Orchestration | **Resolved 2026-09-07, per `docs/rfcs/2026-09-07-evals-sandbox-pool-deferred-decision.md` (that RFC names this exact line as "the fork in the road" it resolves)**: `asyncio.Semaphore`-bounded concurrency, not Ray Core — a concrete, named compound trigger (CI wall-clock exceeding 5 minutes OR the regression-tier corpus exceeding 30 cases) would re-open the question, not a vague "once justified." Ray Core was evaluated and rejected for this workload: multi-host distribution has no purchase on Kelvran's single-host, low-double-digit concurrency need, and `PRD.md`'s own non-goal explicitly excludes the 10K+-rollout scale where Ray would matter. Independently re-confirmed still correct by `docs/upgrade-research/evals-harness-architecture-scaling-round4-2026-09-11.md`'s Finding 2, which also flagged this exact table row as stale (this correction closes that finding) |
| Sandboxing | Docker at v1; gVisor/Firecracker added per the tiering above |
| Statistics | **Real today**: Wilson confidence intervals, stdlib-only (`math`/`statistics`, zero dependencies). numpy/scipy/scikit-learn (bootstrap resampling, Bayesian model comparison, pass@k/pass^k) remain a future target — not installed, not built |
| Judge SDKs | **Real today, Anthropic + OpenAI**, per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`: `evals/judge/providers.py` is the sole file that imports `anthropic` or `openai` — `make_anthropic_call_model()` wired into `evals run --llm-judge`/`evals rollout --llm-judge` (default), plus a same-shaped `make_openai_call_model()` (added 2026-09-05, exactly the follow-on this RFC predicted) — gated by a `RUN_LIVE_LLM_TESTS=1`-opt-in integration test. `judge()`/`llm_judge.py` remain zero-SDK-code and fully testable without a key, but are no longer untouched: `judge()`'s `call_model` parameter was widened 2026-09-07 to also accept a `list[CallModel]`, per `docs/rfcs/2026-09-07-evals-judge-panel-interface.md` — interface-only at the time. **Corrected 2026-09-08, per `docs/rfcs/2026-09-08-evals-judge-panel-reducer.md`: the panel is now real, and a third provider joined this file.** `providers.py` also now imports `boto3` (already a real `evals/` dependency for object-storage ingestion) for a new `make_bedrock_call_model()` — `--llm-judge-panel` builds its 2-judge panel from Bedrock-hosted Claude Sonnet 5 + Claude Haiku 4.5, NOT the Anthropic+OpenAI dual-provider wiring this row originally predicted PoLL's "disjoint model families" finding would be satisfied by — an explicit, accepted same-vendor-correlation tradeoff (see that RFC). `make_openai_call_model()` still exists, unused by anything today (back to its pre-panel "ready-made, no real caller yet" state). Default Anthropic judge model (`claude-haiku-4-5-20251001`, a pinned, dated snapshot id) and default OpenAI judge model (`gpt-4o-mini`) are both still defined but no longer used by anything real — see the same-day correction directly below. `cli.py` originally wired only the Anthropic provider by default for standalone `--llm-judge`; no `--judge-provider` flag exists, since the RFC never asked for one, only for the follow-on function itself to be unblocked. **Corrected 2026-09-08 (later still, same day, once a real AWS credential existed to test against): standalone `--llm-judge` moved to Bedrock too.** It now wires `make_bedrock_call_model(BEDROCK_HAIKU_4_5_MODEL_ID)` — the exact same model id as the panel's own Haiku panelist — so no `ANTHROPIC_API_KEY` (or `OPENAI_API_KEY`) is needed anywhere in `evals`'s judge system anymore; `make_anthropic_call_model()`/`make_openai_call_model()` both now sit fully unused, "ready-made, no real caller," the same state `make_openai_call_model()` was already in. Reusing the panel's own Haiku model id was deliberate, not incidental: it reactivates the cache-key design's cross-mode reuse property (a vote cached from one mode is reusable in the other for the same model id) — which surfaced a real, pre-existing gap in `_load_cached_scores` (it only ever read prior `llm_judge` Scores, never a prior `llm_judge_panel` Score's own embedded `panel_votes`, so the "and vice versa" half of that reuse property had no real test and no real code path until this exact change made it live) — fixed in the same pass. |
| Tracing | **Real today, self-contained**: `evals/tracing.py` wraps every real `run_in_sandbox()` call in a genuine OTel Python SDK span — a locally-held `TracerProvider`, zero OTLP exporter, never `api/otel` (per `docs/rfcs/2026-09-04-evals-trace-span-model.md`). No `gen_ai.*` semantic conventions (confirmed inapplicable to a non-LLM sandbox-execution span); real `process.*`/`container.*` attributes where they genuinely apply. Consuming gateway's own OTel spans via a live collector fan-out remains a future, undecided target — a separate capability from this self-instrumentation |
| Object storage client | **Real today, S3 and GCS**: `evals/ingestion/object_store.py` is the sole file that imports `boto3`/`google.cloud.storage` — the first place `evals` talks to a cloud-provider API directly (distinct from `judge/providers.py`'s LLM-provider SDKs), per `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md` and that RFC's own 2026-09-07 addendum. S3 reuses the same AWS-IAM credential model `gateway`'s Bedrock adapter already depends on (`docs/operations/PROVIDERS.md`); GCS resolves credentials via `google-cloud-storage`'s own default `Client()` (`GOOGLE_APPLICATION_CREDENTIALS`/ambient Workload Identity) rather than a hand-rolled second identity path. Both are additive, sibling schemes behind one `parse_object_storage_uri`/`list_object_keys`/`iter_object_lines` API — neither replaces the other |

A thin TypeScript client SDK is a plausible later addition for JS/TS-agent instrumentation, mirroring DeepEval's own Python-core/TS-client split — not part of v1.

## Cross-Cutting Contract

`evals` consumes `gateway`'s OTel spans and `gatewayevents` via the versioned schema in the root `api/` directory. It never imports `gateway`'s Go source, and `gateway` never imports anything from `evals`. The object-storage leg of that consumption (`evals ingest`, per `docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md` and that RFC's own 2026-09-07 addendum) reads only `gatewayevents_v1`-shaped JSON that a separate log shipper already wrote to either S3 (`docs/operations/vector-gatewayevents-s3.yaml`) or GCS (`docs/operations/vector-gatewayevents-gcs.yaml`) — `evals` never talks to a running `gateway` process directly, and `gateway` has zero awareness that this ingestion path exists.
