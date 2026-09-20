"""Statistical helpers for eval scoring.

`PRD.md`'s success metric is explicit: never report a bare pass rate without
a confidence interval alongside it. The Wilson score interval (not a normal
approximation) is the right tool here because it stays well-behaved exactly
where a golden-tier eval set tends to live: small `n` and/or a proportion
near 0 or 1, both regimes where the normal approximation breaks down.
"""

from __future__ import annotations

import math
import random
from collections.abc import Sequence
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


def pass_at_k(n: int, c: int, k: int) -> float:
    """Return the unbiased pass@k estimate (Chen et al. 2021, the
    Codex/HumanEval paper) for one task: given `n` total samples with `c`
    of them correct, the probability that at least one of a random
    k-sized subset (drawn without replacement from the n samples) is
    correct.

    Computed via the numerically-stable running-product form the paper's
    own reference implementation uses — `1 - prod_{i=n-c+1}^{n} (1 -
    k/i)` — never the raw binomial-coefficient ratio `C(n-c,k)/C(n,k)`
    directly, which overflows/underflows for large `n`. To report the
    real pass@k metric across a suite, call this once per task and
    average the results — this function is deliberately single-task,
    mirroring `wilson_interval`'s own single-proportion scope.

    Args:
        n: total number of samples generated for this task. Must be > 0.
        c: number of those samples that passed (0 <= c <= n).
        k: the k in pass@k. Must satisfy 1 <= k <= n.

    Returns:
        The unbiased pass@k estimate for this task, in [0, 1]. Returns
        exactly 1.0 when there are fewer than `k` failing samples (`n -
        c < k`) — at least one of any k-sized subset must then be
        correct, by pigeonhole, without needing the product formula at
        all.

    Raises:
        ValueError: if `n` is not positive, `c` is out of `[0, n]`, or
            `k` is out of `[1, n]`.
    """
    if n <= 0:
        raise ValueError("n must be > 0")
    if not 0 <= c <= n:
        raise ValueError("c must satisfy 0 <= c <= n")
    if not 1 <= k <= n:
        raise ValueError("k must satisfy 1 <= k <= n")

    if n - c < k:
        return 1.0
    return 1.0 - math.prod(1.0 - k / i for i in range(n - c + 1, n + 1))


def bootstrap_paired_pvalue(
    baseline: Sequence[float],
    candidate: Sequence[float],
    *,
    n_resamples: int = 10_000,
    rng: random.Random | None = None,
) -> float:
    """One-sided paired-bootstrap significance test (Berg-Kirkpatrick,
    Burkett & Klein, EMNLP 2012) for whether `candidate` is genuinely
    better than `baseline` on the SAME set of paired cases (e.g. per-case
    pass/fail scores from two judge providers, or two rounds, on
    identical cases).

    A naive test that just counts how many resampled deltas fall below
    zero is subtly wrong for a paired comparison: the bootstrap resample
    distribution of delta is centered on the OBSERVED delta, not zero, so
    a plain "count below zero" test is only valid under conditions (a
    linear-decomposing metric and a symmetric bootstrap distribution,
    which the central limit theorem only guarantees for large samples) a
    small eval corpus won't reliably satisfy. This test instead
    re-centers correctly: it counts how often a resampled delta exceeds
    TWICE the observed delta — exactly as extreme, in the null
    (delta=0) frame, as falling below zero would be.

    Args:
        baseline: per-case scores for the baseline arm (e.g. 1.0/0.0 per
            case, or any real-valued per-case score).
        candidate: per-case scores for the candidate arm — SAME length
            and SAME case order as `baseline`; this is a PAIRED test.
        n_resamples: number of bootstrap resamples to draw.
        rng: injectable for deterministic tests; defaults to a fresh
            `random.Random()`.

    Returns:
        A one-sided p-value: the probability, under the null hypothesis
        that baseline and candidate are equivalent, of observing a delta
        at least as extreme as the one actually observed. Small values
        (e.g. < 0.05) support "candidate is genuinely better than
        baseline." If the observed delta itself favors baseline (is <=
        0), this correctly returns a p-value near 1.0 — there is no
        evidence for the "candidate is better" direction this test
        checks.

    Raises:
        ValueError: if `baseline`/`candidate` have different lengths or
            are empty, or `n_resamples` is not positive.
    """
    if len(baseline) != len(candidate):
        raise ValueError(
            "baseline and candidate must be the same length (paired), "
            f"got {len(baseline)} and {len(candidate)}"
        )
    if len(baseline) == 0:
        raise ValueError("baseline/candidate must be non-empty")
    if n_resamples <= 0:
        raise ValueError("n_resamples must be > 0")

    if rng is None:
        rng = random.Random()  # noqa: S311 -- Monte Carlo simulation, never cryptographic material

    n = len(baseline)
    observed_delta = (sum(candidate) - sum(baseline)) / n
    indices = range(n)

    exceed_count = 0
    for _ in range(n_resamples):
        sample = rng.choices(indices, k=n)
        resampled_delta = sum(candidate[i] - baseline[i] for i in sample) / n
        if resampled_delta > 2 * observed_delta:
            exceed_count += 1

    return exceed_count / n_resamples


def beta_binomial_prob_a_beats_b(
    successes_a: int,
    trials_a: int,
    successes_b: int,
    trials_b: int,
    *,
    prior_alpha: float = 1.0,
    prior_beta: float = 1.0,
    n_samples: int = 100_000,
    rng: random.Random | None = None,
) -> float:
    """Monte Carlo estimate of P(A's true pass rate > B's true pass rate)
    under a Beta-Binomial Bayesian model — for comparing two eval pass
    rates (e.g. two judge providers, or two rounds' corpus results).

    Each arm's posterior is `Beta(prior_alpha + successes, prior_beta +
    trials - successes)` — the standard Beta-Binomial conjugate update.
    `random.betavariate` samples this posterior directly; no
    `scipy.stats.beta` is needed. P(A beats B) is estimated as the
    fraction of paired posterior-sample draws where A's draw exceeds B's.

    Args:
        successes_a: A's observed successes (0 <= successes_a <=
            trials_a).
        trials_a: A's observed trial count. Must be > 0.
        successes_b: B's observed successes (0 <= successes_b <=
            trials_b).
        trials_b: B's observed trial count. Must be > 0.
        prior_alpha: the shared Beta prior's alpha shape parameter.
            Default 1.0 (paired with the default `prior_beta` below)
            gives the uniform prior over [0, 1] — a real, common,
            uninformative default, not an arbitrary placeholder.
        prior_beta: the shared Beta prior's beta shape parameter.
            Default 1.0.
        n_samples: number of Monte Carlo posterior-pair draws.
        rng: injectable for deterministic tests; defaults to a fresh
            `random.Random()`.

    Returns:
        Estimated P(A beats B), in [0, 1].

    Raises:
        ValueError: if either trial count is non-positive, either
            successes count is out of range, or either prior parameter
            is <= 0.
    """
    if trials_a <= 0 or trials_b <= 0:
        raise ValueError("trials_a and trials_b must both be > 0")
    if not 0 <= successes_a <= trials_a:
        raise ValueError("successes_a must satisfy 0 <= successes_a <= trials_a")
    if not 0 <= successes_b <= trials_b:
        raise ValueError("successes_b must satisfy 0 <= successes_b <= trials_b")
    if prior_alpha <= 0 or prior_beta <= 0:
        raise ValueError("prior_alpha and prior_beta must both be > 0")

    if rng is None:
        rng = random.Random()  # noqa: S311 -- Monte Carlo simulation, never cryptographic material

    alpha_a = prior_alpha + successes_a
    beta_a = prior_beta + (trials_a - successes_a)
    alpha_b = prior_alpha + successes_b
    beta_b = prior_beta + (trials_b - successes_b)

    a_wins = 0
    for _ in range(n_samples):
        if rng.betavariate(alpha_a, beta_a) > rng.betavariate(alpha_b, beta_b):
            a_wins += 1

    return a_wins / n_samples
