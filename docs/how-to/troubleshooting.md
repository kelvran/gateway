# Troubleshoot the gateway

This page takes an operator or integrator from a symptom to a check and a fix: an HTTP status and error code from `/v1/chat/completions`, a process that refuses to start, a `503` from `/readyz`, a Redis or provider outage, or a stream that stopped early. It is for people who run the gateway or write clients against it. The per-subsystem behaviour behind every row is in [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md); this page is the symptom-first path through it and cites its rows (R1, P3, U1, ...).

Use this when something is failing now and you need to know which signal to read and which lever to pull.

This page describes `main` as of 2026-10-08. `gateway/v0.17.0` (2026-10-07) lacks: the JSON error envelope, the `Allow` header on a 405 and the in-band SSE error frame (Step 3 and its streaming variant), `-version` and the `build_info` record (Step 1), the deb/rpm/apk packages and the systemd unit (its GitHub Release carries no assets), the one-second bbolt lock timeout (Step 4; v0.17.0 blocks forever with no log line), the `upstream call failed for model` redaction, the `invalid_tool_choice` and `missing_required_parameter` codes, the `kelvran.configpropagation.publish_failed` counter and the `kelvran.persistence.failed` increment on a failed budget settlement (Step 5). Where a step depends on one of these, it says so.

## Prerequisites

- Shell access to the host, container or pod that runs the gateway, with its stdout (JSON log lines) and stderr.
- The gateway's `config.yaml` and the environment it starts with. The systemd package reads `/etc/kelvran-gateway/config.yaml` and `/etc/kelvran-gateway/env`; see [`deploy/systemd/kelvran-gateway.service`](../../deploy/systemd/kelvran-gateway.service).
- For admin checks: the admin listener (`admin.listen_addr`, default `127.0.0.1:8081`) and the token named by `admin.token_env` (`KELVRAN_ADMIN_TOKEN` in the examples). See [Admin API and RBAC](admin-api-rbac.md).
- A virtual key for test requests (`KELVRAN_KEY` in the examples). The example config's key `team-alpha` has the secret `example-team-alpha-secret-do-not-use`; never use it for anything real.
- `curl`. Replace `kelvran-gateway` below with your binary: `/usr/bin/kelvran-gateway` (package), `$(go env GOPATH)/bin/gateway` (`go install`), or `docker run --rm ghcr.io/kelvran/gateway:<tag>`.

## Step 1: Confirm which build and which config you are looking at

1. Print the build. Every start also logs the same fields once as a `build_info` record. Both are on `main` since 2026-10-08: a `gateway/v0.17.0` binary rejects `-version` (`flag provided but not defined: -version`, exit 2) and logs no `build_info`; on 0.17.0 identify the build by the image tag or Go module version you installed. After a rollout, replicas reporting different versions is the first thing to check.

   ```bash
   kelvran-gateway -version
   # kelvran-gateway <version> (<commit>, built <date>, <go version>, linux/amd64); a source build says "dev"
   ```

2. Validate the config file. Exit 0 prints `config is valid`; exit 1 prints `config error: ...` on stderr. It runs every check `controlplane.Load` performs (required fields, enum values such as `on_corrupt_store`, per-deployment and per-key field rules) plus two cross-deployment checks: `deployment "x": no adapter registered for provider "y"` and `deployment "x" fallback_chains.<class> names "y", which is not a configured deployment`.

   ```bash
   kelvran-gateway -validate -config /etc/kelvran-gateway/config.yaml
   ```

   `-validate` never reads an environment variable, never opens a bbolt store, credential file or audit log, and never dials Redis. A clean `-validate` followed by a refused start is normal; go to Step 4.

## Step 2: Ask the process how it is doing

```bash
curl -s http://127.0.0.1:8080/healthz                       # {"status":"ok"} while the process serves
curl -s -w '\n%{http_code}\n' http://127.0.0.1:8080/readyz  # JSON with "ready" (true|false) and "models" (per-model true|false); 200 or 503
```

- `/healthz` is liveness only. It never reflects Redis or provider reachability.
- `/readyz` answers `503` when any model has no probe-healthy deployment. Probing is off unless `health_probe.interval_seconds` is greater than 0 (there is no default); with it off, `/readyz` is `200` for every model. Keep Kubernetes probes on `/healthz` and point external monitors at `/readyz`. See [Configure routing and failover](routing-and-failover.md).
- Both answer any non-GET method with a plain-text `405 method not allowed`.

Then read the gateway's own JSON log on stdout. The lines that matter most:

```bash
journalctl -u kelvran-gateway -o cat | grep -E '"msg":"(gateway exited|gateway listening|build_info|[a-z_]*_backend_unavailable[a-z_]*|[a-z_]*_persist_failed|credential_reload_[a-z_]*|health_probe_deployment_[a-z]*|guardrail_fail_open|configpropagation_[a-z_]*)"'
```

`gateway listening` means the process is up. `gateway exited` is always the last line of a refused start and carries the reason (Step 4). go-redis dial failures (`redis: connection pool: failed to dial after 5 attempts: ...`) are plain text on stderr, not in the JSON stream; a log pipeline that reads only stdout misses them.

On macOS, Windows or any host without cgroups, the two lines after `build_info`, an ERROR `failed to set GOMEMLIMIT` (from the automemlimit library, through the gateway's logger) and a WARN `automemlimit: could not set GOMEMLIMIT from cgroup`, are harmless; the gateway is up at `gateway listening`.

## Step 3: Read the error envelope from a failing request

Every error on `/v1/chat/completions` and `/v1/embeddings` is `{"error":{"message":...,"type":...,"param":null|"<field>","code":null|"<code>"}}` with `Content-Type: application/json; charset=utf-8`. Only `type` and `code` are stable; the `message` text is not a contract (see [`docs/VERSIONING.md`](../VERSIONING.md)). The JSON envelope is on `main` since 2026-10-08, not in `gateway/v0.17.0`, which returns `text/plain` bodies with the same status codes.

```bash
curl -s -D - http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

Look at the status, `Retry-After`, then `.error.type` and `.error.code`. `Retry-After` and fallback are chat-only: `/v1/embeddings` uses the same envelope and shares most of these codes (it never produces `concurrency_limit_exceeded`, `deployment_capacity_exceeded` or the chat-shape codes), with no `Retry-After` header on any of them and no fallback hop.

| Status | `type` / `code` | Meaning | Check and fix |
|---|---|---|---|
| 401 | `authentication_error` / `null` | No or malformed `Authorization: Bearer` header | Send `Authorization: Bearer <secret>`; no other scheme is accepted |
| 401 | `authentication_error` / `invalid_api_key` | This replica does not know the key | Send the raw secret, never the `key_hash`. If the secret is right, compare `GET /admin/virtual_keys` across replicas; a key created during a Redis outage can be missing on some (Step 5) |
| 403 | `permission_error` / `model_not_allowed`, `source_ip_not_allowed` | The key's `allowed_models` or source-IP list refuses the request | Fix the key's lists; see [Virtual keys and budgets](virtual-keys-and-budgets.md) |
| 429 with `Retry-After` | `rate_limit_error` / `rate_limit_exceeded`, `concurrency_limit_exceeded` | Per-key RPM, TPM or concurrency ceiling | Wait `Retry-After` seconds. `rate_limit_exceeded` on an idle key after a replica was SIGKILLed is a leaked Redis TPM reservation; it clears within `tpm_capacity / tpm_refill_per_second` seconds (row R9) |
| 429, no `Retry-After` | `insufficient_quota` / `insufficient_quota` | Budget cap reached; SDKs treat it as permanent | `GET /admin/virtual_keys/{name}/spend`. If `spent_usd` equals `budget_usd` with no traffic, see the R7 variant below |
| 400 | `invalid_request_error` / `model_not_found` | No deployment for this model (400, not 404) | Fix the client's model name or add a deployment (row U4) |
| 400 | `invalid_request_error` / `content_policy_violation` | A guardrail block, or a block-tier detector error, which the client cannot tell apart | Look for `guardrail_detector_error` in the log (Step 6) |
| 400 | `invalid_request_error` / `empty_messages` (`param: "messages"`), `invalid_prompt_reference`, `streaming_not_supported`, `not_an_embedding_model`, `invalid_json`, `invalid_body`, `invalid_request`, `invalid_tool_choice` (`param: "tool_choice"`), `missing_required_parameter` (`param: "model"` or `"input"`, `/v1/embeddings` only) | Request shape | Fix the request |
| 405 with `Allow` | `invalid_request_error` / `method_not_allowed` | Wrong HTTP method | Use the method named in `Allow` |
| 413 | `invalid_request_error` / `request_too_large` | Body larger than 32 MiB | Shrink the request |
| 501 | `server_error` / `streaming_not_configured`, `embeddings_not_configured` | No deployment of the needed kind is configured | Add one |
| 503 with `Retry-After` | `server_error` / `deployment_capacity_exceeded` | A deployment's own RPM, TPM or concurrency ceiling; ceilings are per replica (row U6) | Raise the ceiling or add replicas; see [Configure routing and failover](routing-and-failover.md) |
| 502 with `Retry-After` | `server_error` / `upstream_error` | Every upstream attempt failed, fallbacks included (row U1) | Step 6. The provider's own text is only in the server log |

An upstream `429` reaches the client as `502`, never `429`. The message is redacted to `upstream provider returned status N` or `upstream call failed for model "<m>"`; the unredacted error, `upstream_status`, `upstream_error_type` and `upstream_retry_after_ms` are in the `chat_completion` or `embeddings` Error log line. The `Retry-After` on a 502 is the larger of the local backoff and the last hop's provider `Retry-After`, capped at 60 s.

## Step 4: Match a refused start to its cause

The process exits 1 and logs `gateway exited` with one of these errors; every row except the last fails before a listener is bound:

| `gateway exited` error contains | Cause | Fix |
|---|---|---|
| `admin.token_env "X" is set but resolves to an empty environment variable` (likewise `viewer_token_env`, `cost_viewer_token_env`, `operator_token_env`) | The named variable is unset in the process environment | Set it (for the package: `/etc/kelvran-gateway/env`) |
| `config_propagation.redis_addr is set but signing_secret_env is missing`, or `config_propagation.signing_secret_env "X" is set but resolves to an empty environment variable` | An unsigned cross-instance channel is refused | Set `config_propagation.signing_secret_env` and the variable it names |
| `hydrating virtual keys from redis at "..."` | `admin.redis_addr` is unreachable or refusing (row R10); `CrashLoopBackOff` on Kubernetes | Fix the address, credentials, TLS or network |
| `boltstore: opening <path>: another process holds the file lock (waited 1s)` | A previous process still holds the bbolt file (row P2) | Stop it, or fix the restart racing its predecessor |
| `opening virtual-key store at "<path>"`, `opening budget store at "<path>"`, `opening prompt store at "<path>"` (the file is corrupt under `admin.on_corrupt_store: fail`, the default, or unreadable; rows P3, P4); `hydrating virtual keys from "<path>"`, `hydrating budget tracker from "<path>"`, `hydrating prompt store from "<path>"` (the file opened but holds a corrupt JSON value; row P5) | A bbolt file cannot be opened or holds a corrupt value (rows P3 to P5) | Fix permissions, or restore offline from a `POST /admin/backup` copy with `-restore-store <kind> -restore-from <file>`; see [Back up and restore](backup-and-restore.md). `admin.on_corrupt_store: reset` renames a corrupt file to `<path>.corrupt-<unix>-<nanos>` and starts empty (logs `persist_store_open_failed`, `persist_store_reset`); it never rescues a permissions error or a corrupt value |
| `opening admin.audit_log_path "<path>"` | The audit JSONL path cannot be opened (row P7) | Fix the path or permissions |
| `constructing embedsim detector: ...` | embed-sim embeds its corpus at start; a `status 403` here means bad or expired AWS credentials (row G1) | Fix the credentials |
| `telemetry: unknown exporter "x" (want "stdout", "otlp", or "none")` | Typo in `telemetry.exporter` | Use one of the three values |
| `no adapter registered for provider`, `fallback_chains.<class> names "y", which is not a configured deployment`, or any other config error | Config shape | Run `-validate` and fix the file |
| `http server: listen tcp <addr>: bind: address already in use` or `admin http server: listen tcp <addr>: ...` | `listen_addr` or `admin.listen_addr` is held by another process | Stop it, or change the address; `-validate` cannot detect this |

## Step 5: Redis is unreachable

With Redis down the process stays up (row R10 is the exception) and the controls that depend on it fail open:

| You see | Row | What is happening | Do |
|---|---|---|---|
| `kelvran.ratelimit.fail_open` rising; log `ratelimit_backend_unavailable`, `ratelimit_tpm_backend_unavailable` or `embeddings_ratelimit_backend_unavailable` | R1 to R3 | Requests are admitted with no limit | Alert on the counter; limits are unenforced until Redis returns |
| `kelvran.budget.fail_open` rising; log `budget_backend_unavailable` | R6 | Requests are admitted with no reservation; their cost is never written to Redis, even after recovery | Reconcile billing from the `chat_completion` log lines of the outage window |
| `kelvran.persistence.failed{kelvran.persistence.store_kind=budget}` (on `main` since 2026-10-08; `gateway/v0.17.0` has only the log line); log `budget_redis_backend_unavailable` with `op=reconcile` | R7 | A settlement failed and the key is left at its cap | See the R7 variant below |
| Log `budget_redis_backend_unavailable` with `op=spent_usd`, `check_alert_bucket` or `check_warn_alerted`, or `admin_spend_read_failed` on the admin listener | R8 | Request-path spend reads as $0 and threshold alerts are suppressed; `GET /admin/virtual_keys/{name}/spend` and `GET /admin/virtual_keys?include=spend` answer `200` with `spend_unavailable: true` and no `spent_usd` (on `main` since 2026-10-10) | Expect missing alerts; do not run the R7 check until Redis is back |
| `kelvran.persistence.failed` with `store_kind` `identity` or `budget`; log `identity_persist_failed` or `budget_persist_failed` right after an admin call answered 204 | R12 | The mutation is live in memory only. A DELETE or rotate whose write failed resurrects the old credential on restart | Fix the store, then re-drive the mutation before any restart (for a delete: re-POST the key, then DELETE it again) |
| `kelvran.configpropagation.publish_failed` (on `main` since 2026-10-08; absent in `gateway/v0.17.0`); log `configpropagation_publish_failed` | R13 | Other replicas never received the mutation | Once Redis is back, re-apply every admin mutation made during the outage against one replica; verify by reading it from another replica |
| `kelvran.configpropagation.subscribe_stopped` above zero | R14 | That replica's subscriber loop has stopped for good | Restart that replica |

Do not rely on a restart to recover mutations: deployment weights are never persisted. The ordered sequence is the Redis outage runbook in [`docs/operations/DEPLOY.md`](../operations/DEPLOY.md).

## Step 6: A provider or a credential is failing

| You see | Row | Check | Fix |
|---|---|---|---|
| 502 `upstream_error` with `upstream provider returned status 401` on a fresh install, and a startup WARN `deployment's upstream API key env var is not set; calls to this deployment will fail` | U1, C3 | The deployment's `api_key_env` (or AWS `*_env`) variable | Set it and restart; see [Provider credentials](provider-credentials.md) |
| Startup WARN `credential file could not be read or is empty; calls will fail`; log `credential_reload_read_failed` on a reload tick | C1, C2 | `test -s <path>` on the `*_file` path | Fix the mount or permissions. No restart: the loop re-reads within `credential_reload.interval_seconds` (default 60 s) and keeps the last good value meanwhile. Write rotations atomically (temp file, then rename) |
| `upstream_status` 401 or 403 in `chat_completion` Error lines for one deployment | C3 | That deployment's credential | Rotate it; see [Rotate credentials](rotate-credentials.md). A `generic` fallback chain can absorb this silently while spending the fallback's quota |
| Log `health_probe_deployment_unhealthy` (no metric); `/readyz` 503 for a model | U3 | After `unhealthy_threshold` (default 3) failed probes the deployment is excluded; `healthy_threshold` (default 2) successes re-include it at 20 % weight. With every deployment of a model unhealthy the router still sends traffic to the last candidate | Fix the provider; wait for `health_probe_deployment_recovered` |
| `kelvran.guardrail.fail_open{kelvran.guardrail.stage}` rising; logs `guardrail_detector_error` then `guardrail_fail_open` | G1 | AWS 403 on expired Bedrock Guardrails or embed-sim credentials is the usual cause; each AWS detector waits up to 5 s per stage | With `*_file` credentials rewrite the file; with `*_env` only a restart helps. Block-tier categories fail closed as 400 `content_policy_violation` and are counted by no metric |
| Nothing at the OTLP collector; INFO `traces export: Post "https://...": ...` about 5 s after the first span and `failed to upload metrics: ...` after about 60 s | T1 | `otlp_endpoint` is `host:port`; the scheme defaults to HTTPS | For a plain-HTTP collector set `OTEL_EXPORTER_OTLP_INSECURE=true`. Requests are never affected |

## Variants

### A key answers 429 `insufficient_quota` with nothing in flight (row R7)

```bash
curl -s -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" http://127.0.0.1:8081/admin/virtual_keys/team-alpha/spend
# fields: spent_usd, budget_usd, budget_reset_interval_seconds, percent_used
curl -s -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" http://127.0.0.1:8081/admin/virtual_keys/team-alpha/inflight
# fields: total_in_flight, by_agent_run_id
```

If `spent_usd` equals `budget_usd` on every replica and `total_in_flight` is 0, a failed Redis settlement left its reservation behind. Either `DELETE /admin/virtual_keys/team-alpha` and re-create the key (this also zeroes legitimate spend), or in Redis run `DEL "budget:<url.QueryEscape(key id)>"`. Rotating the key does not help; the id survives rotation. Set `budget_reset_interval_seconds` so a leak is time-bounded; under a lifetime cap it is permanent.

### A stream ended without `data: [DONE]`

A failure after the first SSE chunk cannot change the committed 200. The stream carries exactly one in-band `data: {"error":{"type":"server_error","code":"upstream_error",...}}` frame and ends without `[DONE]` (row U5). OpenAI SDK stream parsers raise `APIError` on it. There is no fallback after the first chunk. See [Streaming](streaming.md).

### The admin API answers in plain text

Admin errors are not the envelope. `401 missing or malformed Authorization header` and `401 invalid admin token` mean the bearer does not match a tier the route accepts. `POST /admin/backup` answers `501 admin.backup_dir is not configured` until that key is set. Virtual-key upserts, deletes and rotations, `POST /admin/deployments/{name}/weight`, `DELETE /admin/prompts/{id}` and `DELETE /admin/prompts/{id}/labels/{label}` answer `204` with no body; `POST /admin/prompts/{id}`, `PUT /admin/prompts/{id}/labels/{label}`, `POST /admin/backup` and `POST /admin/cache/erase` answer `200` with a JSON body. A `400` from `POST /admin/prompts/{id}` whose body starts `prompt: Upsert: persisting`, or a `500` from `DELETE /admin/prompts/{id}` starting `prompt: Delete: persisting removal`, is a disk fault, not a client error (row P6): fix the disk, then re-POST once, or re-POST and DELETE again.

### `Idempotency-Key` reuse returns 502

The same `Idempotency-Key` with a different body is reported as 502 `upstream_error` with `Retry-After` (row I2). Claims expire after 10 minutes; after a crash a retry re-executes.

## Verify it worked

1. Build and config: `kelvran-gateway -validate -config <file>` prints `config is valid` and exits 0, and `kelvran-gateway -version` prints the same build on every replica (`main` only; on `gateway/v0.17.0` compare image tags or Go module versions instead).
2. Process: `curl -s -w '\n%{http_code}\n' http://127.0.0.1:8080/readyz` prints a body whose `ready` is `true`, then `200`.
3. Envelope: a request with a wrong key returns `401`, and the body's `.error.type` is `authentication_error` with `.error.code` `invalid_api_key`:

   ```bash
   curl -s -w '\n%{http_code}\n' http://127.0.0.1:8080/v1/chat/completions \
     -H 'Authorization: Bearer wrong-key-do-not-use' -H 'Content-Type: application/json' \
     -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
   # {"error":{"message":"...","type":"authentication_error","param":null,"code":"invalid_api_key"}}
   # 401
   ```

4. After a Redis outage: `kelvran.ratelimit.fail_open` and `kelvran.budget.fail_open` stop rising, and a mutation made on one replica reads back from another.

## Not available today

- No admin route resets a key's spend without deleting the key.
- No metric for health-probe transitions, fallback hops, OTLP export failures, audit-append failures or credential reload failures; these are log-only.
- `-validate` cannot detect an unreachable Redis, an unreadable or empty credential file, an unopenable audit log path, a missing environment variable, or a wrong Gemini `:generateContent` or Bedrock `/converse` `base_url` suffix. `kelvran doctor` (on `main` since 2026-10-10, not in `gateway/v0.17.0`) reports the missing variables, unreadable credential files, the packaged layout's permissions and paths, unpriced models and an invalid telemetry exporter; Redis reachability, whether the audit log and persist paths can be opened, and the `base_url` suffixes are still unchecked.
- No Redis logger: go-redis dial failures are plain text on stderr.
- No request log store and no `GET /admin/requests`.
- Prompt-store write failures (row P6) have no log line and no metric.
- No HTTP endpoint reports the running version; use `-version` or the `build_info` log record (both on `main` since 2026-10-08, not in `gateway/v0.17.0`).

## Related

- [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md): the row-per-subsystem table this page cites.
- [`docs/operations/DEPLOY.md`](../operations/DEPLOY.md): the Redis outage runbook and the backup and restore procedure.
- [`docs/operations/TELEMETRY.md`](../operations/TELEMETRY.md) and [Metrics and logs](../reference/metrics-and-logs.md): every metric and log event named above.
- [Error codes](../reference/error-codes.md) and [Data-plane API](../reference/data-plane-api.md): the full envelope vocabulary.
- [Configuration reference](../reference/config.md) and [`gateway/config.example.yaml`](../../gateway/config.example.yaml): every config key named above.
- [Admin API](../reference/admin-api.md): the routes used in the variants.
