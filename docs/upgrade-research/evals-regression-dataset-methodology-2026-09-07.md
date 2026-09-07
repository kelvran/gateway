# Regression-Tier Eval Dataset — Methodology & Sourcing (2026-09-07)

## Question

What real methodology do production LLM-eval platforms and practitioners use to build and curate a regression-tier eval dataset (as opposed to a one-off benchmark or a smoke-test fixture), and what does "a good regression case" actually look like in practice? This is 1 of 4 parallel deep-research passes on Kelvran's "build a real regression-tier eval dataset" backlog item (methodology/sourcing, statistical sizing, pre-traffic bootstrapping, taxonomy/maintenance) — scoped to methodology/sourcing/review-workflow only.

## Summary

Across OpenAI, DeepEval, Inspect AI (UK AISI), Evidently AI, and independent practitioner writing, no platform treats a regression suite as a one-off benchmark import or a hand-frozen gold set alone. The verified pattern: (1) a reusable case primitive decoupled from any single run (DeepEval's "Golden") so it can be replayed across model/app versions, sourced from a deliberate *mix* of hand-authored/synthetic seeds, production-derived traffic, public benchmarks, and model-generated cases — each explicitly tagged by provenance so buckets can be scored and gated separately; (2) a reference value that is usually a human-approved or domain-expert-edited candidate output rather than a pre-existing objective gold answer, and is not required for every case; (3) explicit anti-degradation mechanics — stable per-case IDs, commit-SHA pinning of source data, mandatory version bumps whenever scoring/routing logic changes, a "freshness SLO" with a last-reviewed date per case, and avoidance of same-model-grades-itself contamination; (4) a three-role governance split (evaluation owner / workflow owner / release authority) so no single actor both curates and ships unchecked. This maps cleanly onto Kelvran's existing `evals promote --tier {regression,drift_sample}` mechanism (a frozen `Run`+`Score`, i.e., "trusted judge decided pass/fail at promotion time") — **the gap is process, not code**: an explicit human-review checkpoint before promotion, a provenance-tagging convention, and a freshness/versioning discipline layered on top of the already-shipped promotion command.

## Findings

### Finding 1 — Sourcing is deliberately plural and provenance-tagged, never single-channel (confidence: high)

Real regression suites draw from a mix of hand-authored/synthetic seed cases, a reusable "golden" record decoupled from any single run, and (where available) production-derived, public-benchmark, and model-generated cases — with the source type tagged on every case so buckets can be scored and retired independently. OpenAI's own first-party cookbook demo for regression detection uses a small, fixed, hand-authored list (10 synthetic cases), not promoted production traffic — even a leading vendor's canonical example doesn't default to "mine it from prod logs." DeepEval frames its "Golden" (input + optional `expected_output`, no `actual_output` yet) as specifically what enables regression testing, since the same golden replays against successive versions. Evidently AI recommends three explicit case categories: common scenarios, edge cases (past bug fixes, unusual inputs), and adversarial boundary-pushing scenarios. The Eval Smell Catalog prescribes tagging every case's origin (synthetic-from-production / hand-written / public-benchmark / model-generated) and tracking scores per bucket, specifically to prevent one easy bucket from masking regressions in another. **A stronger claim — that cases should be sourced primarily from real production failures — was explicitly refuted (0-3) across two independent sources**: the confirmed pattern is a plural mix, not a production-first mandate.

### Finding 2 — No pre-existing objective gold answer is required; a human-approved reference at promotion time is the accepted pattern (confidence: high)

DeepEval's Golden model requires only `input`; `expected_output` defaults to `None` with no validator forcing it — docs explicitly call a golden lacking both `expected_output`/`actual_output` a valid "pending test case." Evidently AI: "you also need example answers, at least for some inputs... you can approve or edit some test completions or ask domain experts to do that... Even though some LLM evaluation methods work without reference answers, building this labeled test set is crucial." This is structurally the same pattern as Kelvran's own `evals promote` — freezing a trusted `Run`+`Score` — just generalized beyond promoted historical runs.

### Finding 3 — A good case has a stable ID and the suite must be balanced and score-tracked per category (confidence: high)

Inspect AI's own best-practices doc: use stable sample IDs so a specific sample survives dataset shuffling. QASkills: "if 90 percent of your cases are easy happy-path questions, your average score will look great while your system quietly fails every hard case... track scores per tag, not just overall." A related, stronger claim (a small curated set outright beats a large random/synthetic one) was refuted (0-3) — "balance and track per-tag" survived, "fewer curated beats more random" did not.

### Finding 4 — Ambiguous failures route to an explicit "unscored" state, kept distinct from infrastructure flakiness (confidence: high)

Inspect AI's best-practices doc defines three explicit outcomes: a genuine model-fault verdict (`Score(INCORRECT)`); a run-machinery malfunction (`raise` → errored, excluded from metrics, retryable); and a probabilistic grading instrument that couldn't render a verdict after bounded due diligence (`Score.unscored()` — "only a human could render a verdict now"). Explicit named anti-pattern: "scoring infrastructure failures as 0.0, which deflates accuracy and silently opts the sample out of retries."

### Finding 5 — Reproducibility depends on commit-SHA pinning and version-bumping scoring logic (confidence: high)

Inspect AI: pin any externally-sourced dataset to an exact commit SHA, never a mutable branch name, for reproducibility even if upstream datasets change. Bump the task/suite version whenever the logic that classifies a sample's outcome changes — that routing directly determines which samples enter the denominator of the reported score. Directly relevant to Kelvran's `EvalCase`/`Score`/`Run` model: any change to grading logic should trigger review of existing frozen regression cases, not just new ones.

### Finding 6 — A "freshness SLO" prevents silent quality decay (confidence: medium — single practitioner source, 2-1 vote)

Every case carries a last-reviewed date; cases older than a threshold (90 days suggested, shorter for fast-moving features) are flagged for mandatory re-examination against current failure modes before they can gate a release — not auto-deleted, but not allowed to gate silently forever.

### Finding 7 — Same-model-family generator/judge contamination is a documented, quantified risk (confidence: high)

Using the same or closely-related model to both generate a golden's reference answer and later grade responses against it is a documented contamination pattern (judge/generator collusion, self-preference bias). Independently corroborated by peer-reviewed literature: "LLM Evaluators Recognize and Favor Their Own Generations" (NeurIPS 2024) and "Preference Leakage" (arXiv:2502.01534) quantify up to 28.7% win-rate inflation from generator-judge relatedness. Directly relevant if any LLM-judge is used in Kelvran's promotion decision path — the judge/reference-generator shouldn't be the same model family as the system under evaluation.

### Finding 8 — A three-role governance split, collapsible for small teams (confidence: medium — single practitioner source, 2-1 vote)

Workflow owner (accountable for whether AI behavior is acceptable for the business workflow), Evaluation owner (benchmark quality, dataset hygiene, rubric stability — curates the regression suite itself), Release authority (decides whether a change proceeds given remaining risk) — with the explicit caveat that one team/person can hold multiple roles as long as decision rights are made explicit in advance, before release pressure arrives.

## Kelvran-Specific Recommendation (Finding 9 — synthesis, confidence: medium)

The missing piece is **process, not code**:
(a) Add a lightweight human-review checkpoint before `evals promote --tier regression` — mirroring the same "trusted judge decides correct-enough at a point in time" pattern the project already reserves for golden cases, applied more cheaply to regression.
(b) Tag every promoted case's provenance from day one (production-derived / hand-written / public-benchmark / model-generated) even though the first real batch will be almost entirely non-production, so buckets can be scored and gated separately as real traffic eventually arrives.
(c) Given zero production traffic, source the first batch primarily by dogfooding Kelvran's own gateway/cache/evals CLI against real development usage (real cache hit/miss and routing decisions become the "production-derived" bucket) supplemented by a smaller hand-authored set spanning common/edge/adversarial.
(d) If an LLM judge is used anywhere in the promotion decision, ensure it isn't the same model family as the system under evaluation.
(e) Adopt commit-SHA-pinning, version-bump-on-scoring-change, and a `last_reviewed` field alongside the existing `tier`/`tags`/`flaky` fields immediately — cheap to add now, expensive to retrofit once the corpus grows.

**No verified source gives a specific numeric first-batch-size target** — sizing should come from the parallel statistical-sizing research pass, not this one.

## Caveats

- Claims that regression cases should be sourced "primarily" from real production failures were refuted across two independent sources — directly relevant since Kelvran has zero production traffic today; the confirmed evidence supports a plural mix, not a production-first mandate.
- Specific numeric sizing targets (50-100 cases, 5-per-category minimums) were explicitly refuted and must not be treated as verified practice.
- A claim that mandatory provenance-linkage to a specific incident/ticket is required (not just optional metadata) was refuted.
- Claims about operationally defining "flaky" via pass/fail inconsistency across runs were refuted — Inspect AI's unscored/errored distinction (Finding 4) is the strongest verified answer to "ambiguous vs. flaky," not an inconsistency-rate framing.
- Source-quality: Evidently AI, Eval Smell Catalog, QASkills, and the EvalOps blog are vendor/independent-practitioner content, not first-party platform docs or peer-reviewed research — confidence downgraded to medium where votes split (2-1).

## Open Questions (carried forward)

- Does the parallel pre-traffic-bootstrapping research pass converge with or diverge from this pass's tentative dogfooding recommendation?
- What numeric first-batch-size target does the parallel statistical-sizing pass conclude, and how does it reconcile with the qualitative "balanced, tracked per-tag" guidance confirmed here?
- Does Inspect AI's three-way outcome split (model-fault / infra-fault / instrument-fault-unscored) map cleanly onto Kelvran's existing `flaky` field, or does it need a new explicit "unscored" state?
- For a small/single-maintainer project like Kelvran, does the three-role governance model need a lighter-weight adaptation (e.g. a mandatory second-reviewer sign-off) to be a real, followable process rather than governance theater?
