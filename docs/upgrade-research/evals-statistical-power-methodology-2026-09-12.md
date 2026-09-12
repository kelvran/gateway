# Research: Statistical power methodology beyond Wilson-interval-only confidence bounds

**Date:** 2026-09-12
**Scope:** Whether Kelvran's evals system should adopt bootstrap resampling, Bayesian credible intervals, or pass@k-style methodology beyond its current Wilson-interval-only CI/CD pass-rate gating (`--fail-under`, `--category-fail-under`) and mSPRT early-stopping, or whether "defer numpy/scipy, hand-roll `evals/evals/stats.py`" remains the correct interim call. Already shipped and not re-litigated: Wilson-interval gating itself, mSPRT early-stopping, the LLM-judge-panel majority-vote/tie-handling design, score-caching.

**Note on recovery:** this report's underlying research (109 agents, 26 sources fetched, 115 claims extracted, 25 adversarially verified) completed successfully, but the workflow's own file-write step failed silently on first attempt. This file was reconstructed directly from the workflow's full structured JSON result (verified findings, refuted claims, sources, caveats) with no synthesis or content added — nothing here was regenerated or re-derived.

## Executive Summary

Across all four research questions, the evidence supports staying with the current interim rather than adopting numpy/scipy-based bootstrap, Bayesian, or pass@k machinery now. The binomial-CI literature (an 11-method interval-score comparison, plus an independent simulation study) confirms Wilson is already the field's recommended interval — not a naive placeholder that bootstrap or Bayesian credible intervals meaningfully improve on at small-n, so the premise that richer CI methods are "genuinely more informative" at Kelvran's scale is not supported by anything found. pass@k is confirmed to be a structural category mismatch: it is an n≥k repeated-sampling estimator over binary unit-test outcomes modeling iterative code-generation attempts, fundamentally incompatible with Kelvran's single-trial, judge/deterministic-scored eval shape. The numpy/scipy dependency cost is real but modest and avoidable via an optional-extras pattern with working precedent, though no source directly measured numpy/scipy's cost/benefit for statistics use cases specifically. Finally, adjacent risk-tiered methodology literature explicitly argues statistical sophistication should be adopted only when a named risk demands it, not by default — which, combined with the finding that a young PPI-correction tool's core validity precondition (uniform-random labeling) appears to be violated by Kelvran's own judge-accuracy corpus construction, reinforces that the "deliberate interim" remains the right call today. One notable gap: no confirmed evidence establishes what Inspect AI, promptfoo, DeepEval, or OpenAI Evals concretely do for statistical power beyond a CI gate — that specific sub-question remains unresolved.

## Findings

### Finding 1 — Wilson-interval gating is the literature's own recommended method, not a deficiency to fix

**Confidence: high.** Sources: [Hofer & Held 2022](https://doi.org/10.48550/arxiv.2207.03199), [Orawo 2021](https://doi.org/10.4236/ojs.2021.115047).

Wilson-interval gating is not a naive or inferior choice — it is the literature's own recommended (or co-recommended) method among 11+ competing binomial-CI constructions, confirmed by both a 2022 interval-score comparison paper and a separate 2021 simulation study, corroborated by decades of prior statistics literature (Wilcoxon/Brown-Cai-DasGupta-era consensus). Hofer & Held (2022) evaluate 11 CI methods via expected interval score and conclude verbatim that "the expected interval score recommends the Wilson CI or Bayesian credible intervals with a uniform prior" under standard weighting — i.e., Wilson ties with Bayesian credible intervals for best, not behind them. Orawo (2021), comparing Wald/Clopper-Pearson/Wilson/Likelihood via simulation plus a real clinical dataset, concludes verbatim: "the Wilson and Likelihood intervals are recommended to be used in practice," explicitly rejecting Clopper-Pearson (overly conservative) and Wald (unreliable coverage).

**not_yet** — no trigger exists to replace Wilson itself; it is already the methodologically preferred interval for Kelvran's use case.

### Finding 2 — Bootstrap CIs are only marginally more conservative than Wilson at Kelvran's scale

**Confidence: high.** Source: [Wang & Hutson](https://pmc.ncbi.nlm.nih.gov/articles/PMC4789773/).

At small sample sizes close to Kelvran's own scale, bootstrap confidence intervals are only marginally more conservative than Wilson (and Jeffreys) — not a substantive statistical upgrade. Wang & Hutson's own published table (n≤10 stratum, closest to Kelvran's 24-case judge-accuracy corpus) shows Boot-MUE coverage = 0.965 vs Wilson/Score = 0.956 — about a 1-point difference — and the paper's own text states bootstrap is "slightly more conservative," not dramatically more accurate. A related claim attempting to generalize this to "bootstrap offers no meaningful coverage improvement over Wilson" across all strata failed adversarial verification, so the honest scoped conclusion is: the gap is small specifically in the very-small-n regime, not necessarily zero everywhere.

**not_yet** — trigger would be a demonstrated case where Wilson's coverage gap at Kelvran's actual n causes a real gating error; no such case has occurred.

### Finding 3 — Small-sample judge-bias-correction tooling (PPI) exists but its validity precondition appears violated by Kelvran's own corpus construction

**Confidence: high.** Sources: [evalconfidence](https://pypi.org/project/evalconfidence/), [evalstats](https://github.com/ianarawjo/evalstats/).

Two young, niche statistical packages built specifically for small-sample LLM-eval settings exist: evalconfidence (numpy+scipy core only) and evalstats (valid down to 15 samples, includes Prediction-Powered Inference/PPI judge-bias correction). But PPI's core validity precondition — human labels must be sampled uniformly at random — is violated by design if labels are drawn from targeted/borderline cases, which is how Kelvran's 24-case judge-accuracy corpus appears to be constructed (inferred from other in-session agent context, not directly verified against the real corpus — flagged as an open question below). The maintainers' own simulations show this leaves PPI badly miscalibrated regardless of sample size, persisting from 15 up to 300-400 labeled items under "missing-not-at-random" selection such as always double-checking borderline cases. Both packages are single-author, 2026-vintage, with no independent peer review — weak evidence for "this is how the field does it," but solid evidence for the specific technical precondition.

**not_yet** — trigger: only becomes viable if Kelvran restructures judge-accuracy labeling to include a uniformly-random-sampled subset (not purely boundary-curated); otherwise PPI/bootstrap-judge-correction tooling would give a false sense of statistical rigor regardless of corpus growth.

### Finding 4 — pass@k is a structural category mismatch for Kelvran's eval shape

**Confidence: high.** Sources: [Chen et al. 2021 (HumanEval/Codex)](https://arxiv.org/pdf/2107.03374), ["Don't Pass@k" (Oct 2025)](https://arxiv.org/pdf/2510.04265).

pass@k is defined as an unbiased estimator requiring n≥k independently generated samples per task with a binary (unit-test) pass/fail signal, modeling repeated/iterative generation attempts — structurally distinct from Kelvran's single deterministic-or-judge-scored trial per case. Chen et al. 2021 (the paper that defined pass@k) requires generating n≥k samples per task, counts binary unit-test pass/fail, and computes the probability at least one of a random k-subset passes — explicitly modeling "iterations of approaches and bug fixes" via repeated sampling. OpenAI's own reference implementation confirms the estimator is undefined/degenerate for n<k, i.e., collapses to plain accuracy at n=1 (Kelvran's actual trial count per case). The one located Bayesian extension of pass@k ("Don't Pass@k," Oct 2025) is likewise validated only on k-sampled math-benchmark data, never on single-trial/judge-scored data, despite claiming theoretical extensibility to graded rubrics.

**not_yet** — trigger: Kelvran would need to add a genuinely k-sampled repeated-generation eval mode (e.g., an agent attempting a task multiple independent times) as a new eval unit before pass@k-style estimators have any natural target; it does not apply to gating single judge-scored/deterministic trials regardless of corpus growth.

### Finding 5 — numpy/scipy's dependency cost is real but bounded and avoidable via an optional-extras pattern

**Confidence: medium.** Sources: [evalconfidence](https://pypi.org/project/evalconfidence/), [humpday PR #60](https://github.com/microprediction/humpday/pull/60), [zerodep paper](https://arxiv.org/html/2605.21405).

Adding numpy/scipy carries a real, non-trivial, but bounded and avoidable dependency-weight cost (comparable projects have measured ~33MB for numpy alone), achievable as an optional/extras-gated dependency rather than a hard requirement — but no located source directly measures numpy/scipy's cost/benefit specifically for statistical-computation use cases like Kelvran's. evalconfidence demonstrates a real project shipping with numpy+scipy as its ONLY required core dependency (no scikit-learn/pandas needed). humpday's merged PR quantifies numpy's cost concretely at ~33MB and shows it is removable/optional via an extras mechanism without losing functionality. A related claim that this decoupling required an "11-PR refactor effort" did NOT survive verification, so the true engineering cost of numpy-optionality is not well-established either way. Separately, the zerodep paper explicitly scopes NumPy/SciPy-class compute frameworks OUT of its stdlib-vs-third-party study, so it provides no direct evidence on Kelvran's actual question.

**not_yet** — trigger: if/when Kelvran needs a specific numpy/scipy-only capability (e.g., paired significance testing across model versions or hierarchical pooling that `stats.py` cannot express), it could be added as an optional extra following the evalconfidence/humpday pattern, without imposing the dependency on users who don't need it. There is no cost-based reason to add it purely speculatively.

### Finding 6 — Statistical sophistication should be adopted when a named risk demands it, not by default

**Confidence: high.** Source: [Schultzberg & Frånberg, Spotify (Aug 2026)](https://arxiv.org/html/2608.12949v1).

The governing principle from adjacent statistical-methodology literature is that method sophistication (Bayesian priors, tiered calibration, bootstrap, etc.) should be chosen based on the specific risk an evaluation/experimentation program needs to control, not adopted by default ahead of a concrete need. Schultzberg & Frånberg (Spotify, Aug 2026) state explicitly: "the appropriate method follows from the risks an experimentation program needs to control, not the other way around," building a risk-tiered hierarchy (posterior coherence → FPR control → FDR calibration) and explicitly warning that under-corpus-scale programs should avoid underfitted empirical-Bayes priors in favor of simpler corrections. This is the closest confirmed source to a general adoption-timing rule, though scoped to online A/B testing rather than offline eval gating (an analogical, not literal, application).

**build_now only if a concrete, named risk materializes** — e.g., a real false-positive gate failure blocks good code, a real false-negative gate passes a broken regression, or the corpus grows large enough that Wilson's small-sample conservatism visibly causes borderline pass/fail flips under `--fail-under`/`--category-fail-under` in practice. Absent any such event, **not_yet** is the correct call — consistent with, and reinforcing, the project's prior "deliberate interim" decision.

## Caveats

- The research questions specifically asked what Inspect AI, promptfoo, DeepEval, and OpenAI Evals actually do for statistical power beyond a CI gate. No claim about any of these four platforms survived verification (the one attempt, characterizing evalconfidence's gap analysis of Inspect AI/DeepEval's existing bootstrap/CLT capabilities, was refuted 0-3). This research genuinely does NOT establish what the four named production platforms do internally; treat any claim about "what production platforms do" as unresolved, not confirmed.
- evalconfidence (PyPI, 2 releases, single author, first published June 2026) and evalstats (GitHub, single author, ~127 stars) are small/young projects whose self-reported capabilities (small-sample validity down to 15 samples, PPI correction) are simulation-backed self-claims, not independently peer-reviewed. The "Don't Pass@k" paper (arXiv 2510.04265) is an Oct-2025 preprint, also not peer-reviewed, and its own empirical validation is confined to k-sampled math-competition benchmarks — it never tests a single-trial/judge-scored setting.
- Multiple attempts to show either (a) Wilson is materially inferior to Bayesian/bootstrap alternatives, (b) bootstrap is meaningfully better than Wilson at small n, or (c) any interval is "universally superior" all failed verification — reinforcing, by absence, that no confirmed evidence in this research contradicts Kelvran's existing Wilson-only approach.
- The claim that Kelvran's own judge-accuracy corpus is boundary/edge-case-curated (relevant to Finding 3's PPI-validity point) is an inference from other agent session context visible during this research, not a direct read of Kelvran's actual `evals/` test data — it should be verified against the real corpus before treating the PPI-invalidity conclusion as settled.
- Fast-moving area (Sept 2026 research cutoff): both niche packages and the Bayesian pass@k paper are recent enough that ecosystem consensus may shift within months.

## Open Questions

- What do Inspect AI, promptfoo, DeepEval, and OpenAI Evals actually implement for statistical power/uncertainty beyond a simple CI gate? This remains genuinely unresolved and would need direct primary-source verification against each platform's own docs/changelog.
- Is Kelvran's 24-case judge-accuracy corpus actually constructed via non-uniform (boundary/edge-case-targeted) selection, or could/should it be restructured to include a uniformly-random-sampled subset — this determines whether PPI-style judge-bias correction could ever become valid for Kelvran, independent of corpus size.
- At what specific corpus size or gating-decision frequency would Wilson's small-sample conservatism start causing visible, costly gate friction (e.g., frequent borderline flips near `--fail-under` thresholds) for Kelvran specifically? No source quantified this threshold for Kelvran's own gate configuration.
- Does Kelvran's roadmap include (or should it include) a k-sampled repeated-generation eval mode (e.g., an agent making multiple independent attempts at a task) — the concrete precondition under which pass@k-style estimators would have any natural applicability, which they currently lack given Kelvran's single-trial judge/deterministic scoring.

## Refuted Claims (excluded from findings above)

- evalconfidence's gap analysis characterizing Inspect AI/DeepEval as already having CLT/bootstrap/clustered-SE support (0-3).
- evalstats' PPI mechanism framed as directly applicable to Kelvran's judge-accuracy-corpus-feeds-a-larger-set shape (0-3).
- Brown-Cai-DasGupta's n≤40 sample-size recommendation generalized to imply richer methods aren't needed at Kelvran's scale (0-3) — the underlying Wilson-recommendation conclusion stands (Finding 1), but this specific generalization did not survive verification independently.
- Wilson vs. Bayesian/Jeffreys framed as "essentially interchangeable" in a way that would undercut adopting Bayesian intervals (1-2).
- Wilson score interval claimed to have both near-nominal coverage AND smallest expected length at every tested sample size (0-3).
- Bootstrap claimed to offer "no meaningful coverage improvement" over Wilson in the general case (0-3) — Finding 2's narrower, scoped version (marginal difference specifically at n≤10) is what survived.
- "No universally superior interval method exists" used to blanket-argue against switching off Wilson (0-3).
- Pass@k claimed to produce unstable/misleading rankings specifically in Kelvran's low-trial regime (0-3).
- Scipy's ~70MB dependency weight in the ultralytics package used narrowly (1-2).
- Ultralytics kept scipy as an optional lazy-imported accelerator for performance reasons (1-2).
- The 11-PR refactor-effort claim for achieving numpy-optionality in humpday (1-2).

## Sources Consulted

26 sources fetched across 6 search angles (production platform practice; small-sample statistical rigor; pass@k applicability and origin; dependency cost / build-vs-buy tradeoff; contrarian / premature-optimization skepticism; Bayesian credible interval practitioner adoption); 115 claims extracted, 25 adversarially verified (14 confirmed, 11 refuted, 0 unverified).
