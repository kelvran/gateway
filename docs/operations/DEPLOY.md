# Deployment Guide

## Overview

Two independently deployable units — `gateway` (Go) and `evals` (Python) — joined only by the versioned `api/` contract. This document covers operational deployment only; for *why* they're structured this way, see `DESIGN.md` and `docs/decisions/`. **Cache is not a third deployable** — it ships inside the `gateway` binary; nothing here treats it separately.

## Deployment Models

- **Docker Compose** (local/dev) — `gateway` as a Compose service, plus an optional Redis dependency, for local development. `evals` is a Click CLI, not a long-running process — run it directly, not as a Compose service (see below).
- **Kubernetes / production** — `gateway` and `evals` as separate Deployments, scaled and released independently, sharing only the `api/` contract version they were built against.
- **ECS/Fargate** (added as part of the backlog Phase 7 infra round) — a real alternative to Kubernetes for `gateway` specifically, using Fargate's native secrets injection instead of ESO/IRSA — see below.

## Prerequisites

- Go 1.26+ runtime for `gateway`, Python 3.12+ for `evals` (per `gateway/ARCHITECTURE.md`/`evals/ARCHITECTURE.md`'s tech-stack tables).
- Provider credentials for whichever upstream LLM providers are configured — see `docs/operations/PROVIDERS.md` for exactly which providers exist and what each needs.
- Redis for `gateway`, **only if** `config.yaml`'s `rate_limit.redis_addr` is set (distributed rate limiting across multiple gateway instances, per `docs/rfcs/2026-09-03-distributed-rate-limiting.md`) — omit it entirely for the default, in-memory-only rate limiter. **Postgres is not used by any shipped code today** — it's a `gateway/ARCHITECTURE.md` Tech Stack *future target* for a control-plane config store, not real yet (config is static YAML, loaded once at startup — see that doc's `/internal/admin` entry).
- Fail-fast on missing required environment variables at startup — never start in a half-configured state, per this project's own security conventions (`SECURITY.md`).

## Docker Compose (Local/Dev)

Real: `docker-compose.yml` at the repo root defines a `gateway` service (built from `gateway/Dockerfile`, unmodified) and an *optional* `redis` service gated behind a Compose profile — `docker compose --profile redis up` — since Redis is only needed when `rate_limit.redis_addr` is configured. There is no `postgres` service (nothing uses it, see Prerequisites) and no `evals` service (see Deployment Models above). **Corrected 2026-09-14** — this section previously named every non-`gateway`/`redis` service, which is no longer complete: three more profile-gated optional services now exist — `gateway2` (profile `multi-instance`, a second gateway instance sharing the same Redis, for real multi-instance/distributed-rate-limit testing), `vector` (profile `vector-s3`, a log-shipping sidecar for `GatewayDecisionEvent`→S3, currently blocked pending a real bucket/IAM credential), and `observability` (profile `observability`, `grafana/otel-lgtm` — a real OTel Collector+Prometheus+Tempo+Grafana bundle; see `gateway/ARCHITECTURE.md`'s Tech Stack table for how the gateway's own OTLP exporter connects to it). All three, like `redis`, are opt-in and never started by a bare `docker compose up gateway`.

`gateway/Dockerfile`'s final image also runs as a real non-root user (`USER 65532:65532`, added 2026-09-14) — previously ran as root with a writable root filesystem.

To bring `gateway` up locally:
1. `cp gateway/config.example.yaml gateway/config.yaml` and fill in real values (both files are gitignored except the `.example` one — see `.gitignore`).
2. `cp .env.example .env` and set the real API key(s) your `config.yaml`'s deployments reference.
3. `docker compose up gateway` (add `--profile redis` first if `config.yaml` sets `rate_limit.redis_addr: redis:6379`).

**Corrected 2026-09-11** — this section was stale since 2026-09-07: a real `/healthz` endpoint now exists (`mux.HandleFunc("/healthz", healthzHandler)`, `cmd/gateway/main.go`) — a shallow liveness probe, no auth, returning `200 {"status":"ok"}`, covered by an integration test. Readiness is `GET /healthz` returning `200`, not merely "the container is listening on `:8080`."

**Updated 2026-09-25** — a second, deliberately separate `/readyz` endpoint now also exists (`readyzHandler`, `cmd/gateway/main.go`; data source: `dataplane.Pipeline.ReadinessSummary`, a pure read of the existing deployment probe loop's own state, zero new upstream calls), per `docs/upgrade-research/production-readiness-security-grounding-2026-09-25.md` Finding 2 — a 2026 LiteLLM-monitoring postmortem found "liveness returns 200 while every routed request fails" to be the single most common real-world LLM-gateway monitoring gap, and `/healthz`'s own deliberate provider-independence (correct, unchanged) left nothing filling that gap. `/readyz` returns `200` with a per-canonical-model health breakdown (`{"ready":true,"models":{"claude-haiku-4-5":true,...}}`) if every configured model has at least one healthy deployment, `503` if any model has none. **Point an external monitor at `/readyz`, not the Kubernetes pod `readinessProbe`** — `deploy/k8s/base/deployment.yaml`'s `readinessProbe` still correctly points at `/healthz` (see that file's own comment, unchanged): `/readyz`'s health verdict is pod-wide across every configured canonical model, so wiring it as the k8s readiness probe would empty the Service's entire `EndpointSlice` the moment even one model (out of potentially several this gateway serves) loses every one of its deployments — cutting off traffic for every OTHER, perfectly healthy model too. That is exactly the same "a readiness probe checking a dependency that can fail in lockstep across every replica, emptying the EndpointSlice, worse than doing nothing" anti-pattern this file's 2026-09-14 correction below already reasons about for a *shared* dependency (Redis/upstream provider) — here the failure mode is per-model rather than per-shared-resource, but the blast-radius risk to unrelated traffic is the same shape.

## Kubernetes / Production

**Corrected 2026-09-14**: this section previously said "(Intended shape.)... not created yet" — real, plain Kustomize manifests now exist at `deploy/k8s/` (`deploy/k8s/base/{deployment,service,secret-placeholder,kustomization}.yaml`), referencing the real `ghcr.io/kelvran/gateway` image, per `docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md`. Never applied against a real, live cluster (none exists yet to verify against) — see `deploy/k8s/README.md`'s own disclosed limitations before using this anywhere real: `replicas: 1` is deliberate (no PVC/StatefulSet for `budget.persist_path` cross-replica consistency yet), Secrets management is bring-your-own (External Secrets Operator is the documented, not yet live, decision), and CPU/memory sizing is illustrative, not measured against real traffic. `GOMEMLIMIT` is already handled in code (`cmd/gateway/main.go`, via `github.com/KimMachineGun/automemlimit`, reading the real cgroup memory limit at startup) — the manifest's own `resources.limits.memory` is what that mechanism reads. Liveness and readiness both point at `/healthz`, which checks zero external dependencies (confirmed against `healthzHandler`), so it is safe to reuse for both without the shared-dependency-readiness anti-pattern a Redis/upstream-provider check would risk. Never make the admin/control-plane API internet-facing (per `SECURITY.md`'s operator best practices) — it binds loopback-only inside the container by default, so this is not exposed by the included Service at all, by construction.

**Updated 2026-09-17/18**, per the backlog Phase 7 infra round: "Secrets management is bring-your-own" above is no longer true across the board — `deploy/k8s/base/serviceaccount.yaml` now exists (a dedicated ServiceAccount, needed for IRSA to have anything to attach a trust policy to) and `deploy/k8s/overlays/eks-irsa/` is a real, schema-validated `SecretStore`/`ExternalSecret` pair for EKS specifically, paired with a new `terraform/irsa-trust/` module that provisions the IAM side (an OIDC-federated trust policy scoped to that exact namespace/ServiceAccount). Self-managed Kubernetes without ESO still uses the plain `secret-placeholder.yaml` stub, now via `iam.amazonaws.com/role` (kiam/kube2iam's own annotation convention, which `terraform/irsa-trust/` does NOT provision — kiam/kube2iam's trust principal differs from IRSA's OIDC provider). See `deploy/k8s/README.md`'s "Which annotation for which cluster type" section for the full breakdown across all three deployment targets (EKS, self-managed K8s, and ECS below).

## ECS/Fargate

Added as part of the backlog Phase 7 infra round, per
`docs/upgrade-research/terraform-iac-deployment-automation-2026-09-15.md`'s
Finding 1/7. Real Terraform now exists at `deploy/ecs/` (an
`aws_ecs_task_definition` + its CloudWatch Logs group) and
`terraform/ecs-task-role/` (the task execution role + task role Fargate's
own two-role model requires, IAM-scoped to exactly the Secrets Manager
ARNs and, optionally, Bedrock model ARNs the task needs) — see `deploy/ecs/README.md`
for the exact apply order and for what this deliberately does NOT
provision (cluster, service, networking, load balancer, config-file
delivery — all bring-your-own, same disclosure posture as everything else
in this deployment guide). Like `deploy/k8s/`, never applied against a
real, live ECS cluster — `terraform validate`/`terraform plan` (structural,
no real AWS credentials) is the extent of verification so far.

The one thing genuinely specific to this target vs. Kubernetes+IRSA/ESO:
Fargate injects Secrets Manager values into the container natively via the
task definition's own `secrets` block, resolved by the task EXECUTION role
before the container ever starts — no operator-installed ESO/kiam/kube2iam
equivalent needed at all for this path.

## Redis (Distributed State Backend)

**Added 2026-09-23**, per `docs/upgrade-research/redis-state-backup-recovery-2026-09-22.md` Findings 1/3/4/6 — this section was previously silent on Redis entirely, a real gap once `rate_limit.redis_addr`/`budget.redis_addr`/`admin.redis_addr`/`config_propagation.redis_addr` moved from a rate-limit-only concern to holding financial-ledger (`redisbudget`) and credential-store (`redisstore`) state.

**Local/dev target**: `docker-compose.yml`'s `redis` service (`redis:7-alpine`, `--profile redis`) now runs with `--appendonly yes --appendfsync everysec` and a named, persistent volume — bounding loss to at most ~1s of writes and surviving container recreation, neither of which was true before this date. This is explicitly a local/dev convenience, never the production target.

**Production target — not yet provisioned, named here for the first time rather than left implicit.** Two real options, in order of preference for this data class:
- **AWS MemoryDB** — durability is structural (a distributed transactional log every write is committed to, not a snapshot cadence someone has to remember to configure): "There is no data loss in this scenario as the data was persisted in the transaction log" for a full Multi-AZ cluster loss/rebuild. Preferred for `redisbudget`/`redisstore` specifically, since both hold state whose loss means either silently under-enforcing a spend cap or silently discarding a live credential — not an ordinary cache-miss cost.
- **ElastiCache with Backup and Restore enabled** — the lower-effort alternative if MemoryDB's cost/API-compatibility tradeoffs (not evaluated here) aren't a fit; snapshot-based (up to 35-day retention), restorable into a new cluster, but its point-in-time-recovery ceiling is bounded by snapshot interval, not a transaction log the way MemoryDB's is.
- Neither has been provisioned yet — this is a documented target for the next real infrastructure pass, not a claim that either exists today. If self-hosting instead of using a managed AWS service, provision a real Redis manifest (StatefulSet + PVC) under `deploy/k8s/` — none exists there today; the config *knobs* are documented, the resource that would provision Redis itself is not.

**RPO/RTO target**: no Kelvran doc anywhere previously set an explicit number for any subsystem. Per AWS's own Well-Architected "Backup and restore" DR tier (the appropriate tier for Kelvran's current single-region, one-pilot-customer stage — not Pilot Light/Warm Standby/active-active, which are unjustified cost/complexity for this stage): **RPO ≤ 5 minutes** (via continuous/frequent backup, not an infrequent manual snapshot), **RTO ≤ 60 minutes** (restore + verify + redeploy). Revisit upward (tighter RPO/RTO, a higher DR tier) only once real production traffic volume and a real customer SLA justify the added cost — mirroring this repo's own established "instrument/decide later" posture for other capacity questions, not a permanent ceiling. A quarterly restore-drill practice (restore a real backup into a scratch instance and verify it) is the recommended way to keep this target honest rather than aspirational — not yet adopted, named here as the next step once a production Redis target is actually provisioned.

## Local Bbolt Persistence: Backup & Restore

**Added 2026-09-26.** `identity`/`budget`/`prompt` can each optionally persist to a local bbolt file (`admin.persist_path`/`budget.persist_path`/`prompt.persist_path` in `config.yaml`) for single-process restart durability — see each `boltstore` package's own doc comment. `POST /admin/backup` (`internal/admin/admin.go`'s `backupHandler`, via `dataplane.Pipeline.BackupStores`) writes a timestamped `<store>-<UTC-timestamp>.bbolt` file per configured, persisted store to `admin.backup_dir` — safe to call while the gateway is actively serving traffic (`internal/backup.CopyFile` uses bbolt's own `Tx.CopyFile`, backed by a read transaction against a consistent MVCC snapshot, per that package's own doc comment).

Until now there was no supported way to actually use one of those backup files again — no `Restore` counterpart existed anywhere in this codebase, and even the informal "stop the process, copy the backup over `persist_path`, restart" workaround had never been proven to work through the real store constructors (`identity`/`budget`/`prompt`'s own `boltstore.Open`) the gateway binary actually calls at startup — `internal/backup`'s own tests previously only ever opened a backup file with a raw, schema-blind `bolt.Open` read-check.

**Restoring from a backup — this is an OFFLINE procedure. Do not skip step 1.**

1. **Stop the gateway process.** Restoring a bbolt file that a running gateway still holds an exclusive lock on (and has already loaded into memory) is a real concurrent-access hazard `internal/backup.Restore` deliberately does not attempt to solve — see that function's own doc comment. Never point the restore step below at a `persist_path` a live gateway process is still serving from.
2. Run the restore flag: `gateway -config config.yaml -restore-store=<identity|budget|prompt> -restore-from=<backup-file-path>`. This is a one-shot operator action — it never starts a listener or the normal server-start path. It resolves the target store's own configured `persist_path` out of the same config file a normal `gateway` invocation would load, validates that the backup file is a well-formed, independently-openable bbolt database (opened read-only via a raw `bolt.Open`) *before* touching anything at the destination, then atomically replaces the destination file (written to a temp file in the same directory first, then moved into place with a single `os.Rename`, so a crash mid-restore never leaves a half-written `persist_path`). Refuses to overwrite an existing destination file unless `-restore-force` is also passed — mirroring `backup.CopyFile`'s own refuses-to-overwrite convention, so a stale or wrong backup can't silently clobber a live `persist_path` by accident.
3. Start the gateway normally (no restore flag). The very next startup loads the now-restored file through that store's exact real constructor (`identity`/`budget`/`prompt`'s `boltstore.Open`), exactly like any other pre-existing `persist_path` file — there is no separate "restored mode" once step 2 completes.

This closes the recovery gap on the OPEN side of `cmd/gateway/main.go`'s `openPersistStoreWithRecovery`/`isCorruptStoreErr`: `admin.on_corrupt_store: reset` (the non-default opt-in) renames a corrupted `persist_path` file aside and starts the store fresh and EMPTY rather than failing startup outright — real data loss if no restore step exists to put a prior backup back in its place. A `.corrupt-<unix-seconds>` file left behind by that reset path is itself a valid `-restore-from` source if it's still a well-formed bbolt file (i.e. the corruption was something `bolt.Open` tolerated enough to be readable at all) — Restore's own validation step will tell you either way before it touches anything.

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
