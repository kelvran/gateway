# Deployment Guide

## Overview

Two independently deployable units — `gateway` (Go) and `evals` (Python) — joined only by the versioned `api/` contract. This document covers operational deployment only; for *why* they're structured this way, see `DESIGN.md` and `docs/decisions/`. **Cache is not a third deployable** — it ships inside the `gateway` binary; nothing here treats it separately.

## Deployment Models

- **Docker Compose** (local/dev) — both deployables plus their dependencies (Redis, Postgres) as Compose services, for local development and evaluation.
- **Kubernetes / production** — `gateway` and `evals` as separate Deployments, scaled and released independently, sharing only the `api/` contract version they were built against.

## Prerequisites

- Go 1.25+ runtime for `gateway`, Python 3.12+ for `evals` (per `gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`'s tech-stack tables).
- Provider credentials for whichever upstream LLM providers are configured — see `docs/operations/PROVIDERS.md` for exactly which providers exist and what each needs.
- Redis (rate-limit state, hot cache tier) and Postgres (control-plane config) for `gateway`.
- Fail-fast on missing required environment variables at startup — never start in a half-configured state, per this project's own security conventions (`SECURITY.md`).

## Docker Compose (Local/Dev)

*(Intended shape — not yet runnable, pre-scaffolding.)* A `docker-compose.yml` at the repo root would define: `gateway` (built from `gateway/`), `evals` (built from `evals/`), `redis`, `postgres`. An `.env.example` documents every required variable (provider API keys, `REDIS_URL`, `POSTGRES_URL`) with placeholder values. Bring-up is `docker compose up`; health is checked via each service's `/healthz` endpoint before considering the stack ready.

## Kubernetes / Production

*(Intended shape.)* Separate Deployment + Service per component, a Helm chart or plain manifests under a future `deploy/k8s/` directory (not created yet — code doesn't exist to deploy). Resource requests/limits should be set generously enough to avoid the CFS-throttle-induced GC pause spikes `gateway.md`'s original research flagged for Go under tight Kubernetes CPU quotas. Expose `gateway`'s OTel/metrics endpoint and its main API endpoint separately — never make the admin/control-plane API internet-facing (per `SECURITY.md`'s operator best practices).

## Configuration Reference

*(To be filled in with the real environment-variable table once `gateway`'s and `evals`' config schemas exist.)* Secrets handling rule: never in a committed file, environment variables or a secrets manager only — this doesn't get restated per-variable, it's a blanket rule cross-linked from `SECURITY.md`.

## Independent Deployability & Contract Compatibility

The one thing genuinely specific to a two-deployable system: `gateway` and `evals` can be deployed and upgraded independently, but only within a compatible `api/` contract version range. Before either deployable ships, confirm:

| gateway version | evals version | api/ contract version | Compatible? |
|---|---|---|---|
| *(populated once real releases exist)* | | | |

Rollout order for a coordinated upgrade: bump the `api/` contract first (both sides regenerate bindings, per `RELEASE.md`'s bump-and-validate procedure), then `gateway` and `evals` can each roll out independently afterward, in either order, since both are already speaking the new contract version.

## Upgrade & Migration

See `UPGRADE.md` for the actual breaking-change list (currently empty — nothing has shipped). This document doesn't restate it.

## Troubleshooting / Health Checks

For deeper diagnosis once something's actually running, see `docs/operations/TELEMETRY.md` — this document covers standing the system up, not debugging it once it's misbehaving.
