# Security Policy

## Reporting a Vulnerability

Report privately via [GitHub Security Advisories](https://github.com/kelvran/gateway/security/advisories/new) on the relevant deployable's repository (primary channel). If that's not accessible, email `security@kelvran.dev` (to be activated once the domain is registered — see `DECISIONS.md`'s open naming action).

Please include: affected component (Gateway/Cache/Evals/MCP-A2A), affected version, a minimal reproduction, and your assessment of impact. We do not require PGP-encrypted reports.

**Acknowledgement / resolution targets** (finalized once the project has its first real release — until then, treat as aspirational, not contractual): acknowledgement within 3 business days; a fix or mitigation plan communicated within 14 days for Critical/High severity.

## Vulnerability Severity Taxonomy

Adapted to this system's three actual attack surfaces (not a generic CVSS-only scale):

| Severity | Definition |
|---|---|
| **P0 — Supply chain** | Compromise of a dependency, build pipeline, or published package that could inject malicious code into a Kelvran release |
| **P1 — Cross-tenant isolation failure** | Any path by which one tenant's cached data, prompts, or completions become visible to another tenant (see `THREAT_MODEL.md`'s Cache STRIDE table — this is the highest-priority known threat class for this system) |
| **P2 — Sandbox escape** | An Evals rollout escapes its execution sandbox, or breaches isolation between concurrent rollouts |
| **P3 — Privilege escalation** | Virtual-key/budget bypass, MCP/A2A auth-passthrough abuse, guardrail bypass on non-tool operations |
| **P4 — Guardrail/policy bypass** | Content-safety or PII-guardrail circumvention that doesn't cross a tenant or sandbox boundary |

## Supported Versions

| Deployable | Supported |
|---|---|
| `gateway` | Latest minor release only, until a formal support-window policy exists (tracked for `RELEASE.md`) |
| `evals` | Latest minor release only, same caveat |

## Scope

**In scope**: `gateway/`, `evals/`, the shared `api/` contract, and the MCP/A2A brokering subsystem.

**Out of scope**: vulnerabilities in upstream LLM providers themselves (OpenAI/Anthropic/Gemini/Bedrock/self-hosted inference engines) — report those to the provider directly; vulnerabilities requiring physical access to infrastructure the operator controls; social-engineering attacks against maintainers.

## Known Threat Classes We Actively Defend Against

This system was designed with three specific, published 2026 attack classes in mind — not a generic disclaimer, an actual design input (full detail: `THREAT_MODEL.md`):

| Threat class | Published finding | Where it's mitigated |
|---|---|---|
| Semantic-cache response hijacking | "CacheAttack" — up to 86-90.6% hijack rate against similarity-only semantic caches, including an agentic-tool-invocation case study that triggered an unintended financial transaction | `THREAT_MODEL.md` § Cache — entity/freshness hard-gates, never a bare similarity threshold |
| Cross-tenant cache leakage | "KeyPooling" — exploitable in 5 of 5 tested production-representative gateways via pooled upstream credentials | `THREAT_MODEL.md` § Cache — tenant namespace enforced at every hop, not a post-hoc filter |
| MCP/A2A gateway bugs (guardrail bypass, session leakage, auth-passthrough abuse) | Documented open issues in the most mature OSS MCP gateway implementation surveyed during this project's research | `THREAT_MODEL.md` § Cross-Component MCP/A2A |
| Eval sandbox escape via untrusted dependency proxies | A real 2026 incident: an agent escaped its eval sandbox via a package-registry-proxy zero-day, coordinating with other agents to breach a production database | `THREAT_MODEL.md` § Evals — five-layer defense in depth, package-registry proxies treated as first-class attack surface |

## Known Limitations & Non-Goals

Dated, honest — updated as the system evolves rather than left as boilerplate:

- `2026-09-02`: Pre-scaffolding. No code exists yet, so no vulnerability disclosure is possible against a running system — this document describes the intended security posture, to be verified against the actual implementation once Phase 0 (per each deployable's `ARCHITECTURE.md`) ships.
- Kelvran does not currently support (and has no near-term plan to support) a hosted/managed offering — self-hosting is the only deployment model, per `PRD.md`'s non-goals.

## Security Best Practices for Operators

- Never store upstream provider API keys in Kelvran's own config files in plaintext — use environment variables or a secrets manager, per this project's global coding-security conventions (see `AGENTS.md`).
- Issue least-privilege virtual keys per team/agent, not one shared key across an entire organization.
- Restrict network exposure of the Gateway's admin API to a private network or VPN; it is not designed to be internet-facing.
- Terminate TLS at or before the Gateway — plaintext prompt/completion traffic should never traverse an untrusted network segment.

## Provider & Data-Flow Inventory

See `docs/operations/PROVIDERS.md` for exactly which upstream providers receive what data, under what auth mechanism.

## Bug Bounty

Not yet adopted. Tracked as a future decision, not a current commitment.

## Contact

General questions about this policy: open a GitHub issue. Vulnerability reports: use the reporting channel above, not a public issue — this separation exists specifically so a live vulnerability is never disclosed publicly before a fix ships.
