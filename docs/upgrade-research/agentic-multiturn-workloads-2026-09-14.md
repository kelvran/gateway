# Agentic & Multi-Turn Workload Support — 2026 Production-Practice Survey (2026-09-14)

## Question

Ground truth read first: `PRD.md`'s agentic-traffic framing ("Cost and governance stop at the HTTP
call... no gateway surveyed understands a multi-step *agent run*"), `gateway/ARCHITECTURE.md`'s
Canonical Schema & Provider Adapters section (six real normalization points: tool-call argument
encoding, system-prompt placement, streaming event shape, unknown-field preservation,
provider-side prompt caching via `CacheControl`, reasoning/thinking-block round-tripping), and
`docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md` (design-only `OutboundCredential`
sketch, explicitly gated on "MCP's own spec stabilizing AND Kelvran has a first real MCP/A2A
integration," `internal/mcp` has zero code).

Five research questions, compared against 2026 production practice in agent-framework ecosystems
(LangGraph, CrewAI, AutoGen/AG2, OpenAI's Agents SDK, Anthropic's MCP ecosystem) and comparable LLM
gateways (Portkey, Helicone, OpenRouter): (1) has MCP's own spec stabilized enough to fire Kelvran's
named trigger; (2) is there a real, well-precedented session/conversation-level KV-cache-reuse
pattern distinct from `CacheControl`; (3) do popular agent frameworks expect an OpenAI-compatible
gateway to expose anything Kelvran's canonical schema doesn't (streaming event shapes, parallel
tool-call semantics, handoff metadata); (4) is there a real gap in how Kelvran's existing
`MaxInFlight`/`MaxConcurrentRequests`/RPM/TPM/budget caps compose for agentic fan-out; (5) anything
else a 2026 survey flags as a real, checkable gap.

**Method:** 10 claims survived 3-vote adversarial verification against primary sources (MCP's own
spec/changelog/roadmap post, vLLM/GKE/LMCache project docs and source, OpenAI Agents SDK docs and
source, AutoGen Core docs). This report merges semantic duplicates, cross-references Kelvran's
already-settled internal research (`gateway-mcp-a2a-brokering-2026-09-07.md` +
`-2026-09-11.md`, `gateway-agent-framework-interop-2026-09-12.md`,
`cache-prompt-caching-aware-routing-2026-09-12.md`, `cross-provider-prompt-caching-optimization-2026-09-14.md`,
`gateway-realtime-multimodal-2026-09-13.md`, `evals-agentic-multiturn-2026-09-11.md`) rather than
re-litigating it, and states a build-now/not-yet call with a named trigger per finding. **RQ4
(multi-agent fan-out concurrency/rate-limit composition) and part of RQ5 produced zero surviving
claims in this pass** — see Open Questions; this is disclosed as a genuine gap, not silently
dropped.

---

## Executive Summary

Nothing in this pass moves any of Kelvran's three already-settled agentic-infrastructure verdicts
off `not_yet` — it sharpens the evidence behind each one instead. MCP's spec churn is confirmed
structural and ongoing (the July 2026 revision removed sessions/handshake for horizontal
scalability, a real breaking change), and the MCP maintainers' own August 2026 roadmap post now
explicitly defers agent-identity/delegation auth — the exact piece Kelvran's `OutboundCredential`
design cares about most — to a *future* cycle, so the RFC's named trigger has demonstrably not
fired. A real, well-precedented session/prefix-affinity KV-cache-reuse pattern exists in 2026
production systems (vLLM Router, GKE Inference Gateway/llm-d, LMCache) — but it operates at the
self-hosted-inference-engine layer, a genuinely different mechanism and layer from Kelvran's
`CacheControl` (a per-request marker for opaque hosted providers) and from the *already-researched*
provider-cache-locality routing question (`cache-prompt-caching-aware-routing-2026-09-12.md`,
`not_yet`) — it corroborates rather than changes that verdict, while adding a second, engine-level
precedent family directly relevant to Kelvran's self-hosted (`openaicompat`) backends whenever the
existing traffic-based triggers fire. Agent frameworks' handoff/multi-agent semantics are confirmed
to ride entirely on standard function/tool-calling in both surveyed frameworks (OpenAI Agents SDK,
AutoGen Core) — a clean, evidence-backed "not a gap": Kelvran's canonical schema needs no new
wire primitive for agent handoffs, and even the OpenAI Agents SDK's own richer streaming taxonomy
(`RunItemStreamEvent`, `AgentUpdatedStreamEvent.new_agent`) is synthesized client-side from ordinary
tool-call events, not something a gateway must itself emit.

---

## Findings

### Finding 1 — MCP's spec churn is real, structural, and ongoing; the specific piece relevant to `OutboundCredential` (agent identity/delegation) is explicitly deferred to the *next* roadmap cycle, not settled in the one that just shipped

**Confidence: high** (multiple primary sources, unanimous 3-0 votes on both merged claims)

The 2026-07-28 MCP spec revision removed protocol-level sessions (`Mcp-Session-Id` header,
SEP-2567) and the `initialize`/`initialized` handshake (SEP-2575) entirely, explicitly so a server
"can scale horizontally without holding state" — a structural rearchitecture, not a cosmetic
change, confirmed directly against the live spec changelog and corroborated by Kelvran's own prior
research (`gateway-mcp-a2a-brokering-2026-09-11.md`, which independently reached the identical
conclusion three days earlier). This alone reconfirms that pass's existing `not_yet` verdict; it
does not move it.

New this pass: the MCP maintainers' own August 22, 2026 roadmap post states plainly that "MCP
authorization today is built around a person approving access in a browser," works for interactive
clients, but "more and more of the callers are agents running as cloud workloads with their own
identity... delegating narrower authority to sub-agents" — and lists finalizing DPoP, Workload
Identity Federation, the ID-JAG grant, and standard token exchange as work for the **new** roadmap
cycle, explicitly contrasted with what the post's own "Looking back" section already marks stable
from the *prior* cycle (issuer validation, CIMD, Enterprise-Managed Authorization). Cross-checked
against GitHub: SEP-1933 (Workload Identity Federation) is still Draft/open, not shipped. This is
the exact category of caller (agent/cloud-workload identity, sub-agent delegation) Kelvran's
`OutboundCredential` RFC exists to eventually design against — and it is explicitly *unfinished
work for next cycle*, not something this pass found freshly stabilized.

**Why it matters for Kelvran specifically:** the RFC's own Unresolved Questions ask whether to
build LiteLLM's On-Behalf-Of token-exchange shape or Portkey's simpler injected-credential shape —
both of those real peer patterns exist precisely *because* MCP's own native agent-identity story is
still unfinished, not despite it. This pass's finding doesn't just reconfirm "spec still churning"
in the abstract; it names the specific missing piece (agent identity/delegation auth) that maps
directly onto the RFC's own scope, making the "not yet fired" call more precise than the prior
report could state it.

**Build now / not yet: `not_yet`, reconfirmed with a sharper trigger.** Named trigger, updated:
SEP-1933 (Workload Identity Federation) and the ID-JAG/token-exchange work move out of Draft into a
finalized, shipped spec revision — until then, any `OutboundCredential` implementation would be
designing against a moving target on the exact dimension (agent identity delegation) that matters
most for a gateway's outbound-credential leg. This sits alongside, and does not replace, the prior
report's own trigger (12-month deprecation window closing with no further structural core change,
or two consecutive Go-SDK spec-version bumps with zero new removed/deprecated entries).

---

### Finding 2 — A real, precedented session/prefix-affinity KV-cache-reuse pattern exists in 2026 production self-hosted-inference systems (vLLM Router, GKE/llm-d, LMCache) — a distinct mechanism and layer from `CacheControl`, corroborating rather than changing the already-settled provider-cache-locality `not_yet` verdict

**Confidence: high** (3 sources, 2 unanimous 3-0 + 1 at 2-1 with the dissent being a minor
over-precision quibble, not a substance disagreement)

Three independent, real, currently-active 2026 systems confirm the pattern: **vLLM Router**
(vllm-project/production-stack) implements consistent-hashing "sticky" routing on a session-ID or
user-ID routing key, explicitly framed as "critical for minimizing latency" in conversational
workloads by maximizing KV-cache reuse on the worker that already holds it. **GKE Inference
Gateway**'s llm-d Endpoint Picker (EPP) implements "Prefix-Cache Aware Routing" — it tracks
per-backend-pod prefix-cache indexes and scores replicas by longest-matching-prefix, one factor in
a weighted score alongside load and LoRA affinity (not a hard deterministic rule, per the
verification's own precision correction). **LMCache** is a real, heavily-adopted (11.8k GitHub
stars, NVIDIA Dynamo/CoreWeave-Cohere/Redis/PyTorch-Foundation integrations through mid-2026)
KV-cache layer that extracts and shares KV caches *across engines and queries* — genuine
cross-request/cross-engine reuse, mechanically different from a provider-side `cache_control`
marker, which only flags a prompt segment for the *provider's own opaque server-side cache* and
does nothing to move raw KV tensors between replicas.

**How this relates to Kelvran's own already-settled research:** `cache-prompt-caching-aware-routing-2026-09-12.md`
already found and verified the *provider-hosted* side of this exact problem class (LiteLLM's three
independent fixes, OpenRouter's default sticky routing, AWS SageMaker's `PREFIX_AWARE` strategy) and
returned `not_yet`, gated on two concrete triggers Kelvran has not hit (confirmed multi-replica
same-canonical-model deployments; real traffic showing routing-attributable cache-miss inflation).
`cross-provider-prompt-caching-optimization-2026-09-14.md` (same day as this report) explicitly
deferred to that verdict rather than re-litigating it. This pass's three sources are the
**self-hosted-inference-engine-layer counterpart** to that already-verified provider-hosted-layer
finding — genuinely new evidence, not a duplicate, because it operates on a different layer
(engine-internal KV cache vs. opaque hosted-provider cache) and maps onto a different part of
Kelvran's own architecture: `openaicompat` targets exactly the self-hosted runtimes (vLLM, TGI,
Ollama) that ship vLLM Router/LMCache-style mechanisms natively.

**Why this doesn't change the verdict:** the underlying precondition gating `not_yet` — Kelvran
running ≥2 replicas of the same canonical model with real traffic to measure — is identical whether
the cache being defeated is provider-hosted or self-hosted-engine-internal. No new evidence in this
pass changes that precondition. What *does* change is the "no reference implementation to build
against" risk: whenever the traffic trigger fires, Kelvran now has two independently-precedented
mechanism families to choose from — provider-side sticky/prefix-hash routing (Finding 1 of the
prior report) for hosted deployments, and engine-level session/prefix-affinity routing
(vLLM Router / llm-d EPP / LMCache, this pass) specifically for `openaicompat`-routed self-hosted
deployments — which is a genuinely sharper design starting point than the prior report had alone.

**Build now / not yet: `not_yet`, reconfirmed; verdict and both named triggers from
`cache-prompt-caching-aware-routing-2026-09-12.md` Finding 4 stand unchanged.** This pass adds no
new trigger — it adds a second reference-implementation family for whichever trigger fires first.

---

### Finding 3 — Agent-framework handoffs and multi-agent orchestration ride entirely on standard tool-calling in both surveyed frameworks (OpenAI Agents SDK, AutoGen Core) — a clean, evidence-backed "not a gap" for Kelvran's canonical schema

**Confidence: high** (4 merged claims, 3 at unanimous 3-0, 1 at 2-1 on a scope-precision point that
doesn't affect the conclusion)

The OpenAI Agents SDK's streaming API exposes three event types layered on top of the raw LLM
stream — `RawResponsesStreamEvent` (raw token-level deltas), `RunItemStreamEvent` (fires once per
fully-generated item, using a fixed 11-name enum: `message_output_created`, `handoff_requested`,
`handoff_occured` [sic, intentional backward-compat misspelling], `tool_called`, etc.), and
`AgentUpdatedStreamEvent` (fires on active-agent change, exposing a genuinely typed `new_agent:
Agent[Any]` field, confirmed against the SDK's own source, not just docs prose). A handoff call is
emitted *only* as `handoff_requested`, never also as `tool_called` — the two are kept structurally
distinct in this taxonomy. Separately, and more directly load-bearing for Kelvran: handoff
*metadata* itself (e.g., an escalation reason) is passed via `input_type`, which "just becomes the
JSON-schema `parameters` of that same tool call" — standard function-calling arguments, confirmed
against the SDK's own handoffs docs, with no parallel wire channel. **AutoGen Core** independently
confirms the identical pattern from a wholly different framework: a "delegate tool" is a plain
Python function wrapped in `FunctionTool` that returns a plain topic-type string; AutoGen Core
itself defines no protocol-level `HandoffMessage` type at all (that type exists only in the
separate, higher-level `autogen_agentchat` layer, not Core) — confirmed directly against Core's own
docs, which state a Core-level handoff API "is currently being worked on" for the higher AgentChat
layer specifically because Core itself has none.

**Why this directly answers research question 3:** the concern the question raises — "does an
agent framework expect a gateway to carry structured handoff metadata Kelvran's schema doesn't have
a field for" — does not hold for either framework surveyed. Both frameworks push handoff semantics
down into ordinary tool-call arguments (OpenAI Agents SDK) or an ordinary tool return value (AutoGen
Core), which Kelvran's Chat-Completions-shaped canonical schema already carries via
`ChatRequest.Tools`/`ToolChoice` (already shipped, per `docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md`)
and `Message.ToolCalls`. Even the OpenAI Agents SDK's own richer client-side taxonomy
(`RunItemStreamEvent.handoff_requested`, `AgentUpdatedStreamEvent.new_agent`) is a *derived*,
SDK-internal categorization built by "matching tool names against registered handoffs" over the raw
stream — not a distinct wire-format the underlying Responses API (or a hypothetical gateway) emits.
This is consistent with, and sharpens, `gateway-agent-framework-interop-2026-09-12.md`'s existing
Finding 3 (`not_yet` on building Responses-API compatibility at all, pending real demand): even *if*
Kelvran someday builds that surface, this pass confirms it would not additionally need to emit
`handoff_requested`/`new_agent`-shaped events itself — the SDK computes them client-side from
ordinary tool-call/response events Kelvran's existing streaming pass-through already carries.

**Build now / not yet: `not_yet` — because there is nothing to build.** This is a confirmed
non-gap, valuable specifically because it rules out a plausible-sounding concern rather than because
it identifies new work. No trigger applies; revisit only if a third major framework (LangGraph,
CrewAI) is found to require a genuinely different, protocol-level handoff primitive — no evidence
of that surfaced in this pass or the prior interop research.

---

## Recommendation, mapped to the five research questions

1. **Has MCP's spec stabilized enough to fire the named trigger?** → **`not_yet`, reconfirmed with
   a sharper reason.** The July 2026 stateless rearchitecture is real and structural, and the MCP
   maintainers' own August 2026 roadmap post confirms the specific piece most relevant to
   `OutboundCredential` (agent identity/delegation auth — Workload Identity Federation, ID-JAG,
   token exchange) is explicitly unfinished, deferred to the *next* cycle, with the underlying SEP
   (1933) still in Draft. (Finding 1)
2. **Is there a real session/conversation-level KV-cache-reuse pattern beyond `CacheControl`?** →
   **Yes, real and precedented, but at a different layer (self-hosted inference engine, not opaque
   hosted-provider cache) — `not_yet` for Kelvran, unchanged from the existing verdict.** vLLM
   Router, GKE/llm-d's EPP, and LMCache are all real, shipped 2026 systems implementing this at the
   engine layer, directly relevant to Kelvran's `openaicompat` backends whenever the already-named
   traffic triggers fire; they don't create a new trigger, they add a second reference-implementation
   family for the existing one. (Finding 2)
3. **Do agent frameworks expect anything Kelvran's canonical schema doesn't have?** →
   **No — confirmed non-gap.** Both OpenAI Agents SDK and AutoGen Core push handoff/multi-agent
   metadata entirely through standard tool-calling arguments/return values, which Kelvran's schema
   already carries; even the richer OpenAI Agents SDK streaming taxonomy is client-side-derived, not
   a wire requirement. (Finding 3)
4. **Multi-agent fan-out cost/rate-limit composition gap?** → **Unresolved by this pass — zero
   claims survived adversarial verification on this question.** See Open Questions.
5. **Anything else a 2026 survey flags?** → **Largely unresolved by this pass** beyond what's
   captured in Findings 1–3; no additional claim on a distinct topic survived verification.

---

## Caveats

- **This pass produced confirmed claims for only three of the five research questions** (MCP
  stabilization, KV-cache/session affinity, agent-framework schema expectations). RQ4 (fan-out
  concurrency/rate-limit composition) and the open-ended RQ5 returned no surviving claims at all —
  disclosed honestly rather than papered over with speculative synthesis.
- **Finding 2's GKE/llm-d claim survived at 2-1**, with the dissent being a precision correction
  (prefix-cache match is one weighted scoring factor among several, not a deterministic
  "longest-match wins" rule) rather than a substantive refutation — treat the mechanism as real but
  the "routes to the replica with the longest match" phrasing as a simplification.
- **This is a fast-moving area on both sides.** MCP's own spec revision cadence (three dated
  revisions in ~15 months) means Finding 1's verdict should be re-checked at the next revision, not
  treated as permanently settled. The OpenAI Agents SDK's streaming taxonomy in Finding 3 is
  documented as of a `gpt-5.5`-era SDK version and could gain new event types with future releases.
- **No claim in this pass touched Kelvran's own code** — all three findings are pure external
  requirements/precedent research, cross-referenced against Kelvran's already-settled internal
  reports rather than a fresh code audit.

## Open Questions

1. **Multi-agent fan-out cost/rate-limit composition** — does Kelvran's existing
   `MaxInFlight`/`MaxConcurrentRequests`/RPM/TPM/budget-cap stack compose correctly when one agent
   run spawns many concurrent sub-agent calls (e.g., a supervisor fanning out to 10 parallel
   worker-agent calls under one `agent_run_id`), or is there a real gap comparable systems handle
   differently (e.g., a per-run fan-out budget distinct from a per-key budget)? This pass found no
   evidence either way — needs a dedicated follow-up pass, not a speculative answer here.
2. **Does LangGraph or CrewAI's own orchestration layer require anything beyond standard
   tool-calling for handoffs/sub-agent delegation**, the way OpenAI Agents SDK and AutoGen Core were
   both confirmed not to (Finding 3)? Only two of the four named frameworks were actually surveyed
   with surviving claims in this pass.
3. **Is Kelvran's raw SSE pass-through (Chat-Completions-shaped deltas) sufficient for a client-side
   SDK to build the OpenAI Agents SDK's own `RunItemStreamEvent`/`AgentUpdatedStreamEvent`-style
   categorization on top of it today**, or does something in Kelvran's own streaming normalization
   (e.g., how tool-call argument fragments are re-assembled across chunks, per
   `gateway/ARCHITECTURE.md`'s normalization point #1) interfere with that client-side derivation?
   Finding 3 confirms the *SDK* needs no new gateway primitive in principle, but this specific
   integration path was not directly verified against Kelvran's own streaming code in this pass.
4. **Would GKE/llm-d's or LMCache's specific open-source router component be a viable, adoptable
   dependency** if/when the self-hosted-backend KV-cache-affinity trigger (Finding 2) fires, versus
   Kelvran building a narrower purpose-built consistent-hash router itself — not evaluated in this
   pass, which established only that the pattern is real and precedented, not a build-vs-adopt
   recommendation.

## Synthesis: build_now vs not_yet

| Item | Verdict | Why |
|---|---|---|
| MCP spec stabilization (RQ1) | **`not_yet`**, sharper trigger | Agent-identity/delegation auth (the RFC's own most relevant piece) explicitly deferred to next roadmap cycle; SEP-1933 still Draft |
| Self-hosted KV-cache/session affinity (RQ2) | **`not_yet`**, unchanged verdict | Same production-traffic triggers as the already-settled provider-cache-locality finding; this pass adds a second reference-implementation family, not a new trigger |
| Agent-framework schema/streaming/handoff needs (RQ3) | **`not_yet` — confirmed non-gap** | Handoffs ride on standard tool-calling in both frameworks surveyed; Kelvran's schema already carries this |
| Multi-agent fan-out concurrency composition (RQ4) | **unresolved** | Zero claims survived verification; genuine gap needing a dedicated follow-up pass |
| Open-ended additional gaps (RQ5) | **unresolved** | No additional claim on a distinct topic survived verification |
