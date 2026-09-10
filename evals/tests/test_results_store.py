from __future__ import annotations

from datetime import UTC, datetime
from decimal import Decimal
from pathlib import Path

from evals.models import Run, Score, Span, TrendSnapshot
from evals.results_store import (
    append_runs,
    append_scores,
    append_spans,
    append_trend_snapshots,
    load_runs,
    load_scores,
    load_spans,
    load_trend_snapshots,
)


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


def _make_rate_snapshot(series: str = "quote_grounding_rate") -> TrendSnapshot:
    return TrendSnapshot(
        series=series,
        recorded_at=datetime(2026, 9, 10, 12, 0, 0, tzinfo=UTC),
        n=4,
        rate_value=0.75,
        scorer_type="llm_judge",
        source_command="report",
    )


def _make_cost_snapshot() -> TrendSnapshot:
    return TrendSnapshot(
        series="cost_usd",
        recorded_at=datetime(2026, 9, 10, 12, 5, 0, tzinfo=UTC),
        n=4,
        cost_usd_value=Decimal("0.0000123"),
        scorer_type="llm_judge",
        source_command="report",
    )


def _make_span(span_id: str) -> Span:
    return Span(
        span_id=span_id,
        trace_id="b" * 32,
        run_id="run-001",
        name="sandbox.exec",
        start_time_unix_nano=1_000_000,
        end_time_unix_nano=2_000_000,
        status="OK",
        process_command_args=["echo", "hi"],
        process_exit_code=0,
        container_image_name="alpine:3.20",
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


def test_load_spans_on_nonexistent_path_returns_empty_list(tmp_path: Path):
    assert load_spans(tmp_path / "does-not-exist.jsonl") == []


def test_append_then_load_round_trips_spans(tmp_path: Path):
    path = tmp_path / "traces.jsonl"
    spans = [_make_span("s1"), _make_span("s2")]

    append_spans(spans, path)
    loaded = load_spans(path)

    assert loaded == spans


def test_append_spans_accumulates_across_calls_rather_than_overwriting(
    tmp_path: Path,
):
    path = tmp_path / "traces.jsonl"

    append_spans([_make_span("s1")], path)
    append_spans([_make_span("s2"), _make_span("s3")], path)

    loaded = load_spans(path)

    assert [s.span_id for s in loaded] == ["s1", "s2", "s3"]


def test_load_trend_snapshots_on_nonexistent_path_returns_empty_list(
    tmp_path: Path,
):
    assert load_trend_snapshots(tmp_path / "does-not-exist.jsonl") == []


def test_append_then_load_round_trips_trend_snapshots(tmp_path: Path):
    path = tmp_path / "trend.jsonl"
    snapshots = [_make_rate_snapshot(), _make_cost_snapshot()]

    append_trend_snapshots(snapshots, path)
    loaded = load_trend_snapshots(path)

    assert loaded == snapshots


def test_append_trend_snapshots_accumulates_across_calls_rather_than_overwriting(
    tmp_path: Path,
):
    path = tmp_path / "trend.jsonl"

    append_trend_snapshots([_make_rate_snapshot()], path)
    append_trend_snapshots(
        [_make_cost_snapshot(), _make_cost_snapshot()],
        path,
    )

    loaded = load_trend_snapshots(path)

    assert [s.series for s in loaded] == [
        "quote_grounding_rate",
        "cost_usd",
        "cost_usd",
    ]


def test_trend_snapshot_cost_usd_value_round_trips_as_decimal_not_lossy_float(
    tmp_path: Path,
):
    # The load-bearing precision proof: a Decimal cost value written to
    # JSONL and reloaded must come back as an exact Decimal, never a
    # float that silently lost precision -- mirrors Score.cost_usd's own
    # already-established Decimal-not-float convention.
    path = tmp_path / "trend.jsonl"
    snapshot = _make_cost_snapshot()

    append_trend_snapshots([snapshot], path)
    loaded = load_trend_snapshots(path)

    assert len(loaded) == 1
    assert isinstance(loaded[0].cost_usd_value, Decimal)
    assert loaded[0].cost_usd_value == Decimal("0.0000123")
    assert loaded[0] == snapshot
