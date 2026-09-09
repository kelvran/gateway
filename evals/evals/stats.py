"""Statistical helpers for eval scoring.

`PRD.md`'s success metric is explicit: never report a bare pass rate without
a confidence interval alongside it. The Wilson score interval (not a normal
approximation) is the right tool here because it stays well-behaved exactly
where a golden-tier eval set tends to live: small `n` and/or a proportion
near 0 or 1, both regimes where the normal approximation breaks down.
"""

from __future__ import annotations

import math
from statistics import NormalDist
from typing import NamedTuple

# A closed interval's edge exactly touching 0.0 or 1.0 reads, to a human or
# a CI/CD gate, as "impossible" / "certain" — a claim no finite sample can
# actually support. The literal Wilson formula *does* produce an exact 1.0
# at successes == total (and an exact 0.0 at successes == 0): this clamp is
# a deliberate, documented engineering choice on top of the real formula,
# not a substitute for it, so the reported interval never overstates
# certainty a finite `n` can't back up.
_EPSILON = 1e-9


def wilson_interval(
    successes: int, total: int, confidence: float = 0.95
) -> tuple[float, float]:
    """Return the Wilson score confidence interval for a binomial proportion.

    Args:
        successes: number of successful trials (0 <= successes <= total).
        total: number of trials. Must be > 0.
        confidence: confidence level in (0, 1), e.g. 0.95 for a 95% CI.

    Returns:
        (lower, upper) bound tuple, each strictly within (0.0, 1.0).

    Raises:
        ValueError: if `total` is not positive, `successes` is out of
            [0, total], or `confidence` is not in (0, 1).
    """
    if total <= 0:
        raise ValueError("total must be > 0")
    if not 0 <= successes <= total:
        raise ValueError("successes must satisfy 0 <= successes <= total")
    if not 0 < confidence < 1:
        raise ValueError("confidence must be in the open interval (0, 1)")

    z = NormalDist().inv_cdf(1 - (1 - confidence) / 2)
    n = float(total)
    p_hat = successes / n

    denom = 1 + (z**2) / n
    centre = (p_hat + (z**2) / (2 * n)) / denom
    adjustment = (z / denom) * math.sqrt(
        (p_hat * (1 - p_hat)) / n + (z**2) / (4 * n**2)
    )

    lower = centre - adjustment
    upper = centre + adjustment

    lower = min(max(lower, _EPSILON), 1 - _EPSILON)
    upper = min(max(upper, _EPSILON), 1 - _EPSILON)

    return lower, upper


def mixture_sprt_early_stop(
    successes: int,
    trials_run: int,
    baseline_pass_rate: float,
    confidence: float = 0.95,
    relative_mixing_variance: float = 1.0,
) -> bool:
    """Decide whether a group of repeated trials should stop now, via a
    real mixture sequential probability ratio test (mSPRT) — Robbins
    (1970), popularized for production A/B testing by Johari, Koomen,
    Pekelis & Walsh (KDD 2017), the same paper this project's own prior
    two-checkpoint design already cited as the authoritative source for
    why naively rechecking `wilson_interval` after every trial inflates
    the true false-positive rate (the classical optional-stopping /
    peeking failure mode).

    Unlike that prior design, this is checked after *every single trial*,
    with no pre-declared checkpoints and no Bonferroni correction: by
    Ville's inequality, the mSPRT likelihood ratio is a nonnegative
    martingale under the null, so `P(exists n: stop) <= 1 - confidence`
    holds *simultaneously at every trial count* — the actual anytime-valid
    property the old design's checkpoint restriction was a conservative
    workaround for, not a full substitute.

    The null-hypothesis variance `baseline_pass_rate * (1 -
    baseline_pass_rate)` is used FIXED, never re-estimated from this
    group's own running data — see docs/rfcs/2026-09-05-evals-mixture-
    sprt-early-stopping.md's "A deliberate choice" section: an
    online-estimated variance only gives an asymptotically-valid test,
    with real, measurable false-positive inflation at low trial counts
    unless a substantial warmup is spent first — a cost this early-
    stopping mechanism exists specifically to avoid paying.

    Args:
        successes: successful trials so far in this group.
        trials_run: total trials so far in this group (>= successes). 0
            never stops (no data yet).
        baseline_pass_rate: the pass rate this group's running rate is
            tested against. Must be in the open interval (0, 1) — a
            baseline of exactly 0 or 1 gives zero null variance, a
            degenerate test this project has no reason to support. No
            default: this project has no production traffic yet to
            calibrate one — the operator must always supply it
            explicitly.
        confidence: confidence level in (0, 1). The false-positive rate is
            bounded by `1 - confidence`, at every trial count.
        relative_mixing_variance: the mSPRT's mixing-distribution
            variance, as a multiple of the null variance (Johari et al.'s
            own stated rule of thumb: "on the order of" it, hence a
            default of 1.0). Tunes detection speed/power only — provably
            never affects the false-positive-rate guarantee above, for
            any positive value.

    Returns:
        True if the mSPRT likelihood ratio at this trial count has
        crossed `1 / (1 - confidence)` — a real, valid stop decision.
        False otherwise, including `trials_run == 0`.

    Raises:
        ValueError: if `baseline_pass_rate`/`confidence` is not in the
            open interval (0, 1), `relative_mixing_variance` is not
            positive, or `successes` is out of `[0, trials_run]`.
    """
    if not 0 < baseline_pass_rate < 1:
        raise ValueError("baseline_pass_rate must be in the open interval (0, 1)")
    if not 0 < confidence < 1:
        raise ValueError("confidence must be in the open interval (0, 1)")
    if relative_mixing_variance <= 0:
        raise ValueError("relative_mixing_variance must be > 0")
    if not 0 <= successes <= trials_run:
        raise ValueError("successes must satisfy 0 <= successes <= trials_run")

    if trials_run == 0:
        return False

    null_variance = baseline_pass_rate * (1 - baseline_pass_rate)
    mixing_variance = relative_mixing_variance * null_variance
    n = trials_run
    observed_effect = successes / n - baseline_pass_rate

    n_times_mixing_variance = n * mixing_variance
    denominator = null_variance + n_times_mixing_variance
    # Computed in log-space, not as a raw likelihood ratio compared
    # against 1/alpha directly: an extreme deviation with enough trials
    # drives the real (non-log) ratio's exponent well past what
    # math.exp can represent (a real OverflowError, caught by this
    # module's own property-based test suite, not a hypothetical), while
    # the log-likelihood-ratio itself stays a perfectly ordinary,
    # representable float at every input this function accepts.
    log_likelihood_ratio = 0.5 * math.log(null_variance / denominator) + (
        (n * n_times_mixing_variance * observed_effect**2)
        / (2 * null_variance * denominator)
    )

    alpha = 1 - confidence
    return log_likelihood_ratio >= -math.log(alpha)


class ConfusionMatrix(NamedTuple):
    """A 2x2 confusion matrix for a binary judge verdict vs. a binary
    human/ground-truth label. `true_positive`/`false_positive` treat the
    judge's `True` (PASS) verdict as the positive class."""

    true_positive: int
    false_positive: int
    true_negative: int
    false_negative: int


def confusion_matrix(
    judge_verdicts: list[bool], human_verdicts: list[bool]
) -> ConfusionMatrix:
    """Return the 2x2 confusion matrix for paired judge/human verdicts.

    Args:
        judge_verdicts: one bool per case, the judge's own PASS/FAIL
            verdict.
        human_verdicts: one bool per case, the ground-truth label for
            the same case, in the same order. Must be the same length
            as `judge_verdicts` and non-empty.

    Returns:
        A `ConfusionMatrix` whose four counts sum to `len(judge_verdicts)`.

    Raises:
        ValueError: if the two lists have different lengths, or both
            are empty.
    """
    if len(judge_verdicts) != len(human_verdicts):
        raise ValueError(
            "judge_verdicts and human_verdicts must be the same length, "
            f"got {len(judge_verdicts)} and {len(human_verdicts)}"
        )
    if len(judge_verdicts) == 0:
        raise ValueError("judge_verdicts/human_verdicts must be non-empty")

    tp = fp = tn = fn = 0
    for judge, human in zip(judge_verdicts, human_verdicts, strict=True):
        if judge and human:
            tp += 1
        elif judge and not human:
            fp += 1
        elif not judge and not human:
            tn += 1
        else:
            fn += 1
    return ConfusionMatrix(
        true_positive=tp, false_positive=fp, true_negative=tn, false_negative=fn
    )


def cohens_kappa(judge_verdicts: list[bool], human_verdicts: list[bool]) -> float:
    """Return Cohen's kappa — chance-corrected agreement between a
    judge's verdicts and human/ground-truth labels for the same cases.

    Raw exact-match agreement systematically overstates how good a
    judge actually is: a real 2026 study found exact-match exceeds
    kappa by a ~38.6 percentage-point mean across 21 judges on MT-Bench
    — any judge-accuracy metric built only on raw exact-match will look
    far better than it actually is. Kappa corrects for this by
    subtracting out the agreement rate two independent, chance-only
    raters would produce given each one's own marginal PASS rate:

        kappa = (p_o - p_e) / (1 - p_e)

    where `p_o` is the observed agreement rate ((TP + TN) / N) and
    `p_e` is the chance-expected agreement rate derived from the
    judge's and human's own independent PASS rates. This is
    deliberately NOT the same as Pearson/Spearman/Kendall-tau_b/phi/
    Matthews correlation coefficient — those five are mathematically
    identical to each other for binary verdicts, but kappa is a
    genuinely different, chance-discounted statistic, most different
    from them exactly when the judge's and human's marginal PASS rates
    diverge.

    Args:
        judge_verdicts: one bool per case, the judge's own PASS/FAIL
            verdict.
        human_verdicts: one bool per case, the ground-truth label for
            the same case, in the same order.

    Returns:
        Cohen's kappa, typically in [-1, 1] (1.0 = perfect agreement,
        0.0 = chance-level agreement, negative = worse than chance).

    Raises:
        ValueError: on the same length-mismatch/empty conditions as
            `confusion_matrix`, or if `p_e == 1.0` — both raters have
            zero marginal variance in the identical direction (e.g.
            every verdict on both sides is `True`), making kappa
            undefined (a literal 0/0), not silently `NaN` or `1.0`.
    """
    matrix = confusion_matrix(judge_verdicts, human_verdicts)
    n = float(len(judge_verdicts))

    p_observed = (matrix.true_positive + matrix.true_negative) / n
    judge_positive_rate = (matrix.true_positive + matrix.false_positive) / n
    human_positive_rate = (matrix.true_positive + matrix.false_negative) / n
    p_expected = judge_positive_rate * human_positive_rate + (
        1 - judge_positive_rate
    ) * (1 - human_positive_rate)

    if p_expected >= 1 - _EPSILON:
        raise ValueError(
            "cohens_kappa is undefined when p_expected == 1.0 "
            "(both raters have zero marginal variance in the same direction)"
        )

    return (p_observed - p_expected) / (1 - p_expected)
