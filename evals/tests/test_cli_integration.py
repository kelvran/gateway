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

Both `run` and `rollout` require a `--scores <path>` option (per
docs/rfcs/2026-09-04-evals-score-model.md) — every invocation below supplies
one, and the Score-persistence tests assert against it directly.
"""

from __future__ import annotations

import json
import os
import re

import pytest
from click.testing import CliRunner

import evals.cli as cli_module
import evals.rollout.scheduler as scheduler_module
from evals.cli import main
from evals.results_store import load_runs, load_scores
from evals.rollout.sandbox import SandboxResult

_CI_PATTERN = re.compile(
    r"pass_rate=\d+\.\d{4} \(\d+/\d+\) \d+% CI=\[\d+\.\d{4}, \d+\.\d{4}\]"
)


def test_run_against_golden_fixture_prints_pass_rate_with_ci(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/golden_example.json",
            "--scores",
            str(scores_path),
        ],
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

    persisted = load_scores(scores_path)
    assert len(persisted) == 3
    assert all(s.scorer_type == "deterministic" for s in persisted)
    assert all(s.run_id is None for s in persisted)
    assert persisted[0].eval_case_id == "golden-capital-of-france"
    assert persisted[0].value is True
    assert persisted[2].eval_case_id == "golden-wrong-answer"
    assert persisted[2].value is False


def test_run_missing_suite_file_fails_with_nonzero_exit(tmp_path):
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/does-not-exist.json",
            "--scores",
            str(tmp_path / "scores.jsonl"),
        ],
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


def test_run_with_llm_judge_scores_via_real_wiring_using_a_fake_provider(
    tmp_path, monkeypatch
):
    responses = iter(
        [
            "REASONING: matches exactly.\nVERDICT: PASS\n",
            "REASONING: does not match.\nVERDICT: FAIL\n",
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        return next(responses)

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case: PASS" in result.output
    assert "judge-fail-case: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output
    assert _CI_PATTERN.search(result.output) is not None

    persisted = load_scores(scores_path)
    assert len(persisted) == 2
    assert all(s.scorer_type == "llm_judge" for s in persisted)
    assert all(s.run_id is None for s in persisted)
    assert persisted[0].value is True
    assert persisted[0].rationale == "matches exactly."
    assert persisted[0].bias_mitigations_applied == [
        "cot_forcing",
        "reference_guided_grading",
    ]
    assert persisted[1].value is False


def test_run_llm_judge_requires_a_reference_and_fails_loudly(tmp_path, monkeypatch):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            [
                {
                    "id": "no-ref-case",
                    "revision": 1,
                    "task_spec": {"output": "x"},
                    "reference": None,
                    "tier": "golden",
                }
            ]
        )
    )
    scores_path = tmp_path / "scores.jsonl"

    async def fake_call_model(prompt: str) -> str:
        return "REASONING: n/a.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            str(suite_path),
            "--scores",
            str(scores_path),
            "--llm-judge",
        ],
    )

    # `click.ClickException` maps to exit code 1 — a real, typed failure,
    # never a silent no-op that pretends to have scored anything.
    assert result.exit_code == 1
    assert "requires a reference" in result.output
    assert "pass_rate=" not in result.output
    assert not scores_path.exists()


def test_run_llm_judge_call_error_marks_judge_error_and_does_not_abort_suite(
    tmp_path, monkeypatch
):
    call_count = {"n": 0}

    async def flaky_call_model(prompt: str) -> str:
        call_count["n"] += 1
        if call_count["n"] == 1:
            raise RuntimeError("simulated API failure")
        return "REASONING: ok.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: flaky_call_model
    )

    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case: JUDGE_ERROR" in result.output
    assert "judge-fail-case: PASS" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output

    # The JUDGE_ERROR case never produces a Score — only the one case that
    # was actually judged does.
    persisted = load_scores(scores_path)
    assert len(persisted) == 1
    assert persisted[0].eval_case_id == "judge-fail-case"
    assert persisted[0].value is True


def test_rollout_against_fixture_scores_and_persists_runs(tmp_path, monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        # command is ["echo", <word>] for both fixture cases.
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/rollout_example.json",
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output
    assert _CI_PATTERN.search(result.output) is not None

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 2
    assert persisted_runs[0].eval_case_id == "rollout-echo-hello"
    assert persisted_runs[0].status == "completed"
    assert persisted_runs[0].stdout == "hello-rollout\n"

    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 2
    assert all(s.scorer_type == "deterministic" for s in persisted_scores)
    assert persisted_scores[0].run_id == persisted_runs[0].id
    assert persisted_scores[0].value is True
    assert persisted_scores[1].run_id == persisted_runs[1].id
    assert persisted_scores[1].value is False


def test_rollout_with_llm_judge_scores_captured_stdout_via_real_wiring(
    tmp_path, monkeypatch
):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    responses = iter(
        [
            "REASONING: matches.\nVERDICT: PASS\n",
            "REASONING: no match.\nVERDICT: FAIL\n",
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        return next(responses)

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/rollout_example.json",
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--llm-judge",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 2

    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 2
    assert all(s.scorer_type == "llm_judge" for s in persisted_scores)
    assert persisted_scores[0].run_id == persisted_runs[0].id
    assert persisted_scores[0].rationale == "matches."
    assert persisted_scores[0].bias_mitigations_applied == [
        "cot_forcing",
        "reference_guided_grading",
    ]


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
            "--scores",
            "unused_scores.jsonl",
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
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            "tests/fixtures/rollout_example.json",
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 2
    assert persisted_runs[0].stdout.strip() == "hello-rollout"

    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 2
