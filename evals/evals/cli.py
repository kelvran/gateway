"""`evals` CLI.

`evals run --suite <path>` scores a checked-in suite of `EvalCase`s with the
deterministic scorer, where `task_spec.output` is a value already baked
into the suite file. `evals rollout --suite <path> --results <path>` uses a
distinct `task_spec` convention (`{image, command, timeout_s}`) — output
comes from a real `run_in_sandbox()` execution via the Rollout Scheduler,
never from the suite file itself; each `Run` is appended to `--results`
before scoring. `evals report` prints a pass rate from raw counts. All
three commands that emit a pass rate always print the Wilson confidence
interval alongside it — per `PRD.md`'s explicit success metric, a bare
percentage is never emitted on its own.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Awaitable, Callable
from pathlib import Path

import click

from evals.judge.deterministic import exact_match, regex_match
from evals.judge.llm_judge import judge
from evals.judge.providers import make_anthropic_call_model
from evals.models import EvalCase, Run
from evals.rollout.results_store import append_runs
from evals.rollout.scheduler import run_suite
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


def _judge_output(
    output: str,
    reference: str | None,
    call_model: Callable[[str], Awaitable[str]],
    case_id: str,
) -> bool | None:
    """Score `output` against `reference` with a real LLM-judge call.

    Returns `None` when the judge call itself fails (a malformed response,
    an SDK/network error) — the caller counts that as a judged error and
    moves on to the next case, mirroring
    `evals.rollout.scheduler.run_suite`'s "one case's failure never aborts
    the suite" precedent. A missing reference is a suite-authoring error,
    not a judge-call failure, so it raises rather than being swallowed.
    """
    if reference is None:
        raise click.ClickException(
            f"case {case_id!r}: LLM-judge scoring requires a reference"
        )
    try:
        result = asyncio.run(
            judge(output=output, reference=reference, call_model=call_model)
        )
    except Exception:
        return None
    return result.passed


def _judge_case(
    case: EvalCase, call_model: Callable[[str], Awaitable[str]]
) -> bool | None:
    output = case.task_spec.get("output")
    if output is None:
        raise ValueError(f"case {case.id!r}: task_spec has no 'output' to score")
    return _judge_output(output, case.reference, call_model, case.id)


def _judge_run(
    case: EvalCase, run: Run, call_model: Callable[[str], Awaitable[str]]
) -> bool | None:
    return _judge_output(run.stdout.strip(), case.reference, call_model, case.id)


def format_report(successes: int, total: int, confidence: float = 0.95) -> str:
    """Format a pass rate together with its Wilson CI — never a bare number."""
    pass_rate = successes / total if total else 0.0
    lower, upper = wilson_interval(successes, total, confidence=confidence)
    return (
        f"pass_rate={pass_rate:.4f} ({successes}/{total}) "
        f"{confidence:.0%} CI=[{lower:.4f}, {upper:.4f}]"
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
    "--llm-judge",
    is_flag=True,
    default=False,
    help=(
        "Score with a real Anthropic LLM-judge call instead of the "
        "deterministic scorer. Requires ANTHROPIC_API_KEY in the "
        "environment — see evals.judge.providers.make_anthropic_call_model."
    ),
)
@click.option("--confidence", default=0.95, show_default=True, type=float)
def run_cmd(suite_path: Path, llm_judge: bool, confidence: float) -> None:
    """Run a suite of EvalCases and print pass/fail plus a Wilson CI."""
    cases = _load_cases(suite_path)
    call_model = make_anthropic_call_model() if llm_judge else None

    successes = 0
    total = 0
    for case in cases:
        total += 1
        if llm_judge:
            passed = _judge_case(case, call_model)
            if passed is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                continue
        else:
            passed = _score_case_deterministic(case)
        if passed:
            successes += 1
        click.echo(f"{case.id}: {'PASS' if passed else 'FAIL'}")

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
def rollout_cmd(
    suite_path: Path, results_path: Path, llm_judge: bool, confidence: float
) -> None:
    """Run a suite of EvalCases through the Rollout Scheduler and score them."""
    cases = _load_cases(suite_path)
    runs = asyncio.run(run_suite(cases))
    append_runs(runs, results_path)

    call_model = make_anthropic_call_model() if llm_judge else None

    successes = 0
    total = 0
    for case, run in zip(cases, runs, strict=True):
        total += 1
        if run.status != "completed":
            click.echo(f"{case.id}: {run.status.upper()}")
            continue
        if llm_judge:
            passed = _judge_run(case, run, call_model)
            if passed is None:
                click.echo(f"{case.id}: JUDGE_ERROR")
                continue
        else:
            passed = _score_run_deterministic(case, run)
        if passed:
            successes += 1
        click.echo(f"{case.id}: {'PASS' if passed else 'FAIL'}")

    click.echo(format_report(successes, total, confidence=confidence))


@main.command("report")
@click.option("--successes", required=True, type=int)
@click.option("--total", required=True, type=int)
@click.option("--confidence", default=0.95, show_default=True, type=float)
def report_cmd(successes: int, total: int, confidence: float) -> None:
    """Print a pass rate together with its Wilson CI, given raw counts."""
    click.echo(format_report(successes, total, confidence=confidence))


if __name__ == "__main__":
    main()
