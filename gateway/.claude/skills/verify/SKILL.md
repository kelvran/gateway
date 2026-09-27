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

## ⚠️ `allowed_source_cidrs` YAML shape gotcha (confirmed live, 2026-09-27)

This field is a **map of `"cidr": true`**, exactly like `allowed_models`/`allowed_regions` —
**NOT a YAML list**, even though the Go struct field is `[]string` (which makes a list feel natural).

```yaml
# WRONG — silently parses to a no-op. getMap() returns ok=false on a list value,
# so the whole `if ac, ok := getMap(...); ok` block never runs. NO error anywhere:
# `-validate` says "config is valid", the key just gets an unrestricted (empty)
# AllowedSourceCIDRs with the constraint fully but silently disabled.
allowed_source_cidrs:
  - "::1/128"

# RIGHT
allowed_source_cidrs:
  "::1/128": true
```

Confirmed by live-testing the wrong shape: real IPv4 request to a key restricted to `::1/128`
came back `200 OK` (should have been `403`) with zero warning at load or validate time.

## `-validate` is not a full pre-deploy check

`-validate` only calls `controlplane.Load` + `validateConfig` — it never calls `buildPipeline`, so it
never runs `net.ParseCIDR` on `allowed_source_cidrs` entries (or anything else only checked inside
`buildPipeline`). A malformed CIDR string passes `-validate` cleanly but crashes the process on real
startup (`building pipeline: virtual key ...: invalid CIDR address: ...`). Don't trust `-validate`
as a complete "will this config actually start" gate — it's a narrower check than real startup.

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
