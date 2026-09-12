"""End-to-end integration tests, driven through Click's `CliRunner`, for
`--record-trend` on `report`/`audit-corpus` and the new `evals trend show`
command.

A separate file from `test_cli_integration.py` and `test_cigate_integration.py`,
per this project's own established "many small files" / 800-line-split
precedent (`test_cigate_integration.py`'s own docstring names that precedent
directly).

Mirrors `test_cigate_integration.py`'s fixture/invocation style for
`report`, and `test_cli_integration.py`'s `monkeypatch.setattr(cli_module,
"make_bedrock_call_model", ...)` pattern for `audit-corpus` (no live AWS
call, ever).
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from decimal import Decimal
from pathlib import Path

from click.testing import CliRunner

import evals.cli as cli_module
from evals.cli import main
from evals.models import EvalCase, Score, TrendSnapshot
from evals.results_store import (
    append_scores,
    append_trend_snapshots,
    load_trend_snapshots,
)


def _make_case(case_id: str, **overrides) -> EvalCase:
    defaults = {
        "id": case_id,
        "revision": 1,
        "task_spec": {"output": "right", "match": "exact"},
        "reference": "right",
        "tier": "regression",
        "tags": [],
    }
    defaults.update(overrides)
    return EvalCase(**defaults)


def _write_suite(path: Path, cases: list[EvalCase]) -> None:
    path.write_text(json.dumps([json.loads(c.model_dump_json()) for c in cases]))


def _make_score(**overrides) -> Score:
    defaults = {
        "eval_case_id": "case-1",
        "eval_case_revision": 1,
        "scorer_id": "exact_match",
        "scorer_type": "deterministic",
        "value": True,
    }
    defaults.update(overrides)
    return Score(**defaults)


# -- report --record-trend: cost_usd -----------------------------------------


def test_report_record_trend_produces_a_cost_snapshot_per_scorer_type(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            # deterministic scores always carry a known, exact zero cost
            # in real production data -- Decimal("0") is a certain fact,
            # not an unmeasured None (see Score.cost_usd's own docstring).
            _make_score(eval_case_id="c1", value=True, cost_usd=Decimal("0")),
            _make_score(eval_case_id="c2", value=False, cost_usd=Decimal("0")),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    cost_snapshots = [s for s in snapshots if s.series == "cost_usd"]
    assert len(cost_snapshots) == 1
    assert cost_snapshots[0].scorer_type == "deterministic"
    assert cost_snapshots[0].n == 2
    assert str(cost_snapshots[0].cost_usd_value) == "0"
    assert cost_snapshots[0].source_command == "report"


def test_report_record_trend_cost_snapshot_excludes_unknown_cost_scores_from_n(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=True, cost_usd=Decimal("0.01")),
            # cost_usd=None (unmeasured) -- must be excluded from n, not
            # silently treated as a known zero cost.
            _make_score(eval_case_id="c2", value=True, cost_usd=None),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    cost_snapshots = [
        s for s in load_trend_snapshots(trend_path) if s.series == "cost_usd"
    ]
    assert len(cost_snapshots) == 1
    assert cost_snapshots[0].n == 1
    assert cost_snapshots[0].cost_usd_value == Decimal("0.01")


def test_report_without_record_trend_leaves_a_nonexistent_trend_file_absent(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert not trend_path.exists()


def test_report_without_record_trend_leaves_an_existing_trend_file_byte_unchanged(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)
    trend_path = tmp_path / "trend.jsonl"
    trend_path.write_text("pre-existing content\n")
    before = trend_path.read_bytes()

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert trend_path.read_bytes() == before


def test_record_trend_rejected_without_scores_mode():
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--successes",
            "1",
            "--total",
            "1",
            "--record-trend",
            "unused.jsonl",
        ],
    )

    assert result.exit_code != 0
    assert "--record-trend is only meaningful together with --scores" in (result.output)


# -- report --record-trend: quote_grounding_rate -----------------------------


def test_report_record_trend_produces_quote_grounding_snapshot_for_llm_judge(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="j1",
                scorer_type="llm_judge",
                value=True,
                quote_grounded=True,
            ),
            _make_score(
                eval_case_id="j2",
                scorer_type="llm_judge",
                value=True,
                quote_grounded=False,
            ),
            # quote_grounded=None (unknown) must not count toward n.
            _make_score(
                eval_case_id="j3",
                scorer_type="llm_judge",
                value=True,
                quote_grounded=None,
            ),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    grounding = [s for s in snapshots if s.series == "quote_grounding_rate"]
    assert len(grounding) == 1
    assert grounding[0].n == 2
    assert grounding[0].rate_value == 0.5
    assert grounding[0].scorer_type == "llm_judge"


def test_report_record_trend_quote_grounding_rate_value_is_none_when_all_unknown(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="j1",
                scorer_type="llm_judge",
                value=True,
                quote_grounded=None,
            )
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    grounding = [
        s
        for s in load_trend_snapshots(trend_path)
        if s.series == "quote_grounding_rate"
    ]
    assert len(grounding) == 1
    assert grounding[0].n == 0
    assert grounding[0].rate_value is None


def test_report_record_trend_produces_no_quote_grounding_snapshot_for_deterministic(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [_make_score(eval_case_id="c1", scorer_type="deterministic", value=True)],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    assert not [s for s in snapshots if s.series == "quote_grounding_rate"]


# -- report --record-trend: judge_panel_tie_rate ------------------------------


def test_report_record_trend_produces_a_tie_rate_snapshot_for_llm_judge_panel(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="p1",
                scorer_type="llm_judge_panel",
                value=True,
                quorum_reached=True,
            ),
            _make_score(
                eval_case_id="p2",
                scorer_type="llm_judge_panel",
                value=False,
                quorum_reached=False,
            ),
            _make_score(
                eval_case_id="p3",
                scorer_type="llm_judge_panel",
                value=True,
                quorum_reached=True,
            ),
            _make_score(
                eval_case_id="p4",
                scorer_type="llm_judge_panel",
                value=False,
                quorum_reached=False,
            ),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    ties = [s for s in snapshots if s.series == "judge_panel_tie_rate"]
    assert len(ties) == 1
    assert ties[0].n == 4
    assert ties[0].rate_value == 0.5  # 2 of 4 quorum ties
    assert ties[0].scorer_type == "llm_judge_panel"


def test_report_record_trend_produces_no_tie_rate_snapshot_for_non_panel_scorers(
    tmp_path,
):
    # judge_panel_tie_rate is scoped to llm_judge_panel only -- neither a
    # single-judge score nor a deterministic score has a quorum/tie
    # concept at all.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", scorer_type="deterministic", value=True),
            _make_score(eval_case_id="j1", scorer_type="llm_judge", value=True),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    assert not [s for s in snapshots if s.series == "judge_panel_tie_rate"]
    # Still gets a cost_usd snapshot -- only quote_grounding is scorer-
    # type-gated to llm_judge/llm_judge_panel.
    assert [s for s in snapshots if s.series == "cost_usd"]


# -- report --record-trend: judge_accuracy_kappa -----------------------------


def test_report_record_trend_produces_kappa_snapshot_when_judge_accuracy_matches(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="k1", scorer_type="llm_judge", value=True),
            _make_score(eval_case_id="k2", scorer_type="llm_judge", value=False),
        ],
        scores_path,
    )
    fixture_path = tmp_path / "judge_accuracy_fixture.json"
    fixture_path.write_text(
        json.dumps(
            [
                {
                    "id": "k1",
                    "revision": 1,
                    "task_spec": {"expected_verdict": True},
                    "tier": "golden",
                },
                {
                    "id": "k2",
                    "revision": 1,
                    "task_spec": {"expected_verdict": False},
                    "tier": "golden",
                },
            ]
        )
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--judge-accuracy",
            str(fixture_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    kappa_snapshots = [
        s
        for s in load_trend_snapshots(trend_path)
        if s.series == "judge_accuracy_kappa"
    ]
    assert len(kappa_snapshots) == 1
    assert kappa_snapshots[0].n == 2
    assert kappa_snapshots[0].rate_value == 1.0
    assert kappa_snapshots[0].scorer_type == "llm_judge"


def test_report_record_trend_kappa_rate_value_is_none_when_undefined(tmp_path):
    # Zero-variance verdicts -- cohens_kappa raises ValueError, and
    # report_cmd itself prints kappa_str="undefined". The TrendSnapshot
    # must record that same honest "measured, but no real number" state
    # as rate_value=None, never a fabricated number.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="k1", scorer_type="llm_judge", value=True),
            _make_score(eval_case_id="k2", scorer_type="llm_judge", value=True),
        ],
        scores_path,
    )
    fixture_path = tmp_path / "judge_accuracy_fixture.json"
    fixture_path.write_text(
        json.dumps(
            [
                {
                    "id": "k1",
                    "revision": 1,
                    "task_spec": {"expected_verdict": True},
                    "tier": "golden",
                },
                {
                    "id": "k2",
                    "revision": 1,
                    "task_spec": {"expected_verdict": True},
                    "tier": "golden",
                },
            ]
        )
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--judge-accuracy",
            str(fixture_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    assert "kappa=undefined" in result.output
    kappa_snapshots = [
        s
        for s in load_trend_snapshots(trend_path)
        if s.series == "judge_accuracy_kappa"
    ]
    assert len(kappa_snapshots) == 1
    assert kappa_snapshots[0].rate_value is None
    assert kappa_snapshots[0].n == 2


def test_report_record_trend_persists_even_when_fail_under_gate_fails(tmp_path):
    # Trend history is exactly what you want for a report that fails its
    # own gate -- recording must not be skipped just because the command
    # goes on to exit non-zero.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [_make_score(eval_case_id="c1", value=False)],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--fail-under",
            "0.99",
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code != 0
    snapshots = load_trend_snapshots(trend_path)
    assert [s for s in snapshots if s.series == "cost_usd"]


# -- audit-corpus --record-trend ----------------------------------------------


def test_audit_corpus_record_trend_produces_one_defect_rate_snapshot(
    tmp_path, monkeypatch
):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            [
                {
                    "id": "clean-case",
                    "revision": 1,
                    "task_spec": {"output": "x"},
                    "reference": "x",
                    "tier": "regression",
                },
                {
                    "id": "flagged-case",
                    "revision": 1,
                    "task_spec": {"output": "y"},
                    "reference": "y",
                    "tier": "regression",
                },
            ]
        )
    )
    out_path = tmp_path / "findings.json"
    trend_path = tmp_path / "trend.jsonl"

    responses = iter(
        [
            "REASONING: Clear and correct.\nSEVERITY: no_defect\n",
            "REASONING: The ground truth looks wrong.\nSEVERITY: major\n",
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        return next(responses)

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, **kwargs: fake_call_model,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "audit-corpus",
            "--suite",
            str(suite_path),
            "--out",
            str(out_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    assert len(snapshots) == 1
    snap = snapshots[0]
    assert snap.series == "audit_corpus_defect_rate"
    assert snap.n == 2
    assert snap.rate_value == 0.5
    assert snap.scorer_type is None
    assert snap.source_command == "audit-corpus"


def test_audit_corpus_record_trend_excludes_error_findings_from_defect_rate(
    tmp_path, monkeypatch
):
    """An "error" finding means the case was never actually audited --
    it is not a design-defect opinion at all. It must be excluded from
    BOTH the numerator and the denominator of audit_corpus_defect_rate,
    the same "measured but genuinely undefined" honesty this codebase
    already applies elsewhere, rather than silently counting an
    unaudited case as either a clean pass or a real defect.
    """
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            [
                {
                    "id": "errors-case",
                    "revision": 1,
                    "task_spec": {},
                    "tier": "regression",
                },
                {
                    "id": "flagged-case",
                    "revision": 1,
                    "task_spec": {"output": "y"},
                    "reference": "y",
                    "tier": "regression",
                },
            ]
        )
    )
    trend_path = tmp_path / "trend.jsonl"

    responses = iter(
        [
            None,  # sentinel: the first call raises instead of returning
            "REASONING: The ground truth looks wrong.\nSEVERITY: major\n",
        ]
    )

    async def fake_call_model(prompt: str) -> str:
        response = next(responses)
        if response is None:
            raise ValueError("no text content block")
        return response

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, **kwargs: fake_call_model,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "audit-corpus",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "findings.json"),
            "--record-trend",
            str(trend_path),
        ],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    assert len(snapshots) == 1
    snap = snapshots[0]
    # n excludes the errored case entirely (1 audited case, not 2):
    assert snap.n == 1
    # and that one audited case was flagged major, so the rate is 1.0,
    # not 0.5 (which is what a naive len(cases)-denominator would give).
    assert snap.rate_value == 1.0


def test_audit_corpus_without_record_trend_leaves_trend_file_absent(
    tmp_path, monkeypatch
):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            [{"id": "case-a", "revision": 1, "task_spec": {}, "tier": "regression"}]
        )
    )
    out_path = tmp_path / "findings.json"
    trend_path = tmp_path / "trend.jsonl"

    async def fake_call_model(prompt: str) -> str:
        return "REASONING: fine.\nSEVERITY: no_defect\n"

    monkeypatch.setattr(
        cli_module,
        "make_bedrock_call_model",
        lambda model_id, **kwargs: fake_call_model,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["audit-corpus", "--suite", str(suite_path), "--out", str(out_path)],
    )

    assert result.exit_code == 0, result.output
    assert not trend_path.exists()


# -- trend show ----------------------------------------------------------------


def test_trend_show_prints_every_series_separately_never_combined(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="j1",
                scorer_type="llm_judge",
                value=True,
                quote_grounded=True,
                cost_usd=Decimal("0"),
            )
        ],
        scores_path,
    )
    fixture_path = tmp_path / "judge_accuracy_fixture.json"
    fixture_path.write_text(
        json.dumps(
            [
                {
                    "id": "j1",
                    "revision": 1,
                    "task_spec": {"expected_verdict": True},
                    "tier": "golden",
                }
            ]
        )
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    report_result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--judge-accuracy",
            str(fixture_path),
            "--record-trend",
            str(trend_path),
        ],
    )
    assert report_result.exit_code == 0, report_result.output

    show_result = runner.invoke(main, ["trend", "show", "--path", str(trend_path)])

    assert show_result.exit_code == 0, show_result.output
    output = show_result.output
    # The single most important assertion in this phase: each series gets
    # its own labeled block/line -- never averaged or combined into one
    # number or line. Prove it with real, specific string content, not
    # just a green exit code.
    assert "judge_accuracy_kappa:" in output
    assert "quote_grounding_rate:" in output
    assert "cost_usd:" in output
    assert "n=1 value=1.0" in output  # kappa
    assert "n=1 value=1.0 scorer_type=llm_judge" in output  # quote grounding
    assert "n=1 value=0" in output  # cost
    # No line ever mixes two series' labels or averages their values
    # together into one combined figure.
    for line in output.splitlines():
        series_labels_on_line = sum(
            label in line
            for label in (
                "judge_accuracy_kappa",
                "quote_grounding_rate",
                "audit_corpus_defect_rate",
                "cost_usd",
            )
        )
        assert series_labels_on_line <= 1, f"line mixes series labels: {line!r}"


def test_trend_show_series_filter_prints_only_that_series(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    result = runner.invoke(
        main, ["trend", "show", "--path", str(trend_path), "--series", "cost_usd"]
    )

    assert result.exit_code == 0, result.output
    assert "cost_usd:" in result.output
    assert "quote_grounding_rate" not in result.output
    assert "judge_accuracy_kappa" not in result.output


def test_trend_show_series_filter_accepts_judge_panel_tie_rate(tmp_path):
    # judge_panel_tie_rate is a real --series choice, not just a real
    # TrendSnapshot series -- proves the two lists stayed in sync.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="p1",
                scorer_type="llm_judge_panel",
                value=True,
                quorum_reached=False,
            )
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--record-trend", str(trend_path)],
    )

    result = runner.invoke(
        main,
        [
            "trend",
            "show",
            "--path",
            str(trend_path),
            "--series",
            "judge_panel_tie_rate",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "judge_panel_tie_rate:" in result.output


def test_trend_show_unknown_path_raises_click_exception(tmp_path):
    runner = CliRunner()
    result = runner.invoke(
        main, ["trend", "show", "--path", str(tmp_path / "does-not-exist.jsonl")]
    )

    assert result.exit_code != 0


def test_trend_show_empty_file_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    trend_path.write_text("")

    runner = CliRunner()
    result = runner.invoke(main, ["trend", "show", "--path", str(trend_path)])

    assert result.exit_code != 0
    assert "no TrendSnapshots found" in result.output


def test_trend_show_series_with_no_matching_snapshots_raises_click_exception(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)
    trend_path = tmp_path / "trend.jsonl"
    runner = CliRunner()
    runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--record-trend",
            str(trend_path),
        ],
    )

    result = runner.invoke(
        main,
        [
            "trend",
            "show",
            "--path",
            str(trend_path),
            "--series",
            "judge_accuracy_kappa",
        ],
    )

    assert result.exit_code != 0
    assert "no TrendSnapshots found for series='judge_accuracy_kappa'" in (
        result.output
    )


# -- trend alert ----------------------------------------------------------------


def _write_trend_snapshots(path: Path, snapshots: list[TrendSnapshot]) -> None:
    append_trend_snapshots(snapshots, path)


def _kappa_snapshot(value: float | None, day: int) -> TrendSnapshot:
    return TrendSnapshot(
        series="judge_accuracy_kappa",
        recorded_at=datetime(2026, 9, day, tzinfo=UTC),
        n=1,
        rate_value=value,
        scorer_type="llm_judge",
        source_command="report",
    )


def test_trend_alert_prints_a_triggered_alert_and_exits_zero(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(
        trend_path, [_kappa_snapshot(0.1, 1), _kappa_snapshot(0.15, 2)]
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "[warning] judge_accuracy_kappa" in result.output
    assert "1 alerts triggered (of 1 rules checked)" in result.output


def test_trend_alert_no_triggered_alerts_still_exits_zero(tmp_path):
    # Report-only, per this command's own docstring: never a hard
    # failure just because a rule didn't trigger, or did.
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.9, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "0 alerts triggered (of 1 rules checked)" in result.output


def test_trend_alert_custom_severity_is_carried_into_the_printed_line(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4:critical",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "[critical] judge_accuracy_kappa" in result.output


def test_trend_alert_writes_out_json(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])
    out_path = tmp_path / "alerts.json"

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
            "--out",
            str(out_path),
        ],
    )

    assert result.exit_code == 0, result.output
    written = json.loads(out_path.read_text())
    assert len(written) == 1
    assert written[0]["series"] == "judge_accuracy_kappa"
    assert written[0]["severity"] == "warning"


def test_trend_alert_unknown_series_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "not_a_real_series:below:0.4",
        ],
    )

    assert result.exit_code != 0
    assert "unknown series" in result.output


def test_trend_alert_invalid_direction_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:sideways:0.4",
        ],
    )

    assert result.exit_code != 0
    assert "direction must be 'below' or 'above'" in result.output


def test_trend_alert_non_numeric_value_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:not-a-number",
        ],
    )

    assert result.exit_code != 0
    assert "is not a number" in result.output


def test_trend_alert_empty_trend_file_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    trend_path.write_text("")

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
        ],
    )

    assert result.exit_code != 0
    assert "no TrendSnapshots found" in result.output


def test_trend_alert_negative_window_raises_click_exception(tmp_path):
    # A Round-5 backlog-audit finding: window=-1 previously reinterpreted
    # via Python's own negative-slice semantics as "drop the most-recent
    # values," silently missing a real regression instead of raising a
    # usage error.
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
            "--window",
            "-1",
        ],
    )

    assert result.exit_code != 0
    assert "must be a positive integer" in result.output


def test_trend_alert_zero_window_raises_click_exception(tmp_path):
    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
            "--window",
            "0",
        ],
    )

    assert result.exit_code != 0
    assert "must be a positive integer" in result.output


def test_trend_alert_posts_to_webhook_only_when_alerts_triggered(tmp_path, monkeypatch):
    calls = []

    def fake_urlopen(request, timeout=None):
        calls.append((request.full_url, json.loads(request.data), timeout))

        class _Resp:
            def __enter__(self):
                return self

            def __exit__(self, *exc):
                return False

        return _Resp()

    monkeypatch.setattr(cli_module.urllib.request, "urlopen", fake_urlopen)

    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.1, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
            "--notify-webhook",
            "https://example.invalid/hook",
        ],
    )

    assert result.exit_code == 0, result.output
    assert len(calls) == 1
    url, body, timeout = calls[0]
    assert url == "https://example.invalid/hook"
    assert len(body["alerts"]) == 1
    assert timeout == 10


def test_trend_alert_never_calls_webhook_when_no_alerts_triggered(
    tmp_path, monkeypatch
):
    def fake_urlopen(request, timeout=None):
        raise AssertionError("webhook must not be called when zero alerts triggered")

    monkeypatch.setattr(cli_module.urllib.request, "urlopen", fake_urlopen)

    trend_path = tmp_path / "trend.jsonl"
    _write_trend_snapshots(trend_path, [_kappa_snapshot(0.9, 1)])

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "trend",
            "alert",
            "--path",
            str(trend_path),
            "--threshold",
            "judge_accuracy_kappa:below:0.4",
            "--notify-webhook",
            "https://example.invalid/hook",
        ],
    )

    assert result.exit_code == 0, result.output


# -- report: multi-axis harness-configuration disclosure ---------------------


def test_report_judge_axes_prints_one_line_per_axis_not_one_blended_line(tmp_path):
    """Per docs/rfcs/2026-09-05-evals-multi-axis-judging.md: two Scores
    sharing scorer_type/scorer_id but with different rubric_axis values
    (the --judge-axes shape) must each get their own printed line, not
    be silently blended into one pass-rate figure.
    """
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="c1",
                scorer_type="llm_judge",
                scorer_id="panel:sonnet+haiku",
                rubric_axis="correctness",
                value=True,
            ),
            _make_score(
                eval_case_id="c1",
                scorer_type="llm_judge",
                scorer_id="panel:sonnet+haiku",
                rubric_axis="safety",
                value=False,
            ),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert "llm_judge [axis:correctness]:" in result.output
    assert "llm_judge [axis:safety]:" in result.output
    # The correctness axis is 1/1 (100%), safety is 0/1 (0%) -- neither
    # figure may appear only as one blended 50% line with no per-axis
    # breakdown.
    assert "pass_rate=1.0000 (1/1)" in result.output
    assert "pass_rate=0.0000 (0/1)" in result.output
    # scorer_id (panel composition) must also be disclosed on the
    # blended summary line.
    assert "scorer_id=panel:sonnet+haiku" in result.output


def test_report_single_axis_scores_print_no_per_axis_line(tmp_path):
    """The common (no --judge-axes) case must be byte-identical to
    before this fix -- no [axis:...] line at all when every Score in
    the group shares the same (or no) rubric_axis.
    """
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [_make_score(eval_case_id="c1", scorer_type="llm_judge", value=True)],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert "[axis:" not in result.output


def test_report_record_trend_snapshot_carries_scorer_id_when_uniform(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="c1",
                scorer_type="llm_judge",
                scorer_id="claude-sonnet-5",
                value=True,
                cost_usd=Decimal("0.01"),
            ),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--record-trend", str(trend_path)],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    cost_snapshots = [s for s in snapshots if s.series == "cost_usd"]
    assert len(cost_snapshots) == 1
    assert cost_snapshots[0].scorer_id == "claude-sonnet-5"


def test_report_record_trend_snapshot_scorer_id_is_none_when_group_is_mixed(tmp_path):
    """A group mixing two different scorer_ids under one scorer_type
    (a real possibility once Scores from different runs are combined)
    must record scorer_id=None -- never a fabricated value.
    """
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="c1",
                scorer_type="llm_judge",
                scorer_id="claude-sonnet-5",
                value=True,
                cost_usd=Decimal("0.01"),
            ),
            _make_score(
                eval_case_id="c2",
                scorer_type="llm_judge",
                scorer_id="claude-haiku-4-5",
                value=True,
                cost_usd=Decimal("0.01"),
            ),
        ],
        scores_path,
    )
    trend_path = tmp_path / "trend.jsonl"

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--record-trend", str(trend_path)],
    )

    assert result.exit_code == 0, result.output
    snapshots = load_trend_snapshots(trend_path)
    cost_snapshots = [s for s in snapshots if s.series == "cost_usd"]
    assert len(cost_snapshots) == 1
    assert cost_snapshots[0].scorer_id is None
