"""CLI integration tests for `evals flag-candidates` -- a separate file
from test_cli_integration.py, mirroring test_promote_integration.py's own
"many small files" convention.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

from evals.cli import main
from evals.models import EvalCase, Run
from evals.results_store import append_runs


def _write_suite(path: Path, cases: list[EvalCase]) -> None:
    path.write_text(json.dumps([json.loads(c.model_dump_json()) for c in cases]))


def _make_case(
    case_id: str = "case-1",
    revision: int = 1,
    tier: str = "drift_sample",
    outcome: str = "OUTCOME_UPSTREAM_ERROR",
) -> EvalCase:
    return EvalCase(
        id=case_id,
        revision=revision,
        task_spec={
            "source": "gatewayevents_v1",
            "outcome": outcome,
        },
        reference=None,
        tier=tier,
        tags=[f"gatewayevents-outcome:{outcome}"],
    )


def _make_run(run_id: str, case: EvalCase, status: str = "error") -> Run:
    return Run(
        id=run_id,
        eval_case_id=case.id,
        eval_case_revision=case.revision,
        harness_config={"source": "gatewayevents_v1"},
        status=status,
        latency_ms=0.0,
    )


def test_flag_candidates_prints_candidate_lines_and_summary(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])

    results_path = tmp_path / "runs.jsonl"
    run = _make_run("run-1", case)
    append_runs([run], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "flag-candidates",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "case-1" in result.output
    assert "run-1" in result.output
    assert "gateway-failure-outcome" in result.output
    assert (
        f"evals promote --suite {suite_path} --results {results_path} "
        "--run-id run-1 --tier drift_sample --output"
    ) in result.output
    assert "1 candidates flagged (of 1 drift_sample cases)" in result.output


def test_flag_candidates_out_writes_expected_json_shape(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])

    results_path = tmp_path / "runs.jsonl"
    run = _make_run("run-1", case)
    append_runs([run], results_path)

    out_path = tmp_path / "candidates.json"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "flag-candidates",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    written = json.loads(out_path.read_text())
    assert len(written) == 1
    assert written[0]["eval_case_id"] == "case-1"
    assert written[0]["eval_case_revision"] == 1
    assert written[0]["run_id"] == "run-1"
    assert written[0]["matched_rules"] == ["gateway-failure-outcome"]


def test_flag_candidates_zero_drift_sample_cases_fails_loudly(tmp_path):
    case = _make_case(tier="regression")
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])

    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "flag-candidates",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
        ],
    )

    assert result.exit_code != 0
    assert "no drift_sample EvalCases found" in result.output


def test_flag_candidates_every_case_matching_still_exits_zero(tmp_path):
    case_a = _make_case("case-a")
    case_b = _make_case("case-b", outcome="OUTCOME_RATE_LIMITED")
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case_a, case_b])

    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-a", case_a), _make_run("run-b", case_b)], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "flag-candidates",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "2 candidates flagged (of 2 drift_sample cases)" in result.output


def test_flag_candidates_never_mutates_the_suite_file(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])
    before = suite_path.read_bytes()

    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "flag-candidates",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--out",
            str(tmp_path / "candidates.json"),
        ],
    )

    assert result.exit_code == 0, result.output
    after = suite_path.read_bytes()
    assert before == after
