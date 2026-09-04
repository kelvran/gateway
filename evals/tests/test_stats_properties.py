"""Property-based regression tests for `evals.stats.wilson_interval`.

`tests/test_stats.py` already pins a handful of fixed reference values, but
a fixed-value test only catches a regression if the mutated formula happens
to disagree on *those specific* inputs. These Hypothesis tests instead
assert on mathematical invariants that must hold for *every* valid
(successes, total, confidence) triple — so a future accidental change to
the formula (e.g. a sign flip in the adjustment term, a dropped `z**2`
term, or a broken epsilon clamp) gets caught even if it happens to still
satisfy the handful of hard-coded reference values.

Each property below documents, in its own docstring/comment, *why* it must
hold — these are regression tests in the strict sense, not fuzz-for-
fuzzing's-sake.
"""

from __future__ import annotations

import random

from hypothesis import assume, given, settings
from hypothesis import strategies as st

from evals.stats import _EPSILON, two_checkpoint_early_stop, wilson_interval

# Keep `total` in a range large enough to be a meaningful sample size but
# small enough that float arithmetic near the tails stays well-conditioned
# for the "larger n narrows the interval" property below (which multiplies
# `total` by up to 50x).
_totals = st.integers(min_value=1, max_value=2_000)
# Confidence levels bounded away from the open interval's extremes (0, 1):
# right at the edges `NormalDist().inv_cdf` blows up towards +/-inf, which
# would make every property here trivially true/false rather than
# exercising the real formula.
_confidences = st.floats(
    min_value=0.5, max_value=0.999, allow_nan=False, allow_infinity=False
)


@st.composite
def _successes_and_total(draw: st.DrawFn) -> tuple[int, int]:
    total = draw(_totals)
    successes = draw(st.integers(min_value=0, max_value=total))
    return successes, total


@given(st_and_total=_successes_and_total(), confidence=_confidences)
@settings(max_examples=300)
def test_lower_never_exceeds_upper(st_and_total, confidence):
    successes, total = st_and_total
    # A confidence interval is, by definition, an ordered [lower, upper]
    # pair. If the formula ever produced lower > upper, every downstream
    # consumer (the CLI's format_report, any CI/CD gate) would silently
    # report nonsense.
    lower, upper = wilson_interval(successes, total, confidence=confidence)
    assert lower <= upper


@given(st_and_total=_successes_and_total(), confidence=_confidences)
@settings(max_examples=300)
def test_bounds_strictly_within_open_unit_interval(st_and_total, confidence):
    successes, total = st_and_total
    # stats.py documents a deliberate epsilon clamp specifically so a
    # closed interval never touches 0.0 or 1.0 — a claim no finite sample
    # can support. Both bounds must always land inside [_EPSILON,
    # 1 - _EPSILON], not just "somewhere in (0, 1)".
    lower, upper = wilson_interval(successes, total, confidence=confidence)
    assert _EPSILON <= lower <= 1 - _EPSILON
    assert _EPSILON <= upper <= 1 - _EPSILON


@given(
    st_and_total=_successes_and_total(),
    confidence_a=_confidences,
    confidence_b=_confidences,
)
@settings(max_examples=300)
def test_higher_confidence_is_never_narrower(st_and_total, confidence_a, confidence_b):
    successes, total = st_and_total
    # A 99% CI must never be narrower than a 90% CI for the identical
    # sample — asking for more certainty can only ever cost you width, it
    # can't buy you a tighter interval for free. If a code change ever
    # inverted the relationship between `z` and confidence, this would
    # catch it.
    assume(confidence_a != confidence_b)
    lo_a, hi_a = wilson_interval(successes, total, confidence=confidence_a)
    lo_b, hi_b = wilson_interval(successes, total, confidence=confidence_b)
    width_a = hi_a - lo_a
    width_b = hi_b - lo_b
    if confidence_a < confidence_b:
        assert width_a <= width_b + 1e-9
    else:
        assert width_b <= width_a + 1e-9


@given(
    ratio_num=st.integers(min_value=0, max_value=20),
    ratio_denom=st.integers(min_value=1, max_value=20),
    scale=st.integers(min_value=1, max_value=50),
    confidence=_confidences,
)
@settings(max_examples=300)
def test_larger_total_never_widens_interval_for_same_proportion(
    ratio_num, ratio_denom, scale, confidence
):
    # A larger sample at the *same* observed proportion (successes/total
    # held constant) can only ever make the estimate more precise, never
    # less — more evidence for the same signal must not widen the CI. We
    # hold the ratio fixed by scaling both successes and total by the same
    # integer factor, so the point estimate is bit-for-bit identical while
    # `n` grows.
    assume(ratio_num <= ratio_denom)
    small_total = ratio_denom
    small_successes = ratio_num
    large_total = ratio_denom * scale
    large_successes = ratio_num * scale

    lo_small, hi_small = wilson_interval(
        small_successes, small_total, confidence=confidence
    )
    lo_large, hi_large = wilson_interval(
        large_successes, large_total, confidence=confidence
    )

    width_small = hi_small - lo_small
    width_large = hi_large - lo_large
    assert width_large <= width_small + 1e-9


@given(st_and_total=_successes_and_total(), confidence=_confidences)
@settings(max_examples=300)
def test_interval_contains_the_point_estimate_up_to_the_documented_clamp(
    st_and_total, confidence
):
    successes, total = st_and_total
    # A confidence interval that excludes its own point estimate would
    # normally be a contradiction in terms — the observed proportion is
    # the single most-plausible value and belongs inside its own CI. The
    # one deliberate exception is stats.py's documented epsilon clamp: at
    # successes=0 (or successes=total) the raw Wilson bound is exactly 0.0
    # (or 1.0) and gets nudged inward by _EPSILON so the reported interval
    # never claims absolute certainty. That clamp can only ever move a
    # bound by _EPSILON, so containment is asserted with an _EPSILON
    # tolerance rather than being silently dropped.
    point_estimate = successes / total
    lower, upper = wilson_interval(successes, total, confidence=confidence)
    assert lower - _EPSILON <= point_estimate <= upper + _EPSILON


# --- two_checkpoint_early_stop (see the RFC) ---


@given(
    trials_run=st.integers(min_value=0, max_value=50),
    min_trials=st.integers(min_value=1, max_value=25),
    max_trials_offset=st.integers(min_value=0, max_value=25),
    successes_frac=st.floats(min_value=0.0, max_value=1.0, allow_nan=False),
    baseline=st.floats(min_value=0.01, max_value=0.99, allow_nan=False),
)
@settings(max_examples=300)
def test_never_stops_outside_the_two_checkpoints(
    trials_run, min_trials, max_trials_offset, successes_frac, baseline
):
    # The single most important structural guarantee: no matter what the
    # tally looks like, a decision can only ever fire at exactly
    # trials_run == min_trials or == max_trials -- never in between, never
    # before, never after. This is what makes the function safe to call
    # after every single trial without the caller having to track which
    # trial count is a real checkpoint.
    max_trials = min_trials + max_trials_offset
    successes = round(successes_frac * trials_run)
    result = two_checkpoint_early_stop(
        successes, trials_run, min_trials, max_trials, baseline
    )
    if trials_run not in (min_trials, max_trials) or trials_run == 0:
        assert result is False


def test_naive_continuous_recheck_would_have_failed_this_exact_test():
    """Documents, executably, why this feature needed correcting before any
    code shipped: the ORIGINALLY-FLOATED design (recompute wilson_interval
    after every trial, stop at first exclusion of the baseline) inflates
    the true false-positive rate far above the nominal level under the
    null hypothesis (successes drawn at exactly the baseline rate) -- this
    is the textbook optional-stopping/peeking problem
    (docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md's Motivation).
    Simulated here directly, not asserted from citation alone.
    """
    rng = random.Random(20260904)
    baseline = 0.5
    confidence = 0.95
    n_trials = 200
    n_simulations = 300

    naive_false_stops = 0
    for _ in range(n_simulations):
        successes = 0
        stopped = False
        for trial in range(1, n_trials + 1):
            if rng.random() < baseline:
                successes += 1
            # The naive, rejected design: a PLAIN (uncorrected)
            # wilson_interval call, rechecked after every single trial.
            lower, upper = wilson_interval(successes, trial, confidence=confidence)
            if not (lower <= baseline <= upper):
                stopped = True
                break
        if stopped:
            naive_false_stops += 1

    naive_false_stop_rate = naive_false_stops / n_simulations
    # Under the null (true rate == baseline), a VALID 95%-confidence
    # procedure should false-stop at roughly 5%. The naive continuous
    # recheck should blow well past that -- this assertion is the
    # concrete, executable proof of the Motivation section's claim, not a
    # restatement of it.
    assert naive_false_stop_rate > 0.20


@given(baseline=st.floats(min_value=0.3, max_value=0.7, allow_nan=False))
@settings(max_examples=5, deadline=None)
def test_two_checkpoint_design_keeps_false_stop_rate_bounded_under_the_null(baseline):
    """The regression test that proves the fix: under the null hypothesis
    (successes drawn at exactly `baseline`), the two-checkpoint,
    Bonferroni-corrected design's empirical false-stop rate stays at or
    below the nominal (1 - confidence) level, unlike the naive design
    proven unsound above.
    """
    confidence = 0.95
    min_trials, max_trials = 20, 100
    n_simulations = 400
    rng = random.Random(hash(("two_checkpoint", baseline)) & 0xFFFFFFFF)

    false_stops = 0
    for _ in range(n_simulations):
        successes = 0
        stopped_early = False
        for trial in range(1, max_trials + 1):
            if rng.random() < baseline:
                successes += 1
            if two_checkpoint_early_stop(
                successes, trial, min_trials, max_trials, baseline, confidence
            ):
                stopped_early = True
                break
        false_stops += 1 if stopped_early else 0

    false_stop_rate = false_stops / n_simulations
    # Nominal false-positive budget is (1 - confidence) = 0.05. Allow real
    # Monte Carlo slack (this is a statistical claim, not an exact
    # equality) -- the point is "not wildly inflated like the naive
    # design," proven at roughly 3x the nominal budget as a generous
    # ceiling, still far below the naive design's >20% measured above.
    assert false_stop_rate < 0.15
