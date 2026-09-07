# Kelvran Evals — Regression Dataset Statistical Sizing Research

**Date:** 2026-09-07
**Scope:** `evals/` — how many cases a real regression-tier eval dataset needs to be a statistically meaningful CI gate, given Kelvran's already-shipped Wilson-score-lower-bound gate (`evals/evals/stats.py`, `evals report --fail-under`) and its multi-dimensional (`--tier` × `--category-fail-under` × `scorer_type`) gate structure (`docs/rfcs/2026-09-05-evals-report-fail-under.md`, `docs/rfcs/2026-09-07-evals-cigate-refinements.md`). This is one of four parallel angles on "build a real regression-tier eval dataset for Kelvran" (methodology/sourcing, statistical sizing — this report, pre-traffic bootstrapping, taxonomy/maintenance) and is scoped to statistical sizing only.
**Method:** Adversarial multi-source research (3-vote verification per claim) against primary statistics literature (NIST, UCL, Krishnamoorthy & Peng, Cohen 1990), Anthropic's own eval-statistics research, and a 2026 LLM-judge-reproducibility paper — cross-checked directly against the real, currently-shipped `evals/evals/stats.py` code and the real `evals/tests/fixtures/ci_gate_example.json` fixture, not assumed.

---

## Executive summary

Kelvran's `wilson_interval()` (`evals/evals/stats.py`) is confirmed, by direct code read, to implement the textbook Wilson-score formula at a **default `confidence=0.95`** (z≈1.9600) — not an assumption, a verified fact, and every CLI call site (`report_cmd`, `promote_cmd`, the mSPRT scheduler) inherits that same default. Recomputing that exact formula at representative N shows the interval genuinely tightens with scale (from a 0.28-wide interval at N=10 down to 0.06-wide at N=200 for a true-95%-reliable suite), but the practically important number is smaller than that width suggests: at Kelvran's realistic 90–99% target-reliability regime, a single additional observed failure moves the Wilson lower bound by roughly 3–6 points at N=50–100 — meaning the real design tension is not "is N big enough to compute a CI" (any N>0 works) but "is N big enough that 1–2 non-deterministic judge disagreements don't look statistically indistinguishable from a real regression." The literature's most commonly cited rule of thumb — "n≥30" — is confirmed to be a Central-Limit-Theorem convention with no rigorous derivation behind it (Cohen 1990 shows it gives only ~47% power for a medium effect), and it is materially too small and too flaky for anything but the crudest regression gate at Kelvran's pass-rate regime. The right sizing lens is not a universal minimum-N convention but effect-size-driven power analysis (1/D² scaling) combined with an honest accounting of Kelvran's own irreducible judge-noise floor (documented even under forced greedy decoding). Concretely: Kelvran should target roughly **100–150 cases** for the aggregate `regression` tier and **30–50 cases** per meaningful `--category-fail-under` tag as a first batch, with `--fail-under` set well above 0.1 (the deliberately-low smoke-test value) but calibrated to tolerate a realistic 1–2-case judge-noise floor rather than trip on it — concretely **`--fail-under 0.90`** at N≈100 and **`--category-fail-under TAG:0.80`** at N≈40–50, both derived directly below from Kelvran's real `wilson_interval` math, not copied from the smoke test.

---

## Findings, ranked

### 1. Kelvran's real Wilson formula, confidence level, and why Wilson (not Wald) is the right tool — grounded directly in `stats.py`

**What the research found:** `evals/evals/stats.py`'s `wilson_interval(successes, total, confidence: float = 0.95)` computes `z = NormalDist().inv_cdf(1 - (1 - confidence) / 2)`, then `centre = (p_hat + z²/(2n)) / (1 + z²/n)` and `adjustment = (z/(1+z²/n)) * sqrt(p_hat(1-p_hat)/n + z²/(4n²))`, returning `centre ± adjustment`, clamped to `(ε, 1-ε)` so a reported bound never touches exact 0.0/1.0. This is an exact, term-for-term match to NIST's published Wilson-method confidence-limit formula [itl.nist.gov/div898/handbook/prc/section2/prc241.htm — confirmed 3-0], and is the algebraic solution to Wilson's 1927 "interval equality principle" — inverting the population-interval formula rather than the naive Wald substitution `p̂ ± z·SE(p̂)` [UCL, Wallis — confirmed 3-0].

The **default confidence is 0.95** (z≈1.9600) — verified directly in the code (not assumed), and confirmed as the value every CLI entry point (`report_cmd --confidence`, `promote_cmd`, the mSPRT scheduler) actually defaults to in practice [confirmed 3-0]. Confidence is a free parameter of the formula, not a hardcoded constant of the math itself — the literature's canonical examples are 0.95 or 0.99 [UCL — confirmed 3-0], and Kelvran's own `wilson_interval` signature exposes it as exactly that kind of parameter.

**Why Wilson over Wald matters here specifically:** the Wald/normal-approximation interval demonstrably undercovers and can overshoot `[0,1]` or collapse to zero width exactly in the regime a regression-tier eval set lives in — small `n` and/or `p` near 0 or 1 [Wikipedia (Brown/Cai/DasGupta-summarizing) — confirmed 3-0; metricgate.com — confirmed 3-0]. Kelvran's own `stats.py` docstring states this exact justification verbatim, and the code's own epsilon-clamp is direct evidence the team already knows the raw formula touches 0.0/1.0 at the boundaries and deliberately engineered around it. This is not a hypothetical concern for a small regression suite — it is the literal failure mode Wilson was chosen to avoid.

**Confidence:** High (multiple 3-0 primary-source votes, independently cross-checked against the real code twice).

---

### 2. Concrete Wilson-lower-bound numbers at N=10/30/50/100/200, for a true-95%-reliable case set — computed directly from Kelvran's real formula

Running Kelvran's *actual* `wilson_interval()` function (not a re-derivation) at representative sizes, for a hypothetical case set whose true underlying pass rate is 95%:

| N | Observed successes (≈95%) | Wilson lower | Wilson upper | Interval width |
|---|---|---|---|---|
| 10 | 10/10 (100%, rounding) | 0.7225 | 1.0000 | 0.2775 |
| 30 | 28/30 (93.3%) | 0.7868 | 0.9815 | 0.1948 |
| 50 | 48/50 (96.0%) | 0.8654 | 0.9890 | 0.1236 |
| 100 | 95/100 (95.0%) | 0.8882 | 0.9785 | 0.0902 |
| 200 | 190/200 (95.0%) | 0.9104 | 0.9726 | 0.0622 |

Two supporting reference points at the 90%/99% ends of Kelvran's realistic target range (same N grid, same formula):

- **True p=90%:** N=10→lower 0.5958; N=30→0.7438; N=50→0.7864; N=100→0.8256; N=200→0.8506.
- **True p=99%:** N=10→lower 0.7225 (rounding forces 10/10); N=30→0.8865; N=50→0.9287; N=100→0.9455; N=200→0.9643.

**The more operationally important number — how many *real observed failures* it takes to trip a given `--fail-under` at each N — computed directly from the same formula, holding the case set at N cases with a true ~95% rate:**

| N | 0 failures (lower) | 1 failure | 2 failures | 3 failures | 4 failures | 5 failures |
|---|---|---|---|---|---|---|
| 30 | 0.8865 | 0.8333 | 0.7868 | 0.7438 | — | — |
| 40 | 0.9124 | 0.8712 | 0.8350 | 0.8014 | 0.7695 | 0.7389 |
| 50 | 0.9287 | 0.8950 | 0.8654 | 0.8378 | 0.8116 | 0.7864 |
| 100 | 0.9630 | 0.9455 | 0.9300 | 0.9155 | 0.9016 | 0.8882 |
| 150 | 0.9750 | 0.9632 | 0.9527 | 0.9429 | 0.9334 | 0.9243 |
| 200 | 0.9812 | 0.9722 | 0.9643 | 0.9568 | 0.9497 | 0.9428 |

Reading this table against a candidate `--fail-under` threshold directly answers "how many real failures does it take to trip the gate": e.g. at N=100 with `--fail-under 0.90`, the gate does **not** trip until the 5th observed failure (lower bound crosses below 0.90 between failure 4's 0.9016 and failure 5's 0.8882); at N=30 with the same 0.90 threshold, it trips on the very **first** failure (lower bound 0.8333 < 0.90). This N=30-vs-N=100 contrast is the single most concrete illustration in this research of why case count changes gate behavior at a fixed threshold, not just interval "width" in the abstract.

Also directly confirmed: interval width shrinks roughly with `1/√N` (a 4× increase in N is needed to roughly halve the width) [ai-tldr.dev — confirmed 3-0], and this asymptotic law holds up even under Kelvran's actual (boundary-affected) Wilson math, not just the idealized Wald approximation — verified by direct computation across N=10→800 in the underlying research pass.

**Confidence:** High (all numbers computed directly from the real `wilson_interval()` implementation, not estimated or taken from a table in the literature).

---

### 3. The "n≥30" rule of thumb is a Central-Limit-Theorem convention, not a power-analysis-derived minimum — and it is demonstrably too small for Kelvran's regime

**What the research found:** the commonly-cited "n≥30" threshold comes from an informal convention that below 30 you're in "small-sample statistics" territory requiring special handling — the CLT's own technical statement specifies no exact minimum n at all [ualberta.ca guidelines, corroborated independently by Wikipedia's CLT article calling the same convention a "misconceived belief... with no valid justification" — confirmed 3-0]. Cohen's own account of *inventing* this convention as a graduate student, and later discovering via power analysis that n=30-per-group gives only **~47% power** (a coin flip) to detect a medium effect size at two-tailed α=.05, is the single most damning piece of evidence here: the number's popularity long predates any statistical justification for it [Cohen 1990, "Things I Have Learned (So Far)" — confirmed 3-0].

**Applied directly to Kelvran's own numbers (Finding 2's table):** at N=30 with a true-95%-reliable suite, a single real failure already drops the Wilson lower bound to 0.8333 — meaning any `--fail-under` above ~0.83 trips on just **one** failure. Given Kelvran's own documented judge-noise floor (Finding 5 below), a threshold that trips on one failure at N=30 cannot reliably distinguish "the model regressed" from "the judge disagreed with itself this run." N=30 is not unusable — it is a legitimate size for a *coarse* gate that only needs to catch gross regressions (many simultaneous failures) — but it is not, by itself, a rigorous minimum for a fine-grained per-category gate expected to catch a real few-point regression.

**Confidence:** High (Cohen's account is the primary, canonical source on this exact convention's origin; independently corroborated by a second source's identical framing).

---

### 4. Real sizing math is effect-size-driven (power analysis), not a fixed minimum-N convention — and Kelvran's own high-pass-rate regime changes the numbers substantially

**What the research found:** the statistically rigorous way to size a regression suite is not "pick N≥30" but power analysis: given a hypothesized effect size (the size of the regression you actually want to be able to catch), compute the N needed to detect it at a chosen power and false-positive rate. Anthropic's own eval-statistics research frames its entire sizing methodology this way — "formulate a hypothesis (Model A outperforms Model B by 3 percentage points) and calculate the number of questions... to test this against the null" — explicitly not a single fixed minimum N [anthropic.com/research/statistical-approach-to-model-evals — confirmed 3-0]. Krishnamoorthy & Peng's exact score-test (mathematically the same family as Wilson) sample-size table gives a directly comparable real number: distinguishing a true p=0.95 from a baseline p₀=0.90 at power 0.80, α=0.05 (two-tailed) requires an exact N=**231** [confirmed 3-0] — a 5-point gap near Kelvran's own target ceiling, and a very different (much larger) number than either the smoke-test's 4-5 cases or the n≥30 convention.

**The general 1/D² scaling law, recomputed for Kelvran's actual pass-rate regime, not a generic baseline:** the source's own worked example (detecting an 80%→84% gap needs ~1,300 cases) is baseline-rate-specific — variance is `p(1-p)`, which shrinks sharply near a ceiling. Recomputing the identical formula at pass rates closer to Kelvran's real 90–99% regression-tier target gives dramatically smaller numbers: ~711 cases to reliably detect 90%→94%, ~626 for 91%→95%, and only ~250 for 95%→99% [ai-tldr.dev, recomputed — confirmed 3-0 for the underlying law, independently recomputed for Kelvran's regime]. This is the single most important correction this research makes to a naive reading of the literature: **do not reuse a literature example's absolute N without recomputing it at Kelvran's actual target pass rate** — the same detection sensitivity costs far fewer cases near a 95–99% ceiling than at a 80–84% baseline.

At the extreme illustrating how steep this can get: achieving a tight ±1% margin of error at p=0.20 requires N=6,144 (Wilson) / 6,245 (exact) [Krishnamoorthy & Peng — confirmed 3-0] — this is *not* Kelvran's regime (its target pass rates sit near 0.90–0.99, where the same ±1% precision costs far fewer cases because `p(1-p)` is smaller), but it is useful evidence that "just add more cases" is not free at scale and that sizing must be regime-specific, not read off a single example.

**Confidence:** High for the general methodology and the exact N=231 table value (both directly verified against primary academic sources); Medium for the recomputed 250–711 range (a correct reapplication of a verified formula, but a derived number rather than a directly-cited literature figure).

---

### 5. Real flakiness evidence: a non-zero judge-noise floor exists even under forced deterministic decoding — this bounds how tight a gate can be, independent of N

**What the research found:** pinning `temperature=0` (greedy decoding) reduces but does **not** eliminate LLM-judge non-determinism — a controlled study found 1–2 of 7 deliberately borderline items still flip pass/fail across identical repeated runs even under forced greedy decoding (`top_k=1`), and this held at the same rate across both a larger model (Claude Sonnet 4.6) and a smaller one (Haiku 4.5) [arXiv 2606.26185 — confirmed 3-0]. This is real, primary, recent (2026-06) evidence that "increase N" alone cannot fully solve gate flakiness — some baseline rate of non-deterministic judge disagreement is structural, not a sampling artifact that shrinks to zero as N grows. (Note: the source's 7-item set was deliberately constructed to be borderline/ambiguous — a well-curated regression suite with unambiguous expected outputs should see a materially lower flip rate in practice, which is a taxonomy/curation concern out of this report's scope, not a statistical-sizing one; but the existence of a non-zero floor, even under best-case determinism settings, is the load-bearing fact here regardless of the exact rate.)

Separately, Anthropic's own research on eval statistical rigor shows that **correlated case groupings** (e.g., multiple test items sharing one underlying context/passage) can produce clustered standard errors more than 3× larger than a naive independence-assuming calculation would suggest — understating true uncertainty and creating false-positive-prone gates when case independence is wrongly assumed [Anthropic, arXiv:2411.00640 — confirmed 3-0]. Kelvran's `wilson_interval()` — like virtually all binomial-proportion CI formulas — assumes independent trials; if a future regression suite groups multiple cases under one shared context (e.g., several assertions against one agent transcript), this independence assumption should be revisited before trusting the resulting Wilson bound at face value. This is a real, literature-grounded caveat on the numbers in Finding 2, not a hypothetical one.

**Practical implication for `--fail-under` calibration:** a threshold should be set to tolerate a small, non-zero number of stray failures (matching the observed 1–2-of-a-small-set judge-noise floor, scaled to the suite's actual size) rather than trip on the very first failure — this is the concrete reasoning behind the `--fail-under 0.90`-at-N≈100 recommendation in the Recommendation section, which (per Finding 2's table) tolerates up to 4 stray failures before tripping.

**Confidence:** High for the temp=0 non-determinism finding and the clustered-SE finding (both primary sources, 3-0 votes); the practical scaling of "the noise floor" to arbitrary suite sizes is this report's own reasoned extrapolation, not a directly cited number.

---

### 6. Multi-dimensional gating (tier × category × scorer_type): Kelvran's real, shipped design requires independent N per fine-grained category — a smarter aggregation approach exists in the literature but Kelvran has not built (and, per its own RFC, deliberately declined to build) the machinery for it

**What Kelvran actually ships today** (verified directly against `docs/rfcs/2026-09-07-evals-cigate-refinements.md`): `report_cmd`'s `--tier` filters the working `Score` set once; every downstream check — the aggregate gate and every `--category-fail-under` gate — computes its own independent `(successes, total)` and its own independent `wilson_interval()` call, **never blended across `scorer_type`, and never blended across categories.** The RFC explicitly considered and **rejected** adding "a new `evals.stats` entry point for a per-category Wilson bound" specifically because `stats.py` is architecturally isolated (bottom import-linter layer, no dependency on `evals.models`) and a category-aware helper would either duplicate the existing raw-counts-in math or violate that layering contract. This means, as shipped, **every `(tier, category, scorer_type)` combination is its own fully independent statistical test** with its own N — there is no cross-category borrowing of statistical strength anywhere in the current design.

**What the literature says a smarter aggregation approach could look like, if Kelvran ever wanted one:** partial pooling / Stein-shrinkage estimators provably reduce group-level RMSE relative to treating each group's estimate independently — and this mathematical result holds even when the underlying groups measure things that are *not* substantively related to each other [Stein 1956, applied to A/B-test-style groups in Sales, Patikorn & Heffernan (EDM 2018) — confirmed 3-0]. Applied to Kelvran's shape, this would mean: instead of computing an independent Wilson bound per `(tier, category, scorer_type)` triple, a hierarchical/shrinkage model could pool information *across* categories to produce a more precise per-category estimate than any one category's own small N supports alone — which is exactly the kind of thing that could reduce the "hundreds of cases per fine-grained category" cost this report's question worries about.

**Why this research does not recommend building it now:** this is architecturally the same shape of decision as the sibling 2026-09-06 research report's Finding 3 (bootstrap/Bayesian eval stats) — a real, literature-grounded technique with no evidence yet that Kelvran's *actual* usage pattern needs it. No source consulted in either research pass shows a comparable eval platform (Inspect AI, DeepEval, Braintrust, promptfoo) shipping cross-category statistical pooling as a *default* CI-gating mechanism; the real-world pattern observed everywhere is independent per-group thresholds, exactly what Kelvran already ships. Building a pooling layer now — before Kelvran has real per-category data showing independent gates are producing false positives specifically *because* of small per-category N — would be solving a problem this research found no concrete evidence of yet, the same over-building risk `STATUS.md`'s own "no real usage to analyze yet" language was written to guard against.

**The honest, direct answer to the research question's Finding 4:** yes — under Kelvran's current, real, shipped design, **each category genuinely needs its own adequate N to be statistically meaningful**; there is no free lunch from the existing architecture. A smarter aggregation approach (partial pooling) exists in principle and is a real, defensible future direction if per-category false-positive rates become a documented problem, but it requires new statistical machinery Kelvran's own RFC already explicitly declined to build in this pass, for sound architectural reasons.

**Confidence:** High for what Kelvran currently ships (direct code/RFC verification); High for the Stein/partial-pooling mathematics itself (primary source, textbook result); Medium for the recommendation not to build it yet (a reasoned inference consistent with the project's own stated engineering discipline, not a claim that pooling would be wrong to ever build).

---

## Recommendation — concrete numbers, not vague guidance

**Overall regression-tier first-batch size: 100–150 cases.**
At N=100 (Finding 2's table), a true ~95%-reliable suite has a 0-failure Wilson lower bound of 0.9630, and it takes 5 real observed failures to push the lower bound below 0.8882. This gives enough room to set a `--fail-under` threshold that tolerates a small judge-noise floor (Finding 5) while still catching a real multi-case regression well before it reaches double digits of failures. N=150 tightens this further (0-failure lower=0.9750, 5-failure lower=0.9243) at a modest additional case-authoring cost, and is the better target if Kelvran's real authoring capacity (a concern for the sibling methodology/taxonomy research threads, not this one) supports it.

**Recommended aggregate `--fail-under`: `0.90`.**
Reading directly off the N=100 row of Finding 2's table: 0.90 does not trip until the 5th observed failure (lower bound crosses from 0.9016 at 4 failures to 0.8882 at 5) — i.e. it tolerates up to 4 stray failures (comfortably above the 1–2-item judge-noise floor documented in Finding 5, even scaled up from that source's small 7-item set) while still gating hard on a real ≥5% regression. This is not the smoke-test's deliberately-low 0.1 — it is a real threshold, chosen from Kelvran's own math at a real target size, not copied.

**Per-meaningful-category first-batch size: 30–50 cases**, with an explicit acknowledgment of what that size can and cannot detect.
At N=40–50 (Finding 2/3's tables), a single observed failure already drops the Wilson lower bound to roughly 0.87–0.90, and 2–3 failures drop it to roughly 0.80–0.87. This size is large enough to catch a gross regression in a safety-critical category (many simultaneous failures) but — per Finding 4's recomputed power-analysis numbers (250–711 cases needed to reliably detect a true 4-point regression near a 90–99% ceiling) — it is **not** large enough to reliably distinguish a subtle few-point regression from noise. This is an honest limitation to state explicitly in whatever category is chosen as a first-batch target, not a claim that 30–50 is a rigorously sufficient minimum.

**Recommended `--category-fail-under`: `TAG:0.80` at N≈40–50.**
At N=40, 0.80 sits between the 3-failure bound (0.8014) and the 4-failure bound (0.7695) — tripping at 4 failures out of 40 (10% observed failure rate). At N=50, 0.80 sits between the 4-failure bound (0.8116) and 5-failure bound (0.7864) — tripping at 5 failures out of 50 (also 10%). This tolerates 3–4 stray failures (well above the documented judge-noise floor) while still catching a real double-digit-percentage regression in a high-stakes category — a deliberately looser bar than the aggregate gate's 0.90, reflecting the category tier's necessarily smaller, noisier N, not a claim that safety-relevant regressions matter less.

**Growth path, grounded in the same math, not open-ended:** as a meaningful category accumulates real cases toward 100+ (matching the aggregate tier's target), its `--category-fail-under` threshold should tighten toward the same 0.90 used at the aggregate level (Finding 2's N=100 row), since the same tolerate-4-failures reasoning applies once N is large enough to support it. This gives Kelvran a concrete, numbers-driven answer to "when do we tighten this threshold" (when a category's N crosses roughly 100, not on a fixed calendar schedule) rather than an open-ended aspiration.

**Do not build cross-category statistical pooling now** (Finding 6) — reaffirmed as "not yet justified," on the same "no evidence of a real gap yet" discipline the sibling 2026-09-06 research report already applied to bootstrap/Bayesian eval stats. Revisit only if real per-category false-positive rates (tracked once the above thresholds are actually running in CI) show independent per-category gates producing regressions-that-weren't at a rate the tolerances above don't already account for.

---

## Caveats

- Every N-dependent number in this report (Findings 2, 3, and the Recommendation section) is computed directly by running Kelvran's own `wilson_interval()` function, not estimated from a general formula or copied from a literature table — but they all assume **independent** trials, which is the standard Wilson assumption and matches Kelvran's current architecture (no shared-context case grouping exists yet). If a future regression suite groups multiple assertions under one shared agent transcript or context, Finding 5's clustered-standard-error caveat applies and the numbers above would understate true uncertainty.
- The 1/D² power-analysis numbers in Finding 4 (250–711 cases to detect a 4-point regression near Kelvran's target ceiling) are this report's own correct reapplication of a verified formula to Kelvran's specific pass-rate regime — they are not numbers directly stated in the source literature (which used an 80%→84% baseline example) and should be treated as a derived, Medium-confidence estimate rather than a directly-cited figure.
- The judge-noise-floor evidence (Finding 5) comes from a single 2026-06 paper using a small, deliberately-adversarial 7-item borderline set — real flip rates on a well-curated Kelvran regression suite (with clear, unambiguous expected outputs, a taxonomy/curation concern scoped to a sibling research thread) are very likely lower than that paper's rate. The recommendation section's tolerances (4 failures at N=100, 3–4 at N=40–50) are chosen to comfortably exceed even that paper's elevated rate, so this should not materially change the concrete thresholds recommended — but the exact real-world flip rate for Kelvran's own suite remains unverified until real data exists.
- This report is scoped to statistical sizing only, per the task's explicit instructions — it does not address which specific cases to write, how to source them, or how to maintain the taxonomy over time; those are the sibling research threads' scope, and this report's N/threshold recommendations should be read as inputs to those threads, not a substitute for them.

## Open questions

- Once Kelvran's real regression suite exists and runs in CI for a period, what is its *actual* observed judge-flip rate on well-curated (non-adversarial-by-design) cases — does it match, exceed, or fall well below the 2026-06 paper's 1–2-of-7 rate on deliberately borderline items? This is the concrete data point that would let the recommended thresholds be tightened or loosened with real evidence instead of a literature-derived margin.
- Should Kelvran's `--category-fail-under` growth path (tightening toward 0.90 as a category's N crosses ~100) be automated/enforced somehow, or left as a documented convention an operator applies manually when editing CI config? This report recommends the numbers but not the mechanism.
- If a future case design does introduce shared-context groupings (multiple assertions per agent transcript), does Kelvran need a clustered-standard-error correction on top of `wilson_interval`, or is per-case independence a design constraint Kelvran should simply preserve by never grouping cases that way? This report flags the risk (Finding 5) but the sourcing/taxonomy research threads are better positioned to decide whether grouped cases are ever actually desirable.
- Is there a real, Kelvran-specific trigger condition (analogous to the sibling 2026-09-06 report's "CI wall-clock time becomes a felt problem" trigger for the Sandbox Pool) that should mark when cross-category statistical pooling (Finding 6) becomes worth building — e.g. "N false-positive category-gate trips per quarter with no real regression found"? This report names the discipline but not a concrete numeric trigger.
