import math

import pytest

from evals.stats import mixture_sprt_early_stop, wilson_interval


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


# --- mixture_sprt_early_stop ---
# See docs/rfcs/2026-09-05-evals-mixture-sprt-early-stopping.md


def test_trials_run_zero_never_stops():
    assert not mixture_sprt_early_stop(
        successes=0,
        trials_run=0,
        baseline_pass_rate=0.5,
    )


def test_baseline_pass_rate_out_of_open_unit_interval_raises():
    for bad_baseline in (0.0, 1.0, -0.1, 1.1):
        with pytest.raises(ValueError):
            mixture_sprt_early_stop(5, 10, baseline_pass_rate=bad_baseline)


def test_confidence_out_of_open_unit_interval_raises():
    for bad_confidence in (0.0, 1.0, -0.1, 1.1):
        with pytest.raises(ValueError):
            mixture_sprt_early_stop(
                5, 10, baseline_pass_rate=0.5, confidence=bad_confidence
            )


def test_non_positive_relative_mixing_variance_raises():
    for bad_value in (0.0, -1.0):
        with pytest.raises(ValueError):
            mixture_sprt_early_stop(
                5, 10, baseline_pass_rate=0.5, relative_mixing_variance=bad_value
            )


def test_successes_out_of_range_raises():
    with pytest.raises(ValueError):
        mixture_sprt_early_stop(successes=11, trials_run=10, baseline_pass_rate=0.5)
    with pytest.raises(ValueError):
        mixture_sprt_early_stop(successes=-1, trials_run=10, baseline_pass_rate=0.5)


def test_likelihood_ratio_matches_a_hand_computed_reference_value():
    # Independently re-derive the closed-form mSPRT likelihood ratio
    # inline, rather than re-using any of the function's own internals --
    # a correctness proof of the formula, not a re-implementation.
    successes, trials_run, baseline, confidence, relative_mixing_variance = (
        7,
        10,
        0.4,
        0.95,
        1.0,
    )
    null_variance = baseline * (1 - baseline)
    mixing_variance = relative_mixing_variance * null_variance
    n = trials_run
    theta = successes / n - baseline
    n_tau_sq = n * mixing_variance
    denom = null_variance + n_tau_sq
    expected_ratio = math.sqrt(null_variance / denom) * math.exp(
        (n * n_tau_sq * theta**2) / (2 * null_variance * denom)
    )
    expected_stop = expected_ratio >= 1 / (1 - confidence)

    assert (
        mixture_sprt_early_stop(
            successes, trials_run, baseline, confidence, relative_mixing_variance
        )
        == expected_stop
    )


def test_observed_rate_exactly_at_baseline_never_stops_for_any_trial_count():
    # theta = 0 makes the exponential term exp(0) = 1, so the likelihood
    # ratio reduces to sqrt(null_variance / (null_variance + n*mixing_variance)),
    # which is always < 1 -- and therefore always < 1/alpha for any
    # alpha < 1. An exact-match observed rate can never trigger a stop,
    # regardless of how many trials have run.
    baseline = 0.5
    for n in (1, 2, 5, 10, 100, 1000):
        successes = round(n * baseline)
        assert not mixture_sprt_early_stop(successes, n, baseline)


def test_stops_for_an_extreme_deviation_with_enough_trials():
    # 20/20 successes vs. a baseline of 0.1 is an extreme, unambiguous
    # case a real sequential test must eventually flag.
    assert mixture_sprt_early_stop(
        successes=20,
        trials_run=20,
        baseline_pass_rate=0.1,
    )


def test_does_not_stop_for_a_small_ambiguous_sample():
    # A single trial can never give a valid test enough evidence against
    # a middling baseline.
    assert not mixture_sprt_early_stop(
        successes=1,
        trials_run=1,
        baseline_pass_rate=0.5,
    )


def test_relative_mixing_variance_changes_detection_speed_not_the_decision_rule():
    # Different mixing variances can legitimately disagree on whether a
    # given (successes, trials_run) has crossed the threshold yet -- this
    # is the documented power/speed tradeoff, not a bug. Values found by a
    # real numeric sweep against the function itself, not guessed: a
    # too-tightly-concentrated mixing variance (0.05x the null variance)
    # has NOT yet crossed the threshold for this case, while the
    # literature-recommended default (1.0x) already has.
    successes, trials_run, baseline = 3, 5, 0.1
    tight = mixture_sprt_early_stop(
        successes, trials_run, baseline, relative_mixing_variance=0.05
    )
    default = mixture_sprt_early_stop(
        successes, trials_run, baseline, relative_mixing_variance=1.0
    )
    assert tight is False
    assert default is True
