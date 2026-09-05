"""Data model for evaluation cases.

Matches evals/ARCHITECTURE.md's Data Model sketch:

    EvalCase { id, revision, task_spec, reference | null,
               tier: golden|regression|drift_sample, tags }

`EvalCase` instances are immutable once created — a stable `id` at a given
`revision` never changes shape out from under a dataset. Advancing a case to
a new revision is done via `with_revision()`, which returns a brand-new
instance rather than mutating the original in place, per the org-wide
immutability convention.
"""

from __future__ import annotations

from decimal import Decimal
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field

EvalTier = Literal["golden", "regression", "drift_sample"]


class EvalCase(BaseModel):
    """A single, versioned evaluation case.

    `tier` is set at dataset-registration time (see THREAT_MODEL.md's Evals
    "Spoofing" row: a rollout must never be able to claim its own tier).
    """

    model_config = ConfigDict(frozen=True)

    id: str
    revision: int
    task_spec: dict
    reference: str | None = None
    tier: EvalTier
    tags: list[str] = Field(default_factory=list)

    def with_revision(self, revision: int) -> EvalCase:
        """Return a new `EvalCase` at `revision`, leaving `self` untouched.

        Never mutates `self` — revisions are immutable once created.
        """
        return self.model_copy(update={"revision": revision})


RunStatus = Literal["completed", "timed_out", "error", "skipped"]


class Run(BaseModel):
    """The result of executing one `EvalCase` through the Rollout Scheduler.

    A deliberately narrowed v1 slice of ARCHITECTURE.md's full `Run` sketch,
    per docs/rfcs/2026-09-04-evals-rollout-scheduler.md — every field below
    is either real today or an honest "not measured" `None`, never a
    fabricated placeholder:

    - `cost_usd` defaults to `None`, not `0.0`: `run_in_sandbox()` runs a
      Docker command, not a billed LLM call, so "measured as zero" would be
      false. `None` means "not applicable to this harness yet."
    - `harness_config` carries only `{image, command, timeout_s}` — the
      literal sandbox invocation — not the full `scaffold_version`/
      `tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set the
      architecture sketch describes for a future pluggable multi-step agent
      harness that doesn't exist yet.
    - There is no `Trace`/`Span` field, not even an empty placeholder —
      `api/otel`'s transport is still undecided, so a stub field here would
      be dead code with no consumer.
    - `status="skipped"` (per docs/rfcs/2026-09-04-evals-rollout-cost-
      mitigation.md) means this trial was never attempted at all — a group
      of repeated trials already reached an early-stopping decision. Never
      read a `"skipped"` `Run` as "verified healthy"; it carries no signal
      either way, the same "None means not applicable, never a stand-in
      for a real measurement" convention `cost_usd` already establishes.
    - `cache_key`/`from_cache`/`cache_source_run_id` are also additive, per
      the same RFC: `cache_key` is computed for every `Run` regardless of
      whether caching is active (cheap, and lets a *later* `--use-cache`
      invocation hit against `Run`s produced by an invocation that didn't
      ask for caching); `from_cache=True` only on a genuine cache hit
      (`status` is always `"completed"` in that case — an `error`/
      `timed_out` prior `Run` can never source a hit); `cache_source_run_id`
      points at the real, originally-executed `Run` a hit reused — never a
      chain of reuse-of-reuse.
    """

    model_config = ConfigDict(frozen=True)

    id: str
    eval_case_id: str
    eval_case_revision: int
    harness_config: dict
    status: RunStatus
    exit_code: int | None = None
    stdout: str = ""
    stderr: str = ""
    latency_ms: float
    cost_usd: float | None = None
    error: str | None = None
    skip_reason: str | None = None
    cache_key: str | None = None
    from_cache: bool = False
    cache_source_run_id: str | None = None


ScorerType = Literal["deterministic", "llm_judge"]


class Score(BaseModel):
    """The result of one scorer's judgment on one `EvalCase`'s output.

    A deliberately narrowed v1 slice of ARCHITECTURE.md's `Score` sketch,
    per docs/rfcs/2026-09-04-evals-score-model.md:

    - `run_id` is `None`, not a fabricated `EvalCase.id`, when no real
      `Run` exists behind the scored output (`evals run`'s fixture-baked
      `task_spec.output` case never touches the Rollout Scheduler) —
      mirrors `Run.cost_usd`'s own "`None` means not applicable, never a
      fabricated stand-in" convention. `eval_case_id`/`eval_case_revision`
      are the universal join key both `evals run` and `evals rollout` can
      always honestly supply, regardless of whether a `Run` exists.
    - `scorer_type` is narrowed to the two values this codebase actually
      produces — no `skeptic_panel`/`human`, both `v2`-scoped per `PRD.md`.
    - `rubric_axis` stays `None` always in v1 — `judge()` produces one
      holistic verdict, never a per-axis breakdown.
    - `cost_usd` is `Decimal("0")` (not `None`) for a `deterministic` score
      — that scorer makes categorically zero external calls, by
      construction (`exact_match`/`regex_match` are pure string/regex
      comparisons), so zero is an exact, certain fact, not an estimate.
      This is a deliberate divergence from `Run.cost_usd`'s "`None` = not
      measured, possibly non-zero" convention: the two situations aren't
      analogous. For `llm_judge`, `cost_usd` is the real, computed cost of
      that one Anthropic call (`evals.judge.providers.JudgeCallCost`), or
      `None` if the pinned judge model has no price-table entry —
      genuinely unmeasured, unlike the deterministic case.
    - `cost_usd` is `Decimal`, not `float` — report --scores sums it
      across every `Score` in a group (per docs/rfcs/2026-09-04-evals-
      score-model.md's own named revisit trigger: "the moment evals gets a
      suite-level cost aggregation... Decimal should be adopted
      immediately"). Price-table constants in `providers.py` are built
      from strings (`Decimal("1.00")`), never float literals, to avoid
      reintroducing the exact imprecision Decimal exists to prevent.
    - `score_cache_key`/`from_cache` are additive, per docs/rfcs/2026-09-05
      -evals-score-cache.md, mirroring `Run.cache_key`/`Run.from_cache`'s
      own exact precedent: `score_cache_key` is computed for every real
      `llm_judge` score regardless of whether `--use-score-cache` is
      active (cheap, and lets a *later* cached invocation hit against
      `Score`s an earlier, non-caching invocation produced) — always
      `None` for a `deterministic` score, since that scorer is already
      free and instant, with nothing worth caching. `from_cache=True` only
      on a genuine cache hit, in which case `cost_usd` is the exact,
      certain `Decimal("0")` (no new API call was made) — the same
      "exact fact, not an estimate" reasoning already applied to a
      `deterministic` score's `cost_usd` above, not a new convention.
      Unlike `Run`, there is no `cache_source_score_id` — `Score` has no
      `id` field of its own for a hit to point back at.
    """

    model_config = ConfigDict(frozen=True)

    eval_case_id: str
    eval_case_revision: int
    run_id: str | None = None
    scorer_id: str
    scorer_type: ScorerType
    value: bool
    rationale: str | None = None
    rubric_axis: str | None = None
    bias_mitigations_applied: list[str] = Field(default_factory=list)
    cost_usd: Decimal | None = None
    score_cache_key: str | None = None
    from_cache: bool = False


SpanStatus = Literal["UNSET", "OK", "ERROR"]


class Span(BaseModel):
    """One real OTel-SDK-generated span wrapping a single sandbox execution
    attempt in the Rollout Scheduler, per docs/rfcs/2026-09-04-evals-trace-
    span-model.md.

    Deliberately no `Trace` wrapper: today's harness makes exactly one
    `run_in_sandbox()` call per `Run`, so `Span` joins directly to `Run.id`
    via `run_id` — the same "don't build the diagram-only box" discipline
    already applied to `Run`/`Score` in `evals/ARCHITECTURE.md`'s Data
    Model section. A future multi-step harness making several spans per
    `Run` is the real trigger for introducing `Trace{spans: [Span]}`.

    `span_id`/`trace_id` are real, spec-compliant OTel IDs (16/32 lowercase
    hex chars) generated by the OTel Python SDK's own `RandomIdGenerator`,
    via a locally-held `TracerProvider` in `evals.tracing` — never the
    process-wide `trace.set_tracer_provider()` singleton. No OTLP exporter
    is wired: the SDK is used purely as a correct ID/timestamp/status
    generator, for possible future correlation with gateway's own real
    OTel spans (per `api/otel`'s still-undecided transport), never as a
    live export pipeline.

    `container_id` (added 2026-09-04, alongside a real bug fix in
    `sandbox.py`: killing the local `docker run` CLI process on timeout
    did not stop the container itself — confirmed empirically against a
    real Docker daemon) is the real Docker container ID, captured via
    `--cidfile`, matching the stable `container.id` semantic convention.
    `process_pid` remains deliberately excluded — even now that a real
    PID is technically obtainable from the local `docker run` CLI
    process, that PID identifies the CLI *client* process, not the
    containerized command's own process, and OTel's `process.pid`
    convention means the latter — attaching the former would be a real,
    honest-sounding-but-wrong value, worse than omitting the field.
    `gen_ai.operation.name` is deliberately never used — confirmed, via
    direct inspection of the OTel semantic-conventions registry, to be a
    namespace for LLM/model-inference operations only, not applicable to
    a container-sandbox execution.
    """

    model_config = ConfigDict(frozen=True)

    span_id: str
    trace_id: str
    parent_span_id: str | None = None
    run_id: str
    name: str
    start_time_unix_nano: int
    end_time_unix_nano: int
    status: SpanStatus
    process_command_args: list[str]
    process_exit_code: int | None = None
    container_image_name: str
    container_id: str | None = None
    error: str | None = None
