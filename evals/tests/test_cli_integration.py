"""End-to-end integration tests for `evals.cli`, driven through Click's
`CliRunner` — the real `evals run --suite <fixture>`, `evals rollout`, and
`evals report` commands, exercised exactly as a user would invoke them from
a shell.

`tests/test_stats.py` and `tests/test_llm_judge.py` already unit-test the
pieces (`wilson_interval`, `judge()`) in isolation, but nothing in this
suite before now actually invoked `evals.cli.main` itself — per the plan's
verify step, that path had only ever been exercised by hand. These tests
close that gap and, per `PRD.md`'s stated success metric, assert that a
pass rate is never printed without its Wilson CI sitting right next to it.

The `rollout` command's default-suite tests monkeypatch
`evals.rollout.scheduler.run_in_sandbox` (no Docker needed), mirroring
`tests/test_scheduler.py`'s own pattern; one real end-to-end test is gated
behind `RUN_DOCKER_TESTS=1`, mirroring `tests/test_sandbox_integration.py`.
"""

from __future__ import annotations

import os
import re

import pytest
from click.testing import CliRunner

import evals.rollout.scheduler as scheduler_module
from evals.cli import main
from evals.rollout.results_store import load_runs
from evals.rollout.sandbox import SandboxResult

_CI_PATTERN = re.compile(
    r"pass_rate=\d+\.\d{4} \(\d+/\d+\) \d+% CI=\[\d+\.\d{4}, \d+\.\d{4}\]"
)


def test_run_against_golden_fixture_prints_pass_rate_with_ci():
    runner = CliRunner()
    result = runner.invoke(
        main, ["run", "--suite", "tests/fixtures/golden_example.json"]
    )

    assert result.exit_code == 0, result.output

    # golden_example.json has 3 cases: 2 PASS (exact "Paris", regex digit
    # match), 1 FAIL (exact match "London" vs reference "Paris").
    assert "golden-capital-of-france: PASS" in result.output
    assert "golden-contains-a-number: PASS" in result.output
    assert "golden-wrong-answer: FAIL" in result.output
    assert "pass_rate=0.6667 (2/3)" in result.output

    # The pass rate and its Wilson CI must appear together on the same
    # line — never a bare percentage per PRD.md's success metric.
    assert _CI_PATTERN.search(result.output) is not None


def test_run_missing_suite_file_fails_with_nonzero_exit():
    runner = CliRunner()
    result = runner.invoke(
        main, ["run", "--suite", "tests/fixtures/does-not-exist.json"]
    )

    assert result.exit_code != 0
    assert "does-not-exist.json" in result.output


def test_report_prints_pass_rate_with_ci_never_a_bare_percentage():
    runner = CliRunner()
    result = runner.invoke(main, ["report", "--successes", "8", "--total", "10"])

    assert result.exit_code == 0, result.output
    assert result.output.strip() == ("pass_rate=0.8000 (8/10) 95% CI=[0.4902, 0.9433]")
    assert _CI_PATTERN.search(result.output) is not None


def test_report_respects_custom_confidence_level():
    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--successes", "8", "--total", "10", "--confidence", "0.90"],
    )

    assert result.exit_code == 0, result.output
    assert "90% CI=" in result.output
    assert _CI_PATTERN.search(result.output) is not None


def test_llm_judge_flag_fails_with_documented_typed_error_not_a_silent_noop():
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/golden_example.json",
            "--llm-judge",
        ],
    )

    # `click.ClickException` maps to exit code 1 — a real, typed failure,
    # never a silent no-op that pretends to have scored anything.
    assert result.exit_code == 1
    assert "LLM-judge scoring is not implemented" in result.output
    assert "evals.judge.llm_judge" in result.output
    # No pass-rate line should ever be printed when the command fails
    # before scoring a single case.
    assert "pass_rate=" not in result.output


def test_rollout_against_fixture_scores_and_persists_runs(tmp_path, monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        # command is ["echo", <word>] for both fixture cases.
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/rollout_example.json",
            "--results",
            str(results_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output
    assert _CI_PATTERN.search(result.output) is not None

    persisted = load_runs(results_path)
    assert len(persisted) == 2
    assert persisted[0].eval_case_id == "rollout-echo-hello"
    assert persisted[0].status == "completed"
    assert persisted[0].stdout == "hello-rollout\n"


def test_rollout_missing_suite_file_fails_with_nonzero_exit():
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/does-not-exist.json",
            "--results",
            "unused.jsonl",
        ],
    )

    assert result.exit_code != 0
    assert "does-not-exist.json" in result.output


@pytest.mark.integration
@pytest.mark.skipif(
    os.environ.get("RUN_DOCKER_TESTS") != "1",
    reason="requires a live Docker daemon; set RUN_DOCKER_TESTS=1 to run",
)
def test_rollout_against_a_real_docker_daemon(tmp_path):
    results_path = tmp_path / "results.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/rollout_example.json",
            "--results",
            str(results_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output

    persisted = load_runs(results_path)
    assert len(persisted) == 2
    assert persisted[0].stdout.strip() == "hello-rollout"
