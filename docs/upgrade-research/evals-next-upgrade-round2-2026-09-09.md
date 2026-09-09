# Evals Next-Upgrade Scan — Deep Research, Round 2 (2026-09-09)

*(This file was reconstructed from the completed workflow's structured result after the underlying run did not itself write the file to disk — the content below is the verified research output verbatim, not re-derived.)*

## Question

Now that `evals/v0.3.0` is tagged and every "build now" finding from the
prior round (`docs/upgrade-research/evals-next-upgrade-2026-09-09.md`) has
shipped — a judge-accuracy metric (Cohen's kappa + confusion matrix),
quote-grounded verdicts (measurement-only), and opt-in position-swapped
prompt-debiasing — what genuinely **new** gaps does a fresh 2026
production/research eval-tooling survey surface for `evals` specifically,
comparing against Inspect AI, promptfoo, DeepEval, Braintrust, OpenAI
Evals, LMSYS/Chatbot-Arena-style pairwise comparison, and SWE-bench-/
terminal-bench-style agentic-eval harnesses?

## Context (Kelvran's own real, current state)

Rollout Scheduler + `Run` model + JSONL Results Store; deterministic
scoring + a real 2-judge Bedrock-backed LLM-judge panel (same-vendor,
fail-closed majority-vote) plus standalone `--llm-judge`; `--use-score-cache`
(versioned for `axis` and `debias`); `--judge-axes`; the new judge-accuracy
metric (`--judge-accuracy PATH`); quote-grounded verdicts (`quote_grounded`
on `JudgeResult`/`PanelVote`/`Score`, measurement-only); opt-in
`--judge-debias` (sequential, position-swapped, fail-closed on
disagreement); a real mSPRT early-stopping rule; a 137+-case regression
corpus with a real CI gate; `evals promote`/`evals ingest`; `Span`/OTel
tracing; `.importlinter` layer-contract enforcement; a nightly
judge-accuracy workflow.

## Summary

Synthesizing the 21 verified claims against this round's research
questions: the field's SOTA agentic-coding benchmarks (SWE-bench,
Terminal-Bench, GSO, SWE-bench-Live) confirm binary final-state/unit-test
scoring — not trajectory judging — is the 2026 standard, so Kelvran's
Rollout Scheduler + deterministic/judge scoring has no real structural gap
against that comparison (Finding 1). The single highest-value **new**
finding is benchmark-corpus auditing: a 2026 tool (ABA) found >25% major-
severity defects across 168 benchmarks/34K tasks, and filtering flawed
tasks moved SWE-bench-Verified/Terminal-Bench-2 scores ~10% — directly
actionable against Kelvran's own 137+-case regression corpus, and
Kelvran's existing Span/OTel trace capture uniquely positions it to run
the higher-yield "trajectory-mode" audit variant rather than static-only
(Findings 2-3). A second concrete, low-risk finding is a threat-model
correction: the JudgeDeceiver paper shows position-swap debiasing
(Kelvran's shipped `--judge-debias`) collapses naive prompt-injection
success but barely dents optimized/adaptive injection (79-87% attack
success survives) — `THREAT_MODEL.md` should not imply debiasing is an
injection defense (Finding 10). A same-vendor, single-call random-number-
probe debiasing technique is a promising, cheaply-testable composable
addition via the already-shipped Cohen's-kappa judge-accuracy harness
(Finding 6), while variance-adaptive bandit judge-budget allocation is a
plausible but not-yet-triggered cost-governance upgrade (Finding 4), and
adaptive cheap-then-escalate judge routing was explicitly tested and
rejected by its own authors (Finding 5). CreaEval's decoupled-judging
architecture, PU-learning bias audits, and DVC-style corpus versioning are
all real 2026 techniques with no current Kelvran trigger (Findings 7-9).
Notably, no claim answered the round's first research question: no
principled, data-independent methodology for graduating quote-grounding
from measurement-only to an enforced gate was found in this pass either —
that remains genuinely open.

## Findings

### Finding 1 — Binary final-state/unit-test scoring, not trajectory judging, is 2026 SOTA across the whole agentic-coding-benchmark family Kelvran's Rollout Scheduler should be compared to (confidence: high) — **confirmed non-gap, do not build**

terminal-bench, SWE-bench Verified/Pro, GSO, Terminal-Bench 2.0, and
SWE-bench-Live all validate "via unit tests or observations of the
environment's final state" (confirmed via a 2026 survey paper,
arXiv:2604.00594); terminal-bench's grading is a strict AND over pytest
sub-results (no partial credit) plus `pass_at_k` aggregation; SWE-bench-
Live's live leaderboard tracks only "Resolved" as its binary primary
ranking metric with no partial-credit field. terminal-bench's Docker-
sandboxed harness-connects-agent-to-sandbox structure is directly
analogous to Kelvran's `sandbox.py`+`scheduler.py`. **Conclusion**: Kelvran
should **not** build trajectory-beyond-final-output scoring — its current
approach matches the field.

### Finding 2 — When agentic benchmarks DO capture trajectories, 2026 practice uses them for anti-cheat/leakage verification or runtime/environment-drift auditing, never partial-credit scoring (confidence: high) — **structural context for Finding 3**

SWE-bench-Live's Aug-2026 submission rule requires raw agent rollout
trajectories purely so maintainers can verify no ground-truth leakage
occurred (a separate `results.json` carries actual scoring — trajectories
are provenance evidence, not a scoring input). Separately, ABA's
trajectory-mode auditor (reading recorded execution traces, not just
static task/prompt/grader text) flags 8.5% more major-severity tasks than
static-mode across 8 benchmarks, and uniquely surfaces runtime
contamination/environment drift invisible to static review. **Relevance**:
Kelvran already captures `Span`/OTel trace data per-run — it is
structurally ready to run a trajectory-mode audit of its own regression
corpus rather than a static-only one, which the evidence says would catch
strictly more real defects for the same infra it already has.

### Finding 3 — Automated benchmark auditing at scale found >25% of tasks across 168 benchmarks/34,285 tasks carry major, task-breaking defects, and filtering moved scores ~10% on the two benchmarks structurally closest to Kelvran's own harness (confidence: high) — **BUILD NOW**

ABA's direct PDF text confirms 25.7% major-defect / 15.1% minor-defect /
<60% fully-clean rates across the full corpus (ambiguous design,
environment conflicts, wrong ground truth), and a 9.9%/9.6% average-score
increase plus real ranking shifts specifically on SWE-bench Verified and
Terminal-Bench 2 after removing flawed tasks. **Recommendation**: run a
lightweight static (or, per Finding 2, trajectory-mode) audit pass for
ambiguous-task-design/ground-truth-correctness/environment-conflict
defects against Kelvran's own 137+-case regression corpus. The field-wide
base rate (>25%) is high enough that Kelvran should not assume its own
corpus is immune, and its existing `revision` field + CI gate
infrastructure make acting on findings cheap once surfaced.

### Finding 4 — Variance-adaptive bandit judge-call allocation across a whole corpus run is a validated, published cost-governance mechanism, distinct from Kelvran's existing per-case mSPRT (confidence: medium) — **not yet, worth a lightweight prototype once judge-call volume grows**

"LLM-as-Judge on a Budget" derives a worst-case error bound tied to
per-item variance/budget and reports its variance-adaptive allocation
method significantly outperforms uniform allocation on Summarize-From-
Feedback and HelpSteer2 at equal total budget. This addresses budget
allocation **across** a whole corpus run (which case gets how many judge
calls) — distinct from Kelvran's existing per-case mSPRT early-stopping.
**Verdict**: not yet in full rigor (the formal bandit/concentration-
inequality bound isn't implemented), but the core signal (per-item score
variance) is computable without scipy/numpy, unlike the already-deferred
bootstrap/Bayesian statistics — worth a lightweight prototype once
judge-call volume across the corpus grows large enough to matter for
spend.

### Finding 5 — Adaptive cheap-then-escalate judge routing was tested and explicitly rejected by its own authors — do not build this for Kelvran's panel (confidence: medium) — **explicitly rejected, not a gap**

The variance-adaptive-allocation paper's own §4.4/5.1 states "we
ultimately do not recommend any of the three [escalation] variants" —
hard variance-based routing (cheap proxy judge for easy cases, escalate to
an expensive judge on high variance) has a large dead zone where
escalating some-but-not-all responses rarely changes the outcome (weak
variance-correctness correlation, r=-0.13). **Verdict**: explicitly do
**not** build a cheap-Haiku-first-escalate-to-Sonnet-on-disagreement scheme
for Kelvran's panel; published evidence says this tempting-looking
optimization doesn't pay off.

### Finding 6 — A same-vendor, single-call random-number-probe judge-debiasing technique exists, mechanically distinct from position-swap, and is cheaply testable against Kelvran's own judge-accuracy corpus (confidence: high) — **BUILD NOW as a prototype**

A recent (Aug 2026) paper's technique: instruct the judge to randomly
generate number tokens, measure its latent numeric bias as deviation from
uniform, then rectify the judge's actual scoring-token probabilities at
evaluation time via `softmax(l(y)+λ·B(y))` — one-time offline calibration,
same judge model only, no second call and no cross-model requirement, and
specializable per downstream task by injecting the task definition into
the probe prompts. **Recommendation**: build now as a prototype — composes
cleanly with Kelvran's existing same-vendor Bedrock judges (no new vendor
needed), and can be objectively evaluated against Kelvran's own
hand-labeled 24-case corpus using the already-shipped Cohen's-kappa
judge-accuracy metric before deciding whether to ship it for real, since
the paper is very recent and needs independent validation on Kelvran's own
data, not just the paper's benchmarks.

### Finding 7 — CreaEval's decoupled evidence-extraction/judging architecture mitigates verbosity and leniency bias in multi-step tasks, but is a heavier change than Kelvran's shipped panel with no current trigger (confidence: high) — **not yet, longer-horizon RFC candidate**

CreaEval decouples LLM-as-judge scoring into two phases — a
memory-augmented analysis step extracting structured evidence from
multi-step responses, then a separate judging step scoring only that
evidence, never the raw response — explicitly to mitigate verbosity bias
and leniency bias in complex multi-step creativity tasks. **Verdict**: not
yet for Kelvran — a heavier architectural change (two sequential calls, an
evidence-extraction schema) than the shipped single-call panel; Kelvran's
quote-grounding (measurement-only) is a lighter partial mitigation for an
overlapping failure mode (unsupported claims). Worth flagging as a
longer-horizon RFC candidate specifically if/when Kelvran's judged rollout
trials become more genuinely multi-step-agentic than they are today — no
trigger observed yet.

### Finding 8 — A PU-learning + Partial Optimal Transport bias-audit framework exists but is an uncited, ~3-month-old preprint with no independent replication (confidence: low) — **not yet, track don't act**

A positive-unlabeled (PU) learning + Partial Optimal Transport framework
can audit and correct LLM-judge bias using only a small human-verified
positive set matched against a larger pool of unlabeled outputs in a
shared embedding space, without retraining the judge. **Caveat explicitly
raised during verification**: this is an ~3-month-old, uncited preprint
with no independent replication yet. **Verdict**: not yet — trigger is
independent validation/citation accumulation before Kelvran should
consider adopting; track, don't act.

### Finding 9 — DVC-style git-tracked pointer-file versioning doesn't apply to Kelvran's corpus today; the real trigger is large binary artifacts, which don't yet exist in the corpus (confidence: medium) — **not yet, correctly deferred**

DVC's core mechanism is git-tracked lightweight pointer/metafiles standing
in for large data/model files (stored separately in cache/remote), with
each revision tied to a git commit for GitOps-style PR review and an
auditable immutable history. **Gap check vs. Kelvran**: Kelvran's eval
cases are structured JSON/text already tracked directly in git with an
existing `revision` field — already git-commit-tied and PR-reviewable by
nature, so DVC's large-binary-pointer problem doesn't apply today.
**Verdict**: not yet — the real trigger would be the corpus starting to
bundle large binary artifacts (recorded trajectories, screenshots, sandbox
images) that don't belong directly in git; not the current state at
137+ text-based cases.

### Finding 10 — Optimized/adaptive judge prompt-injection (JudgeDeceiver) survives position-swap debiasing at 79-87% attack success; `THREAT_MODEL.md` should not describe `--judge-debias` as an injection defense (confidence: high) — **BUILD NOW, documentation-only fix**

JudgeDeceiver achieves 88-93% attack success against LLM-as-judge systems
(vs. 10-40% for handcrafted/GCG attacks) on tested open-weight judges, and
— critically — position-swap debiasing (the exact class Kelvran just
shipped as `--judge-debias`) collapses naive/handcrafted injection success
almost to zero but only modestly reduces the optimized attack: position-
agreement-consistency under the paper's own swapped-order check stays at
79-87% for the optimized attack, meaning it still succeeds most of the
time even with position swapping, while handcrafted-attack consistency
collapses to ~0.2%. The paper's own authors' conclusion: their attack is
"robust against positional bias." **Recommendation**: build now as a
`THREAT_MODEL.md` correction — explicitly document that Kelvran's shipped
`--judge-debias` is a bias-consistency check, **not** a defense against
adaptive/optimized judge prompt-injection via task/agent output; this is a
cheap, concrete documentation fix that prevents the threat model from
overstating what's already shipped. A real technical mitigation
(sanitizing/structurally separating untrusted task output from judge
instructions) remains a separate, larger open question.

## Caveats

- No confirmed claim in this round directly answered the round's first
  research question (a principled, data-independent methodology for
  graduating quote-grounded-verdict from measurement-only to an enforced
  gate) — this remains an open gap rather than a found-and-rejected
  option; treat its absence as informative (no 2026 established practice
  was located), not as proof none exists.
- Several sources are very recent (Aug-Sept 2026) uncited preprints — most
  notably the PU-learning/Partial-Optimal-Transport paper (Finding 8) and,
  to a lesser extent, the CreaEval and random-number-debiasing papers
  (Findings 6-7) — weight as emerging/unvalidated rather than established
  practice until independently replicated.
- The JudgeDeceiver paper (Finding 10) is ~2.5 years old (March 2024); its
  scope doesn't go stale, but it was tested only against open-weight
  judges (Mistral, Openchat-3.5), not against Bedrock-hosted Claude
  Sonnet/Haiku — so its exact attack-success numbers may not transfer
  directly to Kelvran's actual judge models.
- Exa/Tavily web-search tools were repeatedly rate-limited during
  verification for several claims — corroboration rested on primary-
  source-only confirmation without independent third-party triangulation
  in those cases (flagged per-claim by verifiers, not a fatal weakness
  given primary-source strength, but a real search-coverage limitation).
- Two claims were dropped for insufficient support during voting (a
  claimed Terminal-Bench 2.0 multi-attempt/threshold-based partial-credit
  conversion, and a claim that a combined criteria-injection+ensembling
  technique reached 85.8% judge accuracy) — do not cite these as Kelvran
  findings.

## Recommendation for Kelvran

1. **Run a lightweight benchmark-audit pass against the regression corpus**
   (Findings 2-3) — static, or better, trajectory-mode using Kelvran's
   existing Span/OTel captures, since trajectory-mode catches strictly
   more real defects for the same infra Kelvran already has.
2. **Correct `THREAT_MODEL.md`** (Finding 10) — document `--judge-debias`
   as a bias-consistency check, not an injection defense. Docs-only, cheap,
   do it now.
3. **Prototype the random-number-probe debiasing technique** (Finding 6)
   against the existing 24-case judge-accuracy corpus via the already-
   shipped Cohen's-kappa metric, before deciding whether to ship it for
   real.
4. **No action needed, confirmed non-gaps**: trajectory-beyond-final-output
   scoring (Finding 1) and adaptive cheap-then-escalate judge routing
   (Finding 5) — both explicitly should NOT be built.
5. **Not yet, correctly deferred**: variance-adaptive bandit judge-budget
   allocation (Finding 4, revisit once judge-call volume grows), CreaEval's
   decoupled-judging architecture (Finding 7, revisit if rollout trials
   become genuinely multi-step-agentic), PU-learning bias audits (Finding
   8, needs independent replication), DVC-style corpus versioning
   (Finding 9, needs large binary artifacts to enter the corpus).

## Open Questions

- Is there any principled, data-independent methodology (not requiring
  months of production traffic) for deciding when to flip quote-grounding
  from measurement-only to an enforced gate — this round found none, so is
  the right next step to define an internal statistical threshold policy
  now, or to keep waiting for real production data as originally planned?
- Would an ABA-style audit (static or, better, trajectory-mode using
  Kelvran's existing Span/OTel captures) of Kelvran's own 137+-case
  regression corpus actually surface major-severity defects anywhere near
  the 25.7% field-wide average, and would filtering them shift Kelvran's
  own pass-rate/Wilson-CI numbers the way it moved SWE-bench Verified/
  Terminal-Bench 2 by ~10%?
- Does the random-number-generation debiasing technique measurably improve
  Kelvran's own Cohen's-kappa judge-accuracy metric on its hand-labeled
  24-case corpus enough to justify the added offline-calibration cost,
  given the technique is unvalidated outside its own paper?
- Does JudgeDeceiver-style optimized prompt injection actually transfer to
  Kelvran's real judge models (Bedrock Claude Sonnet 5 / Haiku 4.5) and
  real sandboxed-rollout task-output channel, or is the 88-93% attack-
  success figure specific to the open-weight models (Mistral, Openchat-3.5)
  the paper tested and not representative of Kelvran's actual exposure?
