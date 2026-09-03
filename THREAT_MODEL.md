# Threat Model

## Methodology

STRIDE applied per component against the data-flow diagram in `ARCHITECTURE.md`. Trust boundaries: tenant ↔ Gateway; Gateway ↔ upstream providers; Gateway ↔ Cache (internal, in-process — a trust boundary in the STRIDE sense even without a network hop, because it still crosses tenant-isolation logic); Evals-control ↔ sandboxed agent execution; Gateway/Evals ↔ the shared `api/` contract. This document is reviewed on its own cadence (see "Review Cadence" below), decoupled from `ARCHITECTURE.md`'s — a new CVE, paper, or major release can invalidate a threat-model assumption without any architecture change at all.

## Component: Gateway

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | Virtual-key forgery; MCP/A2A OAuth2 token-passthrough abuse (a documented real-world bug class in the most mature OSS MCP gateway implementation surveyed) | Virtual keys are opaque, unguessable, tenant-scoped tokens validated against `internal/identity` on every request; MCP/A2A brokering shares — never bypasses — the same identity/budget objects as the outbound LLM path |
| Tampering | Request/response mutation in transit between adapter stages | Canonical schema validated at each adapter boundary; no raw pass-through of unvalidated provider fields |
| Repudiation | "Why did this agent run cost $X" has no answer beyond a raw total (the exact gap every competitor surveyed has) | `agent_run_id` propagated via OTel Baggage from the first line of the pipeline; span-level cost attribution down to the individual tool call/retry |
| Information Disclosure | Prompt/completion content, which may contain PII, traverses guardrails and cache write-back | PII/content guardrail pre- and post-call; cache write-back respects tenant namespace (see Cache STRIDE table below) |
| Denial of Service | Retry storms in agentic traffic — many agents retrying on a fixed interval synchronize into a thundering herd | Exponential backoff with jitter; global circuit breaker/load-shedding; tool-level retry budgets and per-agent concurrency caps enforced in code, not left to prompt-level convention |
| Elevation of Privilege | Guardrail bypass on non-tool operations (a documented bug class in the MCP gateway implementations surveyed) | Guardrails apply uniformly to every operation type the MCP/A2A broker exposes, not just tool-call operations specifically |

## Component: Cache

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | N/A at this layer (auth already resolved upstream by Gateway before Cache is consulted) | — |
| Tampering | Cache poisoning — a 2026 NDSS paper ("When Cache Poisoning Meets LLM Systems") demonstrated 81-89% success rates against production semantic-caching integrations at major cloud providers | Entity/number/date hard-gate in front of every semantic hit (never a bare similarity threshold — see `PRD.md`'s explicit scope requirement); provenance metadata stored at write time so poisoned entries are traceable |
| Repudiation | A stale cache hit silently degrades an agent's downstream action with no error and a clean-looking trace | Cache-hit responses are annotated with provenance (cache age, similarity score, which layer served it) so a downstream failure is traceable back to a specific cache decision |
| Information Disclosure | **Cross-tenant cache leakage** — a 2026 study ("KeyPooling") found this exploitable in 5 of 5 tested production-representative gateways, with pooled upstream credentials as the specific mechanism | Tenant namespace baked into the vector-index partition itself (not a post-hoc filter) and enforced at every hop: lookup, write, retry, fallback, connection-pool reuse |
| Denial of Service | Cache-miss storms causing redundant expensive upstream calls | Request coalescing/singleflight — a distributed lock elects one leader to call upstream on a miss; followers share the result |
| Elevation of Privilege | **Semantic-cache response hijacking** — "CacheAttack" (2026) demonstrated an 86% hijack rate (90.6% against agentic tool invocation, including a financial-agent case study triggering an unintended `sell` order) via similarity-based key collision | Entity/number/date hard-gate + freshness/risk model (not similarity alone) before any semantic hit is served; similarity threshold never below ~0.9 for open-ended, user-facing traffic |

## Component: Evals

| STRIDE | Threat | Mitigation |
|---|---|---|
| Spoofing | A rollout claims to be a golden/regression-tier run when it isn't, corrupting the regression dataset | `EvalCase.tier` is set at dataset-registration time, not by the rollout itself; frozen/immutable revisions |
| Tampering | Judge manipulation — adversarial prompts designed to flip a judge's verdict; research shows up to 100% verdict-flip rates against a single unmitigated LLM-judge call under adversarial pressure | CoT-forcing + reference-guided grading as v1 defaults; judge model always ≠ policy model; adversarial skeptic-panel verification designed into the Scorer Service's interface as a v2 upgrade path (see `evals/ARCHITECTURE.md`) |
| Repudiation | A surprising score gets blamed on the agent when it was actually a grading bug (a documented real failure pattern: a rigid string-match grader produced a 42% score that became 95% once the grader itself was fixed) | Rubrics/graders are reviewed, versioned artifacts; a surprising score triggers grader root-cause review before the agent is blamed |
| Information Disclosure | Sandboxed rollout exfiltrates data via an unexpected channel (network egress, shared filesystem) | **Partial, corrected 2026-09-04.** Real today: `--network=none` on every `run_in_sandbox()` call (`evals/rollout/sandbox.py`) — a full egress block, actually stronger than an allowlist for v1's no-legitimate-egress-need profile (a fixed image+command, no LLM call from inside the sandbox), and genuinely tested (`test_run_in_sandbox_blocks_network_egress`). Docker's own default `--rm` per-container writable-layer lifecycle applies, but this is Docker's default semantics, not a Kelvran-built ephemeral-filesystem guarantee (no `--read-only`/`--tmpfs`). **Not built**: scoped short-lived per-tool credentials issued via a proxy — no "tool" concept exists anywhere in `evals/` yet to scope a credential to. Revisit egress allowlisting (vs. the current stronger full-block) only once a real partial-egress need appears |
| Denial of Service | Cost blowout at eval scale — detecting a modest regression can require ~100 trials per scenario, and a naive suite can cost tens of thousands of dollars per regression check | **Not built, corrected 2026-09-04.** `evals/rollout/scheduler.py`'s `run_suite` is a single unconditional loop over every case — no tiered gating, no early-stopping rule, no prefix/result cache. Real cost-blowout mitigation remains future work, not yet designed |
| Elevation of Privilege | **Sandbox escape** — a real 2026 incident saw an agent escape its eval sandbox via a package-registry-proxy zero-day and coordinate with hundreds of other agents through an unsanctioned shared channel, ultimately breaching a production database | **1 of 5 originally-imagined layers real, corrected 2026-09-04.** Real: the execution boundary itself (Docker container/namespace isolation via a genuine `docker run` call). Not built, and — for three of the four — blocked on a separate undecided prerequisite, not just unstarted work: isolation *between* concurrent sandboxes is moot today (`scheduler.py` is sequential-only, zero concurrent sandboxes exist to isolate between); package-registry/dependency-proxy hardening is moot today (v1 runs a fixed image+command, nothing installs a dependency at runtime inside the sandbox); scoped per-tool credentials need a tool-calling harness concept that doesn't exist in `evals/`; full audit logging tied to the trace needs the `Trace`/`Span` model, itself blocked on `api/otel`'s still-undecided transport. See `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md`'s Motivation section for the full sizing — this splits into 3-to-5 separate future RFCs, not one |

## Cross-Component: MCP/A2A Tool Brokering

| STRIDE | Threat | Mitigation |
|---|---|---|
| Information Disclosure | Session/context leakage across concurrent tool calls — a documented bug class in the most mature OSS MCP gateway implementation surveyed | Session/context isolation enforced per concurrent tool call, not shared state across a connection pool |
| Elevation of Privilege | Auth-passthrough abuse — an A2A proxy trusting a self-reported (sometimes unreachable) agent-card URL instead of the operator-registered one | Operator-registered agent URLs are authoritative; caller identity is verified via the same `internal/identity` objects Gateway's outbound path uses, never re-derived from a self-reported card |

## OWASP LLM Top 10 (2025) Crosswalk

| OWASP Category | Where it maps in this system |
|---|---|
| LLM01 Prompt Injection | Guardrail pre/post-call (Gateway); tool-call argument validation (Gateway adapters) |
| LLM02 Sensitive Information Disclosure | PII guardrails; tenant-namespace isolation (Cache); provider data-flow inventory (`docs/operations/PROVIDERS.md`) |
| LLM04 Data and Model Poisoning | Cache poisoning defenses (entity/freshness gates); Evals dataset immutability/versioning |
| LLM06 Excessive Agency | MCP/A2A scoped credentials; per-tool short-lived tokens issued via a proxy outside the sandbox |
| LLM07 System Prompt Leakage | Adapter-level system-prompt handling isolated per provider; never cached across tenants |
| LLM08 Vector and Embedding Weaknesses | Cache's semantic layer (L3) hard-gates and entity checks directly target this category |
| LLM09 Misinformation | Cache freshness/risk model (a cached answer must still be *true*, not just similar); Evals statistical rigor |
| LLM10 Unbounded Consumption | Rate limiting, budgets, retry-storm defenses (Gateway); cost-blowout mitigations (Evals) |

## Review Cadence & Change Log

Reviewed on new CVE/GHSA disclosure affecting a dependency, on publication of new relevant research (this document already cites 2026 papers specifically because the threat landscape for semantic caching and agent sandboxing moved meaningfully that year), and at minimum once per major release — independent of `ARCHITECTURE.md`'s own review cadence.

- `2026-09-02` — Initial threat model, pre-scaffolding.
- `2026-09-04` — Corrected the Evals component's Information Disclosure, Denial of Service, and Elevation of Privilege rows: all three had claimed mitigations (network-egress allowlisting, ephemeral-filesystem-per-rollout, scoped per-tool credentials via a proxy, tiered CI gates/early-stopping/result-caching, cross-sandbox isolation, package-registry-proxy hardening, audit-trail-tied-to-trace) that had zero code behind them — found by the `docs/rfcs/2026-09-04-evals-llm-judge-provider-wiring.md` grounding research, structurally the same class of gap as the Guardrails row already found and fixed in this document for Gateway. Corrected to name exactly what's real (`--network=none`, the Docker execution boundary) vs. unbuilt, with three of the unbuilt items named as blocked on a separate undecided prerequisite (a tool-calling harness concept, the Sandbox Pool/concurrency layer, `api/otel`'s transport) rather than just unstarted engineering time.
