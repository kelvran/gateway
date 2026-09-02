"""`evals` CLI.

`evals run --suite <path>` scores a checked-in suite of `EvalCase`s with the
deterministic scorer and `evals report` prints a pass rate. Both commands
always print the Wilson confidence interval alongside the pass rate — per
`PRD.md`'s explicit success metric, a bare percentage is never emitted on
its own.
"""

from __future__ import annotations

import json
from pathlib import Path

import click

from evals.judge.deterministic import exact_match, regex_match
from evals.models import EvalCase
from evals.stats import wilson_interval


def _load_cases(suite_path: Path) -> list[EvalCase]:
    raw_cases = json.loads(suite_path.read_text())
    return [EvalCase(**raw_case) for raw_case in raw_cases]


def _score_case_deterministic(case: EvalCase) -> bool:
    output = case.task_spec.get("output")
    if output is None:
        raise ValueError(f"case {case.id!r}: task_spec has no 'output' to score")

    match_kind = case.task_spec.get("match", "exact")
    if match_kind == "exact":
        if case.reference is None:
            raise ValueError(f"case {case.id!r}: exact match requires a reference")
        return exact_match(output, case.reference)
    if match_kind == "regex":
        pattern = case.task_spec.get("pattern")
        if pattern is None:
            raise ValueError(
                f"case {case.id!r}: regex match requires task_spec.pattern"
            )
        return regex_match(output, pattern)
    raise ValueError(f"case {case.id!r}: unknown task_spec.match kind {match_kind!r}")


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
    help="Path to a JSON file containing a list of EvalCase objects.",
)
@click.option(
    "--llm-judge",
    is_flag=True,
    default=False,
    help=(
        "Score with the LLM-judge instead of the deterministic scorer. Needs a "
        "real provider API key and is not wired to a live provider SDK in this "
        "scaffolding pass — see docs/rfcs/2026-09-02-initial-code-scaffolding.md."
    ),
)
@click.option("--confidence", default=0.95, show_default=True, type=float)
def run_cmd(suite_path: Path, llm_judge: bool, confidence: float) -> None:
    """Run a suite of EvalCases and print pass/fail plus a Wilson CI."""
    if llm_judge:
        raise click.ClickException(
            "LLM-judge scoring is not implemented in `evals run` yet — the "
            "judge() function exists (evals.judge.llm_judge) and is unit "
            "tested with a mocked call_model, but no live provider SDK is "
            "wired into the CLI in this scaffolding pass. Not a silent "
            "no-op: use evals.judge.llm_judge.judge() directly with your "
            "own call_model until this lands."
        )

    cases = _load_cases(suite_path)
    successes = 0
    total = 0
    for case in cases:
        passed = _score_case_deterministic(case)
        total += 1
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
