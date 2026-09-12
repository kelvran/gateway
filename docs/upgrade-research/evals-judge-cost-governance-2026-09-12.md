# Evals LLM-Judge Cost Governance (2026-09-12)

Scope: `evals/`'s **own** judge-call spend — running `--llm-judge`/`--llm-judge-panel`
against Bedrock — specifically whether it needs its own budget-governance layer,
analogous to but distinct from the gateway's own end-user spend `budget.Tracker`
(shipped) and from evals' own already-shipped cost-reduction levers: the content-hash-
keyed, cross-mode-reusable score-cache (`--use-score-cache`, `docs/rfcs/2026-09-05-
evals-score-cache.md`) and mixture-SPRT early-stopping (`--early-stop-*`, `docs/rfcs/
2026-09-05-evals-mixture-sprt-early-stopping.md`). Four research questions were posed;
each is answered against real production patterns at Langfuse, Braintrust, and
LangSmith, real academic precedent for smarter-than-uniform judge-query allocation, and
Kelvran's own current, verified code/CI state.

## Context (Kelvran's own real, current state, verified this pass)

`evals/evals/judge/providers.py` wires both `--llm-judge` (standalone) and
`--llm-judge-panel` (2-judge, Claude Sonnet 5 + Claude Haiku 4.5, majority-reduced,
fail-closed on a tie) entirely through AWS Bedrock — no Anthropic-direct or OpenAI key
used anywhere in judging anymore (`docs/rfcs/2026-09-08-evals-judge-panel-reducer.md`).
`Score.cost_usd` (a real `Decimal`, per `docs/rfcs/2026-09-04-evals-score-model.md`)
already captures real judge-call cost on every judged verdict — the raw spend data a
future budget cap would need already exists; only an enforcement/cap mechanism would
be new. The only place judge cost is currently exercised in CI is
`.github/workflows/evals-judge-nightly.yml`: a once-daily cron (deliberately **not**
per-push/PR, to avoid "real, unbounded dollar cost against a live paid provider on
every push" and non-determinism-driven flakiness) running both `--llm-judge` and
`--llm-judge-panel` against `tests/fixtures/regression_corpus_judge_accuracy.json` — a
24-case corpus. That is the whole of Kelvran's own measured judge-call volume today:
~24 standalone-judge calls plus ~48 panel-judge calls (24 cases × 2 panelists), once
per day, on the cheapest available model tier (Haiku 4.5) plus one Sonnet 5 panelist.
The nightly workflow's own `--fail-under`/`--category-fail-under` pass-rate gate was
**removed** after a live dry run found the corpus is deliberately half designed-to-fail
(raw pass_rate lands ~40-55%, not near the gate's assumed ~67% floor) — evals already
has one documented instance of over-eagerly wiring a threshold-based gate before the
real distribution justified it, a directly relevant cautionary precedent for any new
judge-cost threshold/cap. No `--llm-judge`/`--llm-judge-panel` cost-cap flag, sampling
flag, or budget-alert mechanism exists in `cli.py` today.

## Executive Summary

Across Langfuse, Braintrust, and LangSmith — the three production eval/observability
platforms actually checked — **none ship a judge-specific hard budget cap** distinct
from a generic, gateway-wide spend-cap system: Langfuse offers a sampling-rate knob on
evaluator rules plus an explicit tiered (cheap-code-check-first, LLM-judge-second,
human-escalation-third) reference architecture; Braintrust folds judge cost into one
aggregate cost-per-resolved-request metric with no separate governance category; and
LangSmith's only real budget cap is a generic per-org/workspace/key/user, per-time-
window dollar ceiling with zero judge- or eval-specific logic anywhere in it. Real
academic precedent exists for a smarter-than-uniform, variance-adaptive judge-query
allocation strategy that could in principle extract further savings beyond plain early
stopping (arXiv:2602.15481's bandit-based budget allocation; arXiv:2605.10075's Neyman-
style stratified allocation), but both are validated only on verifiable-answer
benchmarks, not open-ended LLM-judge scoring, and no production tool has operationalized
either. Kelvran's own deterministic-scorer-first design — choosing scorer type
(`deterministic` vs `llm_judge` vs `llm_judge_panel`) per category at corpus-authoring
time — is a real but structurally simpler, static analogue of the industry's runtime
"tiered judging" pattern (which runs a cheap check on every case and escalates only
ambiguous ones to the judge at runtime), so it's a partial match, not the full pattern —
and closing that gap would be a scoring-pipeline design change, not a cost-governance
feature. Given Kelvran's own measured judge volume (a 24-case corpus judged once daily,
on the cheapest model tier) is orders of magnitude below the production-batch-eval-run
scale every precedent above assumes, and given mature commercial platforms themselves
don't build a separate judge-budget-cap feature at any scale, **a dedicated judge-cost-
budget-cap layer for evals is not yet justified** — the real trigger is a measured,
recurring judge-spend line item from production-scale eval runs, not the current CI
corpus.

## Findings

### Finding 1 — Sampling a subset of a run for judging is a real, named production pattern, but is layered on top of (not a substitute for) tiered judging and generic budget caps (confidence: high) — **not_yet: named trigger — build when a single eval run's case count materially exceeds the current 24-case nightly corpus (e.g. a production-scale batch run in the hundreds-to-thousands of cases) such that judging every case becomes the dominant cost driver of that run**

Langfuse's LLM-as-a-Judge evaluator "rules" support an explicit sampling-rate parameter
— rules are defined by "filters, sampling rate, and one or more evaluator assignments"
— and Langfuse's own cost-management guidance names sampling a percentage of traces as
a named cost-control tactic alongside targeting specific observations and using cheaper
judge models [Langfuse LLM-as-a-Judge docs]. This is a real, direct precedent for
"judge only a random subset of a large run" as a production cost lever, not a
theoretical one. By contrast, LangSmith's only real cost-governance mechanism (the LLM
Gateway spend-cap system) contains **zero** mentions of sampling, judging, or
evaluation anywhere in its documentation — its only named cost-spike scenario is a
generic runaway-agent retry loop, not eval/judge volume [LangSmith LLM Gateway spend-
policy docs]. The two systems are not competing approaches to the same problem: Langfuse
addresses judge-call *volume* via sampling; LangSmith addresses runaway *spend* via a
generic dollar ceiling. Kelvran does not need this yet because its one real judge-run
site (the nightly corpus) is fixed-size and small; sampling only pays for itself once
per-run case counts are large enough that judging 100% of cases is itself the cost
driver, which is not Kelvran's current shape.

### Finding 2 — Kelvran's deterministic-scorer-first design is a real, static analogue of tiered judging, but not the more sophisticated runtime-escalation form the industry ships — a genuine but narrow gap (confidence: high) — **not_yet: named trigger — build runtime escalation only if/when a specific category currently always calling `--llm-judge` is shown (via judge-accuracy-corpus data) to be resolvable by a cheap deterministic check for the common case, with judge calls reserved for the ambiguous remainder**

Arize's LLM-as-a-judge guide states the industry-standard split plainly: "Use code
evals for deterministic checks (latency, schema, tokens) and LLM judges for everything
else," because code evals are "fast, deterministic, ... incur no additional token
costs or latency," while LLM-as-judge is reserved for "subjective, nuanced, or open-
ended qualities" [Arize LLM-as-a-Judge guide]. Langfuse's engineering guidance goes
further, describing three explicit layers as **not alternatives**: cheap code checks
on every sampled trace, an LLM judge only on the subset needing semantic judgment, and
escalation of disagreements/low-confidence cases to human annotation [Langfuse AI
agent evaluation guide]. Kelvran's regression corpus already reflects the first half of
this idea — only some categories (e.g. guardrail/cache-attack pattern cases) use
`scorer_type: deterministic`, others use `llm_judge`/`llm_judge_panel` — but that
choice is made once, per category, at corpus-authoring time, not per-case at runtime
based on ambiguity. This is a real but structurally narrower version of the pattern:
it captures "don't pay for a judge when a cheap check suffices for this *category*,"
but not "don't pay for a judge when a cheap check suffices for *this specific case*,
even within a category that sometimes needs one." Whether this narrower gap is worth
closing depends on whether any currently-always-judged category is actually resolvable
deterministically most of the time — a question the existing judge-accuracy corpus data
could answer directly, but hasn't been asked yet. Tiered/uncertainty-gated escalation's
real-world payoff is also strongly model- and task-dependent, not a fixed win: on an
easy benchmark the strongest judge model resolved 45% of cases without escalating to a
costlier retrieval-augmented path, but a weaker judge on the same benchmark resolved
only 9% without escalating [arXiv:2608.17994] — so any future runtime-escalation design
for Kelvran would need its own measurement, not an assumed savings number.

### Finding 3 — Variance-adaptive/informativeness-based judge-query allocation is a real, formally-precedented research direction beyond plain early stopping, but unvalidated on open-ended LLM-judge scoring and not shipped by any production tool (confidence: medium) — **not_yet: named trigger — build only once mSPRT-driven repeated-trial volume itself becomes a measured, material cost line at production scale, and profiling shows that cost is concentrated in trials whose outcome was already low-variance/predictable before they ran**

A recent paper formalizes exactly the question Kelvran's mSPRT early-stopping doesn't
yet answer: "given a fixed computational budget B, how to optimally allocate queries
across K prompt-response pairs to minimize estimation error?" — proposing a variance-
adaptive method that "dynamically allocates queries based on estimated score
variances, concentrating resources where uncertainty is highest" rather than querying
every pair a fixed number of times [arXiv:2602.15481]. A second paper operationalizes
a concrete, no-ground-truth-needed version of the same idea: `m_h ∝ N_h ·
(√(p_h(1-p_h)) + δ)`, using a surrogate model's own self-consistency score as a
variance proxy to decide how many labels/judge-calls to spend per stratum, without
needing any oracle labels to compute the allocation itself [arXiv:2605.10075] — this is
the concrete "prioritize calls on the most-informative cases" mechanism the research
question asked about, and it is real, not hypothetical. Two important caveats apply
before treating this as directly applicable: (1) both papers validate only against
benchmarks with verifiable ground-truth answers (MMLU-Redux, MMLU-Pro, GPQA-Diamond,
HelpSteer2, Summarize-From-Feedback) — the second paper explicitly scopes itself away
from open-ended generation evaluation, stating that remains an open problem outside its
scope, which is exactly Kelvran's LLM-judge use case; (2) mSPRT early-stopping already
captures a large share of the available savings for repeated-trial designs by stopping
as soon as a result is statistically clear — the marginal savings a variance-adaptive
allocator would add *on top of* mSPRT, specifically for open-ended judge scoring, has
no direct empirical measurement anywhere in the literature checked. This is a real,
citable methodology to revisit if Kelvran's repeated-trial judge-call volume becomes
large enough to profile, but adopting it today would be building against an unmeasured
and domain-mismatched savings estimate.

### Finding 4 — No production eval/observability platform checked builds a judge-specific budget cap separate from a generic spend-cap system, and Kelvran's own measured judge volume is far below the scale any precedent assumes — a dedicated judge-cost-budget-cap feature is not yet justified (confidence: high) — **not_yet: named trigger — build when evals' own judge-call spend becomes a measured, recurring cost line item from production-scale eval runs (not the current 24-case nightly CI corpus), e.g. once `evals rollout`/`evals promote`-driven production-trace judging or a materially larger regression corpus makes judge spend visible enough to need active tracking, not just the `Score.cost_usd` field it already captures passively**

Braintrust's own cost-efficiency methodology treats "judge calls for quality and
safety" as one line item folded into a single aggregate cost-per-resolved-request
metric — there is no separate budget cap, sampling strategy, or cost ceiling specific
to judge/eval calls anywhere in the product; a user would have to build custom
metadata tagging themselves just to isolate judge spend from the rest [Braintrust
cost-efficiency case study]. LangSmith's LLM Gateway spend-cap system is real and
production-grade (blocks requests past a cap with a `402`, scoped to
org/workspace/key/user over monthly/weekly/daily/hourly windows), but it is the exact
same class of mechanism as Kelvran's already-shipped gateway-side `budget.Tracker` for
end-user spend, not a distinct judge-specific layer — and its own documentation never
mentions evaluation or judging at all [LangSmith LLM Gateway spend-policy docs]. This
matters directly for the research question: two platforms whose entire product is
built around running LLM evaluations at scale for other companies have not judged a
separate judge-cost-governance feature worth building, folding it instead into either a
generic spend cap or a generic cost metric. Kelvran's own judge volume (once-daily, 24
cases, cheapest model tier) sits well below the scale where either vendor's own
customers would presumably need this — and Kelvran already has the two levers that
matter most at that scale shipped (score-cache reuse, mSPRT early-stopping), plus the
raw cost data (`Score.cost_usd`) already captured for whenever active tracking becomes
worth building. Building a dedicated cap now would be governance infrastructure ahead
of any measured need — precisely the shape of premature feature-building the nightly
workflow's own removed pass-rate gate (built ahead of the real data distribution, then
un-built once live data proved the assumption wrong) already cautions against in this
exact codebase.

## Caveats

- **Vendor-marketing claims did not survive verification and are deliberately excluded
  above.** A vendor blog's claim of a three-stage cascade routing only 5-10% of traffic
  to an LLM judge with a specific "~30x cheaper" dollar figure, a general-purpose
  patterns site's claim of a 50-60% escalation-rate cost-crossover threshold, and the
  `optstop` paper's claimed 57-97% compute reduction were all checked and did not
  survive adversarial verification (0-3 or unanimous-against votes) — either
  unsupported by the cited source or contradicted on direct read. Treat any headline
  cost-reduction percentage from a single vendor blog with real skepticism; none of the
  surviving findings above rest on one.
- **The variance-adaptive allocation literature (Finding 3) has a real domain gap.**
  Both supporting papers validate only on benchmarks with verifiable ground-truth
  answers, explicitly disclaiming open-ended generation evaluation as out of scope —
  exactly Kelvran's LLM-judge use case. The methodology is real and well-precedented;
  its applicability to Kelvran's specific scoring shape is not yet demonstrated
  anywhere.
- **Kelvran's own judge-call volume was estimated from the nightly CI workflow and the
  24-case judge-accuracy corpus, not from any production eval-run telemetry** — there
  is no evidence base yet for what judge-call volume looks like once/if `evals
  rollout`/`evals promote` or production-trace ingestion drives judge calls at a
  materially larger scale. All four "not_yet" verdicts above are conditioned on that
  scale not yet existing; this doc should be revisited once it does.
- **The `optstop` paper (adaptive/Bayesian sequential stopping) is a genuinely
  different mechanism from mSPRT**, not a strictly-better replacement for it — it is
  sequential/adaptive sampling per item, not a tiered-judging scheme and not a budget
  cap. No verified evidence surfaced either confirming or refuting whether it would
  outperform Kelvran's existing mSPRT implementation specifically; that comparison, if
  ever wanted, would need its own dedicated research pass.

## Open Questions

- Does any currently-always-`llm_judge` category in Kelvran's regression corpus have a
  measurable sub-population resolvable by a cheap deterministic check, per Finding 2's
  named trigger — has anyone actually looked at the judge-accuracy corpus data with
  that specific question in mind?
- What does judge-call volume actually look like once `evals rollout`/`evals promote`-
  driven production-trace judging runs at real scale, rather than the fixed 24-case
  nightly corpus — is there a realistic near-term path to that scale, or is it
  speculative?
- If Kelvran ever does build a judge-cost tracking/alerting layer, should it reuse
  `evals trend`/`TrendSnapshot` (already real, already used for `judge_panel_tie_rate`
  threshold alerts) rather than a new mechanism — i.e. is "judge cost governance" mostly
  a new `TrendSeriesName` plus a threshold alert away from existing infrastructure,
  rather than a net-new subsystem?
- Would a variance-adaptive allocator (Finding 3) actually help *on top of* mSPRT for
  Kelvran's specific case shapes, or does mSPRT's own stopping rule already capture
  most of the same variance-driven signal implicitly — has anyone run the numbers on
  Kelvran's own judge-accuracy corpus to check?
