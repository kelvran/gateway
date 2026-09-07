"""End-to-end integration tests, driven through Click's `CliRunner`, for the
three CI-gate refinements in docs/rfcs/2026-09-07-evals-cigate-refinements.md:
`--tier` and `--category-fail-under` on `report_cmd`, and flaky exclusion.

A separate file from `test_cli_integration.py` (already at 1500 lines, well
past this project's own established 800-line split precedent that produced
`test_promote_integration.py`) rather than growing that file further, per
this project's "many small files" convention.

`tests/test_report_gate_helpers.py` unit-tests the underlying pure functions
in isolation; this file proves the real `evals run`/`evals report` commands
compose them correctly end-to-end.
"""

from __future__ import annotations

import json
from pathlib import Path

from click.testing import CliRunner

from evals.cli import main
from evals.models import EvalCase, Score, Span
from evals.results_store import append_scores, append_spans, load_scores


def _write_suite(path: Path, cases: list[EvalCase]) -> None:
    path.write_text(json.dumps([json.loads(c.model_dump_json()) for c in cases]))


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


# -- run_cmd/rollout_cmd denormalize tier/tags/flaky onto every Score -------


def test_run_denormalizes_tier_tags_flaky_from_case_onto_every_score(tmp_path):
    suite_path = tmp_path / "suite.json"
    _write_suite(
        suite_path,
        [
            _make_case(
                "case-safety",
                tier="regression",
                tags=["category:safety"],
                flaky=True,
            )
        ],
    )
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main, ["run", "--suite", str(suite_path), "--scores", str(scores_path)]
    )

    assert result.exit_code == 0, result.output
    persisted = load_scores(scores_path)
    assert len(persisted) == 1
    assert persisted[0].tier == "regression"
    assert persisted[0].tags == ["category:safety"]
    assert persisted[0].flaky is True


def test_run_denormalizes_defaults_when_case_has_no_tags_or_flaky(tmp_path):
    suite_path = tmp_path / "suite.json"
    _write_suite(suite_path, [_make_case("case-plain", tier="golden")])
    scores_path = tmp_path / "scores.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        main, ["run", "--suite", str(suite_path), "--scores", str(scores_path)]
    )

    assert result.exit_code == 0, result.output
    persisted = load_scores(scores_path)
    assert persisted[0].tier == "golden"
    assert persisted[0].tags == []
    assert persisted[0].flaky is False


# -- --tier -------------------------------------------------------------------


def test_report_tier_filters_out_non_matching_scores_from_report_and_gate(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="r1", value=True, tier="regression"),
            _make_score(eval_case_id="r2", value=True, tier="regression"),
            # A golden-tier failure that must be excluded entirely once
            # --tier regression is given -- if it leaked through, it
            # would drag the printed pass_rate and the gate's own lower
            # bound down.
            _make_score(eval_case_id="g1", value=False, tier="golden"),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--scores", str(scores_path), "--tier", "regression"]
    )

    assert result.exit_code == 0, result.output
    assert "deterministic: pass_rate=1.0000 (2/2)" in result.output
    assert "g1" not in result.output


def test_report_without_tier_reports_across_every_tier_unfiltered(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="r1", value=True, tier="regression"),
            _make_score(eval_case_id="g1", value=False, tier="golden"),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(main, ["report", "--scores", str(scores_path)])

    assert result.exit_code == 0, result.output
    assert "deterministic: pass_rate=0.5000 (1/2)" in result.output


def test_report_tier_with_no_matching_scores_fails_with_a_distinct_message(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [_make_score(eval_case_id="g1", value=True, tier="golden")], scores_path
    )

    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--scores", str(scores_path), "--tier", "regression"]
    )

    assert result.exit_code != 0
    assert "no Scores found for tier='regression'" in result.output


def test_report_tier_rejected_without_scores_mode():
    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--successes", "1", "--total", "1", "--tier", "regression"]
    )

    assert result.exit_code != 0
    assert "--tier is only meaningful together with --scores" in result.output


def test_report_tier_rejected_with_traces_mode(tmp_path):
    traces_path = tmp_path / "traces.jsonl"
    append_spans(
        [
            Span(
                span_id="a" * 16,
                trace_id="b" * 32,
                run_id="run-1",
                name="sandbox.exec",
                start_time_unix_nano=0,
                end_time_unix_nano=1,
                status="OK",
                process_command_args=["true"],
                container_image_name="alpine:3.20",
            )
        ],
        traces_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--traces", str(traces_path), "--tier", "regression"]
    )

    assert result.exit_code != 0
    assert "--tier is only meaningful together with --scores" in result.output


# -- --category-fail-under ----------------------------------------------------


def test_category_fail_under_fails_even_when_aggregate_fail_under_passes(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            # Aggregate: 3/4 = 0.75, comfortably clears a lenient
            # --fail-under 0.1.
            _make_score(eval_case_id="c1", value=True, tags=[]),
            _make_score(eval_case_id="c2", value=True, tags=[]),
            _make_score(eval_case_id="c3", value=True, tags=["category:safety"]),
            # The one safety-tagged case is 1/2 within its category --
            # clears no reasonable-but-strict bar.
            _make_score(eval_case_id="c4", value=False, tags=["category:safety"]),
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
            "--fail-under",
            "0.1",
            "--category-fail-under",
            "category:safety:0.6",
        ],
    )

    assert result.exit_code != 0, result.output
    assert "deterministic: pass_rate=0.7500 (3/4)" in result.output
    assert "category:safety" in result.output.split("CI/CD gate failed")[-1]


def test_category_fail_under_passes_independent_of_a_low_or_absent_aggregate_gate(
    tmp_path,
):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            # Aggregate is a low 1/4 = 0.25 -- but no --fail-under is
            # given at all, so it's never even checked.
            _make_score(eval_case_id="c1", value=False, tags=[]),
            _make_score(eval_case_id="c2", value=False, tags=[]),
            _make_score(eval_case_id="c3", value=False, tags=[]),
            # The safety category is 1/1 -- clears its own bar on its own.
            _make_score(eval_case_id="c4", value=True, tags=["category:safety"]),
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
            "--category-fail-under",
            "category:safety:0.1",
        ],
    )

    assert result.exit_code == 0, result.output


def test_category_fail_under_still_enforced_with_no_aggregate_fail_under_at_all(
    tmp_path,
):
    # The other half of "independent of the aggregate gate": a category
    # gate that WOULD fail must still fail even when --fail-under is
    # never given at all -- proves the category check doesn't silently
    # depend on --fail-under also being present to run.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=True, tags=["category:safety"]),
            _make_score(eval_case_id="c2", value=False, tags=["category:safety"]),
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
            "--category-fail-under",
            "category:safety:0.99",
        ],
    )

    assert result.exit_code != 0, result.output
    assert "category:safety" in result.output.split("CI/CD gate failed")[-1]


def test_category_fail_under_scoped_within_active_tier_filter(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="r1",
                value=True,
                tier="regression",
                tags=["category:safety"],
            ),
            # Same tag, but golden tier -- must never be counted once
            # --tier regression narrows the universe first.
            _make_score(
                eval_case_id="g1",
                value=False,
                tier="golden",
                tags=["category:safety"],
            ),
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
            "--tier",
            "regression",
            "--category-fail-under",
            "category:safety:0.1",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "category:safety" in result.output
    assert "1/1" in result.output


def test_category_fail_under_unknown_tag_fails_loudly(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True, tags=[])], scores_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--category-fail-under",
            "category:nonexistent:0.5",
        ],
    )

    assert result.exit_code != 0
    assert "no matching Score" in result.output
    assert "category:nonexistent" in result.output


def test_category_fail_under_malformed_raises_usage_error(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores([_make_score(eval_case_id="c1", value=True)], scores_path)

    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--scores",
            str(scores_path),
            "--category-fail-under",
            "malformed-no-colon",
        ],
    )

    assert result.exit_code != 0
    assert "TAG:THRESHOLD" in result.output


def test_category_fail_under_rejected_without_scores_mode():
    runner = CliRunner()
    result = runner.invoke(
        main,
        [
            "report",
            "--successes",
            "1",
            "--total",
            "1",
            "--category-fail-under",
            "safety:0.5",
        ],
    )

    assert result.exit_code != 0
    assert "--category-fail-under is only meaningful together with --scores" in (
        result.output
    )


# -- flaky exclusion -----------------------------------------------------------


def test_flaky_score_excluded_from_aggregate_gate_but_still_printed(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=True, flaky=False),
            _make_score(eval_case_id="c2", value=True, flaky=False),
            _make_score(eval_case_id="c3", value=True, flaky=False),
            # Flaky and wrong -- would drag 3/3 (lower bound 0.4385) down
            # to 3/4 (lower bound 0.3006) if it counted toward the gate.
            _make_score(eval_case_id="c4", value=False, flaky=True),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--fail-under", "0.35"],
    )

    assert result.exit_code == 0, result.output
    # Still visible: the printed denominator includes the flaky case.
    assert "deterministic: pass_rate=0.7500 (3/4)" in result.output
    assert "(1 flaky excluded from gate)" in result.output


def test_non_flaky_score_at_the_same_rate_fails_the_gate_as_a_control(tmp_path):
    # The exact same 3-pass/1-fail shape as the test above, but the
    # failing case is NOT flaky -- proves the previous test's green exit
    # is really caused by flaky exclusion, not by --fail-under 0.35 being
    # too lenient to ever fail anything.
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=True, flaky=False),
            _make_score(eval_case_id="c2", value=True, flaky=False),
            _make_score(eval_case_id="c3", value=True, flaky=False),
            _make_score(eval_case_id="c4", value=False, flaky=False),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main,
        ["report", "--scores", str(scores_path), "--fail-under", "0.35"],
    )

    assert result.exit_code != 0, result.output
    assert "(flaky excluded from gate)" not in result.output


def test_flaky_score_excluded_from_category_gate_too(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(
                eval_case_id="c1", value=True, tags=["category:safety"], flaky=False
            ),
            # Flaky, wrong, and safety-tagged: must not sink the category
            # gate, per this RFC's explicit "no, uniformly" answer to
            # whether flaky exclusion applies to category gates too.
            _make_score(
                eval_case_id="c2", value=False, tags=["category:safety"], flaky=True
            ),
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
            "--category-fail-under",
            "category:safety:0.1",
        ],
    )

    assert result.exit_code == 0, result.output
    assert "(1 flaky excluded from gate)" in result.output


def test_all_flaky_group_skips_the_gate_entirely_without_crashing(tmp_path):
    scores_path = tmp_path / "scores.jsonl"
    append_scores(
        [
            _make_score(eval_case_id="c1", value=False, flaky=True),
            _make_score(eval_case_id="c2", value=False, flaky=True),
        ],
        scores_path,
    )

    runner = CliRunner()
    result = runner.invoke(
        main, ["report", "--scores", str(scores_path), "--fail-under", "0.9"]
    )

    # Nothing eligible to gate on -- neither an automatic pass nor a
    # ValueError from wilson_interval(0, 0).
    assert result.exit_code == 0, result.output
    assert "(2 flaky excluded from gate)" in result.output
