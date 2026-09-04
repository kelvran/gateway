# evals — Architecture

Python service. Runs continuous, statistically rigorous evaluation of the agents/models routed through `gateway`. Never sits in the request path — it calls `gateway`'s API for offline rollouts and samples `gateway`'s production telemetry for online regression detection. For the whole-system view, see the root `ARCHITECTURE.md`.

## Package Layout

```
evals/
  pyproject.toml
  uv.lock                    — own workspace manager; zero cross-awareness with the Go side
  .importlinter               — **not built**: the target layer-contract tool (Python analogue of
                                 gateway/ARCHITECTURE.md's own equally-unbuilt `go-arch-lint` target) —
                                 no such file exists and nothing references `importlinter` in CI or
                                 `pyproject.toml` today, confirmed 2026-09-04
  evals/
    contracts/                — GENERATED Python stubs from api/*.proto — no source dependency, ever.
                                 Real for gatewayevents/v1; empty for otel/ (deliberately not built —
                                 see api/README.md)
    ingestion/                 — real, v1: decode-only, per docs/rfcs/2026-09-03-api-gatewayevents-contract.md
                                 — decodes a checked-in gatewayevents fixture via the generated bindings,
                                 the golden-fixture round-trip test docs/testing/TESTING.md §5 promised.
                                 Live production-trace sampling from a running gateway (the transport is
                                 still undecided) remains a documented future slice, not built here
    results_store.py           — real, v1, per docs/rfcs/2026-09-04-evals-score-model.md: append-only
                                 JSONL persistence, generic over any frozen pydantic model this package
                                 produces (`Run`, `Score`). Moved here from `rollout/` — the mechanism was
                                 never rollout-specific, only its first caller (`Run`) was; `evals run`
                                 (no rollout, no sandbox) is `Score`'s other, equally valid caller
    rollout/                   — real, v1, per docs/rfcs/2026-09-04-evals-rollout-scheduler.md:
                                 `sandbox.py`'s Docker-sandboxed execution wrapper and `scheduler.py`'s
                                 sequential Rollout Scheduler (one EvalCase -> one run_in_sandbox() call
                                 -> one Run, no concurrency/pool this pass). Sandbox Pool (concurrency)
                                 and a Task/Dataset Registry remain unbuilt — diagram-only, per that
                                 RFC's own scope boundary
    judge/                     — LLM-judge + statistics (bootstrap resampling, Bayesian model comparison,
                                 confidence intervals, pass@k / pass^k reliability)
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

**Real today**: `Rollout Scheduler` (sequential — no `Sandbox Pool` yet) and a flat-file `Results Store` (no Dashboard), per `docs/rfcs/2026-09-04-evals-rollout-scheduler.md`. `Scorer Service` is real for both `deterministic` and single-provider `llm_judge` (Anthropic only — no skeptic-panel wiring), per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`, with every judged verdict now persisted as a real `Score` (`docs/rfcs/2026-09-04-evals-score-model.md`), not just printed. The Rollout Scheduler also has real, opt-in cost/DoS mitigation, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`: a result cache (`rollout --use-cache`) and a statistically-sound two-checkpoint early-stopping rule (`--early-stop-*`) for repeated trials — both off by default. The `Stats Engine` gained `two_checkpoint_early_stop` alongside `wilson_interval`; no power calculation beyond that. Every other box (`Task/Dataset Registry`, `Trace Collector`, `Golden/Regression Dataset` promotion, `CI/CD Gate`, `Online Eval Service`) remains diagram-only.

\* Skeptic-panel adversarial verification is a v2 feature per `PRD.md`'s scope note — v1 ships a single LLM-judge with bias mitigations (CoT-forcing, reference-guided grading, judge-model ≠ policy-model), but the `Scorer Service`'s interface is designed to accept multiple judges from day one so the panel is an additive change later, not a rewrite.

Two decisions drive this shape, both taken from how the highest-scale operators run this pattern in practice: **generation is decoupled from evaluation** (a rollout produces a trace and terminates; scoring is a separate, resumable job that reads the trace, so a pod eviction never invalidates already-collected data), and **the trace store is structurally distinct from the dataset store but joinable by ID** (a production trace that reveals a bug is promoted directly into the regression dataset, not re-typed by hand).

## Data Model (sketch)

```
EvalCase { id, revision, task_spec, reference | null, tier: golden|regression|drift_sample, tags }
Run      { id, eval_case_id, eval_case_revision, agent_version, model_id,
           harness_config: {scaffold_version, tool_budget, retry_policy, step_budget, sandbox_tier},
           sandbox_id, status, cost_usd, latency_ms, token_usage {input, output} }
Trace    { trace_id, run_id, spans: [Span] }
Span     { span_id, parent_span_id, gen_ai.operation.name, input, output (opt-in, masked),
           start_time, end_time, attributes }
Score    { run_id, scorer_id, scorer_type: deterministic|llm_judge|skeptic_panel|human,
           value, rationale, rubric_axis, bias_mitigations_applied: [...] }
```

**`EvalCase`, `Run`, and `Score` are real** (`evals/models.py`); only `Trace`/`Span` remain unbuilt. `Run`'s real v1 shape (`docs/rfcs/2026-09-04-evals-rollout-scheduler.md`) is deliberately narrower than the sketch above: `harness_config` carries only `{image, command, timeout_s}` — the literal sandbox invocation the Rollout Scheduler actually makes — not the full `scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set sketched for a future pluggable multi-step agent harness that doesn't exist yet; `cost_usd` defaults to `None` (not `0.0`), since v1's sandbox-only harness makes no billed call; there is no `Trace`/`Span` field at all, not even an empty placeholder, since `api/otel`'s transport is still undecided. `Run` also carries `cache_key`/`from_cache`/`cache_source_run_id` and a `status="skipped"`/`skip_reason` pair, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md` — all additive, all optional, an old JSONL `Run` line without them still loads cleanly via pydantic's declared defaults, verified directly rather than assumed.

`Score`'s real v1 shape (`docs/rfcs/2026-09-04-evals-score-model.md`) also diverges from the sketch, deliberately: it gains `eval_case_id`/`eval_case_revision` (not in the sketch) as the one join key both `evals run` (no `Run` — `task_spec.output` is baked into the suite file, never executed) and `evals rollout` (a real `Run`) can always honestly supply; `run_id` is nullable — `None`, never a fabricated `EvalCase.id` stand-in, when no `Run` exists — mirroring `Run.cost_usd`'s own "`None` means not applicable" convention; `scorer_type` is narrowed to `deterministic|llm_judge` only, dropping the sketch's `skeptic_panel`/`human` (both `v2`-scoped, per `PRD.md`); `rubric_axis` stays permanently `None` in v1, since `judge()` produces one holistic verdict, never a per-axis breakdown. This is also a deliberate divergence from every real framework surveyed while grounding that RFC (promptfoo/Inspect AI/DeepEval/Braintrust all embed scores directly on their one run/sample record rather than as a separate normalized entity) — Kelvran's `evals run`/`evals rollout` split means there is no single atomic run/sample object every score can attach to, so a standalone `Score` joined by the always-real `eval_case_id` is the one shape that honestly serves both commands. `Score.cost_usd` is real judge-call cost, `Decimal` (not `float`, unlike `Run.cost_usd`) since `evals report --scores` sums it across a group — the exact revisit trigger that RFC named for adopting Decimal — and is `Decimal("0")` for a `deterministic` score (an exact, certain fact) vs. `None` only for an unpriced judge model, deliberately not the same convention as `Run.cost_usd`'s `None`.

`harness_config` is a **required** field on `Run`, not optional metadata — harness swaps alone have been shown to move scores 10+ points in comparable systems, so no cross-run comparison is allowed to surface without this visible.

## Harness-Transparency Design

"Transparent" means operationally: model, scaffold version, tool budget, retry policy, step budget, and sandbox tier are recorded on every `Run` and surfaced in every comparison view — never inferred, never left as ambient config. No score is emitted without a confidence interval; no CI/CD gating decision runs without a prior power calculation.

## Sandbox Tiering

Docker (v1, CI/moderate-risk workloads) → gVisor → Firecracker microVM (customer-facing/code-exec workloads, added once real need is proven) — consumed via Kubernetes `runtimeClass` or a managed sandbox provider, never reimplemented. Isolation is enforced *between concurrent trial sandboxes*, not just sandbox-vs-host — package-registry/dependency proxies get the same scrutiny as the sandbox boundary itself.

## Tech Stack

| Concern | Choice |
|---|---|
| Language/runtime | Python 3.12+ |
| Orchestration | `asyncio` at v1; Ray Core actors once concurrent-rollout count justifies it |
| Sandboxing | Docker at v1; gVisor/Firecracker added per the tiering above |
| Statistics | **Real today**: Wilson confidence intervals, stdlib-only (`math`/`statistics`, zero dependencies). numpy/scipy/scikit-learn (bootstrap resampling, Bayesian model comparison, pass@k/pass^k) remain a future target — not installed, not built |
| Judge SDKs | **Real today, Anthropic only**, per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`: `evals/judge/providers.py`'s `make_anthropic_call_model()` is the sole file that imports `anthropic`, wired into `evals run --llm-judge`/`evals rollout --llm-judge`, gated by a `RUN_LIVE_LLM_TESTS=1`-opt-in integration test. `judge()`/`llm_judge.py` themselves are untouched — still zero SDK code, still fully testable without a key. Default judge model is a pinned, dated snapshot id (`claude-haiku-4-5-20251001`), bumped by hand only. A second provider (OpenAI) remains a future, same-shaped follow-on — `call_model`'s existing DI seam needs no signature change to add one |
| Tracing | OTel Python SDK, GenAI semantic conventions, consumed from `api/otel` |

A thin TypeScript client SDK is a plausible later addition for JS/TS-agent instrumentation, mirroring DeepEval's own Python-core/TS-client split — not part of v1.

## Cross-Cutting Contract

`evals` consumes `gateway`'s OTel spans and `gatewayevents` via the versioned schema in the root `api/` directory. It never imports `gateway`'s Go source, and `gateway` never imports anything from `evals`.
