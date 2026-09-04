# Kelvran

**A unified AI infrastructure platform: an LLM gateway with an embedded, risk-gated cache, and an agent-evaluation system built on adversarial verification — one architecture, not three integrations.**

> **Status: real, released, actively developed.** `gateway/v0.1.0` and `evals/v0.1.0` are tagged, released, and live at `github.com/kelvran/gateway` — CI runs green on every push. This is still a deliberately narrow v1 slice, not a feature-complete platform: `docs/rfcs/2026-09-02-initial-code-scaffolding.md` is the authoritative list of what's real vs. intentionally stubbed vs. not built at all, and `STATUS.md` is the current, continuously-updated snapshot of what's shipped since that tag. Check `docs/agents/LOGS.md` for the most recent real progress and `docs/users/USER_GUIDE.md` for what's genuinely implemented vs. still intended.

## Why Kelvran

Every team running LLM traffic in production eventually assembles the same three tools — a gateway, a cache, and an evals system — from three different vendors that don't share a concept of what a *request* is once an autonomous agent, not a human, is the one making it. That gap produces three concrete, documented failures: gateways that can't answer "why did this agent run cost $4," semantic caches that gate reuse on "close enough" instead of "still true" (and are consequently exploitable — see `THREAT_MODEL.md`), and eval systems that trust a single grader despite research showing that grader can be adversarially flipped up to 100% of the time.

Kelvran is one system built around agent-run-level accountability at every layer instead. Full reasoning: `PRD.md`.

## What's Here

| Component | What it does | Deployable |
|---|---|---|
| **Gateway** | Unified API across OpenAI/Anthropic/Gemini/Bedrock/self-hosted models — routing, failover, streaming, virtual keys/budgets, MCP/A2A tool brokering, OTel observability with agent-run-level cost attribution | `gateway/` (Go) |
| **Cache** | Multi-layer response caching (exact → normalized → risk-gated semantic) — embedded inside Gateway, not a network hop; hardened against cross-tenant leakage and semantic-cache hijacking | `gateway/` (Go, internal module) |
| **Evals** | Sandboxed agent-rollout execution, LLM-as-judge scoring with statistical rigor (confidence intervals, harness-transparency), designed for a future adversarial skeptic-panel upgrade | `evals/` (Python) |

## How It Compares

| | Kelvran | LiteLLM | Portkey | TensorZero | Bifrost | Kong AI Gateway | Langfuse | Braintrust |
|---|---|---|---|---|---|---|---|---|
| Language | Go + Python | Python→Rust (migrating) | TypeScript | Rust | Go | Lua/OpenResty + Go | Python/TS | Python/TS/Go/... |
| Agent-run-level cost attribution | **Yes, foundational** | No (call-level only) | No | No | No | No | Partial (tracing only) | Partial |
| Cache reuse gated on correctness, not just similarity | **Yes** | No | No (threshold only) | N/A | No (threshold only) | No (threshold only) | N/A | N/A |
| Adversarial multi-judge eval verification | **Designed in, v2** | N/A | N/A | Single judge | N/A | N/A | Single judge | Single judge |
| Self-hostable | Yes | Yes | Yes (core) | Yes | Yes | Core only | Yes | No (SaaS-primary) |
| Open source | Yes (Apache-2.0) | Yes + paid Enterprise | OSS core + paid | Yes, no paid tier | Yes | OSS core, AI plugins gated | Yes | No |

Full per-competitor detail and citations: `Not-Humans-World/ai-infra-research/gateway.md`, `cache.md`, `evals.md` (parent workspace — the research this project is built from).

## Quickstart

*(Not runnable yet — this describes the intended v1 shape per `PRD.md`.)*

```
# Gateway
cd gateway && go build ./cmd/gateway && ./gateway --config config.yaml

# Evals
cd evals && uv run evals run --suite golden
```

## Architecture at a Glance

```
client/agent → Gateway (auth → rate-limit → Cache lookup → route → provider → guardrail → cost/OTel) → response
                                     │
                                     ▼  (OTel spans + gatewayevents, via api/)
                                   Evals (offline rollouts + online production sampling)
```

Full detail: `ARCHITECTURE.md` → `gateway/ARCHITECTURE.md` / `evals/ARCHITECTURE.md`.

## Documentation

- [`STATUS.md`](./STATUS.md) — live project-status dashboard
- [`PRD.md`](./PRD.md) — what to build and why
- [`DESIGN.md`](./DESIGN.md) — whole-system design + the 3 foundational decisions' rationale
- [`ARCHITECTURE.md`](./ARCHITECTURE.md) — current-state architecture index
- [`docs/decisions/`](./docs/decisions/) — ADRs for the foundational, hard-to-reverse calls
- [`docs/rfcs/`](./docs/rfcs/) / [`docs/plans/`](./docs/plans/) / [`docs/research/RESEARCH.md`](./docs/research/RESEARCH.md) — the proposal → plan → open-questions pipeline
- [`DECISIONS.md`](./DECISIONS.md) — the continuous, terse decision log
- [`THREAT_MODEL.md`](./THREAT_MODEL.md) — STRIDE-per-component + OWASP LLM Top 10 crosswalk
- [`SECURITY.md`](./SECURITY.md) — disclosure policy and known threat classes
- [`docs/testing/TESTING.md`](./docs/testing/TESTING.md) — full test-pyramid strategy
- [`docs/operations/`](./docs/operations/) — `DEPLOY.md`, `TELEMETRY.md`, `PROVIDERS.md`
- [`docs/development/BRANCHES.md`](./docs/development/BRANCHES.md) — branch strategy
- [`docs/users/USER_GUIDE.md`](./docs/users/USER_GUIDE.md) — operator how-to guide
- [`REPO_LAYOUT.md`](./REPO_LAYOUT.md) — literal directory map
- [`AGENTS.md`](./AGENTS.md) / [`CLAUDE.md`](./CLAUDE.md) — instructions for AI coding agents working in this repo (see also `docs/agents/ETHOS.md` and `docs/agents/AGENTS_LEARNING.md`)
- [`CONTRIBUTING.md`](./CONTRIBUTING.md) / [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md) / [`SUPPORT.md`](./SUPPORT.md) — how to contribute and get help
- [`RELEASE.md`](./RELEASE.md) / [`RELEASE_NOTES.md`](./RELEASE_NOTES.md) / [`UPGRADE.md`](./UPGRADE.md) / [`DEPRECATED.md`](./DEPRECATED.md) — release mechanics and history

## License

[Apache-2.0](./LICENSE)
