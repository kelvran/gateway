from __future__ import annotations

from pathlib import Path

from evals.models import Run
from evals.rollout.results_store import append_runs, load_runs


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
