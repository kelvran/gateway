"""Unit tests, in isolation (no `CliRunner`), for the pure helper functions
`report_cmd` uses to implement `--tier`/`--category-fail-under`/flaky
exclusion — see docs/rfcs/2026-09-07-evals-cigate-refinements.md.

`tests/test_cigate_integration.py` covers the same three mechanisms through
the real CLI; this file isolates the small, pure pieces so a failure here
points straight at the filtering/parsing logic itself, not at CLI wiring
around it.
"""

from __future__ import annotations

import click
import pytest

from evals.cli import (
    _filter_scores_by_tier,
    _gate_eligible,
    _parse_category_fail_under,
)
from evals.models import Score


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


# -- _filter_scores_by_tier --------------------------------------------------


def test_filter_scores_by_tier_none_is_a_no_op_returning_same_list():
    scores = [_make_score(tier="golden"), _make_score(tier="regression")]
    result = _filter_scores_by_tier(scores, None)
    # Identity, not just equality -- "no-op" means the exact same object,
    # per this function's own docstring.
    assert result is scores


def test_filter_scores_by_tier_keeps_only_matching_tier():
    golden = _make_score(tier="golden")
    regression = _make_score(tier="regression")
    drift = _make_score(tier="drift_sample")
    result = _filter_scores_by_tier([golden, regression, drift], "regression")
    assert result == [regression]


def test_filter_scores_by_tier_excludes_scores_with_no_tier_at_all():
    # A Score persisted before this RFC (tier defaults to None) must never
    # be treated as a match for any real tier -- None != "regression".
    untiered = _make_score(tier=None)
    result = _filter_scores_by_tier([untiered], "regression")
    assert result == []


def test_filter_scores_by_tier_returns_empty_list_when_nothing_matches():
    result = _filter_scores_by_tier([_make_score(tier="golden")], "regression")
    assert result == []


# -- _gate_eligible -----------------------------------------------------------


def test_gate_eligible_drops_flaky_scores_only():
    stable = _make_score(flaky=False)
    flaky = _make_score(flaky=True)
    result = _gate_eligible([stable, flaky])
    assert result == [stable]


def test_gate_eligible_returns_empty_list_when_everything_is_flaky():
    result = _gate_eligible([_make_score(flaky=True), _make_score(flaky=True)])
    assert result == []


def test_gate_eligible_returns_all_when_nothing_is_flaky():
    scores = [_make_score(flaky=False), _make_score(flaky=False)]
    result = _gate_eligible(scores)
    assert result == scores


# -- _parse_category_fail_under -----------------------------------------------


def test_parse_category_fail_under_empty_tuple_returns_empty_list():
    assert _parse_category_fail_under(()) == []


def test_parse_category_fail_under_parses_a_single_pair():
    result = _parse_category_fail_under(("category:safety:0.95",))
    assert result == [("category:safety", 0.95)]


def test_parse_category_fail_under_parses_multiple_pairs_in_order():
    result = _parse_category_fail_under(("safety:0.9", "compliance:0.8"))
    assert result == [("safety", 0.9), ("compliance", 0.8)]


def test_parse_category_fail_under_threshold_is_the_rightmost_colon_segment():
    # A tag itself may contain colons -- rpartition means only the last
    # segment is ever treated as the threshold.
    result = _parse_category_fail_under(("category:safety:0.5",))
    assert result == [("category:safety", 0.5)]


def test_parse_category_fail_under_missing_colon_raises_usage_error():
    with pytest.raises(click.UsageError, match="TAG:THRESHOLD"):
        _parse_category_fail_under(("no-colon-here",))


def test_parse_category_fail_under_empty_tag_raises_usage_error():
    with pytest.raises(click.UsageError, match="TAG:THRESHOLD"):
        _parse_category_fail_under((":0.5",))


def test_parse_category_fail_under_non_float_threshold_raises_usage_error():
    with pytest.raises(click.UsageError, match="not a valid float"):
        _parse_category_fail_under(("safety:not-a-number",))


def test_parse_category_fail_under_duplicate_tag_raises_usage_error():
    with pytest.raises(click.UsageError, match="more than once"):
        _parse_category_fail_under(("safety:0.9", "safety:0.5"))
