# Does Kelvran's evals harness need multi-step agentic-trajectory benchmarking? (2026-09-13)

**Research target:** whether `evals/evals/rollout/scheduler.py`'s one-`EvalCase`→one-`run_in_sandbox()`→one-`Score`
model needs to become a multi-turn, tool-use-trajectory harness like SWE-bench, terminal-bench,
tau-bench/tau2-bench, or AgentBench — and, critically, whether that would be genuine unmet need or
scope creep for a product that is an **LLM gateway + cache** testing its own routing/cache/guardrail
correctness, not an agent framework.

**Ground truth confirmed in-repo before external research began:** `run_suite()` (scheduler.py:94-170)
is a `for case in cases:` loop — one `EvalCase` → one `run_in_sandbox()` Docker invocation → one `Run` →
one `Score` (deterministic, single LLM-judge, or 2-judge panel). `Run.harness_config` (models.py:71-110)
is deliberately narrowed to `{image, command, timeout_s}` — the literal single sandbox invocation — not
the `scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier` set that
`evals/ARCHITECTURE.md` already sketches for "a future pluggable multi-step agent harness that doesn't
exist yet." `Run` has no `Trace`/`Span` field for a multi-step trajectory — only one `Span` per real
sandbox attempt. There is no multi-turn agent loop, no tool-use trajectory concept, and no Task/Dataset
Registry anywhere in this codebase. `PRD.md` (line 30) names "massively parallel (10K+) eval rollouts"
and, separately, **"agent tool-call/sandbox-state caching"** and **"MCP/A2A tool brokering"** as explicit
v1 non-goals — the last two matter here because they are the exact upstream product features that would
someday *create* a multi-step trajectory to evaluate. 18 claims survived 3-vote adversarial verification
against current (2026) external precedent; findings below merge semantic duplicates and are each tagged
`build_now`, `not_yet`, or (where the honest answer is stronger than "later") `out of scope`.

## Executive Summary

**Verdict: out of scope for the core apparatus (orchestrators, user-simulators, tool-call trajectories,
pass^k reliability, dense subtask credit); not_yet, and arguably never, for the rest.** The 2026 external
landscape (Terminal-Bench 2.0, tau-bench/tau2-bench, Thinkingbox, Long-Horizon-Terminal-Bench) confirms
multi-turn agentic benchmarking is a real, fast-moving, well-instrumented discipline — but every one of
these systems exists to answer "did the **agent** complete a multi-step task," a question about an
*agent's* reasoning and tool-use competence. Kelvran's evals exist to answer a categorically different
question: "did the **gateway/cache/router** make the correct infrastructure decision on this one request."
Even the field's own comparably-scoped precedent — LiteLLM's official eval tutorials — models evaluation
as pure single-turn input→output→score, with zero agentic-trajectory concept anywhere in its docs,
exactly matching Kelvran's shape. The strongest reason this is scope creep rather than deferred work: the
two product features that would *justify* building a trajectory harness (MCP/A2A tool brokering; agent
tool-call/sandbox-state caching) are themselves explicit, named v1 non-goals in `PRD.md` — building the
evaluation infrastructure before the product capability it would evaluate is inverted effort. The one
place external research legitimately sharpens Kelvran's *existing* single-shot model rather than replacing
it is grading philosophy: outcome-only/final-state scoring (which Kelvran already does) is the field's own
dominant default, not a shortcut Kelvran happened to take, but it also has a documented failure mode
("corrupt success" — passing while violating policy along the way) that maps directly onto Kelvran's own
guardrail-correctness testing and is worth a narrow, low-cost mitigation now.

## Findings

### Finding 1 — Multi-turn/tool-use trajectory harnesses (Terminal-Bench 2.0, tau-bench/tau2-bench, Thinkingbox) are a real, mature, well-instrumented 2026 discipline, structurally unlike Kelvran's model
**Confidence: high** (primary sources: ICLR 2026 Terminal-Bench paper; sierra-research/tau2-bench + tau-bench GitHub repos and arXiv paper; Thinkingbox arXiv paper, Microsoft-affiliated)
**Tag: not applicable to Kelvran as a build target — informational baseline only**

Terminal-Bench 2.0 tasks are explicitly interactive: "once the instruction and Docker container are
provided to an agent, it must explore and manipulate the environment by calling tools... to complete the
task" — a series of turns, not one command. tau-bench/tau2-bench "emulate dynamic conversations between a
user (simulated by language models) and a language agent" via an `Orchestrator` that coordinates
agent↔user↔tools across turns, with each domain defining a policy, agent tools, tasks, and optional
user-side tools. Thinkingbox structures each task as a POMDP-style trajectory where "the agent emits
either a user-facing message or a tool action" inside an isolated tool session managed turn-by-turn by an
orchestrator, recording the full trajectory ρ. All three are genuinely multi-step, tool-calling,
state-carrying systems — the opposite of Kelvran's one-Docker-call-per-case model. This is real and
current; it is also solving the problem "is this agent competent," which is not a question Kelvran's
gateway/cache correctness suite needs to answer about itself.

### Finding 2 — Outcome-only/final-state grading is the field's own dominant default, not something Kelvran uniquely simplified to
**Confidence: high** (primary sources: ICLR 2026 Terminal-Bench paper; tau2-bench `docs/evaluation.md`)
**Tag: build_now — validates, no action needed (Kelvran already does this)**

Terminal-Bench is explicit that it is "intentional... an outcome-driven framework": tests "verify that
all outcomes described in the instruction have been achieved by testing properties of the final container
state; they do not test the agent's commands or console output." tau2-bench's default `reward_basis` for
airline/retail/telecom domains is `[DB, COMMUNICATE]` — final-state comparison only; the listed
`evaluation_criteria.actions` is explicitly "one reference trajectory... not the only correct one," and
"any sequence of tool calls that produces an equivalent DB end state passes." Strict trajectory-matching
(`RewardType.ACTION`) is used in only a small subset (~9 of ~100) `banking_knowledge` tasks and *never* in
airline/retail/telecom. This directly validates Kelvran's existing choice to score a finished `Run`
artifact (sandbox stdout/exit code, or a judge verdict on final output) rather than instrumenting a
trajectory — Kelvran's model is not an under-built shortcut relative to the field's actual default, it's
already aligned with the field's dominant, deliberate design.

### Finding 3 — Where mature harnesses go beyond binary outcome grading, they add either dense subtask partial-credit or repeated-trial reliability scoring — both aimed at agent competence signal, neither applicable to Kelvran's request-level correctness testing
**Confidence: high** (primary sources: Long-Horizon-Terminal-Bench arXiv paper; tau-bench GitHub + arXiv paper; Thinkingbox arXiv paper)
**Tag: not_yet / likely never — no current trigger**

Long-Horizon-Terminal-Bench (LHTB) explicitly contrasts itself with Terminal-Bench's binary
solved/unsolved grading: "binary grading hides meaningful differences between models... dense rewards
show how far each agent gets before stopping," via subtask decomposition. LHTB's own scale illustrates
what this apparatus is *for*: agents average 239 episodes, 9.8M tokens, and 88.9 minutes of wall-clock
execution per task across 17 evaluated models under a 90-minute timeout, with 10 of 17 models passing zero
tasks — genuine long-horizon agent-competence measurement. Separately, tau-bench grades reliability via
`pass^k` (probability that all of k independent full-trajectory trials succeed, distinct from
`pass@k`), and Thinkingbox (N=20 independent trials/task) found this is the whole point: "pass@20 is much
higher than pass@1, while all-20 success is far lower... successful trajectories are often discoverable
but not reliably repeatable" — e.g. Claude-class models reaching 66.5% pass@1 but only ~47.5% pass^20 in
Thinkingbox's numbers. All three mechanisms (dense subtask credit, `pass^k`, repeated-trial reliability)
exist to characterize *agent* consistency across a **task an agent might fail differently each time**.
Kelvran's own request-level decisions (cache hit/miss, route choice, budget check) are close to
deterministic given the same input state — the reliability problem these benchmarks solve doesn't have an
analog in what Kelvran's evals are scoring today.

### Finding 4 — Outcome-only grading has a documented failure mode ("corrupt success") that is the one place this research changes Kelvran's actual near-term to-do, not just its philosophy
**Confidence: high** (primary source: arXiv 2603.03116, "Beyond Task Completion," Amadeus France, March 2026)
**Tag: build_now — narrow, low-cost addition; not the trajectory apparatus, just a grading discipline**

Current agentic benchmarks (the paper names SWE-bench, WebArena, GAIA) "assess agents primarily through
outcome metrics... while ignoring the procedures by which outcomes are achieved," enabling "corrupt
success": reaching the correct terminal state while violating mandatory constraints along the way.
Empirically, across models tested on tau-bench, 27–78% of benchmark-reported successes were corrupt
successes concealing policy/procedural violations, and no model reliably completed tasks with full
procedural compliance more than 24% of the time (gated `pass^4`). This is directly relevant to Kelvran's
own outcome-only scoring — not because Kelvran needs trajectories, but because a `Score` that only checks
"did the response look right" can hide the equivalent of a corrupt success in Kelvran's own domain: e.g.
a cache hit that returned the *correct-looking* answer while violating the entity/freshness hard-gate
`AGENTS.md` calls out as non-negotiable, or a guardrail that let a violating request through but the
judge scored the final text as fine anyway. The actionable takeaway is narrow and already fits Kelvran's
existing `Score` model: an eval case's pass/fail should be able to check "and did the *infrastructure
decision itself* obey policy" (e.g. assert `from_cache=False` when a case's fixture requires a cache
miss, or assert a guardrail's block/allow decision independent of the judge's opinion of the final text)
— a same-shape addition to existing regression-corpus cases, not a new orchestrator.

### Finding 5 — Even a directly comparable product category (LLM gateways with native eval tooling) models evaluation as single-turn input→output→score, with zero agentic-trajectory concept
**Confidence: medium** (single primary source: LiteLLM's own official eval-tutorials doc page)
**Tag: build_now — validates scope, no action needed**

LiteLLM's documented eval tutorials (MLflow Evals, AutoEvals) structure evaluation entirely as
`inputs/ground_truth/outputs/score` — one question in, one answer out, one score — with no mention of
agent, trajectory, multi-step, or tool-use turns anywhere on the page, and no separate LiteLLM tutorial
page dedicated to agentic/trajectory eval methodology exists either. This is the single most
directly-comparable precedent available: another LLM gateway product's own native eval primitive is
single-shot, same as Kelvran's. It is evidence, not proof (one vendor's docs page, and per-vendor
practice varies), but it corroborates that gateway-category products don't reach for agentic-trajectory
grading as a default even when they ship eval tooling of their own.

### Finding 6 — Benchmark validity itself (holdout sets, infra-vs-model failure attribution) is a live, separate 2026 concern in the agentic-eval literature — relevant context, not a Kelvran action item
**Confidence: medium** (primary sources: arXiv 2407.01502 "AI Agents That Matter," Princeton, still actively cited in 2026; arXiv 2607.17525v1 "FailureAtlas," thin 2-author preprint, split 2-1 vote)
**Tag: not applicable — this is about *other* benchmarks' methodology, not a Kelvran gap**

"Many agent benchmarks have inadequate holdout sets, and sometimes none at all... agents that take
shortcuts and overfit to the benchmark" is a well-cited (195+ citations, still drawing 2026 follow-on
work) critique of the agentic-benchmark field generally. Separately, a thinner, less-vetted preprint
(FailureAtlas) argues that "agent evaluation frameworks (SWE-bench, WebArena)... attribute all failures
to the agent or model rather than distinguishing infrastructural from cognitive failure modes," and
catalogs zero of its five collected failure-taxonomy entries under "Model Behavior" (all five were
infrastructural: network/transport, streaming/protocol, state/session, governance/cost) — independently
corroborated for SWE-bench specifically by SWE-bench Verified's own stated creation rationale (built
because the original's binary pass/fail conflated benchmark defects with real model failures). Both
findings describe problems *other* benchmarks have with conflating infra bugs and agent-reasoning
failures inside agent-competence scoring — not a gap in Kelvran's evals, since Kelvran's evals score
infrastructure behavior directly and were never trying to separate "model reasoning failure" from
"infra failure" in the first place (there's no agent reasoning being scored to accidentally conflate).

### Finding 7 — Trajectory-level evaluation has a specific technical prerequisite (reconstructable trace/span data covering every tool call and intermediate turn) that Kelvran's current Span model does not provide and was never built to provide
**Confidence: high** (primary definitional source; secondary/reference-quality page, but well-cited to primary papers: TRAJECT-Bench, tau-bench, AgentBench, GAIA, Agent-as-a-Judge)
**Tag: not_yet — correctly deferred, matches Kelvran's own documented reasoning**

Trajectory-level evaluation means scoring "the span tree: the root request, the model's plan, every tool
call with its arguments, every tool response, every model turn in between, and the final response" — and
explicitly requires that a full trajectory be reconstructable from trace/span data first ("if you cannot
reconstruct the trajectory from your traces, you cannot do trajectory-level eval"). Kelvran's own `Run`
model doesn't have this by omission-through-oversight — it's a documented, deliberate gap:
`evals/ARCHITECTURE.md` already sketches the future `harness_config` shape needed for this
(`scaffold_version`, `tool_budget`, `retry_policy`, `step_budget`, `sandbox_tier`), and today's `Span`
model emits exactly one `Span` per real `run_in_sandbox()` attempt, never a `Trace{spans:[Span]}` wrapper,
because — per the model's own docstring — `api/otel`'s transport is still undecided. This finding doesn't
surface a new gap; it confirms Kelvran's own prior architectural reasoning was already correct and the
prerequisite (multi-span trace transport) remains unbuilt for good, already-stated reasons.

## Caveats

- **Time-sensitivity is real and asymmetric.** Terminal-Bench 2.0, tau2-bench/τ³-bench, LHTB, and
  Thinkingbox are all mid-to-late-2026 sources (some within the last 3-4 weeks of the research date);
  this is an accurate snapshot of *current* practice but the field is moving fast enough that a
  re-check in 2-3 months is reasonable if Kelvran's own scope changes.
- **Two sources are weaker than the rest and were downweighted accordingly.** FailureAtlas (Finding 6)
  is a thin, 2-author, non-peer-reviewed preprint documenting only 5 total catalog entries — its
  "Model Behavior row is entirely empty" finding is self-caveated by the authors as a survey-methodology
  limitation, not proof the failure class doesn't exist, and it survived only a split 2-1 vote on the
  SWE-bench/WebArena attribution claim specifically. The aievals.co page (Finding 7) is an
  individual's reference blog, not a benchmark's own primary docs — though it is well-anchored to real,
  correctly-cited primary papers (TRAJECT-Bench, tau-bench, AgentBench, GAIA) for the definitional claim
  it's used for here.
- **Seven claims were refuted during adversarial verification** and are excluded from findings above,
  most notably a claim that tau2-bench's default grading *is* trajectory-aware (0-3 vote — refuted; the
  correct read, confirmed in Finding 2, is the opposite) and two claims about LiteLLM/Portkey
  gateway-native eval/guardrail hooks being single-shot at a *finer* grain than what survived (their
  broader "gateway evals are single-shot" framing survived via Finding 5's LiteLLM-tutorials source; the
  more specific PR/guardrail-hook-level claims did not hold up). See the full refuted list in the
  synthesis input for exact wording.
- **This report addresses "should Kelvran build a trajectory-grading apparatus," not "should Kelvran's
  fixtures include realistic multi-turn conversations as static input."** The latter is a much smaller,
  already-compatible question — `EvalCase.task_spec: dict` is free-form and can already encode a whole
  multi-message conversation as the input to a single `run_in_sandbox()` call testing, e.g., whether the
  gateway correctly counts tokens or makes a cache decision on turn N of a long conversation. That is not
  the same thing as building an orchestrator that decides what happens across turns, and this report does
  not find a need for the latter.
- A companion prior-research report, `docs/upgrade-research/evals-agentic-multiturn-2026-09-11.md`,
  reached the same directional verdict (not_yet / build the upstream agent scaffold, not the grader) from
  a different, non-overlapping set of primary sources (SWE-bench, WebArena, AgentBoard, TheAgentCompany,
  Anthropic's own agentic-eval writeups) two days before this report — the convergence from two
  independent research passes on largely different source sets is itself a moderate-confidence signal,
  not just a repeated assertion.

## Open Questions

1. If Kelvran ever *does* ship MCP/A2A tool brokering or agent tool-call/sandbox-state caching (both
   named v1 non-goals today), does the trigger for building trajectory infrastructure fire immediately,
   or only once a specific bug class (e.g., a cache poisoning a multi-step agent's tool-call sequence)
   is observed in production first?
2. Is there a smaller, compatible middle ground — e.g., a `--conversation-fixture` mode where
   `task_spec` holds a realistic multi-turn message array and the sandbox call asserts per-turn cache/
   routing decisions — that gets most of the "realistic multi-turn input" value from Finding 1's landscape
   without building any of Finding 3's orchestration/reliability apparatus?
3. How would a "corrupt success" style check (Finding 4) actually be wired into the existing
   deterministic/LLM-judge/2-judge-panel `Score` model without becoming its own bolted-on second grading
   axis — e.g., is it a new `Score` field, a separate assertion the regression corpus already has room
   for, or a new case tag?
4. Does Kelvran's `evals/ARCHITECTURE.md`'s already-sketched future `harness_config`
   (`scaffold_version`/`tool_budget`/`retry_policy`/`step_budget`/`sandbox_tier`) need updating in light
   of this research, or does it already correctly anticipate the shape this would take *if* the trigger
   in Open Question 1 ever fires?
