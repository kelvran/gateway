# Kelvran — Product Requirements

> **Snapshot as of 2026-09-02.** This is a one-time, dated artifact capturing what to build and why at project inception — it is not maintained continuously. For current system state, see `ARCHITECTURE.md`. For settled architectural calls, see `DECISIONS.md` and `docs/decisions/`.

## Problem Statement

Teams running production AI agents today stitch together three things that don't talk to each other: an LLM gateway (to avoid hard-coding a single provider), a cache (to avoid paying for the same inference twice), and an evals system (to know whether the agent is actually working). Each exists as a separate product, from a separate vendor, with no shared understanding of what a "request" even is once an agent — not a human — is the one making it.

Three concrete failures fall out of that gap, all independently documented in the research behind this project (see `ai-infra-research/` in the parent workspace):

1. **Cost and governance stop at the HTTP call.** Every gateway surveyed (LiteLLM, Portkey, Kong AI Gateway, Bifrost, TensorZero) understands a single request/response pair as the unit of cost, rate limiting, and observability. None of them understand a multi-step *agent run* — so "why did this agent session cost $4" has no answer beyond a raw total.
2. **Caching is a coin flip, not a judgment.** Every semantic cache surveyed (GPTCache, Redis LangCache, Portkey's semantic mode) gates reuse on a single similarity threshold — "close enough" — not on whether the cached answer is still *true*. 2026 security research (CacheAttack, KeyPooling) shows this same mechanism is actively exploitable: 86-90% hijack rates, cross-tenant leakage in every gateway tested.
3. **Evals trust a single grader.** Every eval framework surveyed (promptfoo, DeepEval, Braintrust, LangSmith) still gates a release on one LLM-as-judge call, despite published research showing up to 100% verdict-flip rates against exactly that setup under adversarial pressure.

Kelvran exists to close these three gaps as one coherent system, not three integrations.

## Jobs-to-be-Done

- **As a platform engineer** running LLM traffic across multiple providers, I need one place that routes, fails over, and enforces budgets — without losing the ability to answer "which agent run spent this money and why."
- **As a cost-conscious team**, I need caching that actually saves money without silently serving a stale or wrong answer to a paying customer.
- **As someone shipping an autonomous agent**, I need to know a "pass" on an eval actually means pass — not that a single lenient grader happened not to notice a problem.

## Scope — v1

**In scope:**
- Gateway: unified request/response schema across OpenAI, Anthropic, Gemini, and OpenAI-compatible self-hosted backends (vLLM/Ollama); streaming pass-through; static + weighted routing; a single fallback chain; virtual API keys; per-key budget/rate limits; OTel span emission with `agent_run_id` propagation from day one.
- Cache: embedded inside the Gateway binary (not a standalone service); exact-match (L1) and normalized-match (L2) layers at v1; semantic (L3) layer with an entity/date hard-gate, not a bare similarity threshold, before it ships — never ship semantic caching without the hard-gate.
- Evals: trace capture via OTel GenAI conventions; deterministic + single LLM-judge scoring at v1, with the *interface* for a multi-judge skeptic panel designed in from the start even though the panel itself is a v2 feature (see `docs/decisions/` and `ARCHITECTURE.md`).

**Explicitly out of scope for v1** (tracked as future `docs/rfcs/` candidates once work starts, not designed now): MCP/A2A tool brokering, cross-provider prompt-cache normalization, adversarial skeptic-panel judging as the default gate, agent tool-call/sandbox-state caching, massively parallel (10K+) eval rollouts, a hosted/SaaS offering.

## Non-Goals

- Kelvran is not an inference engine. It never reimplements PagedAttention/RadixAttention-class GPU KV-cache management — it consumes vLLM/SGLang's own cache metrics as a routing signal, nothing deeper.
- Kelvran does not aim to be the broadest-provider gateway on day one (that's LiteLLM's game, and it's already won on breadth). It aims to be the most *correct* one on the specific failure modes above.
- Kelvran is not, at this stage, a managed/hosted product. Self-hosting is the only supported deployment model until there's a real signal otherwise.

## Success Metrics

- Cache hit rate on repeated/near-duplicate traffic, with a **near-zero measured wrong-answer rate** from semantic hits (the two numbers must be reported together — a hit rate without a correctness number is not a success metric here, it's a vanity metric).
- Cost savings attributable and explainable down to the individual agent run, not just an aggregate dashboard total.
- Eval pass/fail decisions that survive a deliberate, independent attempt to refute them (even at v1's single-judge stage, every judged result carries a disclosed harness configuration and a confidence interval — never a bare percentage).

## Competitive Positioning

See `README.md` for the full comparison table against LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Envoy AI Gateway, Helicone, Langfuse, Braintrust, and Arch/Plano. The one-line version: every one of them is good at breadth, raw speed, or governance depth — none of them treat agent-run-level accountability and adversarial self-skepticism as the product itself.

## Open Questions Carried Forward

The following are deliberately unresolved here and pushed into `DESIGN.md` and, where they're big enough, into `docs/rfcs/` once the corresponding feature work actually starts:
- Exact virtual-key/budget data model (sketched in `DESIGN.md`, not finalized).
- Whether the skeptic-panel protocol for Evals should be pairwise-comparison-based or independent-refutation-based (research favors independent refutation; final call deferred to the RFC that accompanies building it).
- Whether `evals` should default to SemVer or move to CalVer once it ships continuously (see `RELEASE.md`).
