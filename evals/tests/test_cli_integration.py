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
from evals.judge.cache import compute_score_cache_key
from evals.models import PanelVote, Score, Span
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


def test_report_fail_under_passes_when_lower_bound_clears_the_bar():
    # 8/10 -> Wilson lower bound is 0.4902 (the exact reference value
    # test_report_prints_pass_rate_with_ci_never_a_bare_percentage
    # already pins). A --fail-under comfortably below that must exit 0.
    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--successes", "8", "--total", "10", "--fail-under", "0.4"],
    )
    assert result.exit_code == 0, result.output


def test_report_fail_under_gates_on_the_lower_bound_not_the_point_estimate():
    # 8/10's point estimate is 0.8 (would clear 0.6), but its real Wilson
    # lower bound is 0.4902 (would NOT clear 0.6) -- this is the load-
    # bearing proof that --fail-under checks the lower bound, per
    # docs/rfcs/2026-09-05-evals-report-fail-under.md, not the naive
    # point estimate a less careful implementation might use instead.
    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--successes", "8", "--total", "10", "--fail-under", "0.6"],
    )
    assert result.exit_code != 0
    assert "0.4902" in result.output
    assert "0.6000" in result.output


def test_report_fail_under_omitted_reproduces_exact_zero_exit_behavior():
    # The single most important backward-compatibility proof: omitting
    # --fail-under entirely must behave exactly as before it existed,
    # even for a low pass rate that a gate WOULD have failed.
    runner = CliRunner()
    result = runner.invoke(main, ["report", "--successes", "1", "--total", "10"])
    assert result.exit_code == 0, result.output


def _write_scores(path, scores):
    append_scores(scores, path)


def _make_score(
    eval_case_id, scorer_type, value, cost_usd=Decimal("0"), quorum_reached=None
):
    return Score(
        eval_case_id=eval_case_id,
        eval_case_revision=1,
        scorer_id="exact_match" if scorer_type == "deterministic" else "judge-model",
        scorer_type=scorer_type,
        value=value,
        cost_usd=cost_usd,
        quorum_reached=quorum_reached,
    )


def _panel_vote_for_test(output, reference, scorer_id, passed, from_cache=False):
    # The REAL cache key for (output, reference, scorer_id) -- a mismatched
    # placeholder key would make a from_cache exclusion test pass for the
    # wrong reason (a lookup miss, not the from_cache filter itself).
    return PanelVote(
        scorer_id=scorer_id,
        passed=passed,
        rationale="test rationale",
        score_cache_key=compute_score_cache_key(output, reference, scorer_id),
        from_cache=from_cache,
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


def test_report_scores_prints_llm_judge_and_panel_as_separate_never_blended_lines(
    tmp_path,
):
    # docs/rfcs/2026-09-08-evals-judge-panel-reducer.md: llm_judge_panel is
    # a genuinely distinct scorer_type, never folded into llm_judge's own
    # number -- report_cmd needs zero code changes to guarantee this since
    # it already groups purely by scorer_type, but the guarantee itself is
    # worth a dedicated test, mirroring the never-blended test just above.
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            _make_score("c1", "llm_judge", True),
            _make_score("c1", "llm_judge_panel", True, quorum_reached=True),
            _make_score("c2", "llm_judge_panel", False, quorum_reached=False),
        ],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    lines = [line for line in result.output.splitlines() if "pass_rate=" in line]
    assert len(lines) == 2
    assert lines[0].startswith("llm_judge:")
    assert lines[1].startswith("llm_judge_panel:")
    assert "pass_rate=1.0000 (1/1)" in lines[0]
    assert "pass_rate=0.5000 (1/2)" in lines[1]


def test_report_scores_shows_quorum_tie_count_for_llm_judge_panel_group(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            _make_score("c1", "llm_judge_panel", True, quorum_reached=True),
            _make_score("c2", "llm_judge_panel", False, quorum_reached=False),
            _make_score("c3", "llm_judge_panel", False, quorum_reached=False),
        ],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    line = next(
        line
        for line in result.output.splitlines()
        if line.startswith("llm_judge_panel:")
    )
    assert "(2 quorum-tie, fail-closed)" in line


def test_report_scores_shows_no_quorum_tie_note_when_every_panel_verdict_reached_quorum(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [_make_score("c1", "llm_judge_panel", True, quorum_reached=True)],
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    line = next(
        line
        for line in result.output.splitlines()
        if line.startswith("llm_judge_panel:")
    )
    assert "quorum-tie" not in line


def test_report_scores_fail_under_checks_each_scorer_type_group_independently(
    tmp_path,
):
    # Reuses test_report_scores_never_blends_distinct_scorer_types's own
    # fixture exactly: deterministic 2/2 (Wilson lower bound 0.3424),
    # llm_judge 1/3 (Wilson lower bound 0.0615) -- verified directly
    # against evals.stats.wilson_interval, not guessed. A --fail-under of
    # 0.3 clears deterministic's lower bound but not llm_judge's, so the
    # whole command must fail even though one group alone would pass --
    # never averaged/blended across groups.
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
    result = runner.invoke(
        main, ["report", "--scores", str(scores_path), "--fail-under", "0.3"]
    )

    assert result.exit_code != 0
    # Both real numbers are still printed before the gate failure -- an
    # operator sees every group's real report, never a silent short-
    # circuit that hides the passing group's own numbers.
    assert "deterministic: pass_rate=1.0000 (2/2)" in result.output
    assert "llm_judge: pass_rate=0.3333 (1/3)" in result.output
    assert "llm_judge" in result.output.split("CI/CD gate failed")[-1]
    assert "deterministic" not in result.output.split("CI/CD gate failed")[-1]


def test_report_traces_fail_under_checked_against_ok_rate(tmp_path):
    # 2/3 OK -> Wilson lower bound 0.2077, the exact reference value
    # test_report_traces_reads_persisted_spans already pins.
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
    passing = runner.invoke(
        main, ["report", "--traces", str(traces_path), "--fail-under", "0.2"]
    )
    assert passing.exit_code == 0, passing.output

    failing = runner.invoke(
        main, ["report", "--traces", str(traces_path), "--fail-under", "0.5"]
    )
    assert failing.exit_code != 0
    assert "0.2077" in failing.output


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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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


def _monkeypatch_panel_providers(monkeypatch, sonnet_response, haiku_response):
    """Shared setup for a 2-judge Bedrock panel test: distinct, fixed
    responses per judge (never a shared iter()/next() counter) so the
    test's own correctness never depends on asyncio.gather's real
    scheduling order. Both judges go through the same
    make_bedrock_call_model(model_id) factory -- dispatch on model_id to
    return the right fake for each.
    """

    async def fake_sonnet(prompt: str) -> str:
        return sonnet_response

    async def fake_haiku(prompt: str) -> str:
        return haiku_response

    def fake_make_bedrock_call_model(model_id, client=None, region_name=None):
        if model_id == cli_module.BEDROCK_SONNET_5_MODEL_ID:
            return fake_sonnet
        if model_id == cli_module.BEDROCK_HAIKU_4_5_MODEL_ID:
            return fake_haiku
        raise ValueError(f"unexpected model_id in test fake: {model_id!r}")

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", fake_make_bedrock_call_model
    )


def test_run_with_llm_judge_panel_scores_via_real_wiring_using_fake_providers(
    tmp_path, monkeypatch
):
    _monkeypatch_panel_providers(
        monkeypatch,
        sonnet_response="REASONING: matches exactly.\nVERDICT: PASS\n",
        haiku_response="REASONING: matches exactly.\nVERDICT: PASS\n",
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case: PASS" in result.output

    persisted = load_scores(scores_path)
    assert len(persisted) == 2
    assert all(s.scorer_type == "llm_judge_panel" for s in persisted)
    expected_scorer_id = (
        "panel:global.anthropic.claude-sonnet-5"
        "+global.anthropic.claude-haiku-4-5-20251001-v1:0"
    )
    assert persisted[0].scorer_id == expected_scorer_id
    assert persisted[0].quorum_reached is True
    assert persisted[0].panel_votes is not None
    assert len(persisted[0].panel_votes) == 2
    assert {v.scorer_id for v in persisted[0].panel_votes} == {
        cli_module.BEDROCK_SONNET_5_MODEL_ID,
        cli_module.BEDROCK_HAIKU_4_5_MODEL_ID,
    }


def test_run_with_llm_judge_panel_disagreement_is_fail_closed(tmp_path, monkeypatch):
    _monkeypatch_panel_providers(
        monkeypatch,
        sonnet_response="REASONING: matches.\nVERDICT: PASS\n",
        haiku_response="REASONING: does not match.\nVERDICT: FAIL\n",
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case: FAIL" in result.output

    persisted = load_scores(scores_path)
    pass_case = next(s for s in persisted if s.eval_case_id == "judge-pass-case")
    assert pass_case.value is False
    assert pass_case.quorum_reached is False


def test_run_with_llm_judge_and_llm_judge_panel_together_is_a_usage_error(tmp_path):
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code != 0
    assert "mutually exclusive" in result.output


def test_run_with_llm_judge_panel_sums_cost_across_both_providers_from_fakes(
    tmp_path, monkeypatch
):
    class _FakeCostExposingCallModel:
        def __init__(self, response: str, cost: Decimal) -> None:
            self._response = response
            self._cost = cost
            self.last_call_cost = None

        async def __call__(self, prompt: str) -> str:
            self.last_call_cost = SimpleNamespace(cost_usd=self._cost)
            return self._response

    sonnet_fake = _FakeCostExposingCallModel(
        "REASONING: matches.\nVERDICT: PASS\n", Decimal("0.001")
    )
    haiku_fake = _FakeCostExposingCallModel(
        "REASONING: matches.\nVERDICT: PASS\n", Decimal("0.0004")
    )

    def fake_make_bedrock_call_model(model_id, client=None, region_name=None):
        return (
            sonnet_fake
            if model_id == cli_module.BEDROCK_SONNET_5_MODEL_ID
            else haiku_fake
        )

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", fake_make_bedrock_call_model
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code == 0, result.output
    persisted = load_scores(scores_path)
    pass_case = next(s for s in persisted if s.eval_case_id == "judge-pass-case")
    assert pass_case.cost_usd == Decimal("0.0014")


def test_run_with_llm_judge_panel_cost_is_none_when_any_panelist_cost_is_unknown(
    tmp_path, monkeypatch
):
    # The Anthropic fake below exposes no last_call_cost at all (a plain
    # async function, like the un-cost-exposing single-judge fakes
    # elsewhere in this file) -- the WHOLE panel's cost_usd must be None,
    # never a silently-understated partial sum from the OpenAI side alone.
    _monkeypatch_panel_providers(
        monkeypatch,
        sonnet_response="REASONING: matches.\nVERDICT: PASS\n",
        haiku_response="REASONING: matches.\nVERDICT: PASS\n",
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code == 0, result.output
    persisted = load_scores(scores_path)
    assert all(s.cost_usd is None for s in persisted)


def test_run_with_llm_judge_panel_use_score_cache_second_invocation_makes_no_calls(
    tmp_path, monkeypatch
):
    sonnet_calls = {"n": 0}
    haiku_calls = {"n": 0}

    async def fake_sonnet(prompt: str) -> str:
        sonnet_calls["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    async def fake_haiku(prompt: str) -> str:
        haiku_calls["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    def fake_make_bedrock_call_model(model_id, client=None, region_name=None):
        return (
            fake_sonnet
            if model_id == cli_module.BEDROCK_SONNET_5_MODEL_ID
            else fake_haiku
        )

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", fake_make_bedrock_call_model
    )

    scores_path = tmp_path / "scores.jsonl"
    args = [
        "run",
        "--suite",
        "tests/fixtures/llm_judge_example.json",
        "--scores",
        str(scores_path),
        "--llm-judge-panel",
        "--use-score-cache",
    ]
    runner = CliRunner()

    first = runner.invoke(main, args)
    assert first.exit_code == 0, first.output
    assert sonnet_calls["n"] == 2
    assert haiku_calls["n"] == 2

    second = runner.invoke(main, args)
    assert second.exit_code == 0, second.output
    # Every panelist's vote for both cases hits the score cache -- no new
    # calls to either judge at all.
    assert sonnet_calls["n"] == 2
    assert haiku_calls["n"] == 2

    persisted = load_scores(scores_path)
    assert len(persisted) == 4
    second_pass_case = persisted[2]
    assert second_pass_case.from_cache is True
    assert second_pass_case.cost_usd == Decimal("0")
    assert all(v.from_cache for v in second_pass_case.panel_votes)


def test_run_with_llm_judge_panel_use_score_cache_reuses_per_case_not_per_suite(
    tmp_path, monkeypatch
):
    # docs/rfcs/2026-09-08-evals-judge-panel-reducer.md's cache-key design:
    # a panelist's vote is keyed by (output, reference, its own real model
    # id), independent of panel membership or which suite invocation
    # produced it -- reuse happens per CASE, not merely "the whole suite
    # ran before." Seed a real prior llm_judge_panel Score covering only
    # judge-pass-case (via a real --llm-judge-panel invocation against a
    # single-case suite, not hand-constructed), then run the full 2-case
    # suite and confirm ONLY judge-fail-case (genuinely new) triggers
    # fresh calls -- the stronger, more specific proof than "run the
    # identical suite twice, zero new calls" (already covered by the
    # sibling *_second_invocation_makes_no_calls test).
    #
    # This test covers within-panel-mode reuse only. Cross-mode reuse
    # (standalone --llm-judge and --llm-judge-panel sharing a cached
    # Haiku vote for the same model id) is real again as of --llm-judge
    # moving to Bedrock Haiku 4.5 -- see
    # test_run_with_llm_judge_panel_reuses_a_prior_standalone_haiku_score
    # and test_run_with_llm_judge_reuses_a_prior_panel_haiku_vote below.
    single_case_suite = tmp_path / "single_case_suite.json"
    single_case_suite.write_text(
        json.dumps(
            [
                {
                    "id": "judge-pass-case",
                    "revision": 1,
                    "task_spec": {"output": "Paris"},
                    "reference": "Paris",
                    "tier": "golden",
                    "tags": ["judge"],
                }
            ]
        )
    )

    calls = {"n": 0}

    async def fake_judge(prompt: str) -> str:
        calls["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, client=None, region_name=None: fake_judge,
    )

    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    seed = runner.invoke(
        main,
        [
            "run",
            "--suite",
            str(single_case_suite),
            "--scores",
            str(scores_path),
            "--llm-judge-panel",
            "--use-score-cache",
        ],
    )
    assert seed.exit_code == 0, seed.output
    assert calls["n"] == 2  # 1 case x 2 panelists

    full = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge-panel",
            "--use-score-cache",
        ],
    )
    assert full.exit_code == 0, full.output
    # judge-pass-case's 2 votes are reused; judge-fail-case's 2 are fresh.
    assert calls["n"] == 4

    persisted = load_scores(scores_path)
    pass_case_scores = [
        s
        for s in persisted
        if s.eval_case_id == "judge-pass-case" and s.scorer_type == "llm_judge_panel"
    ]
    latest_pass_case_score = pass_case_scores[-1]
    assert all(v.from_cache for v in latest_pass_case_score.panel_votes)
    fail_case_score = next(
        s
        for s in persisted
        if s.eval_case_id == "judge-fail-case" and s.scorer_type == "llm_judge_panel"
    )
    assert all(not v.from_cache for v in fail_case_score.panel_votes)


def test_run_with_llm_judge_panel_reuses_a_prior_standalone_haiku_score(
    tmp_path, monkeypatch
):
    # Real, live property as of --llm-judge moving to Bedrock Haiku 4.5
    # (the same model id as the panel's own Haiku panelist): a vote
    # cached from a standalone --llm-judge run is transparently reusable
    # inside a LATER --llm-judge-panel run for that case, via
    # _load_cached_panel_votes reading prior llm_judge Scores directly.
    _monkeypatch_panel_providers(
        monkeypatch,
        sonnet_response="REASONING: matches.\nVERDICT: PASS\n",
        haiku_response="REASONING: matches.\nVERDICT: PASS\n",
    )
    sonnet_calls = {"n": 0}
    haiku_calls = {"n": 0}
    real_fake_sonnet = cli_module.make_bedrock_call_model(
        cli_module.BEDROCK_SONNET_5_MODEL_ID
    )
    real_fake_haiku = cli_module.make_bedrock_call_model(
        cli_module.BEDROCK_HAIKU_4_5_MODEL_ID
    )

    async def counting_sonnet(prompt: str) -> str:
        sonnet_calls["n"] += 1
        return await real_fake_sonnet(prompt)

    async def counting_haiku(prompt: str) -> str:
        haiku_calls["n"] += 1
        return await real_fake_haiku(prompt)

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, client=None, region_name=None: (
            counting_sonnet
            if model_id == cli_module.BEDROCK_SONNET_5_MODEL_ID
            else counting_haiku
        ),
    )

    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    seed = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge",
            "--use-score-cache",
        ],
    )
    assert seed.exit_code == 0, seed.output
    assert haiku_calls["n"] == 2  # 2 cases, standalone judge is Haiku only
    assert sonnet_calls["n"] == 0

    panel = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge-panel",
            "--use-score-cache",
        ],
    )
    assert panel.exit_code == 0, panel.output
    # Haiku's votes for both cases are reused from the standalone seed --
    # only Sonnet (never called standalone) makes fresh calls.
    assert haiku_calls["n"] == 2
    assert sonnet_calls["n"] == 2

    persisted = load_scores(scores_path)
    panel_scores = [s for s in persisted if s.scorer_type == "llm_judge_panel"]
    assert len(panel_scores) == 2
    for score in panel_scores:
        haiku_vote = next(
            v
            for v in score.panel_votes
            if v.scorer_id == cli_module.BEDROCK_HAIKU_4_5_MODEL_ID
        )
        sonnet_vote = next(
            v
            for v in score.panel_votes
            if v.scorer_id == cli_module.BEDROCK_SONNET_5_MODEL_ID
        )
        assert haiku_vote.from_cache is True
        assert sonnet_vote.from_cache is False


def test_run_with_llm_judge_reuses_a_prior_panel_haiku_vote(tmp_path, monkeypatch):
    # The other half of the same cross-mode property: a Haiku vote cached
    # from a --llm-judge-panel run must be reusable inside a LATER
    # standalone --llm-judge run for that case. This is the "and vice
    # versa" half of the RFC's own cache-key design -- verify it for
    # real rather than assume it follows symmetrically from the other
    # direction just proved above.
    _monkeypatch_panel_providers(
        monkeypatch,
        sonnet_response="REASONING: matches.\nVERDICT: PASS\n",
        haiku_response="REASONING: matches.\nVERDICT: PASS\n",
    )
    haiku_calls = {"n": 0}
    real_fake_haiku = cli_module.make_bedrock_call_model(
        cli_module.BEDROCK_HAIKU_4_5_MODEL_ID
    )

    async def counting_haiku(prompt: str) -> str:
        haiku_calls["n"] += 1
        return await real_fake_haiku(prompt)

    # _monkeypatch_panel_providers (above) already installed a fake
    # dispatcher covering Sonnet; wrap it so Haiku alone gets counted.
    fake_dispatcher = cli_module.make_bedrock_call_model

    def counting_dispatcher(model_id, client=None, region_name=None):
        if model_id == cli_module.BEDROCK_HAIKU_4_5_MODEL_ID:
            return counting_haiku
        return fake_dispatcher(model_id, client, region_name)

    monkeypatch.setattr(cli_module, "make_bedrock_call_model", counting_dispatcher)

    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    seed = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge-panel",
            "--use-score-cache",
        ],
    )
    assert seed.exit_code == 0, seed.output
    assert haiku_calls["n"] == 2  # 2 cases x 1 Haiku panelist each

    standalone = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge",
            "--use-score-cache",
        ],
    )
    assert standalone.exit_code == 0, standalone.output
    # Both cases' Haiku votes are reused from the panel seed -- zero
    # fresh standalone calls.
    assert haiku_calls["n"] == 2

    persisted = load_scores(scores_path)
    standalone_scores = [s for s in persisted if s.scorer_type == "llm_judge"]
    assert len(standalone_scores) == 2
    assert all(s.from_cache for s in standalone_scores)


def test_run_with_llm_judge_panel_never_re_chains_a_cached_vote_off_another_cache_hit(
    tmp_path, monkeypatch
):
    # A previously-cached (from_cache=True) panel Score must never itself
    # be treated as a reusable source -- only a genuinely fresh vote is.
    # Seed the scores file directly with a from_cache=True panel Score
    # (bypassing the CLI, so this isolates exactly the property under
    # test) and confirm a later --use-score-cache invocation still makes
    # real calls for that case, rather than incorrectly reusing the stale
    # cache-hit entry as if it were an original source.
    scores_path = tmp_path / "scores.jsonl"
    _write_scores(
        scores_path,
        [
            Score(
                eval_case_id="judge-pass-case",
                eval_case_revision=1,
                scorer_id=(
                    "panel:global.anthropic.claude-sonnet-5"
                    "+global.anthropic.claude-haiku-4-5-20251001-v1:0"
                ),
                scorer_type="llm_judge_panel",
                value=True,
                cost_usd=Decimal("0"),
                from_cache=True,
                quorum_reached=True,
                panel_votes=[
                    _panel_vote_for_test(
                        "Paris",
                        "Paris",
                        cli_module.BEDROCK_SONNET_5_MODEL_ID,
                        True,
                        from_cache=True,
                    ),
                    _panel_vote_for_test(
                        "Paris",
                        "Paris",
                        cli_module.BEDROCK_HAIKU_4_5_MODEL_ID,
                        True,
                        from_cache=True,
                    ),
                ],
            )
        ],
    )

    sonnet_calls = {"n": 0}
    haiku_calls = {"n": 0}

    async def fake_sonnet(prompt: str) -> str:
        sonnet_calls["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    async def fake_haiku(prompt: str) -> str:
        haiku_calls["n"] += 1
        return "REASONING: matches.\nVERDICT: PASS\n"

    def fake_make_bedrock_call_model(model_id, client=None, region_name=None):
        return (
            fake_sonnet
            if model_id == cli_module.BEDROCK_SONNET_5_MODEL_ID
            else fake_haiku
        )

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", fake_make_bedrock_call_model
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "run",
            "--suite",
            "tests/fixtures/llm_judge_example.json",
            "--scores",
            str(scores_path),
            "--llm-judge-panel",
            "--use-score-cache",
        ],
    )

    assert result.exit_code == 0, result.output
    # Both judges were genuinely called AT LEAST once (once for
    # judge-fail-case, which has no cache entry at all, plus once more for
    # judge-pass-case specifically -- checked directly below -- since the
    # llm_judge_example.json fixture has 2 cases total).
    assert sonnet_calls["n"] == 2
    assert haiku_calls["n"] == 2

    # The precise property under test: judge-pass-case's own persisted
    # panel_votes must both be genuinely fresh (from_cache=False), proving
    # the seeded from_cache=True entry was correctly never reused as a
    # source, rather than just inferring this from a raw global count.
    persisted = load_scores(scores_path)
    pass_case_scores = [
        s
        for s in persisted
        if s.eval_case_id == "judge-pass-case" and s.scorer_type == "llm_judge_panel"
    ]
    latest_pass_case_score = pass_case_scores[-1]
    assert latest_pass_case_score.from_cache is False
    assert all(not v.from_cache for v in latest_pass_case_score.panel_votes)


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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
        cli_module, "make_bedrock_call_model", lambda model_id: flaky_call_model
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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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


def test_run_with_judge_axes_scores_each_axis_independently_and_ands_the_verdict(
    tmp_path, monkeypatch
):
    # judge-pass-case: PASS on both axes -> overall PASS.
    # judge-fail-case: PASS on correctness but FAIL on safety -> overall
    # FAIL -- proving the case-level verdict is a real AND across axes,
    # not just a copy of the first axis's result.
    responses = iter(
        [
            "REASONING: fine.\nVERDICT: PASS\n",  # judge-pass-case/correctness
            "REASONING: fine.\nVERDICT: PASS\n",  # judge-pass-case/safety
            "REASONING: fine.\nVERDICT: PASS\n",  # judge-fail-case/correctness
            "REASONING: risky.\nVERDICT: FAIL\n",  # judge-fail-case/safety
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        return next(responses)

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
            "--judge-axes",
            "correctness,safety",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case [correctness]: PASS" in result.output
    assert "judge-pass-case [safety]: PASS" in result.output
    assert "judge-pass-case: PASS" in result.output
    assert "judge-fail-case [correctness]: PASS" in result.output
    assert "judge-fail-case [safety]: FAIL" in result.output
    assert "judge-fail-case: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output

    persisted = load_scores(scores_path)
    assert len(persisted) == 4
    assert [s.rubric_axis for s in persisted] == [
        "correctness",
        "safety",
        "correctness",
        "safety",
    ]
    assert [s.value for s in persisted] == [True, True, True, False]
    assert all(s.eval_case_id == "judge-pass-case" for s in persisted[:2])
    assert all(s.eval_case_id == "judge-fail-case" for s in persisted[2:])


def test_run_with_judge_axes_any_axis_error_marks_the_whole_case_judge_error(
    tmp_path, monkeypatch
):
    call_count = {"n": 0}

    async def flaky_call_model(prompt: str) -> str:
        call_count["n"] += 1
        # The first axis call for judge-pass-case succeeds; its second
        # axis call fails -- the whole case must become JUDGE_ERROR with
        # no partial Score persisted, mirroring the pre-existing
        # single-axis all-or-nothing behavior.
        if call_count["n"] == 2:
            raise RuntimeError("simulated API failure")
        return "REASONING: fine.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", lambda model_id: flaky_call_model
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
            "--judge-axes",
            "correctness,safety",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge-pass-case: JUDGE_ERROR" in result.output

    persisted = load_scores(scores_path)
    # judge-pass-case contributes zero Scores (its safety call failed);
    # judge-fail-case still contributes its own two.
    assert len(persisted) == 2
    assert all(s.eval_case_id == "judge-fail-case" for s in persisted)


def test_run_with_judge_axes_use_score_cache_discriminates_by_axis(
    tmp_path, monkeypatch
):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            [
                {
                    "id": "axis-case",
                    "revision": 1,
                    "task_spec": {"output": "Paris"},
                    "reference": "Paris",
                    "tier": "golden",
                }
            ]
        )
    )
    scores_path = tmp_path / "scores.jsonl"

    call_count = {"n": 0}

    async def fake_call_model(prompt: str) -> str:
        call_count["n"] += 1
        return "REASONING: ok.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
    )

    runner = CliRunner()
    base_args = [
        "run",
        "--suite",
        str(suite_path),
        "--scores",
        str(scores_path),
        "--llm-judge",
        "--use-score-cache",
    ]

    first = runner.invoke(main, [*base_args, "--judge-axes", "correctness,safety"])
    assert first.exit_code == 0, first.output
    assert call_count["n"] == 2

    second = runner.invoke(main, [*base_args, "--judge-axes", "correctness,safety"])
    assert second.exit_code == 0, second.output
    # Both axes hit the score cache on the second invocation -- zero new
    # judge calls at all.
    assert call_count["n"] == 2

    third = runner.invoke(main, [*base_args, "--judge-axes", "safety,tone"])
    assert third.exit_code == 0, third.output
    # "safety" was already judged above and is a cache hit; "tone" has
    # never been judged under this axis before, so exactly one new call is
    # made -- the cache genuinely discriminates axis-by-axis, not just by
    # (output, reference, scorer_id).
    assert call_count["n"] == 3

    persisted = load_scores(scores_path)
    assert len(persisted) == 6
    assert [s.rubric_axis for s in persisted] == [
        "correctness",
        "safety",
        "correctness",
        "safety",
        "safety",
        "tone",
    ]
    assert persisted[4].from_cache is True
    assert persisted[5].from_cache is False


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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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


def test_rollout_with_llm_judge_panel_scores_captured_stdout_via_real_wiring(
    tmp_path, monkeypatch
):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    async def fake_judge(prompt: str) -> str:
        return "REASONING: matches.\nVERDICT: PASS\n"

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, client=None, region_name=None: fake_judge,
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
            "--llm-judge-panel",
        ],
    )

    assert result.exit_code == 0, result.output

    persisted_runs = load_runs(results_path)
    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 2
    assert all(s.scorer_type == "llm_judge_panel" for s in persisted_scores)
    assert persisted_scores[0].run_id == persisted_runs[0].id
    assert persisted_scores[0].panel_votes is not None
    assert len(persisted_scores[0].panel_votes) == 2


def test_rollout_with_judge_axes_scores_each_axis_independently_and_ands_the_verdict(
    tmp_path, monkeypatch
):
    async def _fake_run_in_sandbox(image, command, timeout_s):
        return SandboxResult(
            exit_code=0, stdout=f"{command[1]}\n", stderr="", timed_out=False
        )

    monkeypatch.setattr(scheduler_module, "run_in_sandbox", _fake_run_in_sandbox)

    # rollout-echo-hello: PASS on both axes -> overall PASS.
    # rollout-wrong-answer: PASS on correctness but FAIL on safety ->
    # overall FAIL. Deliberately ordered with the *first* axis passing and
    # a *later* axis failing -- a buggy "just use the first axis's
    # verdict" implementation would wrongly report PASS here, so this is
    # a real AND-across-axes proof, not just two axes that happen to both
    # fail already.
    responses = iter(
        [
            "REASONING: fine.\nVERDICT: PASS\n",  # rollout-echo-hello/correctness
            "REASONING: fine.\nVERDICT: PASS\n",  # rollout-echo-hello/safety
            "REASONING: fine.\nVERDICT: PASS\n",  # rollout-wrong-answer/correctness
            "REASONING: risky.\nVERDICT: FAIL\n",  # rollout-wrong-answer/safety
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        return next(responses)

    monkeypatch.setattr(
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
            "--judge-axes",
            "correctness,safety",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "rollout-echo-hello [correctness]: PASS" in result.output
    assert "rollout-echo-hello [safety]: PASS" in result.output
    assert "rollout-echo-hello: PASS" in result.output
    assert "rollout-wrong-answer [correctness]: PASS" in result.output
    assert "rollout-wrong-answer [safety]: FAIL" in result.output
    assert "rollout-wrong-answer: FAIL" in result.output
    assert "pass_rate=0.5000 (1/2)" in result.output

    persisted_runs = load_runs(results_path)
    assert len(persisted_runs) == 2

    persisted_scores = load_scores(scores_path)
    assert len(persisted_scores) == 4
    assert [s.rubric_axis for s in persisted_scores] == [
        "correctness",
        "safety",
        "correctness",
        "safety",
    ]
    assert [s.value for s in persisted_scores] == [True, True, True, False]
    assert all(s.run_id == persisted_runs[0].id for s in persisted_scores[:2])
    assert all(s.run_id == persisted_runs[1].id for s in persisted_scores[2:])


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
            "--early-stop-max-trials",
            "10",
            # deliberately omitting --early-stop-baseline-pass-rate
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
            "--early-stop-max-trials",
            "10",
            "--early-stop-baseline-pass-rate",
            "0.1",
        ],
    )

    assert result.exit_code == 0, result.output
    assert result.output.count("SKIPPED") == 4
    # total must reflect only the 2 real trials that actually ran -- the 4
    # never-attempted skipped trials must not count in the denominator.
    # (An all-success group's mSPRT check against baseline=0.1 crosses
    # its threshold at exactly trial 2 -- verified directly against
    # evals.stats.mixture_sprt_early_stop, not guessed.)
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
        cli_module, "make_bedrock_call_model", lambda model_id: fake_call_model
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
            "--early-stop-max-trials",
            "10",
            "--early-stop-baseline-pass-rate",
            "0.1",
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
