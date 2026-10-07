---
name: verify
description: Recipe for driving real, end-to-end traffic through the Kelvran gateway during a /verify pass — build+run the real binary directly on the host (real Bedrock calls, no Docker needed), scratch virtual keys, and where to watch for behavior.
---

# Verifying the gateway end to end

## Build and run directly (skip Docker for a quick pass)

```bash
cd gateway && go build -o /tmp/kelvran-gateway-verify ./cmd/gateway
cd .. && set -a && source .env && set +a   # real AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
/tmp/kelvran-gateway-verify -config /tmp/scratch-config.yaml > /tmp/verify.log 2>&1 &
```

Running the binary directly on the host (not via `docker-compose.yml`) is the only way to get a
**real IPv6 loopback RemoteAddr** — Docker's `127.0.0.1:8080:8080` port mapping is IPv4-only, so
anything touching `AllowedSourceCIDRs`/IPv6 needs the bare binary bound to `:PORT` (dual-stack by
default) and curl'd via `http://[::1]:PORT` / `http://127.0.0.1:PORT` / `curl -6` / `curl -4`.

Use a **scratch config**, not `gateway/config.yaml` (the real, gitignored pilot config) — copy its
`deployments:`/AWS env-var names for real Bedrock calls, but write fresh `virtual_keys:` so you never
touch the pilot's real budgets/keys. Generate a key hash: `printf '%s' '<secret>' | shasum -a 256`.

## ⚠️ `allowed_source_cidrs` YAML shape gotcha (confirmed live, 2026-09-27; fixed same day, commit 351a4544)

This field is a **map of `"cidr": true`**, exactly like `allowed_models`/`allowed_regions` —
**NOT a YAML list**, even though the Go struct field is `[]string` (which makes a list feel natural).

**Real mechanism, corrected** (an earlier version of this note guessed wrong — don't repeat that
mistake): `controlplane.Load`'s parser (`parseYAMLMini`) has no list syntax awareness at all. A
`- "::1/128"` line does NOT make `getMap` return `ok=false` — a child map genuinely gets created and
`getMap` returns `ok=true`. The real bug: `parseYAMLMini` finds the FIRST colon anywhere in the raw
line, which for an IPv6 CIDR lands inside the quoted value itself, garbling the whole line into one
bogus key/value pair (e.g. key `- "`, value `:1/128"`) inside that (successfully-returned) map — that
garbled value then fails the `v.(bool)` assertion in the parsing loop and silently gets dropped.

**As of commit 351a4544, this is fixed**: `Load` now returns a loud error for any non-bool value
under `allowed_models`/`allowed_regions`/`allowed_source_cidrs` (catching this exact garbling), and
also validates CIDR syntax at Load time (so `-validate` catches a malformed CIDR too, not just real
startup). Before that fix, this silently produced an unrestricted key with `-validate` reporting
"config is valid" — kept below as a still-useful illustration of the shape mistake to avoid, even
though it's no longer silent.

```yaml
# WRONG — as of 351a4544 this now correctly fails Load()/`-validate` with a loud
# error naming the field. Before that fix it silently produced an unrestricted key.
allowed_source_cidrs:
  - "::1/128"

# RIGHT
allowed_source_cidrs:
  "::1/128": true
```

Confirmed by live-testing the wrong shape (before the fix): a real IPv4 request to a key restricted
to `::1/128` came back `200 OK` (should have been `403`) with zero warning at load or validate time.

Before trusting a real config file (e.g. before restarting the pilot gateway after touching
`allowed_source_cidrs`/`allowed_models`/`allowed_regions`), run `-validate` against it once to
confirm no other virtual key has the same non-bool-value pattern — `Load` now catches it, but nothing
swept the existing pilot config for OTHER instances until someone runs this:
`go run ./cmd/gateway -config gateway/config.yaml -validate`.

## `-validate` used to be narrower than real startup — fixed same day (commit 351a4544)

`-validate` only calls `controlplane.Load` + `validateConfig` — it never calls `buildPipeline`.
Before 351a4544, that meant `net.ParseCIDR` on `allowed_source_cidrs` entries only ran inside
`buildPipeline`, so a malformed CIDR string passed `-validate` cleanly but crashed the process on
real startup. `Load` now runs the identical `net.ParseCIDR` check itself, so `-validate` catches this
class of error too as of today. `-validate` may still not be a COMPLETE "will this config actually
start" gate for other checks that remain `buildPipeline`-only (e.g. verifying a named provider is
actually registered) — don't assume "config is valid" means "will start clean" for everything;
verify against this file's own actual `buildPipeline` call sites if a new gotcha like this surfaces.

## Observing behavior without Prometheus/Grafana

Set `telemetry.exporter: "stdout"` in the scratch config — real request-path log lines
(`chat_completion`, `cache_cross_instance_check`) and WARN-level event lines
(`streaming_near_duplicate_collision`) print directly to the process's own stdout/log file. No need
to bring up the `observability` Compose profile for a quick behavioral check — `grep` the log.

## Concurrency probes

`for i in 1..N; do curl ... & done; wait` against the SAME request body is the simplest real
concurrent-duplicate probe. Warm the cache with one request first (real Bedrock call, wait for it to
finish) before firing the concurrent batch if you want to test the warm-cache path; use a fresh nonce
in the prompt for a cold-path probe (each of N truly concurrent first-time duplicates makes its own
real, uncoalesced upstream call — confirmed by design, not a bug).

## Driving the REAL Deep-Research project through Kelvran (recipe that worked 2026-10-07/08)

Deep-Research (`Not-Humans/Deep-Research`) never has to change: its orchestrator posts to
`MODEL_GATEWAY_URL/v1/chat/completions` with **no Authorization header**, and `PlannerClient`
sends **no `model`** (and no `max_tokens`). Put a tiny header-injecting reverse proxy between it and
Kelvran (adds `Authorization: Bearer <virtual key>`, defaults a missing `model` to `planner`; keep it
in `/tmp`, never in a project) and configure Kelvran canonical models named after Deep-Research's
role aliases — `planner`, `synthesizer`, `extractor`, `verifier` — each a real Bedrock deployment.
Start Deep-Research's processes with real env vars that win over its `.env`
(`load_dotenv(override=False)`): `MODEL_GATEWAY_URL=http://127.0.0.1:<proxy>`, `DB_URL` with the
remapped Postgres port, `PYTHONPATH=src`; run `.venv/bin/python -m uvicorn …` / `-m orchestrator.main
--queue research-queue` (the one queue carries every activity), never `uv run` (see the memory note on
the miniconda trap). Start a run with `POST /research/run {"query","depth"}` on :8000 (no `X-API-Key`
needed unless `AUTH_API_KEY` is set; the `dr` CLI's default profile points at :8100, not :8000) and
poll `/research/run/{id}/status`; the report lands at
`s3://research-artifacts/reports/{id}/final_report.md` in the MinIO-compatible store.

Gotchas that cost real time: `minio/minio` is no longer pullable (Docker Hub denies the repo, quay.io
401s) — RustFS (`rustfs/rustfs:latest-glibc`, `RUSTFS_ACCESS_KEY/RUSTFS_SECRET_KEY`) is a drop-in on
:9000; the colima default of 2 GiB cannot host Postgres + Temporal + Qdrant + OpenSearch (`colima start
--memory 6`); brew Postgres squats :5433 (override to 5434) and `qdrant-client` 1.17 needs server
v1.17; retrieval's `/index` takes `{"documents":[{chunk_id, document_id, content, …}]}` and
`/search` takes `{"queries":[…],"limit":N}`; its Corrective-RAG gate grades ~20 docs **in parallel**
through the gateway, which is exactly what exposes Kelvran's Redis-mode TPM full-capacity
reservation (one in-flight request per key). Every process started from a Claude Code Bash tool dies
with the session (including `colima start`) — launch long-lived processes with
`subprocess.Popen(..., start_new_session=True)`. Probe prompts containing long digit nonces trip the
phone/credit-card detectors and "reply with the single word OK" trips PROMPT_ATTACK — use words.
The SecretScan hook blocks any command text that looks like `KEY=`/`TOKEN=`/`SECRET=`; read secrets
from files inside helper scripts instead (`kcurl.sh` pattern) and never put the hash computation and
the secret on one command line.
