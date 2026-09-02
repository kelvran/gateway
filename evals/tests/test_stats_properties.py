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

from hypothesis import assume, given, settings
from hypothesis import strategies as st

from evals.stats import _EPSILON, wilson_interval

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
