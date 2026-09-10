# Evals Judge Science & Calibration — Deep Research, Round 4 (2026-09-11)

Scope: `evals/`'s LLM-judge science and calibration lens specifically — not the broader
evals-harness roadmap already covered in `evals-next-upgrade-round3-2026-09-11.md`. Grounded
against `evals/ARCHITECTURE.md`, `PRD.md`'s evals scope, `docs/upgrade-research/evals-judge-panel-
reducer-2026-09-07.md`, and `docs/upgrade-research/evals-judge-temperature-determinism-2026-09-09.md`,
plus direct reads of `evals/evals/stats.py` and `evals/evals/cli.py` to confirm the real, shipped
state (2-judge Bedrock Sonnet-5+Haiku-4.5 panel, majority vote, `--judge-axes`, `--judge-debias`,
quote-grounding, `cohens_kappa()` against a 24-case corpus, `--record-trend`). Externally verified
against 2026-current arXiv papers (judge calibration, bias amplification, self-preference,
agreement-metric studies) and one production-focused blog, each claim adversarially 3-vote-verified
before inclusion here. Six research questions were posed; two (RQ1, RQ5) returned no surviving
verified evidence this round and are carried forward as open questions rather than forced into
findings.

## Executive Summary

No 2026 source survived verification for a genuinely lightweight cross-vendor calibration probe
(RQ1) or for cost-scheduling techniques beyond mSPRT/caching (RQ5) — both remain real, unanswered
questions, not confirmed gaps. The 24-case judge-accuracy corpus (RQ2) is a real, checkable
statistical-power concern: a peer-reviewed methodology for calculating minimum sample size against
a target kappa/CI width exists (`kappaSize`/CATEKAPPA), but it lives in R, and a large 2026 study
found even 812 pairs / 3,048 judgments only reaches "fair to moderate" kappa (0.32–0.53) while a
separate purpose-built study measured κ=0.118 among LLM judges — so 24 cases is undersized relative
to the field's own instruments, but building a formal power calculator is blocked on the same
numpy/scipy-declined tooling decision already on record, not a new problem. On bias (RQ3),
verbosity bias measures small under a single pairwise rubric in the one large 2026 cohort study
found, and is independently well-documented via length-controlled AlpacaEval; self-preference bias
is real and not reliably predicted by judge capability, but its most credible mitigation —
splitting one holistic verdict into independent per-dimension judgments — is structurally what
Kelvran's already-shipped `--judge-axes` provides, so this is a "verify, don't build" finding. On
judge prompting (RQ4), hidden-state linear probes now measurably beat verbalized self-reported
confidence for judge calibration, but the technique requires model-internal activations that a
Bedrock-hosted API judge cannot expose — a real gap, but one architecturally blocked, not merely
deprioritized. On the broader question (RQ6), one paper's finding that multi-agent debate sharply
amplifies judge bias while meta-judge/aggregation approaches resist it retroactively validates
Kelvran's majority-vote (not debate) panel design as the safer choice, and the field's own
typically-low kappa baselines argue for calibrating expectations on Kelvran's kappa trend line
rather than treating a 0.3–0.5 reading as evidence of a broken judge.

## Findings

### Finding 1 — The 24-case judge-accuracy corpus has a real, checkable statistical-power gap; the formal fix is blocked by the same tooling decision Kelvran already made, not a new problem (confidence: high)

Four convergent sources establish that a small hand-labeled corpus produces a wide, hard-to-trust
kappa confidence interval, and that formal tooling for exactly this calculation exists and is
named. CATEKAPPA (arXiv:2606.07062) is an R/Shiny package built specifically to wrap `kappaSize`
(sample-size-for-target-kappa-at-given-power) and `irr` (agreement computation) — i.e., "how big
does my labeled corpus need to be" is an established, checkable statistical question with a named
tool, not folklore. What that tooling would find if applied here is suggested by two independent
data points: a 2026 legal-IR study with a corpus 34x larger than Kelvran's (812 query-document
pairs, 3,048 human judgments, 46 queries) still only reached "fair to moderate" agreement (kappa
0.32–0.53) against human judges, and a separate purpose-built demo study measured inter-LLM-judge
Cohen's kappa at just 0.118 (slight/near-chance by conventional Landis & Koch bands) — both
suggesting that even well-resourced kappa estimates in this space run low and that Kelvran's single
point-estimate kappa from n=24 carries a genuinely wide, currently-unreported confidence interval.
A fourth source (arXiv:2511.02246) adds a general methodological caveat that reinforces why this
matters: statistically-significant LLM-judge findings can still fail to generalize, so a single
point-estimate kappa — significant-looking or not — is not by itself a trustworthy basis for
gating decisions.

**Build now**: nothing new. `evals/evals/stats.py`'s `cohens_kappa()` and the `--record-trend`
history already give Kelvran a real, better-than-exact-match metric and a trend line to watch,
which is the actionable part of this finding.
**Not yet**: a formal power/CI calculator for the kappa estimate (e.g., a `kappaSize`-equivalent
sample-size or CI-width computation). Named trigger: this is gated on the same tooling decision
already on record in the temperature-determinism research (numpy/scipy/scikit-learn not installed,
Wilson-interval-only is deliberate) — `kappaSize` itself is R-only with no maintained Python port
found in this pass. The trigger to revisit is either that prior numpy/scipy decision being reversed
for other reasons, or the corpus growing large enough (via `evals promote`) that a simpler
large-sample approximate-SE formula for kappa becomes worth adding in pure Python — not evaluated
in this pass and not claimed as verified here.

### Finding 2 — Verbosity bias measures small for Kelvran's panel composition but under a narrower scope than Kelvran's actual usage; self-preference bias is real but its named mitigation is what `--judge-axes` already does (confidence: high)

A large 2026 cohort study (21 judges, 9 providers, ~541,000 judgments) found verbosity bias small
(Pearson correlation <0.011) — but explicitly scoped to "a single pairwise rubric," a caveat the
paper's own Limitations section repeats and that the reviewing pass flagged as not automatically
covering a multi-axis panel like Kelvran's `--judge-axes`. Independently, length-controlled
AlpacaEval (Dubois et al., COLM 2024, still the standard citation as of 2026) documents unconstrained
LLM-judge win rates correlating heavily with response length as a real, material confound (length-
controlling raised correlation with Chatbot Arena from 0.94 to 0.98) — so verbosity bias is a real,
citable risk category generally, even though the one 2026 measurement specific to Kelvran's exact
usage pattern (multi-axis, not single holistic pairwise) doesn't exist.

On self-preference bias specifically: a 20-model study (arXiv:2604.22891) found capability is often
uncorrelated or negatively correlated with low self-preference bias — meaning a strong Sonnet-5-
class judge is not automatically protected from favoring same-family output. The same paper names
and measures a concrete mitigation: replacing one holistic verdict with independent per-dimension
forced-choice judgments (cognitive-load decomposition) cut self-preference bias by 31.5% on average.
This is structurally the same pattern as Kelvran's already-shipped `--judge-axes` (independent
per-axis judge calls, AND-combined) — Kelvran already has a real, working instance of the field's
own named mitigation, built for a different original reason (per-axis rubric independence), not
retrofitted for this purpose. Separately, a peer-reviewed 2x2-design study (arXiv:2608.18091, EMNLP
Findings 2026) found self-/other-preference bias is driven by attribution *labeling* itself — a
judge told "this is your own output" vs. "this is another's" shifts scores bidirectionally
regardless of true source. Note: a stronger version of this claim ("blind grading makes self-
preference disappear entirely, a viable near-zero-cost fix") was checked and explicitly **refuted**
(0-3) — the paper's own hedged language ("largely disappears," not "disappears," with a small
residual in the opposite direction) does not support that strength of claim, so do not treat
"just remove labels" as a proven fix.

**Build now**: none additionally required — `--judge-axes` already provides the field's named
self-preference mitigation; using it for panel runs (not just as an optional flag) is a reasonable
operational recommendation but is a usage decision, not a code change. A cheap, genuinely low-cost
verification worth doing: confirm `evals/judge/llm_judge.py`'s prompt templates never leak which
model produced a candidate output (the confirmed labeling-bias mechanism only applies if the judge
is told source identity) — this is a prompt audit, not a build item.
**Not yet**: a dedicated verbosity-bias probe/metric, or treating multi-axis judging as bias-free
by extrapolation from the single-pairwise-rubric study. Named trigger: a future study measuring
verbosity bias specifically under multi-axis/independent-dimension judging protocols — none found
in this pass.

### Finding 3 — Verbalized judge confidence is measurably overconfident; the better-calibrated alternative is architecturally blocked by the Bedrock-hosted API, not merely deprioritized (confidence: high)

A December 2025 Meta FAIR/Meta Superintelligence Labs paper (arXiv:2512.22245) found verbalized
self-reported LLM-judge confidence is systematically overconfident relative to empirical accuracy
across five independent supporting studies, and that linear probes trained on the judge model's own
hidden-state activations consistently outperform verbalized confidence for calibration, tested
across reasoning, math, factuality, coding, and preference-judgment tasks. A stronger, more
specific version of this claim — that the same paper shows temperature=0 is empirically the *worst*
setting for judge calibration and temperature=0.7 the best, directly reinforcing Kelvran's prior
"spurious certainty" decision — was checked and **refuted** (0-3); that specific temperature-vs-
calibration finding is not supported by the primary source as stated, so it should not be cited
alongside the real temperature=0 decision already on record.

This is a real prompt/architecture-engineering gap beyond what Kelvran ships (CoT-forcing,
reference-guided grading, independent refutation, quote-grounding) — but the fix requires access to
the judge model's internal hidden-state activations at inference time, which a hosted commercial
API (AWS Bedrock, Kelvran's current judge transport for both panelists) does not expose for Claude
models. Kelvran also doesn't currently ask its judges for a verbalized confidence score at all
(verdicts are pass/fail plus optional per-axis/debias signals), so there is no existing overconfident
signal to fix — this is a gap that would only materialize if confidence scoring were added later.

**Build now**: nothing — there is no verbalized-confidence feature in Kelvran today to be
overconfident.
**Not yet**: linear-probe-based judge confidence calibration. Named trigger: either (a) Bedrock (or
Anthropic directly) begins exposing per-token hidden-state/logprob-adjacent signals for Claude
models sufficient to train or apply a probe, or (b) Kelvran adds a locally-hosted open-weight judge
model to its panel for some other reason — both are hard architectural changes, not scoped or
proposed here, and the second directly conflicts with the AWS-only operational-simplicity tradeoff
already accepted for the panel.

### Finding 4 — Multi-agent debate would sharply amplify judge bias; Kelvran's majority-vote (not debate) panel design is retroactively validated, not a gap (confidence: high)

A May 2025 (revised Sep 2025) study across 4 judge models, 4 bias types, and 3 benchmarks
(arXiv:2505.19477) found multi-agent-debate (MAD) architectures amplify intrinsic judge biases
sharply after the first debate round, with the elevated bias persisting through later rounds,
while meta-judge/aggregator architectures are comparatively resistant to this amplification (though
not immune to every bias type tested). This is directly relevant to any future proposal to resolve
Kelvran's `quorum_reached=False` fail-closed ties via a debate/reconciliation round between the two
panelists rather than the current independent-vote-then-reduce design: the evidence says that would
likely make bias worse, not better. Kelvran's actual design — two judges vote independently, then a
strict-majority reducer combines the votes, failing closed on a tie — has no argumentative exchange
between judges and is structurally closer to a (non-amplifying) independent-panel pattern than to
MAD, so this finding confirms the existing design choice rather than surfacing a gap to fix.

**Build now**: nothing — this is a "don't build" finding.
**Not yet / do not build**: a debate-style reconciliation step for resolving panel ties or
disagreement. Named trigger: none identified that would flip this recommendation; if quorum
failures become frequent enough to need a resolution mechanism, the evidence favors adding a third
independent panelist-and-revote or an aggregator/meta-judge pattern over a debate round.

### Finding 5 — The field's own typical kappa range is lower than intuition suggests; Kelvran's kappa trend line should be read against that baseline, not an idealized one (confidence: medium)

Beyond the corpus-size question in Finding 1, three sources independently establish that "good"
judge-accuracy kappa in this literature is lower than a naive reader might expect: κ=0.118 in one
purpose-built LLM-annotator study (slight agreement by Landis & Koch bands), 0.32–0.53 ("fair to
moderate") in a 34x-larger legal-IR relevance study, and a documented 33–41 percentage-point
inflation between raw exact-match agreement and Cohen's kappa on MT-Bench-style tasks (i.e., a judge
that "looks" highly accurate by raw agreement can show markedly lower kappa once chance-correction
is applied). None of these papers measure Kelvran's specific panel or corpus, so this is a field-
baseline calibration point, not a direct measurement of Kelvran's own judges — but it is directly
useful for interpreting `--record-trend`'s `judge_accuracy_kappa` series: a reading in the 0.3–0.5
range should not, by itself, be read as evidence the panel is broken, since that range is
consistent with what well-resourced studies report as normal for this kind of task.

**Build now**: a documentation note (not a code change) in `evals/ARCHITECTURE.md` or the judge-
panel RFC recording this field-typical kappa range, so a future reader of the `--record-trend`
history has a real external baseline rather than an intuition-based one.
**Not yet**: any code change — this is purely an interpretation/documentation item, not a gating or
metric change.

## Caveats

- **RQ1 (cross-vendor calibration probe) returned no surviving evidence.** Every claim found this
  round about same-vendor panels, cross-vendor bias-reduction deltas, or a lightweight secondary-
  vendor probe was explicitly refuted (0-3 or 1-2 votes) — including specific numeric claims like
  "10-25pp same-vendor bias vs ≤5pp cross-vendor." This is a genuine absence of verified evidence,
  not a confirmed "no such technique exists" — a differently-scoped search pass might still surface
  something real.
- **RQ5 (cost-aware scheduling beyond mSPRT/caching) returned no surviving evidence** — no claim
  targeting this question passed adversarial verification this round. Treat as unanswered, not
  answered "no."
- Finding 1's four supporting sources are strong on the general methodology and on field-baseline
  kappa numbers, but none of them computed a number specific to a 24-case corpus — the "24 is
  undersized" conclusion is this report's own inference from the cited instruments and baselines,
  not a number any cited paper computed for n=24 directly.
- Finding 2's verbosity-bias-is-small result is explicitly scoped by its own source to "a single
  pairwise rubric" — extending it to Kelvran's multi-axis `--judge-axes` usage is this report's own
  reasonable-but-unverified extrapolation, flagged as such inline.
- Two refutations are worth remembering by name, since they contradict intuitive-sounding claims
  that could otherwise resurface: (1) temperature=0 is NOT shown by arXiv:2512.22245 to be the worst
  calibration setting (that specific claim failed verification 0-3) — do not conflate with Kelvran's
  separately-decided, differently-justified "spurious certainty" rejection of blanket temperature=0;
  (2) blind/label-free grading does NOT "effectively eliminate" self-preference bias per
  arXiv:2608.18091's own hedged language (0-3) — only that labels are *a* driver, not the *sole*
  possible driver, and the effect "largely" rather than fully disappears.
- Source-access constraints recurring across this round (Exa/Tavily rate limits, Google/DuckDuckGo/
  Semantic Scholar robots.txt blocks) meant several claims rest on primary-source-only confirmation
  without independent third-party triangulation of "no contradicting evidence exists" — flagged
  per-claim by the verifiers, consistent with prior rounds' experience with the same tools.

## Recommendation for Kelvran

1. **Build now, documentation only**: record the field-typical kappa baseline (κ≈0.12–0.53 across
   cited studies, 33-41pp exact-match-vs-kappa inflation) in `evals/ARCHITECTURE.md` or the
   judge-panel RFC, so `--record-trend`'s `judge_accuracy_kappa` series is read against a real
   external baseline (Finding 5).
2. **Build now, low-cost audit, not a code change**: confirm `evals/judge/llm_judge.py`'s prompt
   templates never reveal which model produced a candidate output, closing off the one concretely-
   demonstrated driver of label-based self-/other-preference bias (Finding 2's labeling-mechanism
   evidence).
3. **Do not build**: a debate/reconciliation round for resolving panel ties (Finding 4) — the
   evidence points the other way; a formal kappa power/CI calculator (Finding 1) — blocked on the
   same numpy/scipy-declined decision already on record, not a new problem to solve today; a
   dedicated verbosity-bias metric (Finding 2) — no study yet measures this under Kelvran's actual
   multi-axis usage.
4. **Not yet, correctly deferred with a named trigger**: hidden-state-probe-based judge confidence
   calibration (Finding 3) — trigger is Bedrock/Anthropic exposing model-internal signals for
   Claude, or a locally-hosted open-weight judge being added for unrelated reasons; a formal minimum-
   corpus-size calculation for judge-accuracy kappa (Finding 1) — trigger is either the numpy/scipy
   decision being revisited, or the corpus growing large enough via `evals promote` to make a pure-
   Python large-sample approximation worth adding.

## Open Questions

- RQ1: is there a genuinely lightweight (no second always-on API key) way to get partial cross-
  vendor calibration signal for a same-vendor panel — e.g., a periodic, small-sample probe against a
  cheap third-party model run only on the existing 24-case corpus rather than every production call?
  This round found no verified 2026 source addressing this specific shape of question.
- RQ5: beyond mSPRT early-stopping and score caching, does any 2026 source describe a cost-aware
  judge-panel *scheduling* technique (e.g., adaptively skipping the second panelist when the first
  judge's verdict is already high-confidence by some cheap proxy) — not found this round.
- If Kelvran ever adds verbalized judge confidence scoring (it does not today), should it be
  benchmarked against the overconfidence pattern documented in Finding 3 before being trusted for
  any gating decision, given that the better-calibrated alternative (hidden-state probes) is not
  available on the current Bedrock transport?
- Would a pure-Python, large-sample approximate standard-error formula for Cohen's kappa (analogous
  to how `wilson_interval()` already avoids a scipy dependency for the pass-rate metric) be worth
  scoping as its own small RFC, given Finding 1's conclusion that the formal `kappaSize`-style
  calculator is blocked by tooling but the underlying "give the point estimate a CI" need is real?
