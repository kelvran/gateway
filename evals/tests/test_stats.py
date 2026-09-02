import math

import pytest

from evals.stats import wilson_interval


def test_known_reference_8_of_10_at_95_percent():
    # Well-documented reference value for the Wilson score interval at
    # successes=8, total=10, 95% confidence.
    lower, upper = wilson_interval(8, 10, confidence=0.95)
    assert math.isclose(lower, 0.4902, abs_tol=1e-3)
    assert math.isclose(upper, 0.9433, abs_tol=1e-3)


def test_interval_bounds_are_ordered_and_within_unit_range():
    lower, upper = wilson_interval(50, 100)
    assert 0.0 < lower < upper < 1.0


def test_total_zero_raises():
    with pytest.raises(ValueError):
        wilson_interval(0, 0)


def test_successes_greater_than_total_raises():
    with pytest.raises(ValueError):
        wilson_interval(11, 10)


def test_negative_successes_raises():
    with pytest.raises(ValueError):
        wilson_interval(-1, 10)


def test_confidence_out_of_range_raises():
    with pytest.raises(ValueError):
        wilson_interval(5, 10, confidence=1.0)
    with pytest.raises(ValueError):
        wilson_interval(5, 10, confidence=0.0)


def test_all_successes_bounded_below_one():
    lower, upper = wilson_interval(10, 10)
    assert upper < 1.0
    assert lower > 0.0


def test_all_failures_bounded_above_zero():
    lower, upper = wilson_interval(0, 10)
    assert lower > 0.0
    assert upper < 1.0


def test_higher_confidence_widens_interval():
    lower_90, upper_90 = wilson_interval(8, 10, confidence=0.90)
    lower_99, upper_99 = wilson_interval(8, 10, confidence=0.99)
    assert lower_99 < lower_90
    assert upper_99 > upper_90


def test_larger_n_narrows_interval_for_same_proportion():
    lower_small, upper_small = wilson_interval(8, 10)
    lower_large, upper_large = wilson_interval(800, 1000)
    assert (upper_large - lower_large) < (upper_small - lower_small)
