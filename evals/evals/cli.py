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
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from decimal import Decimal
from pathlib import Path

import click

from evals.judge.cache import compute_score_cache_key
from evals.judge.deterministic import exact_match, regex_match
from evals.judge.llm_judge import judge
from evals.judge.providers import DEFAULT_JUDGE_MODEL, make_anthropic_call_model
from evals.models import EvalCase, Run, Score, Span
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


def _load_cases(suite_path: Path) -> list[EvalCase]:
    raw_cases = json.loads(suite_path.read_text())
    return [EvalCase(**raw_case) for raw_case in raw_cases]


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
    """

    passed: bool
    rationale: str | None
    bias_mitigations_applied: list[str]
    cost_usd: Decimal | None
    score_cache_key: str | None
    from_cache: bool
    axis: str | None = None


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


async def _judge_all_axes(
    output: str,
    reference: str,
    call_model: Callable[[str], Awaitable[str]],
    cached_scores: dict[str, Score] | None,
    axes: list[str] | None,
) -> list[_JudgeOutcome] | None:
    """Judge `output` against `reference` once per configured axis (or
    once, holistically, if `axes` is `None`) — one `_judge_with_cache`
    call per axis, per docs/rfcs/2026-09-05-evals-multi-axis-judging.md's
    one-call-per-axis design, never one call trying to cover several.

    Returns `None` if ANY axis's judge call fails — the whole case is
    treated as `JUDGE_ERROR`, mirroring the pre-existing single-axis
    all-or-nothing behavior, rather than persisting a confusing partial
    set of per-axis `Score`s for one case.
    """
    if axes is None:
        outcome = await _judge_with_cache(
            output, reference, DEFAULT_JUDGE_MODEL, call_model, cached_scores
        )
        return None if outcome is None else [outcome]
    outcomes: list[_JudgeOutcome] = []
    for axis in axes:
        outcome = await _judge_with_cache(
            output, reference, DEFAULT_JUDGE_MODEL, call_model, cached_scores, axis=axis
        )
        if outcome is None:
            return None
        outcomes.append(outcome)
    return outcomes


def _judge_case(
    case: EvalCase,
    call_model: Callable[[str], Awaitable[str]],
    cached_scores: dict[str, Score] | None,
    axes: list[str] | None = None,
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
        _judge_all_axes(output, case.reference, call_model, cached_scores, axes)
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


@click.group()
def main() -> None:
    """Kelvran evals CLI."""


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
@click.option("--confidence", default=0.95, show_default=True, type=float)
def run_cmd(
    suite_path: Path,
    scores_path: Path,
    llm_judge: bool,
    use_score_cache: bool,
    judge_axes: str | None,
    confidence: float,
) -> None:
    """Run a suite of EvalCases and print pass/fail plus a Wilson CI."""
    cases = _load_cases(suite_path)
    call_model = make_anthropic_call_model() if llm_judge else None
    cached_scores = _load_cached_scores(scores_path) if use_score_cache else None
    axes = _parse_judge_axes(judge_axes)

    successes = 0
    total = 0
    scores: list[Score] = []
    for case in cases:
        total += 1
        if llm_judge:
            outcomes = _judge_case(case, call_model, cached_scores, axes)
            if outcomes is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                continue
            # A case passes only if every configured axis passes -- a
            # response isn't good if it fails one dimension even though
            # it's correct on every other one. Single-axis (axes=None)
            # degenerates to exactly today's one-outcome behavior.
            passed = all(o.passed for o in outcomes)
            for outcome in outcomes:
                scores.append(
                    Score(
                        eval_case_id=case.id,
                        eval_case_revision=case.revision,
                        run_id=None,
                        scorer_id=DEFAULT_JUDGE_MODEL,
                        scorer_type="llm_judge",
                        value=outcome.passed,
                        rationale=outcome.rationale,
                        bias_mitigations_applied=outcome.bias_mitigations_applied,
                        cost_usd=outcome.cost_usd,
                        score_cache_key=outcome.score_cache_key,
                        from_cache=outcome.from_cache,
                        rubric_axis=outcome.axis,
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
                )
            )
        if passed:
            successes += 1
        click.echo(f"{case.id}: {'PASS' if passed else 'FAIL'}")

    append_scores(scores, scores_path)
    click.echo(format_report(successes, total, confidence=confidence))


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
    confidence: float,
    use_cache: bool,
    use_score_cache: bool,
    judge_axes: str | None,
    early_stop_max_trials: int | None,
    early_stop_baseline_pass_rate: float | None,
    early_stop_relative_mixing_variance: float,
) -> None:
    """Run a suite of EvalCases through the Rollout Scheduler and score them."""
    early_stop_params = (early_stop_max_trials, early_stop_baseline_pass_rate)
    early_stop_given = sum(p is not None for p in early_stop_params)
    if early_stop_given not in (0, len(early_stop_params)):
        raise click.UsageError(
            "--early-stop-max-trials and --early-stop-baseline-pass-rate "
            "must be given together."
        )

    cases = _load_cases(suite_path)
    call_model = make_anthropic_call_model() if llm_judge else None

    cached_runs = None
    if use_cache:
        cached_runs = {
            r.cache_key: r
            for r in load_runs(results_path)
            if r.status == "completed" and not r.from_cache and r.cache_key is not None
        }
    cached_scores = _load_cached_scores(scores_path) if use_score_cache else None
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
        if llm_judge:
            if case.reference is None:
                raise click.ClickException(
                    f"case {case.id!r}: LLM-judge scoring requires a reference"
                )
            outcomes = await _judge_all_axes(
                run.stdout.strip(), case.reference, call_model, cached_scores, axes
            )
            if outcomes is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                return False
            # A case passes only if every configured axis passes -- see
            # run_cmd's identical AND-semantics for why. axes=None
            # degenerates to exactly today's one-outcome behavior.
            passed = all(o.passed for o in outcomes)
            for outcome in outcomes:
                scores.append(
                    Score(
                        eval_case_id=case.id,
                        eval_case_revision=case.revision,
                        run_id=run.id,
                        scorer_id=DEFAULT_JUDGE_MODEL,
                        scorer_type="llm_judge",
                        value=outcome.passed,
                        rationale=outcome.rationale,
                        bias_mitigations_applied=outcome.bias_mitigations_applied,
                        cost_usd=outcome.cost_usd,
                        score_cache_key=outcome.score_cache_key,
                        from_cache=outcome.from_cache,
                        rubric_axis=outcome.axis,
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
        "command, never averaged/blended across groups. Off by default: "
        "omitting this flag reproduces today's exact always-exit-0 "
        "behavior."
    ),
)
def report_cmd(
    successes: int | None,
    total: int | None,
    scores_path: Path | None,
    traces_path: Path | None,
    confidence: float,
    fail_under: float | None,
) -> None:
    """Print a pass rate together with its Wilson CI.

    Three mutually exclusive input modes: raw --successes/--total counts,
    a persisted --scores JSONL file (grouped and reported one line per
    scorer_type — deterministic and llm_judge are never blended into one
    number, since they are different measurement instruments), or a
    persisted --traces JSONL file (an aggregate OK-rate line, never
    grouped — no scorer_type-like partition exists on Span).
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

    # (label, successes, total) triples checked against --fail-under
    # after everything is printed — never short-circuited mid-report, so
    # an operator always sees every real number before a gate failure,
    # per docs/rfcs/2026-09-05-evals-report-fail-under.md.
    gate_checks: list[tuple[str, int, int]] = []

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
        for scorer_type in sorted({s.scorer_type for s in scores}):
            group = [s for s in scores if s.scorer_type == scorer_type]
            group_successes = sum(1 for s in group if s.value)
            click.echo(
                f"{scorer_type}: "
                + format_report(group_successes, len(group), confidence=confidence)
                + f" {_format_group_cost(group)}"
            )
            gate_checks.append((scorer_type, group_successes, len(group)))
    else:
        click.echo(format_report(successes, total, confidence=confidence))
        gate_checks.append(("pass_rate", successes, total))

    if fail_under is not None:
        failures = []
        for label, group_successes, group_total in gate_checks:
            lower, _ = wilson_interval(
                group_successes, group_total, confidence=confidence
            )
            if lower < fail_under:
                failures.append(
                    f"{label}: Wilson lower bound {lower:.4f} "
                    f"< --fail-under {fail_under:.4f}"
                )
        if failures:
            raise click.ClickException("CI/CD gate failed:\n" + "\n".join(failures))


if __name__ == "__main__":
    main()
