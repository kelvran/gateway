import math

import pytest

from evals.stats import two_checkpoint_early_stop, wilson_interval


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


# --- two_checkpoint_early_stop (see the RFC) ---


def test_never_stops_before_min_trials():
    # An all-success group checked against a baseline far below 1.0 would
    # stop immediately under a naive continuous recheck -- this proves it
    # does NOT, for any trial count short of min_trials.
    for trials_run in range(1, 5):
        assert not two_checkpoint_early_stop(
            successes=trials_run,
            trials_run=trials_run,
            min_trials=5,
            max_trials=20,
            baseline_pass_rate=0.1,
        )


def test_never_stops_strictly_between_the_two_checkpoints():
    for trials_run in range(6, 20):
        assert not two_checkpoint_early_stop(
            successes=trials_run,
            trials_run=trials_run,
            min_trials=5,
            max_trials=20,
            baseline_pass_rate=0.1,
        )


def test_trials_run_zero_never_stops():
    assert not two_checkpoint_early_stop(
        successes=0,
        trials_run=0,
        min_trials=0,
        max_trials=10,
        baseline_pass_rate=0.5,
    )


def test_checkpoint_decision_matches_a_direct_bonferroni_corrected_wilson_call():
    # Cross-check against the real wilson_interval function directly,
    # rather than re-deriving the statistic -- this is a correctness proof
    # of the Bonferroni wiring, not a re-implementation.
    successes, trials_run, min_trials, max_trials = 2, 5, 5, 20
    confidence = 0.95
    baseline = 0.9

    corrected = 1 - (1 - confidence) / 2
    lower, upper = wilson_interval(successes, trials_run, confidence=corrected)
    expected = not (lower <= baseline <= upper)

    assert (
        two_checkpoint_early_stop(
            successes, trials_run, min_trials, max_trials, baseline, confidence
        )
        == expected
    )


def test_degenerate_single_checkpoint_uses_plain_confidence_not_bonferroni():
    # min_trials == max_trials: only one real checkpoint exists, so no
    # correction should be applied -- using the Bonferroni-adjusted
    # confidence here would be needlessly conservative for a single check.
    successes, trials_run = 8, 10
    baseline = 0.5
    confidence = 0.95

    lower, upper = wilson_interval(successes, trials_run, confidence=confidence)
    expected = not (lower <= baseline <= upper)

    assert (
        two_checkpoint_early_stop(
            successes, trials_run, trials_run, trials_run, baseline, confidence
        )
        == expected
    )


def test_stops_when_interval_excludes_baseline_at_max_trials():
    # 10/10 successes vs. a baseline of 0.1 is an extreme, unambiguous case
    # -- the interval at n=10 cannot possibly contain 0.1.
    assert two_checkpoint_early_stop(
        successes=10,
        trials_run=10,
        min_trials=5,
        max_trials=10,
        baseline_pass_rate=0.1,
    )


def test_does_not_stop_when_interval_contains_baseline_at_max_trials():
    # 5/10 successes vs. a baseline of 0.5 -- the point estimate IS the
    # baseline, so the interval must contain it.
    assert not two_checkpoint_early_stop(
        successes=5,
        trials_run=10,
        min_trials=5,
        max_trials=10,
        baseline_pass_rate=0.5,
    )
