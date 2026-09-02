# User Guide

Operator-facing "how do I configure and run Kelvran" guide — goes beyond `README.md`'s quickstart, never re-explains *why* something is designed the way it is (that's `ARCHITECTURE.md`/`gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`'s job; this document assumes you've read those or don't need to).

> **Pre-scaffolding notice**: no code exists yet. Every section below describes the intended v1 configuration surface per `PRD.md`, not a tested reality. Sections are honestly marked where a feature isn't implemented yet.

## 1. Before You Start

Decide your topology: local dev (Docker Compose) or production (Kubernetes) — see `docs/operations/DEPLOY.md`. You need at least one upstream LLM provider's credentials before Kelvran does anything useful.

## 2. Provider Credentials

See `docs/operations/PROVIDERS.md` for the full list of supported providers and what each one needs. Secrets handling: environment variables or a secrets manager only, never a committed config file — see `SECURITY.md`.

## 3. Virtual Keys and Budgets

Real and implemented, per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`. Generate a key yourself — Kelvran never generates or stores the raw secret, only its hash:

```bash
openssl rand -hex 32                          # this is the secret — give it to the caller, keep it out of config
printf '%s' '<that secret>' | sha256sum        # this hash goes in config.yaml as key_hash
```

Add it to `config.yaml` under `virtual_keys:`:

```yaml
virtual_keys:
  team-alpha:
    key_hash: "<the sha256 hash from above>"
    budget_usd: 100.0          # optional; omit or 0 for unlimited
    rate_limit:
      burst: 20
      refill_per_second: 10
    allowed_models:            # optional; omit for "every configured model"
      gpt-4o: true
```

Clients authenticate with `Authorization: Bearer <the raw secret>` — never the hash. Budget and rate-limit state are tracked **in memory only** and reset on restart; there is no persistent control-plane store yet (see `STATUS.md`/`DECISIONS.md`). A key that exceeds its budget gets HTTP 429 (distinguishable from a rate-limit 429 only by the error message body, per that RFC's OpenAI-SDK-compatibility rationale); a request for a model outside `allowed_models` gets HTTP 403.

**Not implemented yet:** the "teams" hierarchy (a key inheriting a team's budget/rate-limit ceiling) and live, no-restart key provisioning — both remain flat, single-level, static-YAML-only for now, per that RFC's explicit scope boundary.

## 4. Routing & Failover Configuration

*(Not implemented yet.)* Intended model: a routing config names a model group, a list of deployments within it, and a load-balancing strategy (weighted/latency-based/cost-based); a fallback chain is a separate, explicit list consulted when the primary deployment fails.

## 5. Cache Configuration

*(Not implemented yet — L1/L2 are Phase 0, L3 is Phase 1 per `gateway/ARCHITECTURE.md`.)* Three layers: exact-match (L1), normalized-match (L2), risk-gated semantic (L3). L3's similarity threshold is tunable per content-type/tenant — **never disable the entity/freshness hard-gate to chase a higher hit rate.** This isn't a suggestion: `AGENTS.md`'s Boundaries section lists this as a hard "Never," specifically because of the CacheAttack finding in `THREAT_MODEL.md` (an 86-90% response-hijack rate against exactly this kind of unguarded semantic cache).

## 6. MCP/A2A Tool Brokering

*(Not implemented yet — v2 per `PRD.md`'s scope.)* Registering an MCP server or A2A agent will go through the same identity/budget objects as outbound LLM routing, per `gateway/ARCHITECTURE.md`'s MCP/A2A Subsystem section. Auth-passthrough is intentionally limited — see `THREAT_MODEL.md`'s Cross-Component MCP/A2A row for why.

## 7. Observability

See `docs/operations/TELEMETRY.md` for the full SLI/dashboard/alerting picture. One concrete example once it exists: a request span showing `agent_run_id`, provider, latency, and cost, queryable end to end — not yet implemented, tracked the same way as everything else in this section.

## 8. Running Your First Eval Suite

*(Not implemented yet.)* Intended flow: define an `EvalCase` set (golden tier), point the rollout scheduler at it, get back scores with a confidence interval attached — never a bare pass rate, per `evals/ARCHITECTURE.md`'s harness-transparency design. v1 scoring is a single LLM-judge with bias mitigations; the adversarial skeptic-panel upgrade is v2 — don't expect panel-level rigor from the first working version, and this guide won't overclaim it once it exists either.

## 9. Upgrading

See `UPGRADE.md` for the actual migration steps once something has shipped (currently empty — nothing has).

## 10. Troubleshooting

Common misconfigurations will be documented here once there's a real system to misconfigure. For anything not covered, see `SUPPORT.md`.
