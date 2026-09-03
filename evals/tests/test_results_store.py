from __future__ import annotations

from pathlib import Path

from evals.models import Run, Score
from evals.results_store import append_runs, append_scores, load_runs, load_scores


def _make_run(run_id: str) -> Run:
    return Run(
        id=run_id,
        eval_case_id="case-001",
        eval_case_revision=1,
        harness_config={"image": "alpine:3.20", "command": ["echo", "hi"]},
        status="completed",
        exit_code=0,
        stdout="hi\n",
        latency_ms=12.5,
    )


def _make_score(scorer_id: str) -> Score:
    return Score(
        eval_case_id="case-001",
        eval_case_revision=1,
        run_id="run-001",
        scorer_id=scorer_id,
        scorer_type="deterministic",
        value=True,
    )


def test_load_runs_on_nonexistent_path_returns_empty_list(tmp_path: Path):
    assert load_runs(tmp_path / "does-not-exist.jsonl") == []


def test_append_then_load_round_trips_runs(tmp_path: Path):
    path = tmp_path / "results.jsonl"
    runs = [_make_run("r1"), _make_run("r2")]

    append_runs(runs, path)
    loaded = load_runs(path)

    assert loaded == runs


def test_append_runs_accumulates_across_calls_rather_than_overwriting(
    tmp_path: Path,
):
    path = tmp_path / "results.jsonl"

    append_runs([_make_run("r1")], path)
    append_runs([_make_run("r2"), _make_run("r3")], path)

    loaded = load_runs(path)

    assert [r.id for r in loaded] == ["r1", "r2", "r3"]


def test_load_scores_on_nonexistent_path_returns_empty_list(tmp_path: Path):
    assert load_scores(tmp_path / "does-not-exist.jsonl") == []


def test_append_then_load_round_trips_scores(tmp_path: Path):
    path = tmp_path / "scores.jsonl"
    scores = [_make_score("exact_match"), _make_score("regex_match")]

    append_scores(scores, path)
    loaded = load_scores(path)

    assert loaded == scores


def test_append_scores_accumulates_across_calls_rather_than_overwriting(
    tmp_path: Path,
):
    path = tmp_path / "scores.jsonl"

    append_scores([_make_score("exact_match")], path)
    append_scores([_make_score("regex_match")], path)

    loaded = load_scores(path)

    assert [s.scorer_id for s in loaded] == ["exact_match", "regex_match"]
