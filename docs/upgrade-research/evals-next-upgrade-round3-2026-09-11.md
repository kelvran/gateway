# Evals Next-Upgrade Scan — Deep Research, Round 3 (2026-09-11)

## Question

Now that `evals/v0.4.0` is tagged (2026-09-11) and every "build now" finding
from both prior rounds has shipped — the judge-accuracy metric, quote-grounded
verdicts, opt-in `--judge-debias`, and (new since round 2) the static-mode
`evals audit-corpus` benchmark-defect-auditing tool — what are the highest-
**value** next additions for `evals`, oriented at genuinely large
opportunities rather than narrow bug-hunting? Specifically: (1) is there a
real 2026 production pattern for giving an operator visibility into WHERE in
a multi-step agent trajectory a failure occurred, distinct from scoring it,
checked against current Langfuse/Braintrust/Arize (Phoenix/AX) feature sets;
(2) is there a mature pattern for AUTOMATED regression-corpus growth from
production failures/drift samples, given `evals ingest`'s `drift_sample`
tier already collects raw material that sits idle behind the fully-manual
`evals promote`; (3) is there real precedent for a unified "eval quality
score" or "corpus health dashboard" aggregating judge-accuracy + quote-
grounding + audit-corpus-defect-rate into one operator-facing signal; (4) is
a cross-run/cross-corpus eval-spend budget/quota concept (distinct from the
gateway's own budget system) a real, expected 2026 feature at Kelvran's scale
or premature; (5) anything else genuinely large and strategic.

## Context (Kelvran's own real, current state)

Rollout Scheduler + `Run` model + JSONL Results Store; deterministic scoring
+ a real 2-judge Bedrock-backed LLM-judge panel (same-vendor, fail-closed
majority) plus standalone `--llm-judge`; `--use-score-cache`; `--judge-axes`;
a real judge-accuracy metric (Cohen's kappa + confusion matrix); quote-
grounded verdicts (measurement-only); opt-in `--judge-debias` (position-
swapped, sequential, fail-closed); a real mSPRT early-stopping rule; a
137-case regression corpus with a real CI gate (`--fail-under`,
`--category-fail-under`, `--tier`, `flaky` exclusion); `evals promote`/
`evals ingest` (S3+GCS, `drift_sample` tier, "no separate review path" —
live-sampled production data feeds `evals promote` the same as any other run
source per `DECISIONS.md`, but the promotion step itself is still 100% human-
triggered); `Span`/OTel tracing (deliberately one span per `Run` today — no
`Trace{spans:[Span]}` wrapper, since the harness makes exactly one
`run_in_sandbox()` call per `Run`; the codebase already names its own trigger
for building that wrapper: "a future multi-step harness making several spans
per `Run`"); `.importlinter` layer enforcement; a nightly judge-accuracy
workflow; `evals audit-corpus` (static-mode, report-only, LLM-audited opinion
on ambiguous task design / wrong ground truth / environment conflicts, per
case). `Run.cost_usd`/`Score.cost_usd` are both already tracked per execution
but never aggregated across runs/time; `THREAT_MODEL.md`'s LLM10 row already
names result/score caching and mSPRT as evals' own spend-cost mitigations.

## Summary

The four research questions converge on a consistent pattern: at every
surveyed 2026 platform (Langfuse, Braintrust, Arize AX/Phoenix), the features
Kelvran might reach for turn out to already be architecturally separated the
same way Kelvran already separates them, or turn out not to exist as claimed
at all. For RQ1, no surveyed platform fuses step-level failure localization
into its scorer — Langfuse's Agent Graphs and Braintrust's nested-span model
are pure navigation/debugging aids, kept in a different span type/view than
the "score" object, which directly validates (rather than contradicts) the
deferred-`Trace{spans:[Span]}` design Kelvran's own `models.py` already names
as correct-when-triggered (Finding 1). For RQ3, three separate vendors'
current docs were checked specifically for a unified "eval quality score" or
"corpus health dashboard" and none has one — Langfuse instead ships trended,
per-evaluator dashboards that are explicitly never fused into a composite
(Finding 2) — so the genuinely large-value move for Kelvran is a trend view
of its own three already-separate signals, not a fused score. For RQ2, Arize
AX's documented "auto-add rules" pipeline is a real, named, shipped mechanism
for automatically flagging production-failure candidates into a dataset by
rule (eval label, latency, tokens, tool activity) while still routing
ambiguous/high-risk cases to human review — a concretely more-automated
pattern than Kelvran's current fully-manual `evals promote`, and directly
actionable against the `drift_sample` material already sitting idle (Finding
3), with a real empirical caveat about self-referential-data degradation
that argues for keeping a periodically-refreshed human-labeled anchor rather
than only a one-time review gate (Finding 4). For RQ4, the evidence lands on
"not yet" for a hard budget/quota system but "build now" for a cheap,
precedented, non-blocking cost-trend-with-threshold-warning surfaced
alongside the eval-quality trend view from Finding 2 (Finding 5) — mirroring
Langfuse's own pattern of pairing cost alerts with evaluator/score alerts on
one surface. No genuinely new sixth finding emerged for RQ5 beyond these
four.

## Findings

### Finding 1 — No surveyed 2026 platform fuses step-level failure localization into scoring; the real pattern is a structurally separate trace/graph-navigation layer, which directly validates Kelvran's own already-deferred `Trace{spans:[Span]}` design (confidence: high) — **confirmed non-gap; forward design spec recorded, not built**

Langfuse's Agent Graphs feature is explicitly a visualization/debugging aid
("understand and debug multi-step reasoning processes") with zero mention of
scoring anywhere on its docs page, and its "Expanded" view — one node per
call, loops unrolled into a DAG in execution order — is explicitly the mode
"to follow a specific run end to end or pin down exactly where something
happened," kept deliberately separate from the "Aggregated" structural-
overview mode. Braintrust's trace model independently confirms the same
separation from the opposite direction: spans nest to mirror actual
execution flow (a structural view, not an aggregate score), and `score` is
its own distinct span **type** in the same enum as `llm`/`tool`/`function`/
`task` — a scorer's output is a layered annotation on the trajectory, not
folded into a step's own record. On the negative side, Arize AX's
Agent-as-a-Judge and Agent Trajectory Evaluations were checked directly and
neither produces step-level fault attribution: both read the full trace/tool-
call sequence and emit one holistic label/score attached to the trace's root
span, explicitly filling the gap left by span-level evals ("check one step...
but miss costly mistakes between steps") with a whole-sequence judgment, not
a per-step one — and Agent-as-a-Judge itself is still gated behind a closed
Enterprise beta requiring sales contact, undercutting any claim it's already
a mature baseline. Put together: the 2026 baseline pattern for "where did it
fail" is a **navigable trace/span hierarchy an operator browses**, entirely
separate from whatever scored the trajectory — exactly the shape Kelvran's
`evals/evals/models.py` already specifies for when its own named trigger
fires ("`Span` joins directly to `Run.id`... A future multi-step harness
making several spans per `Run` is the real trigger for introducing
`Trace{spans: [Span]}`"). **Verdict**: do not build this now — Kelvran's
Rollout Scheduler still makes exactly one sandboxed call per `Run` (round
2's Finding 1: binary final-state scoring is itself still the correct SOTA
match), so there is no multi-step trajectory yet to visualize. The forward-
compatible value of this round's research is confirming the *shape* to build
when the trigger fires: nested spans as a pure structural/navigation model,
with any future per-step signal living in a `score`-analogous span type kept
separate from the execution-step spans — not a redesign of `Score` itself,
and not a step-level verdict fused into `Span`.

### Finding 2 — No surveyed 2026 platform (Langfuse, Phoenix, Braintrust) ships a unified "eval quality score"/corpus-health metric aggregating judge-accuracy + agreement + defect-rate signals; the real, precedented, high-value move is a trended dashboard of Kelvran's own already-separate signals, not fusing them (confidence: high) — **build now: trend view; do not build: fused composite score**

Three independent vendor doc-sets were checked specifically for this concept
and none has it. Langfuse's Score Analytics dashboard trends evaluation
scores over time **across multiple evaluators side by side** — its own
"Compare Two Scores" feature explicitly computes correlation/agreement
*between* two distinct named score series (e.g. two different judge models'
helpfulness scores) rather than merging them, and is capped at comparing two
scores at a time; there is no aggregation/averaging mechanism anywhere in its
Scores/Evaluation docs tree. Langfuse also confirms LLM-as-a-Judge scores,
human annotations, and custom API/SDK scores are three parallel,
separately-tracked score types feeding the same underlying `Score` object
(distinguished by a `source` field), never distilled into one number. Arize
Phoenix's own documented evaluation feature set (LLM-based evaluations,
dataset evaluators, evaluator integrations, human annotations) was checked
directly for any unified-quality-score or health-dashboard language and none
exists anywhere on the page. Braintrust's `/docs/evaluate` page was checked
the same way with the same result — only separate, non-aggregated Scorers/
classifiers and Online scoring, no fused number, and its docs index shows
dashboards are something a user assembles ad hoc (via an AI-agent builder
feature), not a shipped predefined health metric. **Verdict**: building a
single fused "eval quality score" for Kelvran (combining judge-accuracy
kappa + quote-grounding rate + `audit-corpus` defect rate into one number)
would run against, not with, the grain of every surveyed platform — do not
build that. What genuinely is a real, precedented, currently-missing, and
comparatively cheap value-add is what Langfuse actually ships: a **trended,
side-by-side view of Kelvran's own three already-separate signals** (judge-
accuracy kappa from the nightly workflow, quote-grounding rate from
`Score.quote_grounded`, and `audit-corpus`'s defect rate) plotted per run/per
CI invocation over time, each kept as its own named series — not combined.
This is directly buildable: `evals/evals/results_store.py`'s JSONL append
model is already the right shape to accumulate a small per-invocation
summary row for each of the three signals, and `evals/evals/cli.py`'s
`report_cmd`/`audit_corpus_cmd` already compute the underlying numbers on
each invocation — the new work is persisting and re-surfacing them over time,
not computing anything new.

### Finding 3 — Arize AX documents a real, named, shipped "auto-add rules" pipeline for automatically mining regression-corpus candidates from production failures by rule, with ambiguous/high-risk cases explicitly routed to human review rather than auto-promoted — a more-automated pattern than Kelvran's fully-manual `evals promote`, directly actionable against the `drift_sample` material already sitting idle (confidence: high) — **build now: rule-based auto-flag step; keep human gate for promotion itself**

Arize's own product docs (not just its blog) confirm this as a real, shipped
feature, not marketing copy: "Auto-add rules" let a team define rules that
automatically add new examples to a dataset whenever incoming spans match
criteria — by evaluation label, latency, token count, or specific tool
called — described explicitly as keeping "the dataset current with what's
actually happening in production, without manual curation." The
documentation's own recommended discipline is important and directly
transferable: "Use automatic collection for clearly defined conditions. Send
ambiguous or high-risk failures through human review before promoting them
into the permanent dataset" — i.e. even Arize's most-automated competitor
still gates final promotion on a human for anything not clean-cut, it just
automates the *flagging* step, not the *promotion* step. By contrast,
Braintrust's documented pattern ("Feed back — pull interesting production
traces into datasets") is fully manual filter-then-export with no rule-based
auto-flagging at all — confirming Kelvran's current `evals promote`
(entirely human-triggered, no automatic candidate surfacing) sits at the
*less*-automated end of what 2026 platforms actually ship, not the norm.
**Applicability to Kelvran, with an honest caveat**: `evals ingest` already
decodes `drift_sample`-tier material from live-sampled production gateway
events, but per `evals/ARCHITECTURE.md`'s own accounting, `GatewayDecisionEvent`
carries only a narrow outcome/trace-ID schema — no prompt/completion content
(`Run.stdout` is always `""`, `EvalCase.reference` is always `None` for
ingested cases) — so an Arize-style auto-add rule keyed on "evaluation label"
in the rich sense (a judge's own pass/fail) cannot apply to ingested rows
today, since ingested `drift_sample` runs never get a `Score` at all (no
content to judge). What genuinely is buildable now, using fields that do
exist in the decoded event (outcome/latency/cost/routing-decision metadata):
a lightweight auto-flag pass over already-ingested `drift_sample` `EvalCase`s
that surfaces candidates matching a rule (e.g. a gateway-reported failure
outcome, an anomalous latency/cost, a specific provider/model routing path)
as promotion *candidates* for a human to review via the existing `evals
promote` command — not a new promotion path, an automated funnel feeding the
existing one. This is additive to `evals/evals/cli.py`'s `ingest_cmd`/
`promote_cmd` and needs no new dependency or schema change to `EvalCase`/
`Run`.

### Finding 4 — Empirical evidence that even substantial (10%) real-data retention only reduces, never eliminates, degradation when training on self-generated data across generations directly argues for retaining a periodically-refreshed human-labeled anchor, not just a one-time human-review gate, in any automated corpus-growth mechanism (confidence: medium) — **caveat/design constraint on Finding 3, not a standalone build item**

The Nature/arXiv "model collapse" line of work (same author team) shows that
even when 10% of original, non-model-generated data is retained per training
round, degradation is not eliminated, only reduced to "minor" — and fully
replacing original data with model-generated data across multiple generations
measurably worsens performance (perplexity loss of 20-28 points in the
controlled experiment). While this literature is about training-data feedback
loops rather than eval corpora specifically, the structural risk transfers
directly to Finding 3's auto-flag mechanism: if Kelvran ever lets auto-flagged,
LLM-judge-scored production cases become the corpus's *dominant* source of
"ground truth" over many promotion cycles, the same self-referential-drift
risk applies — the judge's own errors and the model-under-test's own quirks
could compound into the very corpus later used to validate them, with no
external check. **Design implication, not a new build item**: any auto-flag/
auto-add mechanism built per Finding 3 should keep the hand-curated,
human-labeled slices (`regression_corpus_guardrail.json`,
`regression_corpus_judge_accuracy.json`) as a durable, periodically-refreshed
anchor that auto-flagged cases are validated against or diluted by — not
merely a point-in-time human review at ingestion — echoing round 1's already-
deferred drift-attribution finding (a frozen human-labeled anchor set) rather
than resolving it, but now with a concrete mechanism (Finding 3) that makes
the anchor's importance load-bearing sooner than round 1 anticipated.

### Finding 5 — A hard cross-run spend budget/quota system for evals itself has no real trigger yet, but a cheap, precedented, non-blocking cost-trend-with-threshold-warning is a genuinely large, low-risk value-add that pairs naturally with Finding 2's trend view (confidence: high on the pattern; medium on Kelvran-specific sizing) — **not yet: hard budget/quota; build now: cost-trend + threshold warning**

The FinOps Foundation's own current AI/LLM cost-governance guidance advises
"usage limits, quotas, and throttling mechanisms combined with anomaly
detection" for AI/LLM workloads generally — real, citable, multi-company
practitioner guidance, though advisory ("should"/"consider"), not a hard
mandate. Langfuse's concrete implementation of this pattern for its own
users is instructive: cost is a first-class alertable metric (e.g. p95 cost
on Observations) with a two-tier warning/alert threshold over a configurable
lookback window — but every delivery channel is notify-only (Slack, signed
webhook, GitHub Actions dispatch); nothing in Langfuse's alerting halts
execution or caps spend. Critically, Langfuse explicitly ties this to
**evaluator** workflows specifically: alerts can be created directly from
evaluator pages, prefilled with both score and cost metrics, with test runs
excluded from the alerting scope — a direct precedent for pairing eval-
quality signals with cost signals on one surface, distinct from (and smaller
than) Finding 2's dashboard concept. **Verdict for Kelvran**: a hard spend
quota/budget-enforcement system scoped to evals itself is "not yet" — no
production-scale eval spend exists to size thresholds against, and Kelvran's
own architecture already places hard budget *enforcement* at the gateway
layer for live traffic (a different trust boundary and a different problem:
blocking real user-facing spend vs. an internal CI/regression tool's own
run cost); duplicating that machinery inside `evals` would cut against
`AGENTS.md`'s existing dependency-direction discipline for no proven need.
What is genuinely valuable and cheap: `Run.cost_usd`/`Score.cost_usd` are
already tracked per execution but never aggregated or trended anywhere — a
non-blocking cost-trend line, paired with Finding 2's judge-accuracy/quote-
grounding/defect-rate trend view and surfaced via the same persisted-summary
mechanism, with an optional soft warning (print, not fail) when a run's total
cost crosses an operator-configured threshold, mirrors exactly what Langfuse
ships (notify, don't block) and needs no new infrastructure beyond what
Finding 2 already proposes building.

## Caveats

- Findings 1-2 rest partly on **negative/absence claims** (a feature does
  *not* exist on a given vendor's current docs page) — these were verified
  by direct, live fetches of the specific pages cited, but a vendor could add
  such a feature after this pass's fetch date (2026-09-09/10) without this
  report reflecting it; treat the absence claims as current-as-of-fetch, not
  permanent.
- Finding 3's "auto-add rules" pattern is confirmed as real and shipped by
  Arize's own product docs (not just its blog), but this pass did not
  independently verify the exact schema/fields available on Kelvran's
  decoded `GatewayDecisionEvent` beyond what `evals/ARCHITECTURE.md` already
  documents (no prompt/completion content) — the concrete rule fields
  proposed (outcome/latency/cost/routing metadata) are a reasonable inference
  from that doc, not a field-by-field audit of `evals/evals/ingestion/`.
- Finding 4's model-collapse evidence is drawn from training-data literature,
  not eval-corpus-specific research — the transfer to eval-corpus growth is
  this report's own structural argument, not a claim the cited papers made
  themselves; treat it as a well-reasoned caveat, not a directly-measured
  Kelvran risk.
- Several claims in this round's verification hit search-tool rate limits
  (Exa, Tavily) partway through, so corroboration for a few claims rested on
  primary-source-only confirmation without independent third-party
  triangulation — flagged per-claim by the verifiers, and consistent with
  both prior rounds' experience with the same tools.
- Several claims were explicitly refuted during 3-vote verification and are
  deliberately excluded from the findings above: Arize AX's agent trajectory
  eval does NOT produce step-level verdicts (0-3, used as negative evidence
  *for* Finding 1, not a standalone finding); Weave's per-op scorer
  application to individual trace spans (1-2, insufficiently supported);
  Phoenix's tracing as a distinct scored-checkpoint concept (0-3); two of the
  model-collapse framing claims about MIDS/fairness erosion (1-2 each) — do
  not resurface these without new evidence.

## Recommendation for Kelvran

1. **Build now, no new infrastructure beyond what's already computed**: a
   persisted, trended history of Kelvran's own three already-separate eval-
   quality signals (judge-accuracy kappa, quote-grounding rate, `audit-corpus`
   defect rate) plus a non-blocking cost trend — surfaced side by side, never
   fused into one composite number (Findings 2 and 5). This is the single
   highest-value item this round: it is directly precedented (Langfuse ships
   exactly this shape), genuinely large in operator value (turns three
   one-shot CLI numbers into a real trend an operator can act on), and needs
   no new dependency — `evals/evals/results_store.py`'s JSONL model and
   `cli.py`'s existing `report_cmd`/`audit_corpus_cmd` computations are the
   right building blocks.
2. **Build now, additive, human-review gate preserved**: a rule-based
   auto-flag pass over already-ingested `drift_sample` `EvalCase`s that
   surfaces promotion *candidates* for `evals promote` using outcome/latency/
   cost/routing fields that exist in the decoded gateway event today (Finding
   3) — paired with a durable, periodically-refreshed human-labeled anchor
   discipline rather than a one-time review gate (Finding 4's caveat).
3. **Do not build**: a fused "eval quality score"/corpus-health composite
   number (Finding 2) and step-level failure-verdict fusion into scoring
   (Finding 1) — both run against, not with, the grain of every surveyed 2026
   platform.
4. **Not yet, correctly deferred with a named trigger**: a hard cross-run
   spend budget/quota-enforcement system scoped to evals itself (Finding 5) —
   trigger is real production-scale eval spend large enough to need blocking,
   not just trending; `Trace{spans:[Span]}` for multi-step trajectories
   (Finding 1) — trigger is unchanged from what `evals/evals/models.py`
   already names (a future multi-step harness making several spans per
   `Run`).

## Open Questions

- What are the actual field names/types available on a decoded
  `GatewayDecisionEvent` (per `evals/evals/ingestion/mapping.py`) that Finding
  3's auto-flag rules could key on — is there enough routing/outcome/cost
  granularity to write a meaningfully selective rule, or would every ingested
  `drift_sample` case match too broadly to be useful without richer content?
- For Finding 2's trend view: should the three signals (judge-accuracy,
  quote-grounding, audit-corpus defect rate) be recomputed and appended on
  every CI run automatically, or only on explicit operator request — and
  does either choice change `report_cmd`'s existing `--fail-under` gate
  semantics if a trend regression is ever wired into CI later?
- How large would Kelvran's real eval-run cost need to become before Finding
  5's "not yet" on hard budget enforcement flips to "build now" — is there a
  concrete dollar/run-count threshold worth pre-registering now (as a
  documented trigger, per this project's own "name the exact trigger"
  discipline) rather than deciding reactively later?
- Finding 4 borrows evidence from training-data model-collapse research
  applied to eval corpora by structural analogy, not direct study — is there
  eval-corpus-specific literature (rather than training-data literature) on
  self-referential drift in automatically-grown regression suites that a
  future pass should look for specifically?
