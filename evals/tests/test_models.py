from datetime import UTC, datetime
from decimal import Decimal

import pytest
from pydantic import ValidationError

from evals.models import EvalCase, PanelVote, Run, Score, Span, TrendSnapshot


def _make_case(**overrides) -> EvalCase:
    defaults = {
        "id": "case-001",
        "revision": 1,
        "task_spec": {"prompt": "say hello"},
        "reference": "hello",
        "tier": "golden",
        "tags": ["greeting"],
    }
    defaults.update(overrides)
    return EvalCase(**defaults)


def test_construct_with_all_fields():
    case = _make_case()
    assert case.id == "case-001"
    assert case.revision == 1
    assert case.task_spec == {"prompt": "say hello"}
    assert case.reference == "hello"
    assert case.tier == "golden"
    assert case.tags == ["greeting"]


def test_reference_defaults_to_none():
    case = EvalCase(id="c", revision=1, task_spec={}, tier="regression")
    assert case.reference is None


def test_tags_default_to_empty_list():
    case = EvalCase(id="c", revision=1, task_spec={}, tier="regression")
    assert case.tags == []


def test_invalid_tier_rejected():
    with pytest.raises(ValidationError):
        EvalCase(id="c", revision=1, task_spec={}, tier="not-a-real-tier")


def test_flaky_defaults_to_false():
    case = EvalCase(id="c", revision=1, task_spec={}, tier="regression")
    assert case.flaky is False


def test_flaky_can_be_set_true():
    case = _make_case(flaky=True)
    assert case.flaky is True


def test_old_shape_eval_case_json_line_still_validates_with_flaky_defaulted():
    # The load-bearing backward-compat proof for docs/rfcs/2026-09-07-
    # evals-cigate-refinements.md: an EvalCase JSON blob written before
    # `flaky` existed must still validate cleanly, resolving to False —
    # never a validation error, never a silently-wrong value. Mirrors
    # test_old_shape_run_json_line_still_validates_with_new_fields_
    # defaulted's own precedent for Run.
    old_shape_json = (
        '{"id": "c", "revision": 1, "task_spec": {}, "reference": null, '
        '"tier": "golden", "tags": []}'
    )
    case = EvalCase.model_validate_json(old_shape_json)
    assert case.flaky is False


def test_instances_are_frozen():
    case = _make_case()
    with pytest.raises(ValidationError):
        case.revision = 2  # type: ignore[misc]


def test_with_revision_returns_new_instance_without_mutating_original():
    original = _make_case(revision=1)
    bumped = original.with_revision(2)

    assert original.revision == 1
    assert bumped.revision == 2
    assert bumped is not original
    assert bumped.id == original.id
    assert bumped.task_spec == original.task_spec


def test_with_revision_preserves_immutability_of_result():
    bumped = _make_case().with_revision(5)
    with pytest.raises(ValidationError):
        bumped.revision = 6  # type: ignore[misc]


def _make_run(**overrides) -> Run:
    defaults = {
        "id": "run-001",
        "eval_case_id": "case-001",
        "eval_case_revision": 1,
        "harness_config": {"image": "alpine:3.20", "command": ["echo", "hi"]},
        "status": "completed",
        "exit_code": 0,
        "stdout": "hi\n",
        "latency_ms": 42.0,
    }
    defaults.update(overrides)
    return Run(**defaults)


def test_run_construct_with_all_fields():
    run = _make_run(cost_usd=0.01, stderr="", error=None)
    assert run.id == "run-001"
    assert run.eval_case_id == "case-001"
    assert run.eval_case_revision == 1
    assert run.harness_config == {"image": "alpine:3.20", "command": ["echo", "hi"]}
    assert run.status == "completed"
    assert run.exit_code == 0
    assert run.stdout == "hi\n"
    assert run.latency_ms == 42.0
    assert run.cost_usd == 0.01


def test_run_construct_with_only_required_fields():
    run = Run(
        id="run-002",
        eval_case_id="case-001",
        eval_case_revision=1,
        harness_config={"image": "alpine:3.20", "command": ["true"]},
        status="error",
        latency_ms=1.5,
    )
    assert run.exit_code is None
    assert run.stdout == ""
    assert run.stderr == ""
    assert run.error is None


def test_run_cost_usd_defaults_to_none_not_zero():
    run = _make_run()
    assert run.cost_usd is None


def test_run_invalid_status_rejected():
    with pytest.raises(ValidationError):
        _make_run(status="not-a-real-status")


def test_run_instances_are_frozen():
    run = _make_run()
    with pytest.raises(ValidationError):
        run.status = "error"  # type: ignore[misc]


def test_run_cache_fields_default_to_falsy_none():
    run = _make_run()
    assert run.cache_key is None
    assert run.from_cache is False
    assert run.cache_source_run_id is None


def test_run_skip_reason_defaults_to_none():
    run = _make_run()
    assert run.skip_reason is None


def test_run_status_accepts_skipped():
    run = _make_run(status="skipped", skip_reason="early-stopped")
    assert run.status == "skipped"
    assert run.skip_reason == "early-stopped"


def test_old_shape_run_json_line_still_validates_with_new_fields_defaulted():
    """The load-bearing backward-compat proof for
    docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md: a `Run` JSONL
    line written before cache_key/from_cache/cache_source_run_id/
    skip_reason existed must still `model_validate_json` cleanly, with
    every new field resolving to its declared default — never a
    validation error, never a silently-wrong value.
    """
    old_shape_json = (
        '{"id": "run-001", "eval_case_id": "case-001", "eval_case_revision": 1, '
        '"harness_config": {"image": "alpine:3.20", "command": ["echo", "hi"]}, '
        '"status": "completed", "exit_code": 0, "stdout": "hi\\n", "stderr": "", '
        '"latency_ms": 42.0, "cost_usd": null, "error": null}'
    )
    run = Run.model_validate_json(old_shape_json)
    assert run.skip_reason is None
    assert run.cache_key is None
    assert run.from_cache is False
    assert run.cache_source_run_id is None


def _make_score(**overrides) -> Score:
    defaults = {
        "eval_case_id": "case-001",
        "eval_case_revision": 1,
        "run_id": "run-001",
        "scorer_id": "exact_match",
        "scorer_type": "deterministic",
        "value": True,
    }
    defaults.update(overrides)
    return Score(**defaults)


def test_score_construct_with_all_fields():
    score = _make_score(
        run_id="run-001",
        scorer_id="claude-haiku-4-5-20251001",
        scorer_type="llm_judge",
        value=True,
        rationale="matches exactly",
        bias_mitigations_applied=["cot_forcing", "reference_guided_grading"],
    )
    assert score.eval_case_id == "case-001"
    assert score.eval_case_revision == 1
    assert score.run_id == "run-001"
    assert score.scorer_id == "claude-haiku-4-5-20251001"
    assert score.scorer_type == "llm_judge"
    assert score.value is True
    assert score.rationale == "matches exactly"
    assert score.bias_mitigations_applied == ["cot_forcing", "reference_guided_grading"]


def test_score_construct_with_only_required_fields():
    score = Score(
        eval_case_id="case-001",
        eval_case_revision=1,
        scorer_id="exact_match",
        scorer_type="deterministic",
        value=False,
    )
    assert score.run_id is None
    assert score.rationale is None
    assert score.rubric_axis is None
    assert score.bias_mitigations_applied == []
    assert score.cost_usd is None


def test_score_run_id_defaults_to_none_not_a_fabricated_case_id():
    score = Score(
        eval_case_id="case-001",
        eval_case_revision=1,
        scorer_id="exact_match",
        scorer_type="deterministic",
        value=True,
    )
    assert score.run_id is None
    assert score.run_id != score.eval_case_id


def test_score_cost_usd_can_be_set_explicitly_for_llm_judge():
    score = _make_score(scorer_type="llm_judge", cost_usd=Decimal("0.0000075"))
    assert score.cost_usd == Decimal("0.0000075")


def test_score_cost_usd_zero_for_deterministic_is_exact_not_fabricated():
    score = _make_score(scorer_type="deterministic", cost_usd=Decimal("0"))
    assert score.cost_usd == Decimal("0")
    # Distinct from None -- 0 here is a certain fact (no external call is
    # ever made by a deterministic scorer), not an unmeasured value.
    assert score.cost_usd is not None


def test_score_invalid_scorer_type_rejected():
    with pytest.raises(ValidationError):
        _make_score(scorer_type="skeptic_panel")


def test_score_accepts_llm_judge_panel_scorer_type():
    # docs/rfcs/2026-09-07-evals-judge-panel-interface.md widened
    # ScorerType to include this value; docs/rfcs/2026-09-08-evals-judge-
    # panel-reducer.md made it real -- evals.cli's --llm-judge-panel flag
    # constructs one.
    score = _make_score(scorer_type="llm_judge_panel")
    assert score.scorer_type == "llm_judge_panel"


def test_panel_vote_is_frozen_and_defaults_cache_fields_to_none_and_false():
    vote = PanelVote(
        scorer_id="claude-haiku-4-5-20251001", passed=True, rationale="matches"
    )
    assert vote.score_cache_key is None
    assert vote.from_cache is False
    with pytest.raises(ValidationError):
        vote.passed = False  # type: ignore[misc]


def test_score_llm_judge_panel_carries_panel_votes_and_quorum_reached():
    votes = [
        PanelVote(scorer_id="claude-haiku-4-5-20251001", passed=True, rationale="a"),
        PanelVote(scorer_id="gpt-4o-mini", passed=True, rationale="b"),
    ]
    score = _make_score(
        scorer_type="llm_judge_panel",
        scorer_id="panel:claude-haiku-4-5-20251001+gpt-4o-mini",
        panel_votes=votes,
        quorum_reached=True,
    )
    assert score.panel_votes == votes
    assert score.quorum_reached is True


def test_score_panel_votes_and_quorum_reached_default_to_none_for_non_panel_scores():
    score = _make_score(scorer_type="llm_judge")
    assert score.panel_votes is None
    assert score.quorum_reached is None


def test_score_instances_are_frozen():
    score = _make_score()
    with pytest.raises(ValidationError):
        score.value = False  # type: ignore[misc]


def test_score_tier_tags_flaky_default_to_none_empty_false():
    score = _make_score()
    assert score.tier is None
    assert score.tags == []
    assert score.flaky is False


def test_score_can_carry_denormalized_tier_tags_flaky():
    score = _make_score(tier="regression", tags=["category:safety"], flaky=True)
    assert score.tier == "regression"
    assert score.tags == ["category:safety"]
    assert score.flaky is True


def test_score_invalid_tier_rejected():
    with pytest.raises(ValidationError):
        _make_score(tier="not-a-real-tier")


def test_old_shape_score_json_line_still_validates_with_new_fields_defaulted():
    # The load-bearing backward-compat proof for docs/rfcs/2026-09-07-
    # evals-cigate-refinements.md: a Score JSONL line written before
    # tier/tags/flaky existed must still validate cleanly, with every new
    # field resolving to its declared default.
    old_shape_json = (
        '{"eval_case_id": "case-1", "eval_case_revision": 1, "run_id": null, '
        '"scorer_id": "exact_match", "scorer_type": "deterministic", '
        '"value": true, "rationale": null, "rubric_axis": null, '
        '"bias_mitigations_applied": [], "cost_usd": null, '
        '"score_cache_key": null, "from_cache": false}'
    )
    score = Score.model_validate_json(old_shape_json)
    assert score.tier is None
    assert score.tags == []
    assert score.flaky is False
    assert score.panel_votes is None
    assert score.quorum_reached is None


def test_score_with_panel_votes_round_trips_through_json():
    # docs/rfcs/2026-09-08-evals-judge-panel-reducer.md's load-bearing
    # proof: a real llm_judge_panel Score's embedded per-judge audit
    # trail survives a full serialize/deserialize cycle exactly, since a
    # persisted --scores JSONL line is this codebase's only real
    # cross-process transport for panel_votes.
    votes = [
        PanelVote(scorer_id="claude-haiku-4-5-20251001", passed=True, rationale="a"),
        PanelVote(scorer_id="gpt-4o-mini", passed=False, rationale="b"),
    ]
    score = _make_score(
        scorer_type="llm_judge_panel",
        scorer_id="panel:claude-haiku-4-5-20251001+gpt-4o-mini",
        panel_votes=votes,
        quorum_reached=False,
        value=False,
    )
    round_tripped = Score.model_validate_json(score.model_dump_json())
    assert round_tripped == score
    assert round_tripped.panel_votes == votes
    assert round_tripped.quorum_reached is False


def _make_span(**overrides) -> Span:
    defaults = {
        "span_id": "a" * 16,
        "trace_id": "b" * 32,
        "run_id": "run-001",
        "name": "sandbox.exec",
        "start_time_unix_nano": 1_000_000,
        "end_time_unix_nano": 2_000_000,
        "status": "OK",
        "process_command_args": ["echo", "hello"],
        "container_image_name": "alpine:3.20",
    }
    defaults.update(overrides)
    return Span(**defaults)


def test_span_construct_with_all_fields():
    span = _make_span(
        parent_span_id="c" * 16,
        process_exit_code=0,
        error=None,
    )
    assert span.span_id == "a" * 16
    assert span.trace_id == "b" * 32
    assert span.parent_span_id == "c" * 16
    assert span.run_id == "run-001"
    assert span.name == "sandbox.exec"
    assert span.start_time_unix_nano == 1_000_000
    assert span.end_time_unix_nano == 2_000_000
    assert span.status == "OK"
    assert span.process_command_args == ["echo", "hello"]
    assert span.process_exit_code == 0
    assert span.container_image_name == "alpine:3.20"
    assert span.error is None


def test_span_construct_with_only_required_fields():
    span = _make_span()
    assert span.parent_span_id is None
    assert span.process_exit_code is None
    assert span.error is None


def test_span_error_status_carries_no_exit_code():
    span = _make_span(status="ERROR", error="docker binary not found")
    assert span.status == "ERROR"
    assert span.process_exit_code is None
    assert span.error == "docker binary not found"


def test_span_invalid_status_rejected():
    with pytest.raises(ValidationError):
        _make_span(status="RUNNING")


def test_span_instances_are_frozen():
    span = _make_span()
    with pytest.raises(ValidationError):
        span.status = "ERROR"  # type: ignore[misc]


def _make_trend_snapshot(**overrides) -> TrendSnapshot:
    defaults = {
        "series": "quote_grounding_rate",
        "recorded_at": datetime(2026, 9, 10, 12, 0, 0, tzinfo=UTC),
        "n": 10,
        "rate_value": 0.9,
        "scorer_type": "llm_judge",
        "source_command": "report",
    }
    defaults.update(overrides)
    return TrendSnapshot(**defaults)


def test_trend_snapshot_construct_with_rate_value():
    snap = _make_trend_snapshot()
    assert snap.series == "quote_grounding_rate"
    assert snap.n == 10
    assert snap.rate_value == 0.9
    assert snap.cost_usd_value is None
    assert snap.scorer_type == "llm_judge"
    assert snap.category_tag is None
    assert snap.source_command == "report"


def test_trend_snapshot_construct_with_cost_usd_value():
    snap = _make_trend_snapshot(
        series="cost_usd", rate_value=None, cost_usd_value=Decimal("0.0042")
    )
    assert snap.series == "cost_usd"
    assert snap.cost_usd_value == Decimal("0.0042")
    assert snap.rate_value is None


def test_trend_snapshot_rate_value_can_be_none_for_a_non_cost_series():
    # A real, meaningful state -- e.g. an undefined Cohen's kappa, or no
    # known quote_grounded data yet -- distinct from "not applicable".
    snap = _make_trend_snapshot(series="judge_accuracy_kappa", rate_value=None)
    assert snap.rate_value is None
    assert snap.cost_usd_value is None


def test_trend_snapshot_audit_corpus_defect_rate_has_no_scorer_type():
    snap = _make_trend_snapshot(
        series="audit_corpus_defect_rate",
        scorer_type=None,
        source_command="audit-corpus",
    )
    assert snap.scorer_type is None
    assert snap.source_command == "audit-corpus"


def test_trend_snapshot_category_tag_defaults_to_none():
    snap = _make_trend_snapshot()
    assert snap.category_tag is None


def test_trend_snapshot_invalid_series_rejected():
    with pytest.raises(ValidationError):
        _make_trend_snapshot(series="not-a-real-series")


def test_trend_snapshot_invalid_source_command_rejected():
    with pytest.raises(ValidationError):
        _make_trend_snapshot(source_command="rollout")


def test_trend_snapshot_instances_are_frozen():
    snap = _make_trend_snapshot()
    with pytest.raises(ValidationError):
        snap.n = 99  # type: ignore[misc]


def test_trend_snapshot_rejects_both_rate_value_and_cost_usd_value_set():
    with pytest.raises(ValidationError):
        _make_trend_snapshot(
            series="cost_usd", rate_value=0.5, cost_usd_value=Decimal("1.00")
        )


def test_trend_snapshot_cost_series_with_rate_value_instead_of_cost_usd_value():
    with pytest.raises(ValidationError):
        _make_trend_snapshot(series="cost_usd", rate_value=0.5, cost_usd_value=None)


def test_trend_snapshot_non_cost_series_with_cost_usd_value_instead_of_rate_value():
    with pytest.raises(ValidationError):
        _make_trend_snapshot(
            series="quote_grounding_rate",
            rate_value=None,
            cost_usd_value=Decimal("1.00"),
        )


def test_trend_snapshot_cost_usd_value_round_trips_through_json_as_decimal():
    snap = _make_trend_snapshot(
        series="cost_usd", rate_value=None, cost_usd_value=Decimal("0.0000075")
    )
    round_tripped = TrendSnapshot.model_validate_json(snap.model_dump_json())
    assert round_tripped == snap
    assert isinstance(round_tripped.cost_usd_value, Decimal)
    assert round_tripped.cost_usd_value == Decimal("0.0000075")
