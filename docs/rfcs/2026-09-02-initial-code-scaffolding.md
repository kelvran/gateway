- **Status**: accepted
- **Date**: 2026-09-02
- **Author(s)**: project founder + Claude Code

## Summary

Scaffold the first buildable skeleton of both deployables — `gateway` (Go) and `evals` (Python) — following exactly the package layouts, request/rollout lifecycles, and dependency-direction rules already committed to in `gateway/ARCHITECTURE.md` and `evals/ARCHITECTURE.md`. This is a **skeleton**, not a feature-complete Phase 0: it must compile, pass its own unit tests, and prove out the hardest architectural seams (provider adapter translation, the `cache.Cache` interface boundary, the Go/Python `api/` contract boundary) — but it does not need every capability `PRD.md`'s Phase 0 section eventually lists.

## Motivation

Nothing in this repository currently builds or runs — 45 documentation files describe a system that doesn't exist yet as code. The two riskiest architectural bets already made (Cache embedded behind a narrow interface so it's extractable later; provider adapters normalizing four documented hazards) are unverified until real code proves the seam actually works as designed. Scaffolding now, deliberately scoped small, is cheaper than discovering a design flaw after Phase 0's full feature set is built on top of a wrong foundation.

## Detailed Design

**Scope boundary — read this first.** "Scaffolding" here means: the directory/package structure matches each `ARCHITECTURE.md` exactly, the code compiles and its own tests pass, and the two or three hardest seams are implemented for real (not stubbed) so they're actually proven. Everything else is either a thin, honestly-labeled stub (returns a clear "not implemented" error, never a fake success) or deferred entirely to a future `docs/plans/` entry. This is narrower than the "Phase 0 — MVP" bullet lists in `PRD.md` and the original per-component research (`ai-infra-research/gateway.md`/`evals.md`) — those remain the target for a *later*, larger implementation pass, not this one.

### Gateway (Go) — what's real vs. stubbed

**Real, working, tested:**
- Canonical request/response types (OpenAI Chat-Completions-shaped, per `gateway/ARCHITECTURE.md` §"Canonical Schema").
- Two provider adapters implemented for real: **OpenAI** (near-identity, since the canonical schema already matches its shape) and **Anthropic** (a genuine translation — system-prompt placement, message-shape differences — because a single real translation proves the adapter pattern; a second near-identity adapter would not).
- The `cache.Cache` interface (`port.go`) plus the `inprocess` adapter, implementing **L1 exact-match only** (hash of model+messages+params → cached response, in-memory, TTL-based). The two dormant network adapters (`grpcserver`/`grpcclient`) are created as documented, unimplemented stubs — present as the seam, not functional — per `docs/decisions/0002-cache-embedded-in-gateway.md`.
- A minimal HTTP server (`cmd/gateway`) wiring auth → rate-limit → cache lookup → router → adapter → upstream call → cache write-back, non-streaming only for this pass (streaming is real but harder; deferred, see Unresolved Questions).
- In-memory token-bucket rate limiting (single-instance, per `PRD.md`'s explicit Phase 0 scope note — Redis-backed distributed limiting is Phase 1).
- A single static virtual-key check (one configured key, single-tenant) — real, not a stub, but intentionally far short of the full team/budget/hierarchical-scope model `evals/ARCHITECTURE.md`'s sibling doc describes for later.
- Structured JSON request logging via `log/slog` with a token-usage-based cost calculation against a static price table (float64 arithmetic for this pass — `PRD.md`'s Decimal-precision requirement is explicitly a Phase 1 upgrade, not scaffolding).
- Provider adapter round-trip unit tests (canonical → provider-native → canonical must be lossless), per `docs/testing/TESTING.md` §3's explicit requirement.

**Stubbed, honestly:**
- Gemini, Bedrock, and generic OpenAI-compatible adapters — package exists, returns a clear `ErrNotImplemented`, never a silent no-op.
- Streaming (SSE pass-through) — not implemented this pass; every response is buffered non-streaming. This is a real functional gap, not a nice-to-have, and is called out explicitly rather than glossed over.
- MCP/A2A brokering, guardrails, OTel span emission — no code at all yet; these are Phase 1+ per `PRD.md`'s scope, and scaffolding them now would be speculative.

### Evals (Python) — what's real vs. stubbed

**Real, working, tested:**
- `EvalCase` data model (pydantic, versioned IDs, tier field) per `evals/ARCHITECTURE.md`'s Data Model sketch.
- A Wilson confidence-interval calculator — pure math, fully testable without any external dependency, and directly enforces `PRD.md`'s "never a bare pass rate" success metric.
- A deterministic (exact-match/regex) scorer — real, no external calls needed.
- A CLI (`evals run`, `evals report`) that always prints a Wilson CI alongside any pass rate, per the same requirement.

**Stubbed, honestly:**
- The LLM-as-judge scorer — the prompt template (CoT-forcing, per `evals/ARCHITECTURE.md`) is real, but it requires a live provider API key to actually call out; unit tests exercise it against a fake/mock judge, not a real API, so the test suite never requires secrets to pass.
- Docker-sandboxed rollout execution — a real wrapper around `docker run` exists (not just a stub function), but is exercised by an integration-tagged test that's skipped by default (requires a live Docker daemon) rather than run in the default `pytest` invocation — consistent with `docs/testing/TESTING.md`'s integration-layer guidance.
- Rollout scheduler, trace collector, full CI/CD gate tiers — no code yet; Phase 1+ per `PRD.md`.

### The `api/` Contract

**Not scaffolded this pass.** No `.proto` files are created. Both deployables' Phase-0 code operates independently (Gateway logs JSON to stdout; Evals has no live traffic to sample yet, since Evals is inert without Gateway production traffic per the original research). The contract boundary becomes real the moment Evals needs to ingest Gateway's telemetry — tracked as an open item in `docs/research/RESEARCH.md`, not designed here.

## Drawbacks

- A skeleton this narrow will need real follow-up work almost immediately (streaming, Redis-backed rate limiting, the remaining provider adapters) — accepted, because a wrong foundation discovered late costs more than a visibly incomplete one discovered now.
- Non-streaming-only in the gateway skeleton means it cannot yet serve the exact request shape most real LLM traffic uses — explicitly flagged in `docs/agents/AGENTS_LEARNING.md`'s eventual Evolution Log as the first thing to fix once this scaffolding lands, not hidden.

## Alternatives Considered

1. **Scaffold to full Phase 0 feature parity** (all 5 providers, streaming, distributed rate limiting, Decimal cost accounting) in one pass — rejected: too large for one session to verify carefully, and several of those pieces (Redis-backed rate limiting) need infrastructure (a running Redis) this pass doesn't set up.
2. **Scaffold Gateway only, defer Evals entirely** — rejected: Evals' package layout and its two genuinely-testable-without-external-deps pieces (Wilson CI, deterministic scorer) are cheap to prove out now, and doing both in parallel costs no more than doing one.
3. **Skip real adapter logic, stub everything** — rejected: the entire point of scaffolding is to prove the two riskiest seams (adapter translation, the Cache interface boundary) actually work; stubbing them defeats the purpose.

## Unresolved Questions

- Streaming support timing — is it the very next `docs/plans/` entry after this one, or does distributed rate limiting come first? Not decided here; whichever is picked should get its own plan.
- Exact Go/Python version pins in `go.mod`/`pyproject.toml` — this pass pins to what's locally verified available (Go 1.26.5 installed, targeting `go 1.25` language level per `AGENTS.md`'s stated minimum; Python 3.14.7 installed, targeting `>=3.12` per `evals/ARCHITECTURE.md`) — revisit once CI is real and can enforce a matrix.
