# Evals Regression-Corpus Scaling Methodology — Deep Research (2026-09-12)

## Question

Kelvran's hand-authored regression corpus is exactly 137 cases across 6
category files (`cache_adversarial`/`cost_abuse`/`routing_chaos`/`dogfood`/
`guardrail`/`judge_accuracy`), with real corpus-governance tooling already
shipped (`field_swap_lint`'s O(d²) transposition detector, deliberately
deferred on a "low thousands" perf trigger; `corpus_staleness`'s
citation-freshness checker; `audit_corpus`'s LLM-based design-defect
auditor). Distinct from the already-completed benchmark-interop research
(`evals-benchmark-interop-2026-09-11.md`, which concluded standard external
benchmarks are a category mismatch), this research asks: (1) what real
production methodology exists to scale a hand-authored, adversarial-by-design
infra-correctness corpus by an order of magnitude or more — synthetic
LLM-assisted authoring, production-incident/traffic mining (Kelvran's own
`evals ingest`/`evals promote` pipeline already exists for this), or
crowd-sourced/red-team contribution; (2) at what real corpus size does
`field_swap_lint`'s O(d²) cost actually become measurable, and can this be
estimated now against Kelvran's real implementation without the corpus
growing first; (3) what QC discipline do production test/benchmark
maintainers use to prevent large-scale growth from reintroducing exactly the
defects `audit_corpus`/`corpus_staleness` were built to catch, and does that
tooling need its own redesign past 137 cases; (4) is large-scale growth
actually a near-term priority given Kelvran has zero production traffic to
mine yet.

## Executive Summary

Production-scale evidence for mining real traffic/telemetry into regression
cases is real and industrially proven (NL2Test: 3,196 cases in a 9-month
deployment, 85.4% adoption; LogTesT: transformer-generated invocation
sequences from event logs) — but both techniques require a precondition
Kelvran does not have: real production traffic, which is also the standing,
independently-recorded trigger already blocking multiple other Kelvran
backlog items. LLM-assisted synthetic authoring is real but never
review-free even at its best: 82.4% exact-match / 98% only-after-minor-edits
in one industrial pipeline, and SPICE's automated consensus mechanism swings
from 93.5% to 60% reliability depending on task type — meaning the review
discipline needed is calibrated and task-specific, not a flat spot-check
rate. Empirically benchmarking Kelvran's own real `field_swap_lint.
find_reference_swaps` directly (done in this research pass, not deferred)
confirms the "low thousands" trigger is roughly right in absolute terms
(crossing ~1 second around 1,000–2,000 divergent cases in a single file,
becoming a genuine multi-second problem at 5,000–10,000) but also confirms
the trigger is scoped to one file's divergent-case count, not total corpus
size — and Kelvran's real, current worst-case file sits at only 24 divergent
cases, giving far more headroom than an order-of-magnitude *total* corpus
scale-up would suggest. Finally, weighing Kelvran's own already-completed
statistical-sizing research (100–150 cases recommended in aggregate, 30–50
per category) against the corpus's real per-category counts (17–35 today)
shows the near-term need is a targeted top-up of the 3 most under-provisioned
categories, not an order-of-magnitude scale-up — so hand-authored,
taxonomy-preserving growth remains the correct investment level right now.

## Findings

### Finding 1 — Production-traffic mining into regression cases is a real, industrially-proven methodology, but it needs a precondition Kelvran doesn't have yet

**Verdict: not_yet.** Named trigger: real production traffic exists for
`evals ingest`/`evals promote` to mine from — already the standing,
repeatedly-reconfirmed blocker across multiple prior Kelvran research/backlog
rounds (`DECISIONS.md`'s 2026-09-11 and 2026-09-12 entries both name "zero
real production traffic" as an unfired trigger), not a new finding invented
here.

NL2Test is a real, peer-reviewed (ISSTA 2026), industrially-deployed
existence proof for exactly this growth method: it carves a minimal
replayable request sequence plus reconstructed data dependencies out of a
natural-language scenario description *and* a captured traffic trace, and in
a real 9-month production deployment (starting March 2025) at a large
consumer-facing internet company generated 3,196 test cases with an 85.4%
code-adoption rate. LogTesT is a complementary, smaller-scale technique in
the same family: it fine-tunes a T5-small transformer on paired event-log/
trace data to generate service-invocation sequences that mimic real
operational behavior, explicitly to "augment the regression suite with
new field-informed tests" — the same shape as what Kelvran's own `evals
ingest`/`evals promote` pipeline targets.

One concrete caution from LogTesT's own benchmark numbers, though: log-mined
generation produced >18x more raw cases than a spec-driven baseline suite
(88,158 vs. 4,678) but concentrated that volume onto only 22 of the target
system's API paths versus the baseline's 1,187 — a ~54x *narrowing* of
path/scenario diversity. Applied to Kelvran: traffic mining grows volume by
generating many variations of whatever paths real traffic actually exercises,
not by diversifying scenario coverage — a good match for the `dogfood`
category (which already tests real, observed system behavior) but a weaker
match for the deliberately adversarial categories (`cache_adversarial`/
`cost_abuse`/`routing_chaos`), which by design need cases that "normal"
traffic would rarely generate on its own.

*Confidence: high for the NL2Test/LogTesT mechanics and numbers (both
primary, peer-reviewed sources, 3-0 and 2-1/3-0 votes respectively, directly
fetched and cross-checked against full paper text, not just abstracts).
Medium for the dogfood-vs-adversarial-category fit conclusion — that
application to Kelvran's specific taxonomy is this research's own synthesis,
not a sourced claim.*

### Finding 2 — LLM-assisted authoring's human-review discipline is real and quantified, but it's calibrated and task-specific, not a flat spot-check rate

**Verdict: not_yet** to build a full LLM-assisted case-generation pipeline
(no near-term volume need — see Finding 5). **Build_now**: adopt the
review-discipline norms themselves for any case-authoring Kelvran does from
here forward, pipeline or not.

Even the most mature industrial pipeline surveyed here (NL2Test, same paper
as Finding 1) still required human review to reach production quality: only
82.4% (42/51) of generated regression scenarios exactly matched a
human-authored reference, rising to 98.0% (50/51) usable only after "minor
post-edits" — i.e., roughly 1 in 6 generated cases needed real human
correction even in a pipeline the authors call "highly robust." Separately,
SPICE's automated multi-pass consensus mechanism — which still required a
4-person human labeler team with a third-labeler tiebreaker to validate —
correctly labeled one task type 93.5% of the time but only 60% of the time
on a different task type (Test Coverage Assessment), with the paper's own
explicit recommendation that the weaker task type's automated label be used
only "with caution and accompanied by human review." Together these say the
same thing from two different angles: a uniform "spot-check N% of generated
cases" review policy is the wrong shape; the review burden is genuinely
uneven across task/category types, and any future Kelvran generation
tooling needs per-category calibration, not one global rate.

A concrete, adoptable artifact for that calibration exists (from a secondary
but directly-verified source): Cohen's-kappa bands for judging whether a
review rubric itself is trustworthy — ≥0.8 "Strong" (trust labels, promote to
golden set), 0.6–0.8 "Substantial" (use with a third-reviewer tiebreaker),
below 0.6 "weak" (halt, rewrite the rubric, recalibrate before labels feed
back). This is one document's own practical bucketing, not the canonical
Landis & Koch/McHugh academic scale (which uses different band boundaries
and labels) — but as a concrete, adoptable starting rubric for Kelvran's own
human reviewers, whether reviewing hand-authored or (eventually)
LLM-generated cases, it is directly usable now, cheaply, independent of
whether or when a generation pipeline is ever built.

*Confidence: high for the NL2Test 82.4%/98.0% figures and the SPICE
93.5%/60% figures (primary, peer-reviewed sources, verified against full
paper text: 2-1 and 3-0 votes respectively). Medium for the kappa-band
rubric (secondary source, 2-1 vote, and explicitly not the canonical
academic scale — treat as one practitioner's workable convention, not a
citation-grade standard).*

### Finding 3 — Empirically benchmarked, directly against Kelvran's own real `field_swap_lint`: the "low thousands" trigger is roughly right, and it's scoped per-file, not per-corpus

**Verdict: build_now — already done in this research pass.** RQ2 asked
whether this could be estimated now without waiting for the corpus to grow;
it can, and the answer below is a direct empirical measurement, not an
extrapolation.

`evals/evals/field_swap_lint.py`'s `find_reference_swaps` is explicitly
scoped, by its own design (confirmed by direct code read), to one file's
*divergent* cases (`reference != task_spec["output"]`) — not the corpus
total. Running the real, unmodified function against synthetic `EvalCase`
objects at increasing divergent-case counts on this session's machine gave:

| divergent cases (d) | wall time | pairs compared |
|---|---|---|
| 137 (current corpus total, worst case: all divergent) | 0.002 s | ~9.3K |
| 500 | 0.03 s | ~125K |
| 1,000 | 0.35 s | ~500K |
| 2,000 | 1.08 s | ~2.0M |
| 5,000 | 5.85 s | ~12.5M |
| 10,000 | 39.2 s | ~50.0M |
| 20,000 | 95.7 s | ~200.0M |

This confirms the deferred trigger's own qualitative claim ("low thousands")
is empirically about right in absolute terms: cost crosses roughly 1 second
somewhere between d=1,000 and d=2,000, and becomes a genuine multi-second CI
problem at d=5,000–10,000. (The scaling isn't perfectly quadratic in this
run — e.g. 5k→10k is ~6.7x for a 2x input rather than the expected ~4x — an
artifact partly explained by a real, minor inefficiency in the shipped code:
`for case_b in divergent[i + 1 :]:` re-slices a fresh list copy on every
outer-loop iteration, adding an extra O(d) allocation cost per outer step on
top of the intended O(d²) comparison cost. Worth a cheap follow-up fix
independent of this research's scope, but not urgent at current d.)

Critically, the trigger is **per-file**, not per-corpus, and Kelvran's real
current data (counted directly from the six live fixture files) shows far
more headroom than the corpus's 137-case *total* implies: divergent-case
counts per file today are `cache_adversarial`=6, `cost_abuse`=4,
`dogfood`=0, `guardrail`=0, `judge_accuracy`=24, `routing_chaos`=4 — every
file is one to three orders of magnitude below the ~1,000–2,000 crossover
point. Even a uniform 10x corpus scale-up (137→~1,370, spread proportionally
across categories) would put the *worst* current file (`judge_accuracy`,
already 100% divergent) at only ~240 divergent cases — still comfortably
below the measured 1,000–2,000 threshold. The deferred trigger would only
fire under a much more extreme, single-file-concentrated growth pattern than
"grow the whole corpus by an order of magnitude" implies on its own.

*Confidence: high — this is a direct empirical measurement against the real,
unmodified shipped function (not a re-implementation or estimate), and the
per-file divergent counts are direct counts against the real fixture files.
The exact wall-clock numbers are single-run, single-machine timings (not
averaged, not run on Kelvran's real CI hardware) — treat absolute seconds as
approximate; the order-of-magnitude crossover conclusions are robust to
that.*

### Finding 4 — Real production QC discipline sets a materially higher bar than Kelvran's currently-shipped single-pass audit tooling — cheap to harden now, before scaling, not after

**Verdict: build_now** for a cheap hardening step (a second-pass
re-verification on flagged findings); **not_yet** for a full multi-reviewer
human system.

OpenAI's own disclosed audit of 138 SWE-bench Verified problems — a
benchmark roughly the same size as Kelvran's entire corpus (137 cases), and
one that had *already* been through 3-reviewer human curation at creation
time — needed at least 6 independent expert software-engineer reviewers per
case, plus a distinct re-verification pass by an additional team whenever
any reviewer flagged an issue, before OpenAI trusted the audit's own
findings enough to retire the benchmark from frontier evaluation. The audit
step, in other words, needed to be *more* rigorous than the original
authoring step, not equally rigorous.

Kelvran's `audit_corpus` (confirmed by direct read of its own module
docstring and design) is a single sequential LLM-call-per-case design-defect
auditor with no distinct re-verification pass on its own flagged findings —
report-only, always deferring to a human to act, but with no mechanism that
mirrors OpenAI's "if flagged, re-verify with a second independent pass"
step. `corpus_staleness` (linear per-case git-log citation check) and
`field_swap_lint` (Finding 3) are architecturally sound to scale as-is; the
one piece of Kelvran's existing governance tooling this research found a
concrete, sourced argument to strengthen is `audit_corpus`'s single-pass
design — specifically, adding a cheap second, independently-framed LLM call
on any finding before it's surfaced to a human, mirroring OpenAI's own
real-world audit discipline at a much smaller scale. This is worth doing
*before* corpus growth multiplies the volume of findings a human has to
triage per audit run, not retrofitted after the fact once growth has already
made triage painful.

*Confidence: high for the OpenAI audit-methodology figures (primary source,
official post, verified via archived snapshot since the live page blocks
automated fetches; 2-1 vote but with thorough, primary-sourced verifier
evidence). Medium for the audit_corpus gap-analysis and recommendation —
that comparison is this research's own synthesis against Kelvran's real code,
not an independently-sourced claim.*

### Finding 5 — Large-scale corpus growth is not a real near-term priority; targeted, hand-authored, taxonomy-preserving growth is still the right investment level

**Verdict: not_yet.** Named triggers (both must hold before order-of-
magnitude growth is warranted): (a) real production traffic exists to mine
(blocks Finding 1's highest-value growth method entirely — currently zero,
per Kelvran's own repeatedly-reconfirmed status), **and** (b) Kelvran's
already-completed statistical-sizing recommendation is exceeded by real
per-category counts, not just the total.

Cross-referencing this research against work Kelvran has already done: the
sibling 2026-09-07 statistical-sizing research recommended ~100–150 cases in
aggregate and ~30–50 per meaningful category as a first-batch target,
derived directly from Kelvran's own real `wilson_interval()` math. Counting
directly against the six live fixture files, Kelvran's corpus today is 137
cases total — already inside the low end of that aggregate recommendation,
not under-provisioned overall — but per-category counts (`cache_adversarial`
=17, `cost_abuse`=20, `routing_chaos`=17, `guardrail`=24,
`judge_accuracy`=24, `dogfood`=35) leave 3 of 6 categories below the 30–50
recommendation, and even the two closest (`guardrail`/`judge_accuracy` at
24) haven't reached it. The concrete, evidence-grounded near-term need is
therefore a targeted top-up of roughly 10–30 more cases in the 3
under-provisioned categories to reach a target Kelvran has already derived
for itself — not an order-of-magnitude scale-up of the whole corpus.

This directly answers RQ4: an order-of-magnitude-or-more scale-up is not
what the evidence says Kelvran needs right now. Both growth methods surveyed
above (traffic mining, LLM-assisted authoring) remain legitimate options to
revisit later — specifically once (a) real production traffic exists, which
would unlock Finding 1's mining pipeline, or (b) hand-authoring capacity
itself becomes the actual bottleneck to closing the smaller, already-derived
per-category gaps identified here — but neither condition holds today.
Hand-authored, incremental, taxonomy-preserving growth within the existing
6-category structure remains the correct investment level.

*Confidence: high for the real per-category counts (direct counts against
the live fixture files) and the sibling sizing research's recommendation
(already an independently 3-vote-verified prior research pass, re-cited here
not re-derived). This finding itself is cross-cutting synthesis rather than
a claim that independently passed this round's 3-vote adversarial process —
treat the underlying counts as fact, the "not a near-term priority"
conclusion as well-grounded reasoning from them.*

## Caveats

- **Vote margins on the underlying claims are mixed**: 3 of the 8 confirmed
  claims passed 3-0 unanimous (NL2Test's core mechanism, LogTesT's core
  mechanism, SPICE's ICA/TCA figures); the other 5 passed only 2-1. None
  were refuted outright, and verifier evidence write-ups were thorough and
  primary-sourced even at 2-1 margins, but findings resting more heavily on
  2-1 claims (Findings 1's LogTesT-scale numbers, Finding 2's NL2Test
  post-edit rate and the kappa-rubric source, Finding 4's OpenAI audit
  figures) warrant a notch less confidence than the 3-0 claims.
- **A large number of adjacent claims were refuted (17 refuted vs. 8
  confirmed)** in this same research round — including claims about
  small-batch trust-gates before scaling synthetic generation, fully
  automated LLM-judge gatekeeping loops at 8,000-item scale, SWE-bench
  Pro's 24%-ground-truth-defect audit, eval-suite value not scaling
  linearly with case count, and several blog-sourced recommendations about
  incident-mining priority and human-review sampling rates. This is itself
  informative: much of the readily-findable secondary-source content on
  "how to scale a test/eval corpus" did not survive adversarial
  verification, and several of the refuted sources were blog-quality
  (agentpatterns.ai, futureagi.com, tianpan.co, zivis.ai, arthur.ai) rather
  than primary/peer-reviewed — treat any future research on this exact
  question with the same skepticism toward secondary sources.
- **Domain mismatch**: NL2Test and LogTesT (Finding 1) are API-regression-
  test-carving tools for microservice traffic replay, not eval-corpus tools
  for LLM-gateway adversarial correctness testing — real and load-bearing as
  an existence proof for the *mining* mechanism, but analogous rather than
  domain-identical to Kelvran's actual use case.
- **Finding 3's benchmark is a single run on this session's own machine**,
  not averaged across repeated runs and not measured on Kelvran's real CI
  hardware — exact per-N seconds will vary by machine; the order-of-
  magnitude crossover conclusions (≈1s at 1-2K, multi-second at 5-10K) are
  robust to that variance, but should not be quoted as precise SLA numbers.
- **Findings 3, 4, and 5 all include this research's own direct-code and
  direct-fixture-file grounding** (reading `field_swap_lint.py`,
  `audit_corpus.py`, `corpus_staleness.py`, and counting the six live
  `regression_corpus_*.json` fixtures) rather than resting purely on
  externally-sourced, 3-vote-verified claims — that grounding is accurate as
  of this research date but should be re-verified against the live repo
  before being cited in a future document, per this project's own
  documented doc-vs-code staleness gotcha.

## Open Questions

1. Should `evals ingest`/`evals promote`'s traffic-mining path (Finding 1)
   be scoped specifically to the `dogfood` category once real production
   traffic exists, rather than applied uniformly across the taxonomy — given
   LogTesT's own evidence that traffic-mined growth concentrates on whatever
   paths real traffic exercises, which is a weaker fit for the deliberately
   adversarial categories?
2. Should the Cohen's-kappa reviewer-agreement rubric (Finding 2) be written
   into Kelvran's corpus-authoring guidelines now — matching the precedent
   already set by writing down the GAP convention in the round-4 governance
   research — even before any LLM-assisted generation pipeline exists to
   gate?
3. Is `audit_corpus`'s sequential-Bedrock-call design (already flagged
   elsewhere in the backlog as blocked on a shared cost-attribution race) the
   same blocker that would need solving before a second-pass re-verification
   step (Finding 4) could be added cheaply, or are the two changes
   independent and shippable separately?
4. `corpus_staleness`'s per-citation git-log subprocess shell-out was not
   empirically benchmarked in this pass (unlike `field_swap_lint`) — does it
   have its own distinct scaling concern (linear-but-real subprocess-spawn
   wall-clock cost accumulating with corpus size, rather than an O(n²)
   algorithmic issue), and would it benefit from the same kind of direct
   empirical benchmark this research ran for `field_swap_lint`?
