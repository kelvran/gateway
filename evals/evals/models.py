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
