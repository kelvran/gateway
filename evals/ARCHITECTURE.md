# evals — Architecture

Python service. Runs continuous, statistically rigorous evaluation of the agents/models routed through `gateway`. Never sits in the request path — it calls `gateway`'s API for offline rollouts and samples `gateway`'s production telemetry for online regression detection. For the whole-system view, see the root `ARCHITECTURE.md`.

## Package Layout

```
evals/
  pyproject.toml
  uv.lock                    — own workspace manager; zero cross-awareness with the Go side
  .importlinter               — real as of 2026-09-05, per gateway's own equivalent `go-arch-lint`
                                 wiring: 1 layers contract (cli > {rollout.scheduler, ingestion.decode}
                                 > {tracing, results_store, rollout.cache, rollout.sandbox, judge.*}
                                 > {models, stats}, built from the real, verified import graph, not
                                 assumed) plus 2 independence contracts (judge's 3 scoring strategies
                                 never depend on each other; rollout's cache and sandbox stay decoupled).
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
    ingestion/                 — real, v1: decode-only, per docs/rfcs/2026-09-03-api-gatewayevents-contract.md
                                 — decodes a checked-in gatewayevents fixture via the generated bindings,
                                 the golden-fixture round-trip test docs/testing/TESTING.md §5 promised.
                                 Live production-trace sampling from a running gateway (the transport is
                                 still undecided) remains a documented future slice, not built here
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

**Real today**: `Rollout Scheduler` (sequential — no `Sandbox Pool` yet) and a flat-file `Results Store` (no Dashboard), per `docs/rfcs/2026-09-04-evals-rollout-scheduler.md`. `Scorer Service` is real for both `deterministic` and single-provider `llm_judge` (Anthropic + OpenAI, per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md` — `cli.py` still wires only Anthropic by default, no skeptic-panel wiring), with every judged verdict now persisted as a real `Score` (`docs/rfcs/2026-09-04-evals-score-model.md`), not just printed. The Rollout Scheduler also has real, opt-in cost/DoS mitigation: a Run-level result cache (`rollout --use-cache`, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md`) and a real mixture-SPRT (anytime-valid) early-stopping rule (`--early-stop-*`, per `docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md` — upgraded 2026-09-05 from that first RFC's original two-checkpoint, Bonferroni-corrected design) for repeated trials — both off by default. A second, independent opt-in cache exists one layer up, at the judge call itself: `--use-score-cache` (both `evals run`/`evals rollout`), per `docs/rfcs/2026-09-05-evals-score-cache.md`, skips re-calling the LLM judge for a case whose `(output, reference, scorer_id)` already has a real `Score` on file. `llm_judge`'s single holistic verdict can also be split into independent per-axis verdicts via `--judge-axes` (one real judge call per configured axis, AND-combined into the case-level pass/fail), per `docs/rfcs/2026-09-05-evals-multi-axis-judging.md`; omitting the flag reproduces the original single-verdict behavior exactly. The `Stats Engine` gained `mixture_sprt_early_stop` alongside `wilson_interval`; no power calculation or bootstrap resampling beyond that. Every other box (`Task/Dataset Registry`, `Trace Collector`, `Golden/Regression Dataset` promotion, `CI/CD Gate`, `Online Eval Service`) remains diagram-only.

\* Skeptic-panel adversarial verification is a v2 feature per `PRD.md`'s scope note — v1 ships a single LLM-judge with bias mitigations (CoT-forcing, reference-guided grading, judge-model ≠ policy-model). **Corrected 2026-09-05**: PRD.md's v1 line describes this as "the interface for a multi-judge skeptic panel designed in from the start," but the real code is a much thinner seam than that phrasing implies — `judge()` (`evals/judge/llm_judge.py`) takes exactly one `call_model: Callable[[str], Awaitable[str]]` parameter (no list, no aggregation, no voting), and `Score.scorer_type` (`evals/models.py`) is a closed `Literal["deterministic", "llm_judge"]`. Adding a real panel later means widening that `Literal` and building new multi-judge aggregation/voting logic from scratch — an additive *feature*, yes, but not a drop-in against an already-designed multi-judge *interface*; no such interface exists yet.

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

**`EvalCase`, `Run`, `Score`, and (as of `docs/rfcs/2026-09-04-evals-trace-span-model.md`) `Span` are real** (`evals/models.py`); only `Trace` remains unbuilt. `Run`'s real v1 shape (`docs/rfcs/2026-09-04-evals-rollout-scheduler.md`) is deliberately narrower than the sketch above: `harness_config` carries only `{image, command, timeout_s}` — the literal sandbox invocation the Rollout Scheduler actually makes — not the full `scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set sketched for a future pluggable multi-step agent harness that doesn't exist yet; `cost_usd` defaults to `None` (not `0.0`), since v1's sandbox-only harness makes no billed call. `Run` also carries `cache_key`/`from_cache`/`cache_source_run_id` and a `status="skipped"`/`skip_reason` pair, per `docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md` — all additive, all optional, an old JSONL `Run` line without them still loads cleanly via pydantic's declared defaults, verified directly rather than assumed.

`Span`'s real v1 shape also diverges from the sketch above, deliberately: no `Trace{spans:[Span]}` wrapper — today's harness makes exactly one `run_in_sandbox()` call per `Run`, so `Span.run_id` joins directly to `Run.id`, the same "don't build the diagram-only box" discipline already applied to `Task/Dataset Registry`/`Sandbox Pool`; no `gen_ai.operation.name` — confirmed, via direct inspection of the OTel semantic-conventions registry, to be an LLM/model-inference-only namespace, not applicable to a container-sandbox execution; real (if experimental/incubating) `process.*`/`container.*`-sourced fields (`process_command_args`, `process_exit_code`, `container_image_name`, and — added 2026-09-04 alongside a real `sandbox.py` timeout-enforcement bug fix — `container_id`, the stable `container.id` semantic convention, captured via `docker run --cidfile`) replace the sketch's generic `input`/`output`/`attributes`. `process_pid` remains deliberately excluded even now that a PID is technically obtainable: the local `docker run` CLI process's PID is not the containerized command's own process, and attaching it as `process.pid` would be a real, honest-sounding-but-wrong value. `span_id`/`trace_id` are real, spec-compliant OTel IDs generated via a locally-held `TracerProvider` in `evals/tracing.py` — no OTLP exporter is wired; the SDK is used purely as a correct ID/timestamp/status generator, resolving `THREAT_MODEL.md`'s "full audit logging tied to the trace" gap entirely inside `evals`, never blocked on `api/otel`'s still-undecided transport (a doc-vs-code framing correction — that gap was never actually about consuming gateway's own spans).

`Score`'s real v1 shape (`docs/rfcs/2026-09-04-evals-score-model.md`) also diverges from the sketch, deliberately: it gains `eval_case_id`/`eval_case_revision` (not in the sketch) as the one join key both `evals run` (no `Run` — `task_spec.output` is baked into the suite file, never executed) and `evals rollout` (a real `Run`) can always honestly supply; `run_id` is nullable — `None`, never a fabricated `EvalCase.id` stand-in, when no `Run` exists — mirroring `Run.cost_usd`'s own "`None` means not applicable" convention; `scorer_type` is narrowed to `deterministic|llm_judge` only, dropping the sketch's `skeptic_panel`/`human` (both `v2`-scoped, per `PRD.md`); `rubric_axis` is `None` for a holistic verdict, or a real axis name (e.g. `"correctness"`, `"safety"`) when `--judge-axes` was given (`docs/rfcs/2026-09-05-evals-multi-axis-judging.md`) — never populated for `deterministic`. This is also a deliberate divergence from every real framework surveyed while grounding that RFC (promptfoo/Inspect AI/DeepEval/Braintrust all embed scores directly on their one run/sample record rather than as a separate normalized entity) — Kelvran's `evals run`/`evals rollout` split means there is no single atomic run/sample object every score can attach to, so a standalone `Score` joined by the always-real `eval_case_id` is the one shape that honestly serves both commands. `Score.cost_usd` is real judge-call cost, `Decimal` (not `float`, unlike `Run.cost_usd`) since `evals report --scores` sums it across a group — the exact revisit trigger that RFC named for adopting Decimal — and is `Decimal("0")` for a `deterministic` score (an exact, certain fact) vs. `None` only for an unpriced judge model, deliberately not the same convention as `Run.cost_usd`'s `None`.

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
| Judge SDKs | **Real today, Anthropic + OpenAI**, per `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`: `evals/judge/providers.py` is the sole file that imports `anthropic` or `openai` — `make_anthropic_call_model()` wired into `evals run --llm-judge`/`evals rollout --llm-judge` (default), plus a same-shaped `make_openai_call_model()` (added 2026-09-05, exactly the follow-on this RFC predicted) — gated by a `RUN_LIVE_LLM_TESTS=1`-opt-in integration test. `judge()`/`llm_judge.py` themselves remain untouched — still zero SDK code, still fully testable without a key. Default Anthropic judge model is a pinned, dated snapshot id (`claude-haiku-4-5-20251001`); default OpenAI judge model is `gpt-4o-mini` — both bumped by hand only. `cli.py` deliberately still wires only the Anthropic provider by default — no `--judge-provider` flag exists, since the RFC never asked for one, only for the follow-on function itself to be unblocked |
| Tracing | **Real today, self-contained**: `evals/tracing.py` wraps every real `run_in_sandbox()` call in a genuine OTel Python SDK span — a locally-held `TracerProvider`, zero OTLP exporter, never `api/otel` (per `docs/rfcs/2026-09-04-evals-trace-span-model.md`). No `gen_ai.*` semantic conventions (confirmed inapplicable to a non-LLM sandbox-execution span); real `process.*`/`container.*` attributes where they genuinely apply. Consuming gateway's own OTel spans via a live collector fan-out remains a future, undecided target — a separate capability from this self-instrumentation |

A thin TypeScript client SDK is a plausible later addition for JS/TS-agent instrumentation, mirroring DeepEval's own Python-core/TS-client split — not part of v1.

## Cross-Cutting Contract

`evals` consumes `gateway`'s OTel spans and `gatewayevents` via the versioned schema in the root `api/` directory. It never imports `gateway`'s Go source, and `gateway` never imports anything from `evals`.
