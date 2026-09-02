# Kelvran — Architecture

This is a thin, current-state index. It does not duplicate the substantive design content in `gateway/ARCHITECTURE.md` and `evals/ARCHITECTURE.md` — read those for the actual internals. This document exists to show how the two deployables fit together and to be the one page that links everywhere else. For the reasoning behind *why* the system is shaped this way, see `DESIGN.md` and `docs/decisions/`. For how it can be attacked, see `THREAT_MODEL.md`.

## Component Map

| Component | Purpose | Deployable | Path |
|---|---|---|---|
| Gateway | Unified LLM API proxy/router: schema normalization, routing/failover, quota, streaming, guardrails, cost attribution, MCP/A2A brokering | `gateway` (Go) | `gateway/` |
| Cache | Multi-layer response caching (exact/normalized/risk-gated semantic), embedded — not a network hop | `gateway` (Go) | `gateway/internal/cache/` |
| Evals | Sandboxed agent-rollout execution, LLM-judge scoring, statistics, harness-transparent reporting | `evals` (Python) | `evals/` |
| Shared contract | Versioned OTel + cost/usage event schema — the only surface either deployable is allowed to depend on across the language boundary | both | `api/` |

## Request-Flow Walkthrough

**Gateway path** (every inbound LLM call): client/agent request → auth (virtual key → team/budget) → rate limit → Cache lookup (L1 exact → L2 normalized → L3 risk-gated semantic) → hit returns immediately; miss → router selects a provider deployment → provider adapter translates the canonical request → upstream call (streaming pass-through) → response translated back to canonical shape → guardrail post-call check → Cache write-back → cost/OTel finalize (always runs, even on error) → response to client. Full detail: `gateway/ARCHITECTURE.md`.

**Evals path** (never in the request path, two independent triggers): *offline* — a rollout scheduler calls Gateway's own API to score a candidate model/prompt/route before it's promoted; *online* — a trace collector samples Gateway's production `gatewayevents`/OTel telemetry, promotes failures into the regression dataset, and feeds judged-quality signals back toward Gateway's routing table and Cache's freshness gate (both are v2+ consumers of this signal, per `PRD.md`'s scope). Full detail: `evals/ARCHITECTURE.md`.

## Cross-Cutting: Contract Versioning

Both deployables generate language-native bindings from the same protobuf source in `api/` (see `api/README.md`). `buf breaking` runs in CI against that directory specifically. Neither deployable imports the other's source code, ever — the dependency-direction rules enforced in each deployable's own `ARCHITECTURE.md` both terminate at this boundary, not across it.

## Cross-Cutting: Identity & Cost

Gateway's `internal/identity` (virtual keys, teams, budgets, tenant resolution) and Cache's tenant-namespace isolation are the same primitive under different names — one schema, defined once in `gateway/internal/identity`, used by both. Cost accounting (`internal/costaccounting`, Decimal-precision) is likewise a single engine feeding Gateway's budget enforcement, Cache's future break-even calculations, and — via `gatewayevents` — Evals' cost-per-run metric.

## What This Document Deliberately Does Not Cover

Provider-specific data flows → `docs/PROVIDERS.md`. Attack surface and mitigations → `THREAT_MODEL.md`. Directory-level file map → `REPO_LAYOUT.md`. Release/versioning mechanics → `RELEASE.md`.
