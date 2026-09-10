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

from datetime import datetime
from decimal import Decimal
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

EvalTier = Literal["golden", "regression", "drift_sample"]


class EvalCase(BaseModel):
    """A single, versioned evaluation case.

    `tier` is set at dataset-registration time (see THREAT_MODEL.md's Evals
    "Spoofing" row: a rollout must never be able to claim its own tier).

    `flaky` (added 2026-09-07, per docs/rfcs/2026-09-07-evals-cigate-
    refinements.md) is a durable, case-level declaration — set once by a
    human curator, exactly like `tier`/`tags` — that this case is known to
    be non-deterministic/environmentally noisy. It lives here, not on
    `Run`, because *why* a case is flaky doesn't change trial-to-trial; a
    `Run`-level flag would mean re-declaring it on every single execution
    for no real benefit. Mirrors DeepEval's own `LLMTestCase(flaky=True)`
    precedent: a dedicated, type-checked field, deliberately not encoded
    as a magic string inside `tags` (which stays an open-ended,
    operator-chosen label — overloading it with one reserved value that
    silently changes gate behavior would be a real footgun). Denormalized
    onto every `Score` this case produces (see `Score.flaky`'s own
    docstring) so `evals.cli.report_cmd` can exclude it from a
    `--fail-under`/`--category-fail-under` gate computation while still
    printing its real result — it still runs, it's still visible, it just
    never trips a gate on a known-noisy signal.
    """

    model_config = ConfigDict(frozen=True)

    id: str
    revision: int
    task_spec: dict
    reference: str | None = None
    tier: EvalTier
    tags: list[str] = Field(default_factory=list)
    flaky: bool = False

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


ScorerType = Literal["deterministic", "llm_judge", "llm_judge_panel"]


class PanelVote(BaseModel):
    """One judge's independent verdict within a multi-judge panel, per
    docs/rfcs/2026-09-08-evals-judge-panel-reducer.md.

    Defined here, not in `evals.judge.llm_judge` (where the research that
    designed this shape sketched it) — `.importlinter`'s layers contract
    places `evals.judge.llm_judge` ABOVE `evals.models`, so `Score`
    (below) embedding a type from `evals.judge.llm_judge` (above) would
    be a real, CI-caught layering violation. `evals.judge.llm_judge`
    imports this type downward instead; that module has never previously
    imported from `evals.models`, a real, deliberate new dependency edge,
    not an accident.

    `score_cache_key`/`from_cache` mirror `Score`'s own same-named fields'
    exact convention (see `Score`'s docstring) — `None`/`False` when this
    vote came from `judge()`'s own cache-agnostic internal panel branch
    (that module has no caching concept at all, per its own docstring); a
    real, always-computed value only when `evals.cli`'s panel-aware
    caching wrapper (`_judge_panel_with_cache`) produced or reused it.
    """

    model_config = ConfigDict(frozen=True)

    scorer_id: str
    passed: bool
    rationale: str
    score_cache_key: str | None = None
    from_cache: bool = False
    # trigger_quote/quote_grounded, added per docs/rfcs/2026-09-09-evals-
    # quote-grounded-verdict.md: this panelist's own verbatim QUOTE, and
    # whether it's a real substring of the judged output/reference.
    # Measurement-only -- recorded, never used to discard or reweight a
    # vote in reduce_panel_votes. None for a PanelVote built before this
    # field existed.
    trigger_quote: str | None = None
    quote_grounded: bool | None = None


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
    - `scorer_type` is narrowed to `deterministic`/`llm_judge`/
      `llm_judge_panel` — the `Literal` was widened interface-only on
      2026-09-07 (per docs/rfcs/2026-09-07-evals-judge-panel-interface.md,
      before any scorer constructed a `Score` with the panel value) and
      the panel itself became real on 2026-09-08 (per docs/rfcs/2026-09-
      08-evals-judge-panel-reducer.md — `evals.cli`'s `--llm-judge-panel`
      flag now constructs real `llm_judge_panel` Scores via
      `evals.judge.llm_judge.judge()` called with more than one
      `call_model`). Deliberately named to match Inspect AI's
      `multi_scorer()` naming, not ARCHITECTURE.md's original sketch
      spelling (`skeptic_panel`) — see the interface RFC's Design section
      for why. `human` (also in the original sketch) stays dropped, still
      `v2`-scoped per `PRD.md`.
    - `rubric_axis` is `None` for a holistic verdict (the default), or the
      configured axis name (e.g. `"correctness"`, `"safety"`) when
      `evals.cli`'s `--judge-axes` requested one real `judge()` call per
      axis instead of one call covering all of them — per docs/rfcs/2026-
      09-05-evals-multi-axis-judging.md. Always `None` for a
      `deterministic` score, which has no concept of a rubric axis.
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
    - `tier`/`tags`/`flaky` (added 2026-09-07, per docs/rfcs/2026-09-07-
      evals-cigate-refinements.md) are denormalized, verbatim copies of
      the originating `EvalCase`'s own fields of the same name, captured
      at the exact moment `run_cmd`/`rollout_cmd` construct this `Score`
      — the same "compute once, at write time, regardless of whether a
      later feature needs it yet" convention `Run.cache_key`/`Score.
      score_cache_key` already established, chosen over having
      `evals.cli.report_cmd` join back to a `--suite` file at report
      time (which would let a `--scores` file outlive or diverge from
      the suite it was scored against, a real staleness risk this
      convention avoids). `tier` is `EvalTier | None`, not a bare
      `EvalTier` — `None` for a `Score` built before this field existed
      (an old JSONL line still validates, resolving to `None` via this
      declared default) or, in principle, any future scoring path with
      no real `EvalCase` behind it; never a guessed tier. `tags` defaults
      to `[]`, `flaky` to `False` — both `EvalCase`'s own defaults,
      reproduced exactly. `evals.cli.report_cmd`'s `--tier` and
      `--category-fail-under` read `tier`/`tags` here; its flaky-
      exclusion logic reads `flaky` here — never the originating
      `EvalCase` directly, which `report_cmd` never loads.
    - `panel_votes`/`quorum_reached` (added 2026-09-08, per docs/rfcs/2026-
      09-08-evals-judge-panel-reducer.md) are real, populated fields only
      for `scorer_type="llm_judge_panel"` — `None` for every other
      scorer_type, the same "`None` = not applicable" convention already
      used throughout this class (see `tier` above). `panel_votes` is the
      full per-judge audit trail (never collapsed away once computed);
      `quorum_reached` is `True` iff a strict majority (`count * 2 >
      panel_size`) agreed — `False` on a tie means `value` was set
      fail-closed, not that no verdict exists. `cost_usd` for a panel
      score is the `Decimal` SUM of every panelist's real per-call cost
      (or `None` if any one panelist's cost is unmeasured), never just
      the first judge's — a real divergence from a single-judge
      `llm_judge` score's `cost_usd`, which is exactly one call's cost.
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
    tier: EvalTier | None = None
    tags: list[str] = Field(default_factory=list)
    flaky: bool = False
    panel_votes: list[PanelVote] | None = None
    quorum_reached: bool | None = None
    # quote_grounded, per docs/rfcs/2026-09-09-evals-quote-grounded-
    # verdict.md: whether the judge's own QUOTE cites a real, verbatim
    # span of the judged output/reference. Measurement-only -- recorded,
    # never used to gate --fail-under. Single-judge: copied directly from
    # JudgeResult.quote_grounded. Panel: an AND-aggregate across
    # panel_votes (None if panel_votes is empty/absent). None for a Score
    # built before this field existed.
    quote_grounded: bool | None = None


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


TrendSeriesName = Literal[
    "judge_accuracy_kappa",
    "quote_grounding_rate",
    "audit_corpus_defect_rate",
    "cost_usd",
]


class TrendSnapshot(BaseModel):
    """One persisted history point for one named quality-trend series.

    Per this repo's own already-documented principle (`evals.cli`'s
    module docstring: "never blended across `deterministic` and
    `llm_judge`") and every 2026 eval platform surveyed while designing
    this feature (Langfuse/Braintrust/Arize all trend named score series
    side by side, never fused into one composite) -- a `TrendSnapshot`
    always names exactly one `series`. There is no combined "eval
    quality score" anywhere in this model or in `evals trend show`,
    which reads it.

    Exactly one of `rate_value`/`cost_usd_value` is the "active" field
    for a given `series` -- `series == "cost_usd"` uses `cost_usd_value`
    (a real `Decimal`, per `Score.cost_usd`'s own established Decimal-
    not-float cost-accounting convention -- never cast to `float`, which
    would reintroduce the exact imprecision `Decimal` exists to
    prevent); every other `series` uses `rate_value`. The field that is
    NOT active for a given `series` is always `None` -- enforced below.
    `rate_value` being `None` for its own active series is still a
    legitimate, meaningful value, distinct from "not applicable": a
    `judge_accuracy_kappa` snapshot records `None` when Cohen's kappa is
    genuinely undefined (zero-variance verdicts -- the same
    `kappa_str == "undefined"` case `evals.cli.report_cmd` already
    handles), and a `quote_grounding_rate` snapshot records `None` when
    no `Score` in that report run had a known `quote_grounded` value yet
    (`n=0`) -- both are honest "measured, but no real number exists"
    facts, mirroring `Run.cost_usd`/`Score.cost_usd`'s own "`None` means
    not applicable, never a fabricated stand-in" convention used
    throughout this file. `cost_usd_value` has no such "measured but
    undefined" case in any real call site today -- a real total cost is
    always computable -- so the validator below treats it more strictly.

    `category_tag` is reserved for a future per-category trend
    (mirroring `Score.tags`/`report_cmd --category-fail-under`'s own
    tag-scoping) -- no real call site in `evals.cli` populates it yet;
    always `None` today.

    `scorer_type` is `None` for `audit_corpus_defect_rate` (audited per
    suite file, not per scorer) and real (`"deterministic"`/
    `"llm_judge"`/`"llm_judge_panel"`) for the other three series, all
    computed inside `report_cmd`'s own per-`scorer_type` grouping loop --
    never blended across scorer types, the same discipline `report_cmd`'s
    own pass_rate/CI lines already enforce.
    """

    model_config = ConfigDict(frozen=True)

    series: TrendSeriesName
    recorded_at: datetime
    n: int
    rate_value: float | None = None
    cost_usd_value: Decimal | None = None
    scorer_type: ScorerType | None = None
    category_tag: str | None = None
    source_command: Literal["report", "audit-corpus"]

    @model_validator(mode="after")
    def _check_value_field_matches_series(self) -> TrendSnapshot:
        """Enforce the one-field-per-series rule described in this
        class's own docstring.

        Deliberately NOT a bare "exactly one of the two is not-None"
        check -- that would reject the legitimate `rate_value=None`
        cases described above. The real invariant enforced here is
        narrower and more useful: the field that does NOT belong to
        `series` must never carry a value -- `cost_usd_value` must be
        `None` for any non-`cost_usd` series, and `rate_value` must be
        `None` for `series == "cost_usd"` (cost is always a real,
        computed `Decimal` in every real `evals.cli` call site, never
        legitimately unmeasured the way kappa/quote-grounding can be).
        """
        if self.series == "cost_usd":
            if self.cost_usd_value is None:
                raise ValueError(
                    "TrendSnapshot(series='cost_usd') requires cost_usd_value "
                    "to be set."
                )
            if self.rate_value is not None:
                raise ValueError(
                    "TrendSnapshot(series='cost_usd') must not set rate_value "
                    "-- cost_usd_value is the only field this series ever "
                    "populates."
                )
        elif self.cost_usd_value is not None:
            raise ValueError(
                f"TrendSnapshot(series={self.series!r}) must not set "
                "cost_usd_value -- only series='cost_usd' ever populates it."
            )
        return self
