"""CLI integration tests for `evals promote`, per docs/rfcs/2026-09-05-
evals-golden-regression-promotion.md — a separate file from
test_cli_integration.py (already at its own 800-line cap) rather than
growing that file further, per this project's own "many small files"
convention.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

from evals.cli import main
from evals.models import EvalCase, Run, Score
from evals.results_store import append_runs, append_scores


def _write_suite(path: Path, cases: list[EvalCase]) -> None:
    path.write_text(json.dumps([json.loads(c.model_dump_json()) for c in cases]))


def _make_case(case_id: str = "case-1", revision: int = 1) -> EvalCase:
    return EvalCase(
        id=case_id,
        revision=revision,
        task_spec={"image": "alpine:3.20", "command": ["echo", "hi"]},
        reference="hi",
        tier="regression",
        tags=["original-tag"],
    )


def _make_run(run_id: str, case: EvalCase) -> Run:
    return Run(
        id=run_id,
        eval_case_id=case.id,
        eval_case_revision=case.revision,
        harness_config={
            "image": "alpine:3.20",
            "command": ["echo", "hi"],
            "timeout_s": 30,
        },
        status="completed",
        latency_ms=12.3,
        stdout="wrong output",
    )


def test_promote_writes_new_case_to_output_suite(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])

    results_path = tmp_path / "runs.jsonl"
    run = _make_run("run-1", case)
    append_runs([run], results_path)

    output_path = tmp_path / "promoted.json"
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--run-id",
            "run-1",
            "--tier",
            "drift_sample",
            "--output",
            str(output_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert (
        "promoted case-1@1 (run run-1) -> case-1-promoted-run-1 (tier=drift_sample)"
        in result.output
    )

    promoted = json.loads(output_path.read_text())
    assert len(promoted) == 1
    assert promoted[0]["id"] == "case-1-promoted-run-1"
    assert promoted[0]["revision"] == 1
    assert promoted[0]["tier"] == "drift_sample"
    assert promoted[0]["task_spec"] == case.task_spec
    assert promoted[0]["reference"] == "hi"
    assert "original-tag" in promoted[0]["tags"]
    assert "promoted-from:case-1@1" in promoted[0]["tags"]
    assert "promoted-from-run:run-1" in promoted[0]["tags"]


def test_promote_rejects_golden_tier(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])
    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--run-id",
            "run-1",
            "--tier",
            "golden",
            "--output",
            str(tmp_path / "promoted.json"),
        ],
    )

    # Click's own Choice validation rejects "golden" before promote_cmd's
    # body ever runs — the load-bearing proof that tier immutability
    # (THREAT_MODEL.md's Evals Spoofing row) is enforced at the CLI
    # surface, not just documented.
    assert result.exit_code != 0
    assert "golden" in result.output.lower()
    assert not (tmp_path / "promoted.json").exists()


def test_promote_regression_tier_requires_a_failing_score(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])
    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            Score(
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                run_id="run-1",
                scorer_id="exact_match",
                scorer_type="deterministic",
                value=True,  # passed — nothing to regression-test
            )
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--run-id",
            "run-1",
            "--tier",
            "regression",
            "--output",
            str(tmp_path / "promoted.json"),
        ],
    )

    assert result.exit_code != 0
    assert "nothing to regression-test" in result.output
    assert not (tmp_path / "promoted.json").exists()


def test_promote_regression_tier_succeeds_with_a_failing_score(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])
    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            Score(
                eval_case_id=case.id,
                eval_case_revision=case.revision,
                run_id="run-1",
                scorer_id="exact_match",
                scorer_type="deterministic",
                value=False,  # a real failure
            )
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--scores",
            str(scores_path),
            "--run-id",
            "run-1",
            "--tier",
            "regression",
            "--output",
            str(tmp_path / "promoted.json"),
        ],
    )

    assert result.exit_code == 0, result.output
    promoted = json.loads((tmp_path / "promoted.json").read_text())
    assert promoted[0]["tier"] == "regression"


def test_promote_missing_run_id_fails_loudly(tmp_path):
    case = _make_case()
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case])
    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-1", case)], results_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "promote",
            "--suite",
            str(suite_path),
            "--results",
            str(results_path),
            "--run-id",
            "run-does-not-exist",
            "--tier",
            "drift_sample",
            "--output",
            str(tmp_path / "promoted.json"),
        ],
    )

    assert result.exit_code != 0
    assert "run-does-not-exist" in result.output
    assert not (tmp_path / "promoted.json").exists()


def test_promote_appends_rather_than_overwrites_existing_output(tmp_path):
    case_a = _make_case("case-a")
    case_b = _make_case("case-b")
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [case_a, case_b])
    results_path = tmp_path / "runs.jsonl"
    append_runs([_make_run("run-a", case_a), _make_run("run-b", case_b)], results_path)

    output_path = tmp_path / "promoted.json"
    runner = CliRunner()
    for run_id in ("run-a", "run-b"):
        result = runner.invoke(
            main,
            [
                "promote",
                "--suite",
                str(suite_path),
                "--results",
                str(results_path),
                "--run-id",
                run_id,
                "--tier",
                "drift_sample",
                "--output",
                str(output_path),
            ],
        )
        assert result.exit_code == 0, result.output

    promoted = json.loads(output_path.read_text())
    assert len(promoted) == 2
    assert {c["id"] for c in promoted} == {
        "case-a-promoted-run-a",
        "case-b-promoted-run-b",
    }
