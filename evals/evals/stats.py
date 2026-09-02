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
