"""`evals` CLI.

`evals run --suite <path>` scores a checked-in suite of `EvalCase`s with the
deterministic scorer, where `task_spec.output` is a value already baked
into the suite file. `evals rollout --suite <path> --results <path>` uses a
distinct `task_spec` convention (`{image, command, timeout_s}`) — output
comes from a real `run_in_sandbox()` execution via the Rollout Scheduler,
never from the suite file itself; each `Run` is appended to `--results`
before scoring. `evals report` prints a pass rate from raw counts, or from
a persisted `Score`s JSONL file (`--scores`, one line per distinct
`scorer_type` present — never blended across `deterministic` and
`llm_judge` — including that group's real total `cost_usd`). All three
commands that emit a pass rate always print the Wilson confidence interval
alongside it — per `PRD.md`'s explicit success metric, a bare percentage
is never emitted on its own.

`evals ingest --source s3://<bucket>/<prefix> --out <path>` (or a
`gs://<bucket>/<prefix>` source) lists and decodes real `gatewayevents_v1`
objects from object storage (per
docs/rfcs/2026-09-07-evals-trace-ingestion-object-storage.md) — the
production-trace-sampling leg `evals/ingestion/`'s own `ARCHITECTURE.md`
entry names, distinct from the golden-fixture round-trip decode test.
Given `--suite`/`--results` too, each decoded event is also mapped (via
`evals.ingestion.mapping`) into the same `EvalCase`+`Run` shapes
`promote` reads from every other source, closing that RFC's own
"Unresolved Questions" entry: live-sampled data feeds `evals promote`
the same as any other run source, no separate review path.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path

import click
from dotenv import load_dotenv
from google.protobuf.json_format import MessageToJson

from evals.ingestion.decode import decode_gateway_decision_event
from evals.ingestion.mapping import gateway_decision_event_to_eval_case_and_run
from evals.ingestion.object_store import (
    iter_object_lines,
    list_object_keys,
    parse_object_storage_uri,
)
from evals.judge.cache import compute_score_cache_key
from evals.judge.deterministic import exact_match, regex_match
from evals.judge.llm_judge import judge, reduce_panel_votes
from evals.judge.providers import (
    BEDROCK_HAIKU_4_5_MODEL_ID,
    BEDROCK_SONNET_5_MODEL_ID,
    DEFAULT_JUDGE_MODEL,
    make_anthropic_call_model,
    make_bedrock_call_model,
)
from evals.models import EvalCase, PanelVote, Run, Score, Span
from evals.results_store import (
    append_runs,
    append_scores,
    append_spans,
    load_runs,
    load_scores,
    load_spans,
)
from evals.rollout.scheduler import EarlyStopConfig, run_suite
from evals.stats import wilson_interval

# evals/.env, next to evals/pyproject.toml — never the process's cwd,
# since `evals` commands get run from different directories across this
# codebase's own docs/scripts. Real, already-exported env vars (a CI
# secret, an explicit `export`) always win over this file: `load_dotenv`'s
# default `override=False` is relied on deliberately, not just accepted.
_ENV_FILE_PATH = Path(__file__).resolve().parent.parent / ".env"


def _load_env_file(path: Path = _ENV_FILE_PATH) -> None:
    """Populate `os.environ` from `path` if it exists, without overriding
    any variable already set. A no-op, not an error, when `path` doesn't
    exist — the file is a local-dev convenience, never a requirement
    (CI and production set real env vars directly).
    """
    load_dotenv(path, override=False)


def _load_cases(suite_path: Path) -> list[EvalCase]:
    raw_cases = json.loads(suite_path.read_text())
    return [EvalCase(**raw_case) for raw_case in raw_cases]


def _append_cases_to_suite(cases: list[EvalCase], path: Path) -> None:
    """Append every case in `cases` to the JSON-array suite file at path in
    one read-modify-write pass, creating it if it doesn't exist yet — the
    batched counterpart to `_load_cases`, for `evals promote` (see
    docs/rfcs/2026-09-05-evals-golden-regression-promotion.md) and `evals
    ingest --suite` (see docs/rfcs/2026-09-07-evals-trace-ingestion-
    object-storage.md). Unlike results_store.py's JSONL append helpers, a
    suite file is a plain JSON array matching `_load_cases`'s own format,
    so this reads-modifies-rewrites the whole file rather than appending
    a line — the same tradeoff `_load_cases` itself already made (a suite
    file is meant to stay small and human-reviewable, not an
    append-only results log). Batched (one read-modify-write for every
    case in `cases`) rather than looped call-by-call, so a caller
    appending many cases from one invocation (`ingest_cmd`, potentially
    one per ingested event) never pays an O(n^2) read-rewrite cost. A
    no-op if `cases` is empty — never touches `path` (doesn't even
    create it) when there's nothing to append.
    """
    if not cases:
        return
    existing = json.loads(path.read_text()) if path.exists() else []
    existing.extend(json.loads(c.model_dump_json()) for c in cases)
    path.write_text(json.dumps(existing, indent=2) + "\n")


def _append_case_to_suite(case: EvalCase, path: Path) -> None:
    """Single-case convenience wrapper around `_append_cases_to_suite`,
    for `evals promote`'s one-case-per-invocation call site.
    """
    _append_cases_to_suite([case], path)


def _score_output_deterministic(
    output: str,
    reference: str | None,
    match_kind: str,
    pattern: str | None,
    case_id: str,
) -> bool:
    if match_kind == "exact":
        if reference is None:
            raise ValueError(f"case {case_id!r}: exact match requires a reference")
        return exact_match(output, reference)
    if match_kind == "regex":
        if pattern is None:
            raise ValueError(
                f"case {case_id!r}: regex match requires task_spec.pattern"
            )
        return regex_match(output, pattern)
    raise ValueError(f"case {case_id!r}: unknown task_spec.match kind {match_kind!r}")


def _score_case_deterministic(case: EvalCase) -> bool:
    output = case.task_spec.get("output")
    if output is None:
        raise ValueError(f"case {case.id!r}: task_spec has no 'output' to score")

    return _score_output_deterministic(
        output,
        case.reference,
        case.task_spec.get("match", "exact"),
        case.task_spec.get("pattern"),
        case.id,
    )


def _score_run_deterministic(case: EvalCase, run: Run) -> bool:
    """Score a real `Run`'s captured stdout against `case.reference`.

    A `Run` that didn't complete (timed out or errored launching the
    sandbox) never passes — there's no output to have gotten right.
    """
    if run.status != "completed":
        return False

    return _score_output_deterministic(
        run.stdout.strip(),
        case.reference,
        case.task_spec.get("match", "exact"),
        case.task_spec.get("pattern"),
        case.id,
    )


def _deterministic_scorer_id(case: EvalCase) -> str:
    match_kind = case.task_spec.get("match", "exact")
    return "exact_match" if match_kind == "exact" else "regex_match"


def _load_cached_scores(scores_path: Path) -> dict[str, Score]:
    """Build the `--use-score-cache` lookup table from `scores_path`,
    mirroring `rollout_cmd`'s own `cached_runs` construction pattern
    exactly (see `load_runs(results_path)` there): only real `llm_judge`
    Scores that are themselves NOT cache hits are eligible sources — the
    same "never chain a hit off a hit" discipline `Run.from_cache`
    filtering already established, applied here for the identical reason.
    Safe to call even if `scores_path` doesn't exist yet (a fresh file is
    a valid starting state, per `load_scores`'s own doc comment).
    """
    return {
        s.score_cache_key: s
        for s in load_scores(scores_path)
        if s.scorer_type == "llm_judge"
        and not s.from_cache
        and s.score_cache_key is not None
    }


# (real scorer_id, call_model) pairs, in declared/call order -- unlike
# judge()'s own opaque list[CallModel], evals.cli always knows each
# panelist's real model id (it built the panel from named provider
# factories), which is exactly the piece judge() itself cannot supply.
# See docs/rfcs/2026-09-08-evals-judge-panel-reducer.md.
PanelSpec = list[tuple[str, Callable[[str], Awaitable[str]]]]


def _load_cached_panel_votes(scores_path: Path) -> dict[str, PanelVote]:
    """Panel-mode analogue of `_load_cached_scores`. A panelist's vote is
    cache-eligible whether it came from a prior STANDALONE `--llm-judge`
    Score for that exact model id, or from a prior `--llm-judge-panel`
    run's own embedded `panel_votes` -- both keyed identically (per
    docs/rfcs/2026-09-08-evals-judge-panel-reducer.md's cache-key design:
    a panelist's own cache key never depends on panel membership or
    size), since a judge's verdict on a given (output, reference, axis)
    triple doesn't depend on whether it happened to be called standalone
    or as one panel member. Never chains a hit off a hit, mirroring
    `_load_cached_scores` exactly.
    """
    votes: dict[str, PanelVote] = {}
    for s in load_scores(scores_path):
        if s.from_cache:
            continue
        if s.scorer_type == "llm_judge" and s.score_cache_key is not None:
            votes.setdefault(
                s.score_cache_key,
                PanelVote(
                    scorer_id=s.scorer_id,
                    passed=s.value,
                    rationale=s.rationale or "",
                    score_cache_key=s.score_cache_key,
                    from_cache=False,
                ),
            )
        elif s.scorer_type == "llm_judge_panel" and s.panel_votes is not None:
            for vote in s.panel_votes:
                if not vote.from_cache and vote.score_cache_key is not None:
                    votes.setdefault(vote.score_cache_key, vote)
    return votes


def _parse_judge_axes(raw: str | None) -> list[str] | None:
    """Parse `--judge-axes`' comma-separated value into a real list, or
    `None` if the flag was never given — `evals.judge.llm_judge.judge()`'s
    own `axis=None` meaning "one holistic verdict" reproduced at the CLI
    layer. Whitespace around each axis name is stripped; an axis name
    containing only whitespace is dropped rather than producing a
    confusing blank axis.
    """
    if raw is None:
        return None
    axes = [a.strip() for a in raw.split(",")]
    axes = [a for a in axes if a]
    if not axes:
        raise click.UsageError("--judge-axes was given but contains no real axis names")
    return axes


def _last_judge_call_cost_usd(
    call_model: Callable[[str], Awaitable[str]],
) -> Decimal | None:
    """Read the real cost of the most recent judge call off `call_model`.

    `call_model` only has to satisfy `Callable[[str], Awaitable[str]]` —
    `last_call_cost` is an extra attribute the real
    `evals.judge.providers._AnthropicCallModel` exposes, not part of that
    base contract, so a test fake (a plain async function) that doesn't
    have it falls back to `None` here rather than raising.
    """
    last_call_cost = getattr(call_model, "last_call_cost", None)
    return last_call_cost.cost_usd if last_call_cost is not None else None


@dataclass(frozen=True)
class _JudgeOutcome:
    """The uniform result of judging one case, whether via a real
    `judge()` call or a `--use-score-cache` hit — lets both `run_cmd` and
    `rollout_cmd` build their `Score` from one shape regardless of which
    path produced it, per docs/rfcs/2026-09-05-evals-score-cache.md.

    `panel_votes`/`quorum_reached` (added 2026-09-08, per docs/rfcs/2026-
    09-08-evals-judge-panel-reducer.md) are `None` for a single-judge
    outcome (`_judge_with_cache`) — populated only by `_judge_panel_with_
    cache`. `score_cache_key` stays `None` for a panel outcome: a panel's
    REDUCED verdict is never itself cached (only each panelist's own
    vote is, via `panel_votes[i].score_cache_key`) — see that RFC's
    cache-key design.
    """

    passed: bool
    rationale: str | None
    bias_mitigations_applied: list[str]
    cost_usd: Decimal | None
    score_cache_key: str | None
    from_cache: bool
    axis: str | None = None
    panel_votes: list[PanelVote] | None = None
    quorum_reached: bool | None = None


async def _judge_with_cache(
    output: str,
    reference: str,
    scorer_id: str,
    call_model: Callable[[str], Awaitable[str]],
    cached_scores: dict[str, Score] | None,
    axis: str | None = None,
) -> _JudgeOutcome | None:
    """Score `output` against `reference` with a real LLM-judge call,
    reusing a prior cached `Score` instead when `--use-score-cache` is
    active and a real hit exists for the exact same
    `(output, reference, scorer_id, axis)` — see
    `evals.judge.cache.compute_score_cache_key`.

    `axis`, when given, scopes this one call to a single named rubric
    dimension (per docs/rfcs/2026-09-05-evals-multi-axis-judging.md) — the
    caller (`_judge_case`/`_score_and_record`) is responsible for calling
    this once per configured axis, never once trying to cover several.
    `None` (the default) reproduces the exact original holistic-verdict
    behavior.

    Deliberately a plain `async def`, awaited directly, never driving its
    own internal `asyncio.run()` — `rollout_cmd`'s `_score_and_record` may
    call this from inside `run_suite`'s own already-running event loop
    when early-stopping is active, and a nested `asyncio.run()` there
    raises "cannot be called from a running event loop" (a real bug this
    project's own test suite already caught once, for the direct `judge()`
    call this function now replaces).

    Returns `None` when the judge call itself fails (a malformed
    response, an SDK/network error) — the caller counts that as a judged
    error and moves on, mirroring `evals.rollout.scheduler.run_suite`'s
    "one case's failure never aborts the suite" precedent. Never raised
    for a cache hit, since no call was made to fail.
    """
    cache_key = compute_score_cache_key(output, reference, scorer_id, axis=axis)
    cached = cached_scores.get(cache_key) if cached_scores is not None else None
    if cached is not None:
        return _JudgeOutcome(
            passed=cached.value,
            rationale=cached.rationale,
            bias_mitigations_applied=cached.bias_mitigations_applied,
            # Exact, certain zero -- no new API call was made. Mirrors a
            # deterministic Score's own "exact fact, not an estimate"
            # cost_usd convention, not a new one.
            cost_usd=Decimal("0"),
            score_cache_key=cache_key,
            from_cache=True,
            axis=axis,
        )
    try:
        result = await judge(
            output=output, reference=reference, call_model=call_model, axis=axis
        )
    except Exception:
        return None
    return _JudgeOutcome(
        passed=result.passed,
        rationale=result.rationale,
        bias_mitigations_applied=result.bias_mitigations_applied,
        cost_usd=_last_judge_call_cost_usd(call_model),
        score_cache_key=cache_key,
        from_cache=False,
        axis=axis,
    )


def _sum_panel_cost(costs: list[Decimal | None]) -> Decimal:
    """Sum every panelist's real per-call cost. `None` (an unpriced model,
    or a test fake with no cost side-channel) for ANY fresh panelist makes
    the WHOLE panel's cost `None` — never a silently-understated partial
    sum — mirroring `Score.cost_usd`'s own "genuinely unmeasured, never a
    fabricated stand-in" convention, generalized from one judge to any
    judge in the panel. Callers only call this when at least one real
    cost is expected (an all-cache-hit panel short-circuits to the exact,
    certain `Decimal("0")` before ever reaching this function).
    """
    total = Decimal("0")
    for cost in costs:
        if cost is None:
            return None  # type: ignore[return-value]
        total += cost
    return total


async def _judge_panel_with_cache(
    output: str,
    reference: str,
    panel: PanelSpec,
    cached_votes: dict[str, PanelVote] | None,
    axis: str | None = None,
) -> _JudgeOutcome | None:
    """Panel analogue of `_judge_with_cache` — scores `output` against
    `reference` with a real 2+ judge panel, majority-reduced via
    `reduce_panel_votes`, per docs/rfcs/2026-09-08-evals-judge-panel-
    reducer.md.

    When `cached_votes is None` (the common default, caching off):
    delegates directly to `judge()`'s own real multi-callable branch —
    this exercises that branch as genuine production code, not just
    unit-tested in isolation — then remaps its placeholder-id votes to
    each panelist's REAL model id and computes each vote's own cache key
    unconditionally (mirrors `Score.score_cache_key`'s "always computed
    regardless of whether caching is active this run" convention), so a
    LATER cache-enabled invocation can reuse them even though this one
    never consulted a cache itself.

    When `cached_votes is not None`: does per-panelist fine-grained
    skip/call, reusing `judge()`'s existing SINGLE-callable path (never
    the panel branch, never a private parsing function) for any panelist
    that needs a fresh call — never re-chains a cache hit off another
    cache hit, mirroring `_judge_with_cache`'s own discipline.

    A single panelist's call failure is a whole-case `JUDGE_ERROR`
    (returns `None`), mirroring `_judge_all_axes`'s pre-existing
    all-or-nothing precedent for multi-axis judging.
    """
    if cached_votes is None:
        try:
            result = await judge(
                output=output,
                reference=reference,
                call_model=[cm for _, cm in panel],
                axis=axis,
            )
        except Exception:
            return None
        real_ids = [scorer_id for scorer_id, _ in panel]
        votes = [
            PanelVote(
                scorer_id=real_ids[i],
                passed=v.passed,
                rationale=v.rationale,
                score_cache_key=compute_score_cache_key(
                    output, reference, real_ids[i], axis=axis
                ),
                from_cache=False,
            )
            for i, v in enumerate(result.panel_votes or [])
        ]
        cost_usd = _sum_panel_cost([_last_judge_call_cost_usd(cm) for _, cm in panel])
        return _JudgeOutcome(
            passed=result.passed,
            rationale=None,
            bias_mitigations_applied=result.bias_mitigations_applied,
            cost_usd=cost_usd,
            score_cache_key=None,
            from_cache=False,
            axis=axis,
            panel_votes=votes,
            quorum_reached=result.quorum_reached,
        )

    votes: list[PanelVote] = []
    fresh_costs: list[Decimal | None] = []
    for scorer_id, call_model in panel:
        key = compute_score_cache_key(output, reference, scorer_id, axis=axis)
        cached = cached_votes.get(key)
        if cached is not None:
            votes.append(
                PanelVote(
                    scorer_id=scorer_id,
                    passed=cached.passed,
                    rationale=cached.rationale,
                    score_cache_key=key,
                    from_cache=True,
                )
            )
            continue
        try:
            result = await judge(
                output=output, reference=reference, call_model=call_model, axis=axis
            )
        except Exception:
            return None
        votes.append(
            PanelVote(
                scorer_id=scorer_id,
                passed=result.passed,
                rationale=result.rationale,
                score_cache_key=key,
                from_cache=False,
            )
        )
        fresh_costs.append(_last_judge_call_cost_usd(call_model))

    verdict = reduce_panel_votes(votes)
    all_cached = all(v.from_cache for v in votes)
    cost_usd = Decimal("0") if all_cached else _sum_panel_cost(fresh_costs)
    return _JudgeOutcome(
        passed=verdict.passed,
        rationale=None,
        bias_mitigations_applied=verdict.bias_mitigations_applied,
        cost_usd=cost_usd,
        score_cache_key=None,
        from_cache=all_cached,
        axis=axis,
        panel_votes=votes,
        quorum_reached=verdict.quorum_reached,
    )


async def _judge_all_axes(
    output: str,
    reference: str,
    axes: list[str] | None,
    call_model: Callable[[str], Awaitable[str]] | None = None,
    cached_scores: dict[str, Score] | None = None,
    panel: PanelSpec | None = None,
    cached_votes: dict[str, PanelVote] | None = None,
) -> list[_JudgeOutcome] | None:
    """Judge `output` against `reference` once per configured axis (or
    once, holistically, if `axes` is `None`) — one call per axis, per
    docs/rfcs/2026-09-05-evals-multi-axis-judging.md's one-call-per-axis
    design, never one call trying to cover several.

    Exactly one of `call_model` (single-judge, via `_judge_with_cache`)
    or `panel` (multi-judge, via `_judge_panel_with_cache`) must be given
    — callers (`_judge_case`/`_score_and_record`) decide which mode is
    active up front from the mutually-exclusive `--llm-judge`/
    `--llm-judge-panel` flags, per docs/rfcs/2026-09-08-evals-judge-
    panel-reducer.md.

    Returns `None` if ANY axis's judge call fails — the whole case is
    treated as `JUDGE_ERROR`, mirroring the pre-existing single-axis
    all-or-nothing behavior, rather than persisting a confusing partial
    set of per-axis `Score`s for one case.
    """

    async def _one_axis(axis: str | None) -> _JudgeOutcome | None:
        if panel is not None:
            return await _judge_panel_with_cache(
                output, reference, panel, cached_votes, axis=axis
            )
        return await _judge_with_cache(
            output, reference, DEFAULT_JUDGE_MODEL, call_model, cached_scores, axis=axis
        )

    if axes is None:
        outcome = await _one_axis(None)
        return None if outcome is None else [outcome]
    outcomes: list[_JudgeOutcome] = []
    for axis in axes:
        outcome = await _one_axis(axis)
        if outcome is None:
            return None
        outcomes.append(outcome)
    return outcomes


def _judge_case(
    case: EvalCase,
    axes: list[str] | None = None,
    call_model: Callable[[str], Awaitable[str]] | None = None,
    cached_scores: dict[str, Score] | None = None,
    panel: PanelSpec | None = None,
    cached_votes: dict[str, PanelVote] | None = None,
) -> list[_JudgeOutcome] | None:
    """`run_cmd`'s own entry point into `_judge_all_axes` — synchronous,
    since `run_cmd` itself never runs inside an event loop (unlike
    `rollout_cmd`'s `_score_and_record`, which awaits `_judge_all_axes`
    directly), so wrapping it in `asyncio.run()` here is safe.
    """
    output = case.task_spec.get("output")
    if output is None:
        raise ValueError(f"case {case.id!r}: task_spec has no 'output' to score")
    if case.reference is None:
        raise click.ClickException(
            f"case {case.id!r}: LLM-judge scoring requires a reference"
        )
    return asyncio.run(
        _judge_all_axes(
            output,
            case.reference,
            axes,
            call_model=call_model,
            cached_scores=cached_scores,
            panel=panel,
            cached_votes=cached_votes,
        )
    )


def format_report(successes: int, total: int, confidence: float = 0.95) -> str:
    """Format a pass rate together with its Wilson CI — never a bare number."""
    pass_rate = successes / total if total else 0.0
    lower, upper = wilson_interval(successes, total, confidence=confidence)
    return (
        f"pass_rate={pass_rate:.4f} ({successes}/{total}) "
        f"{confidence:.0%} CI=[{lower:.4f}, {upper:.4f}]"
    )


def _format_group_cost(scores: list[Score]) -> str:
    """Sum a `Score` group's real `cost_usd` for `report --scores`.

    Per docs/rfcs/2026-09-04-evals-score-model.md's own named revisit
    trigger ("the moment evals gets a suite-level cost aggregation...
    Decimal should be adopted immediately"), which this crosses. A `Score`
    with `cost_usd=None` (currently unreachable in v1 — only one priced
    judge model exists) is excluded from the sum and counted explicitly,
    never silently treated as zero.
    """
    known_costs = [s.cost_usd for s in scores if s.cost_usd is not None]
    unknown_count = len(scores) - len(known_costs)
    total_cost = sum(known_costs, start=Decimal("0"))
    if unknown_count:
        return f"total_cost_usd={total_cost} ({unknown_count} unknown excluded)"
    return f"total_cost_usd={total_cost}"


def _format_span_report(spans: list[Span], confidence: float) -> str:
    """Format a `report --traces` line: a Wilson-CI-bearing OK rate (per
    PRD.md's "never a bare percentage" convention, applied here too) plus
    average sandbox execution duration.

    Deliberately labeled `ok_rate`, not `pass_rate` — a Span's OK/ERROR
    status measures whether the sandbox execution itself completed
    without error, a real infra-reliability signal, never an eval-quality
    judgment the way `Score.value`/`Run` pass/fail is. Never grouped (no
    `scorer_type`-like partition exists on `Span`) — one aggregate line,
    mirroring `--successes`/`--total`'s own single-line simplicity.
    """
    total = len(spans)
    ok_count = sum(1 for s in spans if s.status == "OK")
    ok_rate = ok_count / total if total else 0.0
    lower, upper = wilson_interval(ok_count, total, confidence=confidence)
    avg_duration_ms = (
        sum((s.end_time_unix_nano - s.start_time_unix_nano) / 1_000_000 for s in spans)
        / total
    )
    return (
        f"spans: ok_rate={ok_rate:.4f} ({ok_count}/{total}) "
        f"{confidence:.0%} CI=[{lower:.4f}, {upper:.4f}] "
        f"avg_duration_ms={avg_duration_ms:.2f}"
    )


def _filter_scores_by_tier(scores: list[Score], tier: str | None) -> list[Score]:
    """Filter `scores` down to only those whose denormalized `Score.tier`
    equals `tier` exactly. A no-op (returns `scores` unchanged, by
    identity) when `tier` is `None` — `report_cmd`'s pre-existing,
    tier-agnostic behavior, reproduced exactly when `--tier` is omitted.
    See docs/rfcs/2026-09-07-evals-cigate-refinements.md.
    """
    if tier is None:
        return scores
    return [s for s in scores if s.tier == tier]


def _gate_eligible(scores: list[Score]) -> list[Score]:
    """Drop every `flaky=True` Score.

    Per docs/rfcs/2026-09-07-evals-cigate-refinements.md: a flaky-tagged
    case still runs and is still printed in the report, but never counts
    toward a `--fail-under` or `--category-fail-under` gate computation
    — uniformly, whether the gate is the aggregate one or a category one.
    """
    return [s for s in scores if not s.flaky]


def _parse_category_fail_under(raw: tuple[str, ...]) -> list[tuple[str, float]]:
    """Parse repeatable `--category-fail-under TAG:THRESHOLD` strings into
    `(tag, threshold)` pairs, in the order given.

    `str.rpartition(":")` is used deliberately — the threshold is always
    the rightmost `:`-delimited segment, so a tag itself may contain
    colons. Raises `click.UsageError` (never silently drops or guesses)
    on a missing colon, an empty tag, a non-float threshold, or the same
    tag given more than once — mirroring `_parse_judge_axes`'s own
    "explicit, never silent" precedent for malformed CLI input.
    """
    parsed: list[tuple[str, float]] = []
    seen_tags: set[str] = set()
    for entry in raw:
        tag, sep, raw_threshold = entry.rpartition(":")
        if not sep or not tag:
            raise click.UsageError(
                f"--category-fail-under {entry!r} must be in TAG:THRESHOLD form."
            )
        try:
            threshold = float(raw_threshold)
        except ValueError as e:
            raise click.UsageError(
                f"--category-fail-under {entry!r}: {raw_threshold!r} is not a "
                "valid float."
            ) from e
        if tag in seen_tags:
            raise click.UsageError(
                f"--category-fail-under given more than once for tag {tag!r} "
                "-- which threshold would win is never obvious, so this is "
                "refused rather than silently taking the last one."
            )
        seen_tags.add(tag)
        parsed.append((tag, threshold))
    return parsed


@click.group()
def main() -> None:
    """Kelvran evals CLI."""
    _load_env_file()


@main.command("run")
@click.option(
    "--suite",
    "suite_path",
    required=True,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help=(
        "Path to a JSON file containing a list of EvalCase objects, each "
        "with a task_spec.output value already baked in. For real sandboxed "
        "execution instead, see `evals rollout`."
    ),
)
@click.option(
    "--scores",
    "scores_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help="JSONL file each Score is appended to (created if it doesn't exist).",
)
@click.option(
    "--llm-judge",
    is_flag=True,
    default=False,
    help=(
        "Score with a real Anthropic LLM-judge call instead of the "
        "deterministic scorer. Requires ANTHROPIC_API_KEY in the "
        "environment — see evals.judge.providers.make_anthropic_call_model."
    ),
)
@click.option(
    "--llm-judge-panel",
    is_flag=True,
    default=False,
    help=(
        "Score with a 2-judge Bedrock panel (Claude Sonnet 5 + Claude "
        "Haiku 4.5, via AWS Bedrock's Converse API) instead of a single "
        "judge, majority-reduced via evals.judge.llm_judge."
        "reduce_panel_votes -- fail-closed on a disagreement. Mutually "
        "exclusive with --llm-judge. Both judges share the same vendor/"
        "architecture, an explicit, accepted tradeoff for AWS-only "
        "operational simplicity -- see that RFC's own honest accounting "
        "of the weaker bias-reduction premise this implies. Requires AWS "
        "credentials resolvable via boto3's standard credential chain "
        "(AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_REGION or "
        "equivalent). See docs/rfcs/2026-09-08-evals-judge-panel-reducer.md."
    ),
)
@click.option(
    "--use-score-cache",
    is_flag=True,
    default=False,
    help=(
        "Skip re-calling the LLM judge for a case whose "
        "(output, reference, scorer_id) already has a real (non-cached) "
        "Score in --scores. Off by default — existing invocations without "
        "this flag behave exactly as today. See "
        "docs/rfcs/2026-09-05-evals-score-cache.md."
    ),
)
@click.option(
    "--judge-axes",
    default=None,
    help=(
        "Comma-separated rubric axes (e.g. correctness,safety) to judge "
        "independently, one real LLM-judge call per axis, instead of one "
        "holistic verdict. Only meaningful together with --llm-judge or "
        "--llm-judge-panel. Omit for the original single-verdict "
        "behavior, unchanged. See "
        "docs/rfcs/2026-09-05-evals-multi-axis-judging.md."
    ),
)
@click.option("--confidence", default=0.95, show_default=True, type=float)
def run_cmd(
    suite_path: Path,
    scores_path: Path,
    llm_judge: bool,
    llm_judge_panel: bool,
    use_score_cache: bool,
    judge_axes: str | None,
    confidence: float,
) -> None:
    """Run a suite of EvalCases and print pass/fail plus a Wilson CI."""
    if llm_judge and llm_judge_panel:
        raise click.UsageError(
            "--llm-judge and --llm-judge-panel are mutually exclusive."
        )

    cases = _load_cases(suite_path)
    call_model = make_anthropic_call_model() if llm_judge else None
    panel: PanelSpec | None = None
    cached_scores = None
    cached_votes = None
    if llm_judge_panel:
        panel = [
            (
                BEDROCK_SONNET_5_MODEL_ID,
                make_bedrock_call_model(BEDROCK_SONNET_5_MODEL_ID),
            ),
            (
                BEDROCK_HAIKU_4_5_MODEL_ID,
                make_bedrock_call_model(BEDROCK_HAIKU_4_5_MODEL_ID),
            ),
        ]
        cached_votes = (
            _load_cached_panel_votes(scores_path) if use_score_cache else None
        )
    else:
        cached_scores = _load_cached_scores(scores_path) if use_score_cache else None
    axes = _parse_judge_axes(judge_axes)

    successes = 0
    total = 0
    scores: list[Score] = []
    for case in cases:
        total += 1
        if llm_judge or llm_judge_panel:
            outcomes = _judge_case(
                case,
                axes,
                call_model=call_model,
                cached_scores=cached_scores,
                panel=panel,
                cached_votes=cached_votes,
            )
            if outcomes is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                continue
            # A case passes only if every configured axis passes -- a
            # response isn't good if it fails one dimension even though
            # it's correct on every other one. Single-axis (axes=None)
            # degenerates to exactly today's one-outcome behavior.
            passed = all(o.passed for o in outcomes)
            scorer_type = "llm_judge_panel" if llm_judge_panel else "llm_judge"
            scorer_id = (
                "panel:" + "+".join(sid for sid, _ in panel)
                if llm_judge_panel
                else DEFAULT_JUDGE_MODEL
            )
            for outcome in outcomes:
                scores.append(
                    Score(
                        eval_case_id=case.id,
                        eval_case_revision=case.revision,
                        run_id=None,
                        scorer_id=scorer_id,
                        scorer_type=scorer_type,
                        value=outcome.passed,
                        rationale=outcome.rationale,
                        bias_mitigations_applied=outcome.bias_mitigations_applied,
                        cost_usd=outcome.cost_usd,
                        score_cache_key=outcome.score_cache_key,
                        from_cache=outcome.from_cache,
                        rubric_axis=outcome.axis,
                        tier=case.tier,
                        tags=list(case.tags),
                        flaky=case.flaky,
                        panel_votes=outcome.panel_votes,
                        quorum_reached=outcome.quorum_reached,
                    )
                )
                if outcome.axis is not None:
                    verdict = "PASS" if outcome.passed else "FAIL"
                    click.echo(f"{case.id} [{outcome.axis}]: {verdict}")
        else:
            passed = _score_case_deterministic(case)
            scores.append(
                Score(
                    eval_case_id=case.id,
                    eval_case_revision=case.revision,
                    run_id=None,
                    scorer_id=_deterministic_scorer_id(case),
                    scorer_type="deterministic",
                    value=passed,
                    # Exact, certain zero — a deterministic scorer never
                    # makes an external call. See Score.cost_usd's own
                    # docstring for why this is Decimal("0"), not None.
                    cost_usd=Decimal("0"),
                    tier=case.tier,
                    tags=list(case.tags),
                    flaky=case.flaky,
                )
            )
        if passed:
            successes += 1
        click.echo(f"{case.id}: {'PASS' if passed else 'FAIL'}")

    append_scores(scores, scores_path)
    click.echo(format_report(successes, total, confidence=confidence))


@main.command("promote")
@click.option(
    "--suite",
    "suite_path",
    required=True,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help="Path to the JSON EvalCase suite file the promoted Run's own case came from.",
)
@click.option(
    "--results",
    "results_path",
    required=True,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help="JSONL file of persisted Runs (see --results on `rollout`).",
)
@click.option(
    "--scores",
    "scores_path",
    default=None,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help=(
        "JSONL file of persisted Scores (see --scores on `run`/`rollout`). "
        "Optional; when given together with --tier regression, at least "
        "one matching Score for --run-id must have value=false (a real "
        "failure) — promoting a passing run to the regression tier has "
        "nothing to regression-test. Not checked for --tier drift_sample, "
        "or when --scores is omitted entirely."
    ),
)
@click.option("--run-id", "run_id", required=True, help="The Run.id to promote.")
@click.option(
    "--tier",
    required=True,
    type=click.Choice(["regression", "drift_sample"]),
    help=(
        'The new EvalCase\'s tier. "golden" is deliberately not a valid '
        "choice here — per THREAT_MODEL.md's Evals Spoofing row, tier is "
        "set at dataset-registration time by a human, never derived from "
        "a rollout's own outcome."
    ),
)
@click.option(
    "--output",
    "output_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help=(
        "JSON suite file the new EvalCase is appended to (created if it doesn't exist)."
    ),
)
def promote_cmd(
    suite_path: Path,
    results_path: Path,
    scores_path: Path | None,
    run_id: str,
    tier: str,
    output_path: Path,
) -> None:
    """Promote a Run's underlying EvalCase into a new, frozen EvalCase at --tier.

    See docs/rfcs/2026-09-05-evals-golden-regression-promotion.md. The new
    case is a distinct identity (a new id, revision 1) — never a new
    revision of the original, since it's a derived, separate case, not an
    edit to the one that ran. task_spec/reference are copied verbatim from
    the original case at the exact revision the Run actually used, frozen
    at that point regardless of later edits to --suite.
    """
    cases_by_key = {(c.id, c.revision): c for c in _load_cases(suite_path)}
    run = next((r for r in load_runs(results_path) if r.id == run_id), None)
    if run is None:
        raise click.ClickException(f"no Run with id {run_id!r} found in {results_path}")

    original_case = cases_by_key.get((run.eval_case_id, run.eval_case_revision))
    if original_case is None:
        raise click.ClickException(
            f"Run {run_id!r} references EvalCase "
            f"{run.eval_case_id!r}@{run.eval_case_revision}, not found in {suite_path}"
        )

    if scores_path is not None and tier == "regression":
        matching = [s for s in load_scores(scores_path) if s.run_id == run_id]
        if not matching or all(s.value for s in matching):
            raise click.ClickException(
                f"--tier regression requires at least one failing Score "
                f"(value=false) for Run {run_id!r} in {scores_path}; found "
                f"{len(matching)} matching Score(s), none failing — "
                "nothing to regression-test."
            )

    new_case = EvalCase(
        id=f"{original_case.id}-promoted-{run.id}",
        revision=1,
        task_spec=original_case.task_spec,
        reference=original_case.reference,
        tier=tier,
        tags=[
            *original_case.tags,
            f"promoted-from:{original_case.id}@{original_case.revision}",
            f"promoted-from-run:{run.id}",
        ],
        # Carried forward verbatim, not re-decided here -- see
        # docs/rfcs/2026-09-07-evals-cigate-refinements.md: no new
        # --flaky flag on this command; a curator who wants to override
        # it can hand-edit the resulting (small, human-reviewable)
        # suite file directly.
        flaky=original_case.flaky,
    )
    _append_case_to_suite(new_case, output_path)
    click.echo(
        f"promoted {original_case.id}@{original_case.revision} (run {run.id}) "
        f"-> {new_case.id} (tier={tier}) in {output_path}"
    )


@main.command("ingest")
@click.option(
    "--source",
    "source",
    required=True,
    help=(
        "Object-storage source to list, e.g. s3://bucket/gatewayevents/v1/ "
        "or gs://bucket/gatewayevents/v1/. s3:// and gs:// are supported "
        "today — see docs/rfcs/2026-09-07-evals-trace-ingestion-object-"
        "storage.md."
    ),
)
@click.option(
    "--out",
    "out_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help=(
        "JSONL file each successfully-decoded GatewayDecisionEvent is "
        "appended to (created if it doesn't exist), one protojson object "
        "per line — the same wire shape "
        "evals/tests/fixtures/gateway_decision_event.json already uses. "
        "Written unconditionally, regardless of whether --suite/--results "
        "are also given."
    ),
)
@click.option(
    "--suite",
    "suite_path",
    default=None,
    type=click.Path(dir_okay=False, path_type=Path),
    help=(
        "EvalCase suite JSON file (same format as `run`/`rollout`/"
        "`promote`'s own --suite) each successfully-decoded event's "
        "synthetic, tier=drift_sample EvalCase is appended to (created if "
        "it doesn't exist) — in addition to, never instead of, --out. "
        "Must be given together with --results; the pair makes ingested "
        "production traces immediately promotable via `evals promote "
        "--tier drift_sample`, per docs/rfcs/2026-09-07-evals-trace-"
        "ingestion-object-storage.md's own resolved open question."
    ),
)
@click.option(
    "--results",
    "results_path",
    default=None,
    type=click.Path(dir_okay=False, path_type=Path),
    help=(
        "JSONL Run results file (same format as `rollout`'s own "
        "--results, and what `promote`'s own --results reads) each "
        "successfully-decoded event's synthetic Run is appended to "
        "(created if it doesn't exist). Must be given together with "
        "--suite."
    ),
)
def ingest_cmd(
    source: str,
    out_path: Path,
    suite_path: Path | None,
    results_path: Path | None,
) -> None:
    """List and decode gatewayevents_v1 objects from object storage.

    Lists every object under `--source`, reads every line of every
    object, and decodes each one via the existing, real
    `evals.ingestion.decode.decode_gateway_decision_event` — this
    command's own job is exactly listing/reading/reporting, never
    re-implementing wire-format decoding (per
    docs/rfcs/2026-09-03-api-gatewayevents-contract.md's contract
    discipline). A line that fails to decode is counted as an error and
    skipped — one bad line never aborts the whole ingest run, mirroring
    `evals rollout`'s own "one case's failure never aborts the suite"
    precedent.

    When `--suite`/`--results` are also given, every successfully-decoded
    event is additionally mapped (via
    `evals.ingestion.mapping.gateway_decision_event_to_eval_case_and_run`)
    into the same `EvalCase`+`Run` shapes `evals promote` reads from every
    other source, and appended there — see that module's own docstring
    for exactly what is/isn't honestly derivable from a
    `GatewayDecisionEvent`. Omitting both reproduces the original
    decode-only behavior exactly.
    """
    if (suite_path is None) != (results_path is None):
        raise click.UsageError("--suite and --results must be given together.")

    try:
        scheme, bucket, prefix = parse_object_storage_uri(source)
    except ValueError as e:
        raise click.ClickException(str(e)) from e
    keys = list_object_keys(scheme, bucket, prefix)
    if not keys:
        raise click.ClickException(f"no objects found under {source}")

    decoded_count = 0
    error_count = 0
    new_cases: list[EvalCase] = []
    new_runs: list[Run] = []
    with out_path.open("a", encoding="utf-8") as out_file:
        for key in keys:
            for line in iter_object_lines(scheme, bucket, key):
                try:
                    event = decode_gateway_decision_event(line)
                except Exception:
                    error_count += 1
                    continue
                decoded_count += 1
                # indent=None -- a single compact line, matching the
                # newline-delimited-JSON shape the object-storage body
                # itself already uses (never a pretty-printed, multi-line
                # blob that would break the "one line per event" contract
                # of --out).
                out_file.write(MessageToJson(event, indent=None) + "\n")
                if suite_path is not None:
                    case, run = gateway_decision_event_to_eval_case_and_run(event)
                    new_cases.append(case)
                    new_runs.append(run)

    if suite_path is not None:
        _append_cases_to_suite(new_cases, suite_path)
        append_runs(new_runs, results_path)

    click.echo(
        f"ingested {len(keys)} object(s) from {source}: "
        f"{decoded_count} decoded, {error_count} failed to decode"
    )
    if suite_path is not None:
        click.echo(
            f"promotable: {len(new_cases)} EvalCase(s) appended to "
            f"{suite_path}, {len(new_runs)} Run(s) appended to {results_path}"
        )


@main.command("rollout")
@click.option(
    "--suite",
    "suite_path",
    required=True,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help=(
        "Path to a JSON file containing a list of EvalCase objects, each "
        "with task_spec={image, command, timeout_s?, match?, pattern?} — a "
        "distinct convention from `evals run`'s baked-in task_spec.output. "
        "Output is captured from a real run_in_sandbox() execution."
    ),
)
@click.option(
    "--results",
    "results_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help="JSONL file each Run is appended to (created if it doesn't exist).",
)
@click.option(
    "--scores",
    "scores_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help="JSONL file each Score is appended to (created if it doesn't exist).",
)
@click.option(
    "--traces",
    "traces_path",
    required=True,
    type=click.Path(dir_okay=False, path_type=Path),
    help=(
        "JSONL file each real sandbox-execution Span is appended to "
        "(created if it doesn't exist). One Span per genuinely-executed "
        "trial -- never for a cache hit or an early-stop skip. See "
        "docs/rfcs/2026-09-04-evals-trace-span-model.md."
    ),
)
@click.option(
    "--llm-judge",
    is_flag=True,
    default=False,
    help=(
        "Score each completed Run's captured stdout with a real Anthropic "
        "LLM-judge call instead of the deterministic scorer. Requires "
        "ANTHROPIC_API_KEY in the environment."
    ),
)
@click.option(
    "--llm-judge-panel",
    is_flag=True,
    default=False,
    help=(
        "Score with a 2-judge Bedrock panel (Claude Sonnet 5 + Claude "
        "Haiku 4.5, via AWS Bedrock's Converse API) instead of a single "
        "judge, majority-reduced via evals.judge.llm_judge."
        "reduce_panel_votes -- fail-closed on a disagreement. Mutually "
        "exclusive with --llm-judge. Both judges share the same vendor/"
        "architecture, an explicit, accepted tradeoff for AWS-only "
        "operational simplicity -- see that RFC's own honest accounting "
        "of the weaker bias-reduction premise this implies. Requires AWS "
        "credentials resolvable via boto3's standard credential chain "
        "(AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_REGION or "
        "equivalent). See docs/rfcs/2026-09-08-evals-judge-panel-reducer.md."
    ),
)
@click.option("--confidence", default=0.95, show_default=True, type=float)
@click.option(
    "--use-cache",
    is_flag=True,
    default=False,
    help=(
        "Skip re-running an EvalCase whose (task_spec, harness_config) hash "
        "already has a completed Run in --results. Never applied to "
        'tier="drift_sample" cases. Off by default — existing invocations '
        "without this flag behave exactly as today. See "
        "docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md."
    ),
)
@click.option(
    "--use-score-cache",
    is_flag=True,
    default=False,
    help=(
        "Skip re-calling the LLM judge for a case whose "
        "(output, reference, scorer_id) already has a real (non-cached) "
        "Score in --scores. Off by default — existing invocations without "
        "this flag behave exactly as today. See "
        "docs/rfcs/2026-09-05-evals-score-cache.md."
    ),
)
@click.option(
    "--judge-axes",
    default=None,
    help=(
        "Comma-separated rubric axes (e.g. correctness,safety) to judge "
        "independently, one real LLM-judge call per axis, instead of one "
        "holistic verdict. Only meaningful together with --llm-judge. "
        "Omit for the original single-verdict behavior, unchanged. See "
        "docs/rfcs/2026-09-05-evals-multi-axis-judging.md."
    ),
)
@click.option(
    "--early-stop-max-trials",
    default=None,
    type=int,
    help=(
        "Resource ceiling: a repeated-trial group (grouped by "
        "eval_case_id+revision) is force-stopped at this many trials "
        "regardless of the mSPRT's own decision. Must be given together "
        "with --early-stop-baseline-pass-rate."
    ),
)
@click.option(
    "--early-stop-baseline-pass-rate",
    default=None,
    type=float,
    help=(
        "Baseline pass rate a group's running rate is tested against, "
        "via a real anytime-valid mixture-SPRT check after every single "
        "trial — never a fixed checkpoint; see "
        "docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md."
    ),
)
@click.option(
    "--early-stop-relative-mixing-variance",
    default=1.0,
    show_default=True,
    type=float,
    help=(
        "mSPRT mixing-distribution variance, as a multiple of the null "
        "variance. Tunes detection speed/power only — never affects the "
        "false-positive-rate guarantee, for any positive value."
    ),
)
def rollout_cmd(
    suite_path: Path,
    results_path: Path,
    scores_path: Path,
    traces_path: Path,
    llm_judge: bool,
    llm_judge_panel: bool,
    confidence: float,
    use_cache: bool,
    use_score_cache: bool,
    judge_axes: str | None,
    early_stop_max_trials: int | None,
    early_stop_baseline_pass_rate: float | None,
    early_stop_relative_mixing_variance: float,
) -> None:
    """Run a suite of EvalCases through the Rollout Scheduler and score them."""
    if llm_judge and llm_judge_panel:
        raise click.UsageError(
            "--llm-judge and --llm-judge-panel are mutually exclusive."
        )

    early_stop_params = (early_stop_max_trials, early_stop_baseline_pass_rate)
    early_stop_given = sum(p is not None for p in early_stop_params)
    if early_stop_given not in (0, len(early_stop_params)):
        raise click.UsageError(
            "--early-stop-max-trials and --early-stop-baseline-pass-rate "
            "must be given together."
        )

    cases = _load_cases(suite_path)
    call_model = make_anthropic_call_model() if llm_judge else None
    panel: PanelSpec | None = None
    cached_scores = None
    cached_votes = None
    if llm_judge_panel:
        panel = [
            (
                BEDROCK_SONNET_5_MODEL_ID,
                make_bedrock_call_model(BEDROCK_SONNET_5_MODEL_ID),
            ),
            (
                BEDROCK_HAIKU_4_5_MODEL_ID,
                make_bedrock_call_model(BEDROCK_HAIKU_4_5_MODEL_ID),
            ),
        ]
        cached_votes = (
            _load_cached_panel_votes(scores_path) if use_score_cache else None
        )
    else:
        cached_scores = _load_cached_scores(scores_path) if use_score_cache else None

    cached_runs = None
    if use_cache:
        cached_runs = {
            r.cache_key: r
            for r in load_runs(results_path)
            if r.status == "completed" and not r.from_cache and r.cache_key is not None
        }
    axes = _parse_judge_axes(judge_axes)

    successes = 0
    total = 0
    scores: list[Score] = []
    span_sink: list[Span] = []

    async def _score_and_record(case: EvalCase, run: Run) -> bool:
        """Grade one real trial exactly once, whether awaited from inside
        `run_suite` (early-stop active, via `EarlyStopConfig.score_fn`) or
        from the post-hoc loop below (early-stop inactive) — never both,
        avoiding the double-billing risk a naive implementation would
        introduce for --llm-judge. Never called for a status="skipped"
        Run either way, so `total` here always matches a
        genuinely-attempted trial count.

        Deliberately async and calling `_judge_all_axes` directly (never
        via `_judge_case`, which drives its own internal `asyncio.run()`):
        this function can be invoked from inside `run_suite`'s own
        already-running event loop when early-stopping is active, and a
        nested `asyncio.run()` call in that situation raises "cannot be
        called from a running event loop" — a real bug caught by this
        project's own test suite before shipping, not a hypothetical.
        """
        nonlocal successes, total
        total += 1
        if run.status != "completed":
            click.echo(f"{case.id}: {run.status.upper()}")
            return False
        if llm_judge or llm_judge_panel:
            if case.reference is None:
                raise click.ClickException(
                    f"case {case.id!r}: LLM-judge scoring requires a reference"
                )
            outcomes = await _judge_all_axes(
                run.stdout.strip(),
                case.reference,
                axes,
                call_model=call_model,
                cached_scores=cached_scores,
                panel=panel,
                cached_votes=cached_votes,
            )
            if outcomes is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                return False
            # A case passes only if every configured axis passes -- see
            # run_cmd's identical AND-semantics for why. axes=None
            # degenerates to exactly today's one-outcome behavior.
            passed = all(o.passed for o in outcomes)
            scorer_type = "llm_judge_panel" if llm_judge_panel else "llm_judge"
            scorer_id = (
                "panel:" + "+".join(sid for sid, _ in panel)
                if llm_judge_panel
                else DEFAULT_JUDGE_MODEL
            )
            for outcome in outcomes:
                scores.append(
                    Score(
                        eval_case_id=case.id,
                        eval_case_revision=case.revision,
                        run_id=run.id,
                        scorer_id=scorer_id,
                        scorer_type=scorer_type,
                        value=outcome.passed,
                        rationale=outcome.rationale,
                        bias_mitigations_applied=outcome.bias_mitigations_applied,
                        cost_usd=outcome.cost_usd,
                        score_cache_key=outcome.score_cache_key,
                        from_cache=outcome.from_cache,
                        rubric_axis=outcome.axis,
                        tier=case.tier,
                        tags=list(case.tags),
                        flaky=case.flaky,
                        panel_votes=outcome.panel_votes,
                        quorum_reached=outcome.quorum_reached,
                    )
                )
                if outcome.axis is not None:
                    verdict = "PASS" if outcome.passed else "FAIL"
                    click.echo(f"{case.id} [{outcome.axis}]: {verdict}")
        else:
            passed = _score_run_deterministic(case, run)
            scores.append(
                Score(
                    eval_case_id=case.id,
                    eval_case_revision=case.revision,
                    run_id=run.id,
                    scorer_id=_deterministic_scorer_id(case),
                    scorer_type="deterministic",
                    value=passed,
                    cost_usd=Decimal("0"),
                    tier=case.tier,
                    tags=list(case.tags),
                    flaky=case.flaky,
                )
            )
        if passed:
            successes += 1
        click.echo(f"{case.id}: {'PASS' if passed else 'FAIL'}")
        return passed

    async def _run_and_score() -> list[Run]:
        if early_stop_given:
            early_stop = EarlyStopConfig(
                max_trials=early_stop_max_trials,
                baseline_pass_rate=early_stop_baseline_pass_rate,
                score_fn=_score_and_record,
                confidence=confidence,
                relative_mixing_variance=early_stop_relative_mixing_variance,
            )
            runs = await run_suite(
                cases,
                cached_runs=cached_runs,
                early_stop=early_stop,
                span_sink=span_sink,
            )
            # Scoring already happened inside run_suite via score_fn above
            # — this loop is purely for operator visibility into skipped
            # trials, and deliberately never touches successes/total (a
            # "skipped" Run was never attempted, so it must never affect
            # either).
            for case, run in zip(cases, runs, strict=True):
                if run.status == "skipped":
                    click.echo(f"{case.id}: SKIPPED")
        else:
            runs = await run_suite(cases, cached_runs=cached_runs, span_sink=span_sink)
            for case, run in zip(cases, runs, strict=True):
                await _score_and_record(case, run)
        return runs

    runs = asyncio.run(_run_and_score())
    append_runs(runs, results_path)

    append_scores(scores, scores_path)
    append_spans(span_sink, traces_path)
    click.echo(format_report(successes, total, confidence=confidence))


@main.command("report")
@click.option("--successes", default=None, type=int)
@click.option("--total", default=None, type=int)
@click.option(
    "--scores",
    "scores_path",
    default=None,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help=(
        "Path to a JSONL file of persisted Scores (see --scores on `run`/"
        "`rollout`). Mutually exclusive with --successes/--total and "
        "--traces. Every Score in the file counts as one trial (no dedup, "
        "no eval_case_id filtering); reported as one pass_rate/CI/"
        "total_cost_usd line per distinct scorer_type found, never blended."
    ),
)
@click.option(
    "--traces",
    "traces_path",
    default=None,
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    help=(
        "Path to a JSONL file of persisted Spans (see --traces on "
        "`rollout`). Mutually exclusive with --successes/--total and "
        "--scores. Reports the OK-vs-ERROR rate (with Wilson CI) across "
        "every real sandbox execution, plus average duration — an infra-"
        "reliability signal, never an eval-quality judgment."
    ),
)
@click.option("--confidence", default=0.95, show_default=True, type=float)
@click.option(
    "--fail-under",
    default=None,
    type=float,
    help=(
        "Real CI/CD gate: exit non-zero if a reported rate's Wilson "
        "LOWER bound (not the bare point estimate) is below this "
        "threshold. Using the lower bound, not the point estimate, means "
        "the gate only passes when we are actually --confidence-certain "
        "the true rate clears the bar — a deliberately narrower, "
        "already-computed-CI-backed check, not the full power "
        "calculation evals/ARCHITECTURE.md's own Data Model section "
        "names as the aspiration (see docs/rfcs/2026-09-05-evals-report-"
        "fail-under.md). In --scores mode, every scorer_type group is "
        "checked independently — one group failing fails the whole "
        "command, never averaged/blended across groups. Excludes any "
        "flaky=True Score from the computation (never from the printed "
        "line) — see --category-fail-under and docs/rfcs/2026-09-07-"
        "evals-cigate-refinements.md. Off by default: omitting this flag "
        "reproduces today's exact always-exit-0 behavior."
    ),
)
@click.option(
    "--tier",
    default=None,
    type=click.Choice(["golden", "regression", "drift_sample"]),
    help=(
        "Only meaningful together with --scores: filter to Scores whose "
        "denormalized EvalCase.tier matches exactly, before any grouping "
        "or gating. Applied once, up front — every printed line and "
        "every gate check (--fail-under and --category-fail-under) then "
        "operates on this narrowed set, never the full file. Omit for "
        "today's exact untiered behavior. See docs/rfcs/2026-09-07-"
        "evals-cigate-refinements.md."
    ),
)
@click.option(
    "--category-fail-under",
    "category_fail_under_raw",
    multiple=True,
    metavar="TAG:THRESHOLD",
    help=(
        "Repeatable. An independent, separately-enforced --fail-under-"
        "style gate scoped to only the Scores whose denormalized "
        "EvalCase.tags contains TAG (a plain literal string match — "
        "'category:safety' is a recommended naming convention, not an "
        "enforced one). Checked in ADDITION to --fail-under, never "
        "instead of it, and never required to co-occur with it. Applies "
        "within whatever --tier filter is also active. Computed "
        "independently per scorer_type, exactly like the aggregate gate "
        "— never blended. A TAG with zero matches is a hard error, not a "
        "silent no-op. Only meaningful together with --scores. See "
        "docs/rfcs/2026-09-07-evals-cigate-refinements.md."
    ),
)
def report_cmd(
    successes: int | None,
    total: int | None,
    scores_path: Path | None,
    traces_path: Path | None,
    confidence: float,
    fail_under: float | None,
    tier: str | None,
    category_fail_under_raw: tuple[str, ...],
) -> None:
    """Print a pass rate together with its Wilson CI.

    Three mutually exclusive input modes: raw --successes/--total counts,
    a persisted --scores JSONL file (grouped and reported one line per
    scorer_type — deterministic and llm_judge are never blended into one
    number, since they are different measurement instruments), or a
    persisted --traces JSONL file (an aggregate OK-rate line, never
    grouped — no scorer_type-like partition exists on Span).

    --tier and --category-fail-under are --scores-only refinements (per
    docs/rfcs/2026-09-07-evals-cigate-refinements.md): neither --traces
    nor raw counts carry the case-classification data either needs.
    """
    counts_partial = (successes is None) != (total is None)
    if counts_partial:
        raise click.UsageError("--successes and --total must be given together.")

    counts_given = successes is not None and total is not None
    modes_given = sum((counts_given, scores_path is not None, traces_path is not None))
    if modes_given > 1:
        raise click.UsageError(
            "--scores, --traces, and --successes/--total are mutually exclusive."
        )
    if modes_given == 0:
        raise click.UsageError(
            "Provide one of --scores, --traces, or both --successes and --total."
        )

    category_gates = _parse_category_fail_under(category_fail_under_raw)
    if tier is not None and scores_path is None:
        raise click.UsageError("--tier is only meaningful together with --scores.")
    if category_gates and scores_path is None:
        raise click.UsageError(
            "--category-fail-under is only meaningful together with --scores."
        )

    # (label, successes, total) triples checked against --fail-under;
    # (label, successes, total, threshold) quadruples checked against
    # their own --category-fail-under threshold. Both are evaluated only
    # after everything is printed — never short-circuited mid-report, so
    # an operator always sees every real number before a gate failure,
    # per docs/rfcs/2026-09-05-evals-report-fail-under.md.
    gate_checks: list[tuple[str, int, int]] = []
    category_gate_checks: list[tuple[str, int, int, float]] = []

    if traces_path is not None:
        spans = load_spans(traces_path)
        if not spans:
            raise click.ClickException(f"{traces_path}: no Spans found")
        click.echo(_format_span_report(spans, confidence))
        ok_count = sum(1 for s in spans if s.status == "OK")
        gate_checks.append(("traces ok_rate", ok_count, len(spans)))
    elif scores_path is not None:
        scores = load_scores(scores_path)
        if not scores:
            raise click.ClickException(f"{scores_path}: no Scores found")
        scores = _filter_scores_by_tier(scores, tier)
        if tier is not None and not scores:
            raise click.ClickException(
                f"{scores_path}: no Scores found for tier={tier!r}"
            )

        matched_category_tags: set[str] = set()
        for scorer_type in sorted({s.scorer_type for s in scores}):
            group = [s for s in scores if s.scorer_type == scorer_type]
            group_successes = sum(1 for s in group if s.value)
            eligible = _gate_eligible(group)
            excluded = len(group) - len(eligible)
            note = f" ({excluded} flaky excluded from gate)" if excluded else ""
            # docs/rfcs/2026-09-08-evals-judge-panel-reducer.md: a
            # quorum-tie is a real, disagreement-driven fail-closed
            # verdict, not a genuine unanimous FAIL -- surface the count
            # so an operator can tell the two apart, mirroring the
            # flaky-exclusion note's own pattern exactly.
            tie_note = ""
            if scorer_type == "llm_judge_panel":
                ties = sum(1 for s in group if s.quorum_reached is False)
                if ties:
                    tie_note = f" ({ties} quorum-tie, fail-closed)"
            click.echo(
                f"{scorer_type}: "
                + format_report(group_successes, len(group), confidence=confidence)
                + f" {_format_group_cost(group)}"
                + note
                + tie_note
            )
            if eligible:
                eligible_successes = sum(1 for s in eligible if s.value)
                gate_checks.append((scorer_type, eligible_successes, len(eligible)))

            for cat_tag, cat_threshold in category_gates:
                tagged = [s for s in group if cat_tag in s.tags]
                if not tagged:
                    continue
                matched_category_tags.add(cat_tag)
                # No extra "category:" prefix here -- cat_tag already IS
                # the literal EvalCase.tags entry being matched (e.g.
                # "category:safety" per this RFC's recommended, but not
                # enforced, naming convention); prefixing again would
                # print a confusing "[category:category:safety]".
                tagged_label = f"{scorer_type} [{cat_tag}]"
                tagged_successes = sum(1 for s in tagged if s.value)
                tagged_eligible = _gate_eligible(tagged)
                tagged_excluded = len(tagged) - len(tagged_eligible)
                tagged_note = (
                    f" ({tagged_excluded} flaky excluded from gate)"
                    if tagged_excluded
                    else ""
                )
                click.echo(
                    f"{tagged_label}: "
                    + format_report(
                        tagged_successes, len(tagged), confidence=confidence
                    )
                    + tagged_note
                )
                if tagged_eligible:
                    tagged_eligible_successes = sum(
                        1 for s in tagged_eligible if s.value
                    )
                    category_gate_checks.append(
                        (
                            tagged_label,
                            tagged_eligible_successes,
                            len(tagged_eligible),
                            cat_threshold,
                        )
                    )

        unmatched = [t for t, _ in category_gates if t not in matched_category_tags]
        if unmatched:
            tier_note = f" (within --tier {tier!r})" if tier is not None else ""
            raise click.ClickException(
                "--category-fail-under given for tag(s) with no matching "
                f"Score: {', '.join(sorted(unmatched))}{tier_note}"
            )
    else:
        click.echo(format_report(successes, total, confidence=confidence))
        gate_checks.append(("pass_rate", successes, total))

    if fail_under is not None or category_gate_checks:
        failures = []
        if fail_under is not None:
            for label, group_successes, group_total in gate_checks:
                lower, _ = wilson_interval(
                    group_successes, group_total, confidence=confidence
                )
                if lower < fail_under:
                    failures.append(
                        f"{label}: Wilson lower bound {lower:.4f} "
                        f"< --fail-under {fail_under:.4f}"
                    )
        for label, cat_successes, cat_total, cat_threshold in category_gate_checks:
            lower, _ = wilson_interval(cat_successes, cat_total, confidence=confidence)
            if lower < cat_threshold:
                failures.append(
                    f"{label}: Wilson lower bound {lower:.4f} "
                    f"< --category-fail-under {cat_threshold:.4f}"
                )
        if failures:
            raise click.ClickException("CI/CD gate failed:\n" + "\n".join(failures))


if __name__ == "__main__":
    main()
