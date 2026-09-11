# Multi-Step / Agentic Evaluation Methodologies for Evals v2 (2026-09-11)

Scope: whether and how `evals`' v2 upgrade should move beyond its current
one-`EvalCase`-to-one-`run_in_sandbox()`-call model (real, sequential Rollout Scheduler; real
Docker-sandboxed execution with `network=none`, read-only root FS, memory/cpu/pids-limit caps;
deterministic + single/2-judge-panel LLM scoring; 137-case regression corpus; real Span/tracing
per sandbox execution; mSPRT early-stopping — all shipped, per `evals/ARCHITECTURE.md`) toward a
genuine multi-turn/multi-tool-call harness, grounded against 2026-era external precedent (SWE-bench,
τ-bench, WebArena, AgentBoard, TheAgentCompany, AgentEvals) and Anthropic's own public agentic-eval
engineering writeups. 19 claims survived 3-vote adversarial verification; 6 claims were refuted and
excluded from the findings below (kept for transparency in Caveats). Every finding is tagged
**build_now** or **not_yet**, with the exact, checkable trigger for a future revisit.

## Executive Summary

**Verdict: not_yet, across all four research questions — and this is itself the headline finding, not a hedge.**
The strongest available 2026 precedent (SWE-bench, τ-bench, WebArena, and Anthropic's own internal
harnesses) converges on a pattern Kelvran's v1 already matches more closely than it first appears:
mature systems keep the **scoring harness** single-shot-artifact-based (a finished patch, a finished
DB state, a finished feature-passes-boolean) while pushing genuine multi-turn state — conversation
history, tool-call sequences, intermediate file/DB state — into an **upstream agent scaffold** that is
architecturally distinct from the grader. Kelvran's `Run` model already anticipates this split and has
*already* deliberately deferred it: `evals/ARCHITECTURE.md` sketches a future `harness_config` shape
(`scaffold_version`, `tool_budget`, `retry_policy`, `step_budget`, `sandbox_tier`) for "a future
pluggable multi-step agent harness that doesn't exist yet," and today's real `Span` model has "no
`Trace{spans:[Span]}` wrapper — today's harness makes exactly one `run_in_sandbox()` call per `Run`."
Nothing in the external research changes that calculus, because Kelvran's own product — a single-turn
sandbox-command gateway — has no multi-turn agentic behavior to evaluate yet. Partial credit and
tool-call-level scoring are both real, well-precedented mechanisms elsewhere (TheAgentCompany's
checkpoint formula; AgentEvals' `ToolArgsMatchMode`), but every mature harness surveyed adds this
machinery only once a task genuinely can't be judged end-to-end — Anthropic's own guidance frames this
explicitly as a cost-benefit decision tied to task difficulty, not a default. The concrete trigger for
all four questions is the same one: **Kelvran's product must first ship a feature where a single
logical task requires multiple sequential tool-calls/turns with state carried between them.** Until
then, building multi-step eval infrastructure would be evaluating a capability that doesn't exist.

## Findings

### Finding 1 — RQ1: Mature harnesses keep the grader single-shot; multi-turn state lives in an upstream agent scaffold, not inside the scoring harness itself
**Confidence: high** (SWE-bench primary docs+source, cross-corroborated by Anthropic's own primary engineering writeups)

SWE-bench's documented harness pipeline is **Setup → Patch Application → Test Execution → Grading →
Reporting**, operating on an already-generated final patch via `--predictions_path` — a static
artifact, not live agentic state. Any multi-turn tool-call/conversation history that produced that
patch happens entirely upstream, in an agent scaffold (SWE-agent, mini-swe-agent, OpenHands) that the
harness's own docs treat as a separate concern (a distinct "Inference" reference page). This is the
single most directly-relevant precedent for Kelvran: **the canonical agentic-coding benchmark's own
scoring layer is single-shot**, exactly like Kelvran's current `Rollout Scheduler` → `Scorer Service`
split, which already assumes a finished `Run.stdout`/exit-code artifact to score.

Where mature systems *do* model persistent cross-turn state, they use one of two concrete shapes,
neither of which is "raw conversation history in the grader":
- **A structured trajectory object.** AgentEvals represents an agent run as an ordered list of
  OpenAI-format chat messages (or LangChain `BaseMessage`s) with tool calls nested as `function{name,
  arguments}` — this is the state a trajectory-match evaluator compares turn-by-turn. τ-bench frames
  the same idea at the product level: "dynamic conversations between a user (simulated by language
  models) and a language agent" using domain-specific API tools and policy guidelines.
- **A persistent, real backing environment with explicit reset semantics.** WebArena runs a Gym-style
  `env.reset()`/`env.step()` loop against real Shopping/Reddit/GitLab/Wikipedia/Maps sites with live
  backend databases; the repo's own instructions require an explicit reset step after each batch of
  tasks specifically because state otherwise carries forward. Anthropic's own long-running-agent
  harness goes further and explicitly rejects raw/compacted conversation history as the persistence
  mechanism ("each new session begins with no memory of what came before... compaction isn't
  sufficient") in favor of filesystem + git artifacts: a `claude-progress.txt` log, a `feature_list.json`
  status file, and git commit history. Anthropic's harness design also formally distinguishes
  **transcript** (the full tool-call/reasoning record of a trial) from **outcome** (the actual
  resulting environment state, e.g. a database record) — and treats per-trial clean-environment
  isolation as required infrastructure specifically to prevent state leakage across trials (their own
  documented failure mode: a model gaining an unfair advantage by reading a previous trial's git
  history).

**What this means for Kelvran**: the "isolation" half of this pattern is *already real* — every
`run_in_sandbox()` call gets a fresh container (`network=none`, read-only root FS, resource caps), the
same clean-environment discipline Anthropic's harness treats as foundational. The "structured
trajectory" half has no Kelvran analog today, on purpose: `Run.harness_config` is deliberately narrower
than a sketched future shape that would carry `tool_budget`/`step_budget`/`scaffold_version`, and
`Span` deliberately has no `Trace` wrapper because "today's harness makes exactly one
`run_in_sandbox()` call per `Run`."

**Verdict: not_yet.** Trigger: the first `EvalCase` whose correctness genuinely depends on
*intermediate* turns or tool calls — not just the final sandbox stdout/exit-code/file state — i.e.
the first time Kelvran needs to grade *how* a task got solved, not just *whether* it did. Until that
exists, extending `Run`/`Span` to carry a trajectory would be building the "diagram-only box" this
project has already declined to build for `Task/Dataset Registry` and `Sandbox Pool`.

### Finding 2 — RQ2: Partial credit is real and well-precedented, but the most mature production harness surveyed gets granularity from decomposition into more binary units, not from stepwise credit inside one task — and Anthropic's own guidance and shipped practice pull in different directions
**Confidence: high** (multiple primary sources on both sides of the tension; one split vote)

Two real patterns coexist in 2026 practice, and they are in tension with each other:

- **Binary-per-task, granularity via decomposition or repeated trials.** SWE-bench's grading step is a
  strict binary resolve/not-resolve — verified two ways: the docs describe no partial-credit
  mechanism, and the actual harness source contains an internal `ResolvedStatus` enum with
  `FULL`/`PARTIAL`/`NO` buckets whose `PARTIAL` case is *collapsed to `resolved=False`* in the emitted
  report, i.e. even where partial signal exists internally, it is deliberately discarded. τ-bench
  scores a binary pass/fail per trajectory (comparing end-of-conversation DB state to an annotated goal
  state) and gets reliability signal via a repeated-trial metric (`Pass^k`) rather than intra-run
  partial credit. Anthropic's own long-running-build harness — its most mature, largest-scale internal
  agentic harness in this research set — scores a boolean `passes` field per discrete feature and gets
  fine-grained signal from decomposing work into 200+ small features, not from stepwise credit inside
  any one feature ("only mark features as 'passing' after careful testing").
- **Genuine stepwise/partial credit inside one task.** Anthropic's own evals-methodology guidance
  explicitly recommends the opposite of the above for multi-component tasks: "for tasks with multiple
  components, build in partial credit," illustrated with a support agent that identifies the problem
  and verifies the customer but fails to process the refund — meaningfully better than failing
  immediately, and the guidance argues this continuum shouldn't be collapsed to pass/fail. AgentBoard
  was built specifically to fix this gap in prior agent benchmarks, which it diagnoses as "mostly
  focus[ed] on the final success rate, revealing few insights during the process," via a fine-grained
  progress-rate metric. TheAgentCompany gives the most concrete, reusable **formula** found in this
  research: tasks are divided into checkpoints with point values across three dimensions (**Action
  Completion**, **Data Accuracy**, **Collaboration**), and the partial-completion score is
  `S_partial = 0.5·(Result/Total) + 0.5·S_full`, where `S_full = 1` only if every checkpoint passes —
  a deliberate blend that rewards partial progress without letting it fully substitute for total
  success.

**The tension itself is the useful finding for Kelvran**: even Anthropic's own stated recommendation
(build in partial credit) and its own largest shipped harness (binary-passes-boolean +
decomposition) don't agree — decomposing a task into more, smaller binary-graded `EvalCase`s is
operationally cheaper than building a stepwise-credit rubric, and it's what got shipped at production
scale. Kelvran's existing `EvalCase`/`tags`/`tier` model already supports this cheaper path today with
zero new primitives — a monolithic multi-step case can already be split into several tagged
`EvalCase`s.

**Verdict: not_yet** for a new `Score.partial_credit`/checkpoint field. Trigger, in priority order: (1)
first try decomposing any candidate multi-step `EvalCase` into multiple smaller `EvalCase`s (the
zero-new-primitives path, and the one Anthropic's own largest harness actually uses); (2) only if a
task is genuinely non-decomposable (truly sequential/stateful, where an intermediate correct action
still matters even though a later step fails) AND Kelvran observes real cases where identical binary
scores are masking materially different quality — a "mostly correct" output scored identically to
"completely wrong" in a way that's visibly distorting `wilson_interval`/`--fail-under` gating — build a
TheAgentCompany-style weighted-checkpoint field on `Score` (`checkpoints: list[{name, points, passed}]`
+ the `0.5·(Result/Total) + 0.5·S_full` blend). Note: no source surveyed shows how to feed a
non-boolean partial-credit score into a Wilson-interval-based CI gate — that would be new statistical
design work for Kelvran, not an adopted pattern (see Open Questions).

### Finding 3 — RQ3: Tool-call correctness has a real, shipped, non-LLM primitive (AgentEvals' configurable match modes), but adopting it requires new Kelvran data model — and the strongest counter-evidence against building it (Anthropic advising against rigid tool-call-path grading) did not survive verification
**Confidence: medium** (one high-confidence primary source for the core primitive; the fault-classification corroboration is single-source, LLM-judge-dependent, split-vote)

Yes — there is a real, well-precedented, non-LLM way to score tool-call correctness specifically,
separate from final-output correctness. AgentEvals ships `create_trajectory_match_evaluator` with a
configurable `ToolArgsMatchMode` (`exact`/`ignore`/`subset`/`superset`) plus a
`tool_args_match_overrides` dict for per-tool custom comparator functions — explicitly designed to
handle cases needing looser equality for LLM-generated arguments (e.g. `"san francisco"` vs. `"San
Francisco"`) on a per-tool-call basis. This operates on the same structured trajectory representation
described in Finding 1 (ordered messages with embedded `tool_calls`), and is a rule-based comparator
distinct from (and more granular than) the library's own final-output-only LLM-judge evaluators.
τ-bench independently corroborates the *pattern* — its `auto_error_identification.py` classifies
faults specifically at the tool-call level (`used_wrong_tool`, `used_wrong_tool_argument`), alongside
non-tool-call fault types (`goal_partially_completed`, `took_unintended_action`) — but this specific
mechanism is LLM-judge-based and is flagged by τ-bench's own docs as potentially inaccurate, and it
only reached a 1-1 split vote in verification. A claim that Anthropic explicitly advises *against*
rigid tool-call-path grading (as a brittleness anti-pattern) was checked and **refuted 0-3** — i.e.
there is no found evidence undermining tool-call-correctness scoring as a legitimate technique, which
strengthens rather than weakens the case that this is a real, adoptable primitive when needed.

**This would require new Kelvran primitives.** Kelvran's `Run` model has no place today to hold an
ordered tool-call sequence — `harness_config` is `{image, command, timeout_s}`, a single exec, not a
tool-calling loop. The natural extension point already exists, though: the real `Span` model (one
OTel-shaped span per `run_in_sandbox()` call today) is the precedented place to grow a
one-span-per-tool-call model, rather than inventing a parallel structure. Scoring would need a new
`scorer_type` (e.g. `tool_trajectory_match`) mirroring AgentEvals' match-mode design.

**Verdict: not_yet.** Trigger: the same precondition as Finding 1/4 — Kelvran's product must first
invoke multiple tool calls per task. Once it does, the concrete, lowest-risk adoption path is
AgentEvals' rule-based `ToolArgsMatchMode` pattern extended onto `Span` (no new LLM-judge
infrastructure required), not τ-bench's LLM-judge-based fault classifier, whose own docs flag it as
potentially inaccurate.

### Finding 4 — RQ4: Multi-step agentic evaluation is premature for v2 — Kelvran's own architecture docs have already deliberately deferred this, consistent with the strongest available precedent's own cost-benefit framing
**Confidence: high** (Anthropic primary source on the cost-benefit framing; direct, current inspection of Kelvran's own `ARCHITECTURE.md`)

Anthropic's own harness-design writeup frames adding an evaluator/harness component as an explicit
conditional cost-benefit decision, not a default: "the evaluator is not a fixed yes-or-no decision. It
is worth the cost when the task sits beyond what the current model does reliably solo" — and
elsewhere notes that for tasks within a model's solo-reliable range, "the evaluator became unnecessary
overhead." Separately, Anthropic distinguishes genuinely agentic coding evals (agents that "write
programs, run tests, install dependencies, and iterate over multiple turns," where "the runtime is no
longer a passive container") from static output-scoring benchmarks — this is the real shape of what a
*bona fide* multi-turn agentic eval requires, and it is a materially different thing from Kelvran's
current single sandboxed command execution.

Kelvran's own repo has already run this cost-benefit calculation once, in the same direction this
research converges on: `evals/ARCHITECTURE.md` explicitly sketches a richer future `Run.harness_config`
(`scaffold_version`, `tool_budget`, `retry_policy`, `step_budget`, `sandbox_tier`) "for a future
pluggable multi-step agent harness that doesn't exist yet" and deliberately ships a narrower real v1
(`{image, command, timeout_s}`) instead — the identical "don't build the diagram-only box" discipline
already applied to `Task/Dataset Registry` and `Sandbox Pool` elsewhere in the same document. Given
in the research brief itself: Kelvran's own product usage today is single-turn sandbox commands. There
is no evidence the product performs multi-step agentic tool-calling loops that the eval harness would
need to catch up to — building this infrastructure now would mean evaluating a capability the product
doesn't have.

**Verdict: not_yet — this is the load-bearing verdict of the whole report.** Exact, checkable trigger:
Kelvran's own product (the gateway) ships a feature where a single logical task requires multiple
sequential tool-calls/turns with state carried between them (e.g. an agent-facing product surface that
itself performs a tool-calling loop, or a multi-step provider-negotiation flow that needs to be judged
turn-by-turn rather than by final outcome). Until that trigger fires, the eval harness's current
single-turn-sandbox model is correctly matched to what it evaluates, not lagging behind it — and every
piece of external precedent surveyed (SWE-bench's single-shot grading layer, Anthropic's own explicit
cost-benefit framing, Kelvran's own prior "don't build it yet" decisions) points the same direction.

## Caveats

- **Refuted claims, for transparency**: three specific counter-hypotheses about tool-call grading
  and multi-agent architecture were checked and refuted (0-3 or 1-2 votes), and are excluded from the
  findings above: (1) that Anthropic explicitly advises *against* rigid tool-call-path grading; (2) that
  Anthropic's long-running-build harness verifies functional correctness purely behaviorally via browser
  automation rather than scoring tool calls; (3) that Anthropic's Opus-4.6-era harness ran as one
  continuous session rather than resetting context between turns. None of these refutations changes any
  finding above, but they mean the report does not rely on (and found no support for) an
  anti-tool-call-scoring argument, nor on a "no-reset-needed" counter-precedent to Finding 1's isolation
  requirement.
- **Split votes / lower-confidence sub-claims**: the τ-bench fault-classification claim (Finding 3) and
  the Anthropic partial-credit-recommendation claim (Finding 2) each landed on a 2-1 or 1-1 vote rather
  than unanimous — both are still included because independent primary-source verification confirmed
  the underlying quotes, but they carry more uncertainty than the unanimous claims.
- **Time-sensitivity**: Anthropic's own engineering posts are Nov 2025-Jan 2026 (fresh relative to this
  2026-09-11 research date). SWE-bench, τ-bench, WebArena, and AgentBoard are established
  2023-2024-era academic benchmarks; their core scoring *architecture* is stable and unlikely to have
  changed, though τ-bench's specific task content is explicitly marked superseded by τ²/τ³-bench (the
  `Pass^k` methodology itself carries forward unchanged).
- **No source surveyed addresses statistical treatment of partial-credit/non-boolean scores inside a
  Wilson-interval-style CI gate** — Kelvran's `evals report --fail-under` machinery assumes a boolean
  pass/fail per trial. If Finding 2's trigger ever fires, the statistics layer, not just the scoring
  model, would need new design work with no adopted external precedent to lean on.
- **This report does not independently verify Kelvran's own current-usage claim** (single-turn sandbox
  commands only) — that was taken as given in the research brief, not re-derived from the gateway's own
  code in this pass. If that premise changes, Finding 4's verdict should be re-checked first.

## Open Questions

1. Does Kelvran's own product roadmap include any near-term multi-step/agentic tool-calling feature
   that would fire Finding 4's trigger sooner than "eventually, someday"?
2. If/when multi-turn eval becomes necessary, should Kelvran extend the existing `Span` model (one span
   per tool call, per Finding 3) or introduce a new `Trajectory`/`Turn` entity — `Span` is a natural fit
   but was not designed with tool-call granularity in mind, and no source surveyed validates extending
   an OTel-span model this way.
3. If Finding 2's checkpoint-based partial-credit path is ever built, how should
   `wilson_interval`/`mixture_sprt_early_stop` (both boolean-trial-based today) be redesigned to consume
   a continuous or checkpoint-weighted score without silently changing what `--fail-under` means for
   existing binary-scored suites?
4. Is there a real precedent for tool-call-correctness scoring reliable enough to CI-gate on (like
   Kelvran's `evals report --fail-under`), rather than just report descriptively — AgentEvals' rule-based
   match-modes seem promising for this since they avoid an LLM judge, but no source surveyed shows them
   used in a hard CI-blocking gate the way Kelvran already gates `deterministic`/`llm_judge` scores.

## Sources

- Anthropic, "Demystifying evals for AI agents" — https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents
- Anthropic, "Effective harnesses for long-running agents" — https://www.anthropic.com/engineering/effective-harnesses-for-long-running-agents
- Anthropic, "Harness design for long-running apps" — https://www.anthropic.com/engineering/harness-design-long-running-apps
- Anthropic, "Infrastructure noise" — https://www.anthropic.com/engineering/infrastructure-noise
- SWE-bench harness reference docs — https://swebench.com/SWE-bench/reference/harness/
- AgentEvals (langchain-ai) — https://github.com/langchain-ai/agentevals
- τ-bench (Sierra Research) — https://github.com/sierra-research/tau-bench
- WebArena — https://github.com/web-arena-x/webarena
- AgentBoard — https://arxiv.org/abs/2401.13178
- TheAgentCompany — https://arxiv.org/abs/2412.14161
- Kelvran internal: `evals/ARCHITECTURE.md` (Rollout Scheduler, `Run`/`Span` model, deferred multi-step
  harness sketch)
