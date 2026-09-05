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
from decimal import Decimal
from types import SimpleNamespace

import pytest
from click.testing import CliRunner

import evals.cli as cli_module
import evals.rollout.scheduler as scheduler_module
from evals.cli import main
from evals.models import Score, Span
from evals.results_store import (
    append_scores,
    append_spans,
    load_runs,
    load_scores,
    load_spans,
)
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


def _write_scores(path, scores):
    append_scores(scores, path)


def _make_score(eval_case_id, scorer_type, value, cost_usd=Decimal("0")):
    return Score(
        eval_case_id=eval_case_id,
        eval_case_revision=1,
        scorer_id="exact_match" if scorer_type == "deterministic" else "judge-model",
        scorer_type=scorer_type,
        value=value,
        cost_usd=cost_usd,
    )


def test_report_scores_reads_persisted_deterministic_scores(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            _make_score("c1", "deterministic", True),
            _make_score("c2", "deterministic", True),
            _make_score("c3", "deterministic", False),
        ],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert "deterministic: pass_rate=0.6667 (2/3)" in result.output
    assert "total_cost_usd=0" in result.output
    assert _CI_PATTERN.search(result.output) is not None


def test_report_scores_never_blends_distinct_scorer_types(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            _make_score("c1", "deterministic", True),
            _make_score("c2", "deterministic", True),
            _make_score("c1", "llm_judge", True, cost_usd=Decimal("0.001")),
            _make_score("c2", "llm_judge", False, cost_usd=Decimal("0.002")),
            _make_score("c3", "llm_judge", False, cost_usd=Decimal("0.003")),
        ],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    # deterministic: 2/2 (blended with llm_judge's 1/3 would read as 3/5 —
    # confirms the two groups are never combined into one number).
    assert "deterministic: pass_rate=1.0000 (2/2)" in result.output
    assert "llm_judge: pass_rate=0.3333 (1/3)" in result.output
    lines = [line for line in result.output.splitlines() if "pass_rate=" in line]
    assert len(lines) == 2
    assert lines[0].startswith("deterministic:")
    assert lines[1].startswith("llm_judge:")
    # Cost is summed within each group, never across groups.
    assert "total_cost_usd=0" in lines[0]
    assert "total_cost_usd=0.006" in lines[1]


def test_report_scores_notes_unknown_cost_entries_rather_than_treating_as_zero(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            _make_score("c1", "llm_judge", True, cost_usd=Decimal("0.001")),
            _make_score("c2", "llm_judge", True, cost_usd=None),
        ],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert "total_cost_usd=0.001 (1 unknown excluded)" in result.output


def test_report_scores_empty_file_fails_with_nonzero_exit(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    scores_path.write_text("")

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code != 0
    assert "no Scores found" in result.output


def test_report_scores_mutually_exclusive_with_raw_counts(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(scores_path, [_make_score("c1", "deterministic", True)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--successes",
            "1",
            "--total",
            "1",
        ],
    )

    assert result.exit_code != 0
    assert "mutually exclusive" in result.output


def test_report_successes_without_total_fails_with_nonzero_exit():
    runner = CliRunner()
    result = runner.invoke(main, ["report", "--successes", "1"])

    assert result.exit_code != 0
    assert "must be given together" in result.output


def test_report_with_no_input_mode_fails_with_nonzero_exit():
    runner = CliRunner()
    result = runner.invoke(main, ["report"])

    assert result.exit_code != 0
    assert "Provide one of" in result.output


def test_report_scores_missing_file_fails_via_click_path_validation():
    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--scores", "tests/fixtures/does-not-exist.jsonl"]
    )

    assert result.exit_code != 0
    # click's own exists=True validation must catch this, not the later,
    # vaguer "no Scores found" ClickException.
    assert "no Scores found" not in result.output
    assert "does-not-exist.jsonl" in result.output


def _make_span(status="OK", start_ns=0, duration_ms=10):
    return Span(
        span_id="a" * 16,
        trace_id="b" * 32,
        run_id="run-001",
        name="sandbox.exec",
        start_time_unix_nano=start_ns,
        end_time_unix_nano=start_ns + duration_ms * 1_000_000,
        status=status,
        process_command_args=["echo", "hi"],
        container_image_name="alpine:3.20",
    )


def test_report_traces_reads_persisted_spans(tmp_path):
    traces_path = tmp_path / "traces.jsonl"
    append_spans(
        [
            _make_span(status="OK", duration_ms=10),
            _make_span(status="OK", duration_ms=20),
            _make_span(status="ERROR", duration_ms=5),
        ],
        traces_path,
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--traces", str(traces_path)])

    assert result.exit_code == 0, result.output
    # Wilson CI verified via the exact expected string, not _CI_PATTERN --
    # that regex is scoped to the "pass_rate=" label, which this
    # deliberately never uses (see _format_span_report's docstring).
    assert result.output.strip() == (
        "spans: ok_rate=0.6667 (2/3) 95% CI=[0.2077, 0.9385] avg_duration_ms=11.67"
    )


def test_report_traces_empty_file_fails_with_nonzero_exit(tmp_path):
    traces_path = tmp_path / "traces.jsonl"
    traces_path.write_text("")

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--traces", str(traces_path)])

    assert result.exit_code != 0
    assert "no Spans found" in result.output


def test_report_traces_mutually_exclusive_with_scores(tmp_path):
    traces_path = tmp_path / "traces.jsonl"
    append_spans([_make_span()], traces_path)
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(scores_path, [_make_score("c1", "deterministic", True)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--traces", str(traces_path), "--scores", str(scores_path)],
    )

    assert result.exit_code != 0
    assert "mutually exclusive" in result.output


def test_report_traces_mutually_exclusive_with_raw_counts(tmp_path):
    traces_path = tmp_path / "traces.jsonl"
    append_spans([_make_span()], traces_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--traces", str(traces_path), "--successes", "1", "--total", "1"],
    )

    assert result.exit_code != 0
    assert "mutually exclusive" in result.output


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
    # No cost is fabricated for a fake provider that doesn't expose
    # last_call_cost — the plain-async-function fake above has no such
    # attribute, so cost_usd must fall back to None, not a guess.
    assert persisted[0].cost_usd is None


def test_run_with_llm_judge_persists_real_cost_from_a_cost_exposing_fake(
    tmp_path, monkeypatch
):
    responses = iter(
        [
            "REASONING: matches exactly.\nVERDICT: PASS\n",
            "REASONING: does not match.\nVERDICT: FAIL\n",
        ]
    )

    class _FakeCostExposingCallModel:
        def __init__(self) -> None:
            self.last_call_cost = None

        async def __call__(self, prompt: str) -> str:
            self.last_call_cost = SimpleNamespace(cost_usd=0.0001234)
            return next(responses)

    fake_call_model = _FakeCostExposingCallModel()
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

    persisted = load_scores(scores_path)
    assert len(persisted) == 2
    assert persisted[0].cost_usd == Decimal("0.0001234")
    assert persisted[1].cost_usd == Decimal("0.0001234")


def test_run_deterministic_scores_have_exact_zero_cost(tmp_path):
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

    persisted = load_scores(scores_path)
    assert len(persisted) == 3
    assert all(s.cost_usd == Decimal("0") for s in persisted)


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


def test_run_use_score_cache_second_invocation_makes_no_new_judge_calls(
    tmp_path, monkeypatch
):
    call_count = {"n": 0}

    async def fake_call_model(prompt: str) -> str:
        call_count["n"] += 1
        if call_count["n"] == 1:
            return "REASONING: matches exactly.\nVERDICT: PASS\n"
        return "REASONING: does not match.\nVERDICT: FAIL\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    scores_path = tmp_path / "scores.jsonl"
    args = [
        "run",
        "--suite",
        "tests/fixtures/llm_judge_example.json",
        "--scores",
        str(scores_path),
        "--llm-judge",
        "--use-score-cache",
    ]
    runner = CliRunner()

    first = runner.invoke(main, args)
    assert first.exit_code == 0, first.output
    assert call_count["n"] == 2

    second = runner.invoke(main, args)
    assert second.exit_code == 0, second.output
    # Both cases hit the score cache on the second invocation -- no new
    # judge calls at all.
    assert call_count["n"] == 2
    assert "judge-pass-case: PASS" in second.output
    assert "judge-fail-case: FAIL" in second.output

    persisted = load_scores(scores_path)
    assert len(persisted) == 4
    assert persisted[2].from_cache is True
    assert persisted[2].cost_usd == Decimal("0")
    assert persisted[2].value == persisted[0].value
    assert persisted[2].rationale == persisted[0].rationale
    assert persisted[3].from_cache is True


def test_run_without_use_score_cache_rejudges_every_time(tmp_path, monkeypatch):
    call_count = {"n": 0}

    async def fake_call_model(prompt: str) -> str:
        call_count["n"] += 1
        return "REASONING: ok.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    scores_path = tmp_path / "scores.jsonl"
    args = [
        "run",
        "--suite",
        "tests/fixtures/llm_judge_example.json",
        "--scores",
        str(scores_path),
        "--llm-judge",
    ]
    runner = CliRunner()
    runner.invoke(main, args)
    runner.invoke(main, args)

    assert call_count["n"] == 4


def test_rollout_use_score_cache_second_invocation_makes_no_new_judge_calls(
    tmp_path, monkeypatch
):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    call_count = {"n": 0}

    async def fake_call_model(prompt: str) -> str:
        call_count["n"] += 1
        return "REASONING: ok.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    args = [
        "rollout",
        "--suite",
        "tests/fixtures/rollout_example.json",
        "--results",
        str(results_path),
        "--scores",
        str(scores_path),
        "--traces",
        str(traces_path),
        "--llm-judge",
        "--use-score-cache",
    ]
    runner = CliRunner()

    first = runner.invoke(main, args)
    assert first.exit_code == 0, first.output
    assert call_count["n"] == 2

    second = runner.invoke(main, args)
    assert second.exit_code == 0, second.output
    # Every real Run is re-executed (no --use-cache here), but each one's
    # captured stdout is byte-identical to the first invocation's, so the
    # judge call itself is skipped both times on the second pass.
    assert call_count["n"] == 2

    persisted = load_scores(scores_path)
    assert len(persisted) == 4
    assert persisted[2].from_cache is True
    assert persisted[3].from_cache is True


def test_rollout_against_fixture_scores_and_persists_runs(tmp_path, monkeypatch):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        # command is ["echo", <word>] for both fixture cases.
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
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
            "--traces",
            str(traces_path),
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
    assert persisted_scores[0].cost_usd == Decimal("0")
    assert persisted_scores[1].run_id == persisted_runs[1].id
    assert persisted_scores[1].value is False
    assert persisted_scores[1].cost_usd == Decimal("0")

    persisted_spans = load_spans(traces_path)
    assert len(persisted_spans) == 2
    assert persisted_spans[0].run_id == persisted_runs[0].id
    assert persisted_spans[0].status == "OK"
    assert persisted_spans[1].run_id == persisted_runs[1].id
    assert persisted_spans[1].status == "OK"


def test_rollout_use_cache_second_invocations_cache_hits_produce_no_new_spans(
    tmp_path, monkeypatch
):
    call_count = {"n": 0}

    async def _fake_run_in_sandbox(image, command, timeout_s):
        call_count["n"] += 1
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    args = [
        "rollout",
        "--suite",
        "tests/fixtures/rollout_example.json",
        "--results",
        str(results_path),
        "--scores",
        str(scores_path),
        "--traces",
        str(traces_path),
        "--use-cache",
    ]
    runner = CliRunner()

    first = runner.invoke(main, args)
    assert first.exit_code == 0, first.output
    second = runner.invoke(main, args)
    assert second.exit_code == 0, second.output

    # 2 real executions total (both from the first invocation) -- the
    # second invocation's 2 cases are both cache hits, so zero new spans.
    persisted_spans = load_spans(traces_path)
    assert len(persisted_spans) == 2


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
    traces_path = tmp_path / "traces.jsonl"
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
            "--traces",
            str(traces_path),
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


def _repeated_trial_suite_path(tmp_path, n: int, case_id: str = "repeated-case"):
    suite_path = tmp_path / "repeated_suite.json"
    suite_path.write_text(
        json.dumps(
            [
                {
                    "id": case_id,
                    "revision": 1,
                    "task_spec": {
                        "image": "alpine:3.20",
                        "command": ["echo", "hello-rollout"],
                        "timeout_s": 30,
                        "match": "exact",
                    },
                    "reference": "hello-rollout",
                    "tier": "golden",
                }
                for _ in range(n)
            ]
        )
    )
    return suite_path


def test_rollout_use_cache_skips_second_invocations_sandbox_calls(
    tmp_path, monkeypatch
):
    call_count = {"n": 0}

    async def _fake_run_in_sandbox(image, command, timeout_s):
        call_count["n"] += 1
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    args = [
        "rollout",
        "--suite",
        "tests/fixtures/rollout_example.json",
        "--results",
        str(results_path),
        "--scores",
        str(scores_path),
        "--traces",
        str(traces_path),
        "--use-cache",
    ]
    runner = CliRunner()

    first = runner.invoke(main, args)
    assert first.exit_code == 0, first.output
    assert call_count["n"] == 2

    second = runner.invoke(main, args)
    assert second.exit_code == 0, second.output
    # Both real cases hit the cache on the second invocation -- no new
    # sandbox calls at all.
    assert call_count["n"] == 2

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 4
    assert persisted_runs[2].from_cache is True
    assert persisted_runs[3].from_cache is True


def test_rollout_without_use_cache_reruns_everything_every_time(tmp_path, monkeypatch):
    call_count = {"n": 0}

    async def _fake_run_in_sandbox(image, command, timeout_s):
        call_count["n"] += 1
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    args = [
        "rollout",
        "--suite",
        "tests/fixtures/rollout_example.json",
        "--results",
        str(results_path),
        "--scores",
        str(scores_path),
        "--traces",
        str(traces_path),
    ]
    runner = CliRunner()
    runner.invoke(main, args)
    runner.invoke(main, args)

    assert call_count["n"] == 4


def test_rollout_early_stop_flags_must_be_given_together(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
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
            "--scores",
            str(scores_path),
            "--traces",
            str(traces_path),
            "--early-stop-min-trials",
            "2",
            # deliberately omitting the other two flags
        ],
    )
    assert result.exit_code != 0
    assert "must be given together" in result.output


def test_rollout_early_stop_skips_remaining_trials_and_total_excludes_them(
    tmp_path, monkeypatch
):
    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout="hello-rollout\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    suite_path = _repeated_trial_suite_path(tmp_path, n=6)
    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--traces",
            str(traces_path),
            "--early-stop-min-trials",
            "2",
            "--early-stop-max-trials",
            "10",
            "--early-stop-baseline-pass-rate",
            "0.01",
        ],
    )

    assert result.exit_code == 0, result.output
    assert result.output.count("SKIPPED") == 4
    # total must reflect only the 2 real trials that actually ran -- the 4
    # never-attempted skipped trials must not count in the denominator.
    assert "(2/2)" in result.output

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 6
    assert sum(1 for r in persisted_runs if r.status == "skipped") == 4

    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 2  # never scored for skipped runs


def test_rollout_early_stop_with_llm_judge_never_double_calls_judge(
    tmp_path, monkeypatch
):
    async def _always_succeed(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout="hello-rollout\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _always_succeed)

    judge_call_count = {"n": 0}

    async def fake_call_model(prompt: str) -> str:
        judge_call_count["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_anthropic_call_model", lambda: fake_call_model
    )

    suite_path = _repeated_trial_suite_path(tmp_path, n=6)
    results_path = tmp_path / "results.jsonl"
    scores_path = tmp_path / "scores.jsonl"
    traces_path = tmp_path / "traces.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "rollout",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--traces",
            str(traces_path),
            "--llm-judge",
            "--early-stop-min-trials",
            "2",
            "--early-stop-max-trials",
            "10",
            "--early-stop-baseline-pass-rate",
            "0.01",
        ],
    )

    assert result.exit_code == 0, result.output
    # Exactly 2 real (non-skipped) trials ran before the group stopped --
    # the judge must be called exactly twice, never 4 times (double-billing).
    assert judge_call_count["n"] == 2


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
            "--traces",
            "unused_traces.jsonl",
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
    traces_path = tmp_path / "traces.jsonl"
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
            "--traces",
            str(traces_path),
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

    persisted_spans = load_spans(traces_path)
    assert len(persisted_spans) == 2
    for span in persisted_spans:
        assert span.status == "OK"
        assert span.container_id is not None
        assert len(span.container_id) == 64
