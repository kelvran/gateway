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


def two_checkpoint_early_stop(
    successes: int,
    trials_run: int,
    min_trials: int,
    max_trials: int,
    baseline_pass_rate: float,
    confidence: float = 0.95,
) -> bool:
    """Decide whether a group of repeated trials should stop now.

    Per docs/rfcs/2026-09-04-evals-rollout-cost-mitigation.md: this is
    deliberately NOT "recompute `wilson_interval` after every trial and
    stop at first exclusion of `baseline_pass_rate`" — that is the
    classical optional-stopping / peeking failure mode (Armitage,
    McPherson & Rowe 1969; Johari, Koomen, Pekelis & Walsh, KDD 2017),
    which inflates the true false-positive rate well above `1 - confidence`
    because a fixed-sample confidence interval like `wilson_interval` is
    only valid at one pre-committed sample size.

    Instead, this checks *only* at exactly two pre-declared checkpoints —
    `trials_run == min_trials` and `trials_run == max_trials` — and never
    on any other trial count, so a caller invoking this after every single
    trial still gets the correct, alpha-controlled two-checkpoint
    procedure without having to track which trial count is a real
    checkpoint itself. Bonferroni correction: the two checks split the
    total false-positive budget `1 - confidence` evenly between them (each
    checkpoint uses `confidence' = 1 - (1 - confidence) / 2`), so the
    *combined* false-positive probability across both checkpoints stays
    bounded by `1 - confidence` (union bound) — simple, stdlib-only, and
    provably valid, at the cost of being more conservative than a
    non-uniform group-sequential boundary (O'Brien-Fleming) would be. When
    `min_trials == max_trials` (a degenerate single checkpoint), no
    correction is applied, since only one check exists.

    Args:
        successes: successful trials so far in this group.
        trials_run: total trials so far in this group (>= successes).
        min_trials: the first checkpoint — no stop decision before this.
        max_trials: the second checkpoint — the group's hard ceiling.
        baseline_pass_rate: the pass rate this group's running rate is
            checked against. No default: this project has no production
            traffic yet to calibrate one, per the RFC's Unresolved
            Questions — the operator must always supply it explicitly.
        confidence: confidence level in (0, 1), passed straight through to
            `wilson_interval` (with the Bonferroni adjustment above).

    Returns:
        True only if `trials_run` is exactly `min_trials` or `max_trials`
        AND the (possibly Bonferroni-corrected) Wilson interval at that
        trial count excludes `baseline_pass_rate`. False otherwise,
        including for every trial count in between the two checkpoints.
    """
    if trials_run == 0 or trials_run not in (min_trials, max_trials):
        return False

    checkpoint_confidence = (
        confidence if min_trials == max_trials else 1 - (1 - confidence) / 2
    )
    lower, upper = wilson_interval(
        successes, trials_run, confidence=checkpoint_confidence
    )
    return not (lower <= baseline_pass_rate <= upper)
