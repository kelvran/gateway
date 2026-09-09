"""End-to-end integration tests, driven through Click's `CliRunner`, for
`report_cmd`'s `--judge-accuracy` flag, per
docs/rfcs/2026-09-09-evals-judge-accuracy-metric.md.

A separate file from `test_cigate_integration.py`, matching this project's
own "many small files, one per feature" convention.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

from evals.cli import main
from evals.models import Score
from evals.results_store import append_scores


def _write_fixture(path: Path, cases: list[dict]) -> None:
    path.write_text(json.dumps(cases))


def _fixture_case(case_id: str, expected_verdict, revision: int = 1) -> dict:
    return {
        "id": case_id,
        "revision": revision,
        "task_spec": {"output": "irrelevant", "expected_verdict": expected_verdict},
        "reference": "irrelevant",
        "tier": "regression",
        "tags": ["category:judge"],
        "flaky": False,
    }


def _make_score(**overrides) -> Score:
    defaults = {
        "eval_case_id": "case-1",
        "eval_case_revision": 1,
        "scorer_id": "llm_judge",
        "scorer_type": "llm_judge",
        "value": True,
    }
    defaults.update(overrides)
    return Score(**defaults)


def test_judge_accuracy_prints_kappa_and_confusion_matrix_without_affecting_fail_under(
    tmp_path,
):
    fixture_path = tmp_path / "judge_accuracy.json"
    _write_fixture(
        fixture_path,
        [
            _fixture_case("c1", True),
            _fixture_case("c2", True),
            _fixture_case("c3", False),
            _fixture_case("c4", False),
        ],
    )
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            # Judge agrees on c1, c2, c4; disagrees on c3 (judge says
            # True, ground truth is False) -- a real, non-trivial
            # confusion matrix, not all-agree/all-disagree.
            _make_score(eval_case_id="c1", value=True),
            _make_score(eval_case_id="c2", value=True),
            _make_score(eval_case_id="c3", value=True),
            _make_score(eval_case_id="c4", value=False),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--judge-accuracy",
            str(fixture_path),
            "--fail-under",
            "0.0",
        ],
    )

    assert result.exit_code == 0, result.output
    # group_successes counts Score.value (the judge's own PASS verdict per
    # case, c1/c2/c3=True, c4=False) -- 3/4, independent of ground truth.
    assert "llm_judge: pass_rate=0.7500 (3/4)" in result.output
    assert "llm_judge [judge-accuracy]:" in result.output
    assert "n=4" in result.output
    assert "tp=2" in result.output
    assert "fp=1" in result.output
    assert "tn=1" in result.output
    assert "fn=0" in result.output


def test_judge_accuracy_excludes_cases_with_a_null_expected_verdict(tmp_path):
    fixture_path = tmp_path / "judge_accuracy.json"
    _write_fixture(
        fixture_path,
        [
            _fixture_case("c1", True),
            _fixture_case("c2", None),
        ],
    )
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=True),
            _make_score(eval_case_id="c2", value=True),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--judge-accuracy", str(fixture_path)],
    )

    assert result.exit_code == 0, result.output
    # Only c1 has a real ground truth; c2 (null expected_verdict) is
    # silently excluded from the join, not counted.
    assert "n=1" in result.output


def test_judge_accuracy_missing_expected_verdict_key_is_a_fixture_bug(tmp_path):
    fixture_path = tmp_path / "judge_accuracy.json"
    fixture_path.write_text(
        json.dumps(
            [
                {
                    "id": "c1",
                    "revision": 1,
                    "task_spec": {"output": "irrelevant"},
                    "reference": "irrelevant",
                    "tier": "regression",
                    "tags": [],
                    "flaky": False,
                }
            ]
        )
    )
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--judge-accuracy", str(fixture_path)],
    )

    assert result.exit_code != 0
    assert "no task_spec.expected_verdict" in result.output


def test_judge_accuracy_zero_matches_is_a_hard_error(tmp_path):
    fixture_path = tmp_path / "judge_accuracy.json"
    _write_fixture(fixture_path, [_fixture_case("c1", True)])
    scores_path = tmp_path / "scores.jsonl"
    # A Score for an entirely different case id -- never joins.
    append_scores([_make_score(eval_case_id="does-not-exist", value=True)], scores_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--judge-accuracy", str(fixture_path)],
    )

    assert result.exit_code != 0
    assert "no Score" in result.output


def test_judge_accuracy_rejected_without_scores_mode(tmp_path):
    fixture_path = tmp_path / "judge_accuracy.json"
    _write_fixture(fixture_path, [_fixture_case("c1", True)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--successes",
            "1",
            "--total",
            "1",
            "--judge-accuracy",
            str(fixture_path),
        ],
    )

    assert result.exit_code != 0
    assert "--judge-accuracy is only meaningful together with --scores" in result.output
