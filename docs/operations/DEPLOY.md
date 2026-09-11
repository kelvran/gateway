# Deployment Guide

## Overview

Two independently deployable units — `gateway` (Go) and `evals` (Python) — joined only by the versioned `api/` contract. This document covers operational deployment only; for *why* they're structured this way, see `DESIGN.md` and `docs/decisions/`. **Cache is not a third deployable** — it ships inside the `gateway` binary; nothing here treats it separately.

## Deployment Models

- **Docker Compose** (local/dev) — `gateway` as a Compose service, plus an optional Redis dependency, for local development. `evals` is a Click CLI, not a long-running process — run it directly, not as a Compose service (see below).
- **Kubernetes / production** — `gateway` and `evals` as separate Deployments, scaled and released independently, sharing only the `api/` contract version they were built against.

## Prerequisites

- Go 1.26+ runtime for `gateway`, Python 3.12+ for `evals` (per `gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`'s tech-stack tables).
- Provider credentials for whichever upstream LLM providers are configured — see `docs/operations/PROVIDERS.md` for exactly which providers exist and what each needs.
- Redis for `gateway`, **only if** `config.yaml`'s `rate_limit.redis_addr` is set (distributed rate limiting across multiple gateway instances, per `docs/rfcs/2026-09-03-distributed-rate-limiting.md`) — omit it entirely for the default, in-memory-only rate limiter. **Postgres is not used by any shipped code today** — it's a `gateway/ARCHITECTURE.md` Tech Stack *future target* for a control-plane config store, not real yet (config is static YAML, loaded once at startup — see that doc's `/internal/admin` entry).
- Fail-fast on missing required environment variables at startup — never start in a half-configured state, per this project's own security conventions (`SECURITY.md`).

## Docker Compose (Local/Dev)

Real: `docker-compose.yml` at the repo root defines a `gateway` service (built from `gateway/Dockerfile`, unmodified) and an *optional* `redis` service gated behind a Compose profile — `docker compose --profile redis up` — since Redis is only needed when `rate_limit.redis_addr` is configured. There is no `postgres` service (nothing uses it, see Prerequisites) and no `evals` service (see Deployment Models above).

To bring `gateway` up locally:
1. `cp gateway/config.example.yaml gateway/config.yaml` and fill in real values (both files are gitignored except the `.example` one — see `.gitignore`).
2. `cp .env.example .env` and set the real API key(s) your `config.yaml`'s deployments reference.
3. `docker compose up gateway` (add `--profile redis` first if `config.yaml` sets `rate_limit.redis_addr: redis:6379`).

**Corrected 2026-09-11** — this section was stale since 2026-09-07: a real `/healthz` endpoint now exists (`mux.HandleFunc("/healthz", healthzHandler)`, `cmd/gateway/main.go`) — a shallow liveness probe, no auth, returning `200 {"status":"ok"}`, covered by an integration test. Readiness is `GET /healthz` returning `200`, not merely "the container is listening on `:8080`."

## Kubernetes / Production

*(Intended shape.)* Separate Deployment + Service per component, a Helm chart or plain manifests under a future `deploy/k8s/` directory (not created yet — a real, working `gateway` Docker image exists per its multi-stage `Dockerfile`, but no Kubernetes manifests reference it yet). Resource requests/limits should be set generously enough to avoid the CFS-throttle-induced GC pause spikes `gateway.md`'s original research flagged for Go under tight Kubernetes CPU quotas. Expose `gateway`'s OTel/metrics endpoint and its main API endpoint separately — never make the admin/control-plane API internet-facing (per `SECURITY.md`'s operator best practices).

## Configuration Reference

`gateway`'s config schema is real (`gateway/internal/gateway/controlplane/config.go`) — see `gateway/config.example.yaml` for every real section (`virtual_keys`, `deployments` incl. `weight`, `telemetry`, `budget`, `rate_limit`, `cache` incl. `l2`/`l3`, `guardrails`, `price_table`) with inline documentation. That's the YAML schema; the real per-*environment-variable* table (a narrower, separate thing) is:

| Variable | Required | Read by | Purpose |
|---|---|---|---|
| One per configured deployment's `api_key_env` value (e.g. `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` — see `.env.example`) | No — logged as a warning at startup if unset, not fatal (`cmd/gateway/main.go`'s `buildPipeline`); calls to that deployment will fail at request time instead | `cmd/gateway`'s `buildPipeline`, via `os.Getenv` | The named deployment's upstream provider API key |

Everything else `gateway` needs is YAML config, **not** an environment variable — the config file path (`-config` flag, defaults to `config.yaml`), the Redis address (`rate_limit.redis_addr`), the budget-persistence path (`budget.persist_path`), and every virtual key's secret (only its SHA-256 *hash* ever lives in `config.yaml`; the raw secret is client-held and sent as a bearer token, never stored by the gateway process at all — see `docs/rfcs/2026-09-02-virtual-keys-budgets.md`). Secrets handling rule: never in a committed file, environment variables or a secrets manager only — this doesn't get restated per-variable, it's a blanket rule cross-linked from `SECURITY.md`.

`evals` has its own, separate env-var table — real, judge-related credentials, never YAML config:

| Variable | Required | Read by | Purpose |
|---|---|---|---|
| `ANTHROPIC_API_KEY` | Only for `evals run --llm-judge` | `judge/providers.py`'s `make_anthropic_call_model` (Anthropic SDK reads it directly) | Single-judge mode's direct Anthropic API call |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_REGION` | Only for `evals run --llm-judge-panel` | `judge/providers.py`'s `make_bedrock_call_model` (boto3's standard credential chain) | The 2-judge panel's two Bedrock-hosted Claude calls (Sonnet 5 + Haiku 4.5) |
| `OPENAI_API_KEY` | Not required by anything today | `make_openai_call_model` exists but has no real caller (the panel moved to Bedrock) | Reserved for a future caller |

Unlike `gateway`, `evals` auto-loads these from `evals/.env` (gitignored; copy from `evals/.env.example`) via `evals.cli.main`'s `_load_env_file()` — a local-dev convenience, not a requirement. A real, already-exported process env var (a CI secret, an explicit `export`) always wins over the file (`override=False`), so CI and production behavior is unaffected either way.

## Independent Deployability & Contract Compatibility

The one thing genuinely specific to a two-deployable system: `gateway` and `evals` can be deployed and upgraded independently, but only within a compatible `api/` contract version range. Before either deployable ships, confirm:

| gateway version | evals version | api/ contract version | Compatible? |
|---|---|---|---|
| v0.1.0 | v0.1.0 | `api/gatewayevents/v1` (initial) | ✅ |
| v0.9.0 | v0.8.0 | `api/gatewayevents/v1` (unchanged since v0.1.0 — additive-only field/enum additions, no breaking version bump yet) | ✅ — current, as of 2026-09-11 |

**Corrected 2026-09-11**: this table previously stopped at `v0.1.0`/`v0.1.0`, calling it "the only released pair so far" — stale since the second release. Both deployables have since versioned independently at least once (`gateway/v0.8.0` shipped alone, `evals` staying at `v0.7.0`, per `DECISIONS.md`) — see `gateway/changelog/`/`evals/changelog/` for the full per-version history; this table only needs to track the `api/` contract compatibility boundary, not every release.

Rollout order for a coordinated upgrade: bump the `api/` contract first (both sides regenerate bindings, per `RELEASE.md`'s bump-and-validate procedure), then `gateway` and `evals` can each roll out independently afterward, in either order, since both are already speaking the new contract version.

## Upgrade & Migration

See `UPGRADE.md` for the actual breaking-change list (currently empty — no breaking change has shipped in a release yet). This document doesn't restate it.

## Troubleshooting / Health Checks

For deeper diagnosis once something's actually running, see `docs/operations/TELEMETRY.md` — this document covers standing the system up, not debugging it once it's misbehaving.
