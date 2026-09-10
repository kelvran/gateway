# Evals Corpus Governance & Benchmark-Integrity Research — Round 4 (2026-09-11)

Scope: `evals/`'s dataset/corpus governance and benchmark-integrity lens, motivated directly
by this session's own experience — a ~65-agent adversarial triage swarm found and fixed 9
real stale/incorrect entries across the 137-case regression corpus (transposed fields,
mis-encoded bytes, stale claims after real code migrations) and discovered a previously
undocumented "GAP" corpus convention. Grounded against `DECISIONS.md`'s triage entries and
`evals/ARCHITECTURE.md`'s regression-corpus section; externally verified against the
HuggingFace evaluation guidebook, Langfuse's/Braintrust's own dataset-drift practice, DOCER
(a doc-staleness GitHub Action) and its underlying academic study, DyePack and two 2026
contamination preprints, LSHBloom, and the test-smell catalog, each claim adversarially
3-vote-verified before inclusion here.

## Executive Summary

Comparing this session's 65-agent triage findings against 2026 benchmark-integrity and
eval-platform practice: (1) writing the GAP convention down as an explicit authoring
guideline is real, low-risk, buildable-now work — no external framework (HuggingFace's
guidebook, Langfuse's dataset docs, or any checklist found) already documents this exact
`reference`=target/`output`=honest-FAIL pattern, so Kelvran would be codifying its own genuine
invention, not restating known practice. (2) A git-log-based citation-staleness checker (flag
a case whose `verified_against` file:line was touched after the case's own revision date) is
buildable now with no new infrastructure — it has a real, if imperfect, industry precedent (a
PR-triggered GitHub Actions doc-staleness tool, DOCER, built on an academically measured
~29%-of-top-1000-repos staleness base rate) and fills a gap that even mature commercial eval
platforms (Langfuse) do not automate; Langfuse's own dataset governance is auto-versioned per
mutation but relies on humans to notice and archive staleness. (3) Contamination-detection
machinery (DyePack-style backdoors, canary strings) is real 2025-2026 research, but the entire
literature targets public benchmarks used for cross-model leaderboard ranking — a threat
model that doesn't match Kelvran's internal, non-benchmarked regression suite, and recent
evidence shows even genuine contamination rarely reorders rankings (0.997 leaderboard-rank
correlation) — so a canary string is a near-zero-cost "build now if you want it" addition, but
heavier detection machinery is "not yet." (4) No existing tool or named test-smell targets the
guardrail-13/18 bug class (adjacent same-file data-value transposition) at Kelvran's scale —
large-scale dedup tools (LSHBloom) are built for 10M+ document corpora and the test-smell
catalog's "duplication" entries describe repeated code steps, not swapped data fields — so a
bespoke, cheap pairwise exact-swap check across same-category-file cases is real, buildable-now
work with no off-the-shelf substitute.

## Findings

### Finding 1 — Codify the GAP convention as an explicit authoring guideline
**Confidence: medium**

Writing down the GAP convention (`task_spec.output` = honest current-state FAIL, `reference`
= ideal target, until the underlying gap is fixed) as an explicit corpus-authoring guideline
is real, low-risk, high-value work — and no external eval-dataset framework already documents
this exact pattern, so it is a genuine Kelvran invention worth capturing, not a restatement of
known practice. `DECISIONS.md`'s triage entries record that the ~65-agent swarm
reverse-engineered the GAP convention from case rationale text (confirmed in 5
`cache_adversarial` + 3+ `routing_chaos` cases) and that this saved two of the swarm's own
false "confirmed_defect" verdicts from being wrongly "fixed" during synthesis — the convention
already prevented a real error once it was understood, which is itself the argument for
writing it down before the next audit pass has to rediscover it. External corroboration is by
absence: two adjacent claims about explicit authoring/review guidance from Langfuse's own
golden-dataset content were checked and refuted on verification (a claimed dedup-recommendation,
1-2; a claimed human-review-defines-golden distinction, 0-3) — no eval-platform vendor content
currently documents this reference-vs-output GAP pattern or an equivalent.

**Verdict: BUILD NOW.** The write-up should state, as a rule: any case whose `task_spec.output`
documents a real, currently-unfixed system defect must set `reference` to the ideal/target
behavior, never edit `reference` to match `output` to "make it pass," and must carry a
discoverable marker (a tag or a required rationale phrase) so a future auditor recognizes the
pattern instead of flagging `reference`/`output` divergence as a defect.

### Finding 2 — A git-log-based citation-staleness checker is buildable now
**Confidence: high**

An automated, git-history-based check that flags a case's `verified_against` file:line
citation as stale (the cited file was modified after the case's own authoring/revision) is
buildable now with no new infrastructure, and has real precedent in a working, PR-triggered
GitHub Actions tool (DOCER) built on an academically measured ~29%-of-top-1000-GitHub-repos
doc-staleness base rate — though DOCER's own mechanism (code-element-existence diffing) is a
related but distinct technique from the git-log/touch-date check Kelvran would need. Two peer
papers establish both that doc/code-reference staleness is common and measurable at scale
(28.9% of the 1000 most-starred GitHub repos had at least one outdated code-element reference
in their docs) and that automated, CI-wired detection is a proven, shippable pattern: a real,
live GitHub Actions tool that triggers `on: pull_request` and scans docs for code-element
references that no longer exist anywhere in the source. Two caveats: (a) DOCER's mechanism is
existence-based, not recency-based — Kelvran's proposed check (`git log` on the cited path,
compare last-touch date against the case's own `revision`/authoring date) is a different,
arguably simpler mechanism; (b) whether a PR-triggered check is cheaper than a periodic batch
job is this research's own unverified inference, not something either paper measured.

**Verdict: BUILD NOW.**

### Finding 3 — Even Langfuse doesn't automate staleness detection; Kelvran's `EvalCase.revision` remains adequate
**Confidence: high**

A mature, widely-used production eval platform (Langfuse) does not ship any automated
drift-detection or staleness-alerting feature for golden/regression datasets — staleness
handling is a purely manual, human-judgment process — even though the platform does
auto-version every dataset-item mutation and lets you query point-in-time snapshots. This
confirms directly that dataset versioning-on-every-mutation plus point-in-time retrieval is
real and shipped — the same shape as Kelvran's existing `EvalCase.revision` field, so the
earlier research pass's conclusion that Kelvran has no real dataset-versioning gap at its
current scale continues to hold. But staleness detection itself (as opposed to versioning) is
nowhere automated even in this mature platform: the one plausible counter-feature (Langfuse
Alerts, which can threshold on Scores) explicitly excludes dataset-experiment test runs from
its alerting path, applying only to live production evaluator scores.

**Verdict: BUILD NOW (Finding 2) would put Kelvran ahead of, not behind, what a mature
commercial eval platform currently automates for this exact problem.**

### Finding 4 — Canary strings: real technique, low priority at Kelvran's current scale
**Confidence: medium**

Embedding a canary string in a public eval corpus is a real, established, near-zero-cost
contamination-mitigation technique (HuggingFace's evaluation guidebook cites BigBench's
precedent) — but Kelvran's regression corpus is not the kind of artifact this technique
protects, since it is an internal system-behavior regression suite, not a public cross-model
leaderboard benchmark that other labs would want to game or that gets scraped into training
corpora to inflate comparative scores. Three adjacent, more specific claims attempting to tie
an explicit contamination-prevention checklist obligation to public GitHub eval corpora were
checked and refuted (0-3 each): a claimed llm-guidelines.org checklist requirement for
held-out subsets/canary strings/pre-exposure investigation on new benchmarks, a claimed
rule-based/generative-construction contamination rationale, and a claimed
Gaussian-budgeting sample-sizing critique. Heavier, provable contamination-detection machinery
(DyePack-style backdoor sampling with exact false-positive-rate guarantees, EMNLP 2025) is
real, rigorous research, but built for detecting whether a model's training set included a
specific PUBLIC benchmark — an orthogonal problem to Kelvran's own use of its corpus
(regression-testing its own gateway code, not scoring/ranking third-party LLMs). Separately,
recent (unreviewed, Aug/Sept 2026 preprint) evidence shows even genuine test-set contamination
in public benchmarks rarely reorders relative leaderboard rankings (0.997 rank correlation
across 47 public models; only 3 of 188 model-benchmark cases showed corroborated
differential/rank-distorting contamination), and a private held-out test set only closes one
of five taxonomized contamination routes (direct), leaving four (derivative, temporal,
distributional, acquired) formally unaddressed by holding data out alone.

**Verdict: BUILD NOW IF DESIRED** (a single marker string is near-zero cost, a reasonable
low-effort hedge), but **NOT YET** for heavier detection machinery. Named trigger: revisit
only if Kelvran's regression corpus starts being used to publicly benchmark/rank third-party
models, or sees meaningful reuse/scraping by outside projects.

### Finding 5 — A bespoke adjacent-case field-swap detector is real, buildable-now work
**Confidence: high**

No existing tool or named test-smell category is built to mechanically catch the
guardrail-13/18 bug class (a swapped/transposed data value between two adjacent,
back-to-back-authored data-driven test cases in the same file) — large-scale near-duplicate
detectors (LSHBloom) are designed and evaluated exclusively at internet/billion-document scale
(39M measured, extrapolated to 5B+), and the established test-smell catalog's "Test Code
Duplication" entry describes repeated code/setup steps across test methods, not swapped data
fields between separate cases. A cheap, bespoke, brute-force pairwise check (flag any two
same-category-file cases whose `task_spec` is near-identical except `output`/`reference` are
each other's value) is real, buildable-now work with no off-the-shelf substitute, and is
computationally trivial at Kelvran's ~137-case scale (no need for LSH-style approximate
methods meant for millions of records).

**Verdict: BUILD NOW.** The guardrail-13/18 fix pattern (found only via a full adversarial LLM
audit) could plausibly have been caught mechanically and far more cheaply by a purpose-built
structural check specifically targeting adjacent-case field-swap similarity.

## Caveats

Two of the three contamination-literature preprints underlying Finding 4 (the 5-type
taxonomy, arXiv:2608.29463; the leaderboard-ranking-correlation paper, arXiv:2609.02899) are
unreviewed, days-to-weeks old, and have zero citations at time of research — treat their
specific numbers as provisional, not settled science, though the primary-source verification
of what each paper claims is solid. DyePack (EMNLP 2025) is peer-reviewed and more
load-bearing. The HuggingFace evaluation guidebook is explicitly flagged by its own
maintainers as no-longer-maintained (as of Dec 2025) — the canary-string recommendation itself
is long-standing (BigBench, ~2022) and uncontested, but the source document is stale as a
whole. The GAP-convention finding (Finding 1) is the weakest-sourced of the findings in
external-research terms — it rests on Kelvran's own `DECISIONS.md` as primary evidence plus
the absence of contrary external precedent, not on a positive external confirmation that this
exact pattern is best practice elsewhere.

## Open Questions

- Is a PR-triggered citation-staleness check actually cheaper or more effective than a
  periodic batch/cron job for Kelvran's case — no source directly compares these two
  integration costs.
- At what corpus size, public-reuse level, or third-party-benchmarking use case would canary
  strings or heavier contamination detection become genuinely warranted for Kelvran?
- Should the GAP convention be encoded as a machine-checkable invariant (e.g. a required tag
  or lint rule) rather than purely prose documentation, to make the next audit swarm's job
  structural instead of interpretive?
- Does the bespoke adjacent-case field-swap detector (Finding 5) generalize cleanly as the
  corpus grows past a few hundred cases per category file, or would a smarter near-duplicate
  method become necessary at some threshold well below LSHBloom's internet-scale target?
