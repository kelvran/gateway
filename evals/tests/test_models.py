import pytest
from pydantic import ValidationError

from evals.models import EvalCase


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
