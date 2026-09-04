from decimal import Decimal

import pytest
from pydantic import ValidationError

from evals.models import EvalCase, Run, Score


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


def test_score_instances_are_frozen():
    score = _make_score()
    with pytest.raises(ValidationError):
        score.value = False  # type: ignore[misc]
