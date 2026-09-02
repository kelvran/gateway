# evals — Architecture

Python service. Runs continuous, statistically rigorous evaluation of the agents/models routed through `gateway`. Never sits in the request path — it calls `gateway`'s API for offline rollouts and samples `gateway`'s production telemetry for online regression detection. For the whole-system view, see the root `ARCHITECTURE.md`.

## Package Layout

```
evals/
  pyproject.toml
  uv.lock                    — own workspace manager; zero cross-awareness with the Go side
  .importlinter               — CI-enforced layer contract (Python analogue of go-arch-lint)
  evals/
    contracts/                — GENERATED Python stubs from api/*.proto — no source dependency, ever
    ingestion/                 — consumes api/gatewayevents + api/otel only; production-trace sampling
    rollout/                   — sandboxed agent rollout orchestration
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
| Statistics | numpy / scipy / scikit-learn; Wilson/bootstrap confidence intervals |
| Judge SDKs | Native Anthropic/OpenAI SDKs |
| Tracing | OTel Python SDK, GenAI semantic conventions, consumed from `api/otel` |

A thin TypeScript client SDK is a plausible later addition for JS/TS-agent instrumentation, mirroring DeepEval's own Python-core/TS-client split — not part of v1.

## Cross-Cutting Contract

`evals` consumes `gateway`'s OTel spans and `gatewayevents` via the versioned schema in the root `api/` directory. It never imports `gateway`'s Go source, and `gateway` never imports anything from `evals`.
