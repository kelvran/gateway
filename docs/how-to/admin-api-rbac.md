# Configure the Admin API and its credential tiers

This page shows a gateway operator how to turn on the admin listener, issue one bearer token per job (Admin, Viewer, CostViewer, Operator), call the routes each tier may use, and read the audit trail of who did what. It is for the person who runs the gateway and hands credentials to teammates, finance, and on-call.

Use this when someone other than you needs to read config, watch spend, or rotate a key without holding the full admin token.

## Prerequisites

- A gateway at gateway/v0.16.0 (2026-09-28) or later. v0.15.0 carries the four tiers and `GET /admin/audit` but neither `GET /admin/virtual_keys/{name}/inflight` nor `admin.mtls`; on v0.15.0 an `admin.mtls` block is silently ignored and the listener stays plain HTTP.
- Write access to the gateway's `config.yaml` and to the environment the process starts with.
- `curl` and `openssl` on the machine you operate from.
- For the virtual-key steps, the SHA-256 of each client secret. See [Virtual keys and budgets](virtual-keys-and-budgets.md).

## Which tier opens which route

Admin is a superset of every other tier. Operator authenticates no read route. CostViewer authenticates exactly one route.

| Route | Admin | Viewer | CostViewer | Operator |
|---|---|---|---|---|
| `GET /admin/config` | yes | yes | - | - |
| `GET /admin/audit` (registered only when `audit_log_path` is set) | yes | yes | - | - |
| `GET /admin/virtual_keys` | yes | yes | - | - |
| `GET /admin/virtual_keys/{name}/spend` | yes | yes | yes | - |
| `GET /admin/virtual_keys/{name}/inflight` | yes | yes | - | - |
| `GET /admin/prompts`, `GET /admin/prompts/{id}`, `GET /admin/prompts/{id}/versions/{version}` | yes | yes | - | - |
| `POST /admin/virtual_keys/{name}/rotate` | yes | - | - | yes |
| `POST /admin/deployments/{name}/weight` | yes | - | - | yes |
| `POST /admin/cache/erase` | yes | - | - | yes |
| `POST /admin/virtual_keys/{name}`, `DELETE /admin/virtual_keys/{name}` | yes | - | - | - |
| `POST /admin/prompts/{id}`, `DELETE /admin/prompts/{id}`, `PUT /admin/prompts/{id}/labels/{label}`, `DELETE /admin/prompts/{id}/labels/{label}` | yes | - | - | - |
| `POST /admin/backup` (answers 501 until `backup_dir` is set) | yes | - | - | - |
| `GET /admin/debug/pprof/*` (mounted only when `enable_pprof: true`) | yes | - | - | - |

Three rules apply everywhere on this listener:

- Authentication is `Authorization: Bearer <token>`. The `Bearer ` prefix is case-sensitive and the token must be non-empty. Tokens are compared in constant time.
- A missing or malformed header and a valid token of the wrong tier both get `401` with a plain-text body (`missing or malformed Authorization header` or `invalid admin token`). There is no `403`.
- Admin tokens and client virtual keys are separate credential spaces. A virtual key never authenticates on `/admin`, and an admin token never authenticates on `/v1`.

Error bodies on `/admin` are plain text, not the JSON error envelope the data plane uses. See [Error codes](../reference/error-codes.md).

## Steps

### 1. Add the `admin` section to `config.yaml`

```yaml
admin:
  listen_addr: "127.0.0.1:8081"
  token_env: "KELVRAN_ADMIN_TOKEN"
  viewer_token_env: "KELVRAN_ADMIN_VIEWER_TOKEN"
  cost_viewer_token_env: "KELVRAN_ADMIN_COST_VIEWER_TOKEN"
  operator_token_env: "KELVRAN_ADMIN_OPERATOR_TOKEN"
  enable_audit_log: true
  audit_log_path: "/var/lib/kelvran-gateway/admin-audit.jsonl"
  persist_path: "/var/lib/kelvran-gateway/identity.db"
  backup_dir: "/var/lib/kelvran-gateway/backups"
  on_corrupt_store: "fail"
  enable_pprof: false
```

- `token_env` is the switch. Without it there is no admin server at all, and `audit_log_path` is silently ignored.
- Only `token_env` is required. Omit any `*_token_env` you do not want; an unconfigured tier authenticates nothing.
- `listen_addr` defaults to `127.0.0.1:8081` (loopback) when omitted.
- `enable_audit_log` defaults to `true`, even when the whole `admin` section is absent. `on_corrupt_store` defaults to `fail`; `reset` is the only other value. `enable_pprof` defaults to `false`.
- `persist_path` is the bbolt file that makes admin-created, deleted and rotated keys survive a restart. `redis_addr` (with `redis_password_env`, `redis_username`, `redis_tls`) is the Redis alternative.
- The commented block at the end of the `admin` section in [gateway/config.example.yaml](../../gateway/config.example.yaml) lists every key above, including `cost_viewer_token_env` and `audit_log_path`.

Every key is described in [Configuration reference](../reference/config.md).

### 2. Put the tokens in the environment and start the gateway

```bash
export KELVRAN_ADMIN_TOKEN="$(openssl rand -hex 32)"
export KELVRAN_ADMIN_VIEWER_TOKEN="$(openssl rand -hex 32)"
export KELVRAN_ADMIN_COST_VIEWER_TOKEN="$(openssl rand -hex 32)"
export KELVRAN_ADMIN_OPERATOR_TOKEN="$(openssl rand -hex 32)"
./gateway -config config.yaml
```

Tokens never live in `config.yaml`. Each `*_token_env` names an environment variable that is read once at startup. If a configured variable resolves empty, the process exits before binding any listener, with a message of the form `admin.viewer_token_env "KELVRAN_ADMIN_VIEWER_TOKEN" is set but resolves to an empty environment variable`. `gateway -validate` never reads environment variables, so it cannot catch this; only a real start does.

Hand each token to its audience: Viewer to support and dashboards, CostViewer to finance, Operator to on-call, Admin to the smallest group you can.

### 3. Call the routes with each tier

Replace `team-alpha`, `team-beta` and `gpt4o-primary` with your own names. `team-alpha` and `gpt4o-primary` must already exist (an unknown name answers `404`); `team-beta` is created by the last call.

```bash
ADMIN=http://127.0.0.1:8081

# Viewer or Admin: read the loaded config and list keys
curl -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN" $ADMIN/admin/config
curl -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN" $ADMIN/admin/virtual_keys

# CostViewer (also Viewer, Admin): one key's spend, and nothing else
curl -H "Authorization: Bearer $KELVRAN_ADMIN_COST_VIEWER_TOKEN" $ADMIN/admin/virtual_keys/team-alpha/spend
# {"spent_usd":"12.34","budget_usd":"100","budget_reset_interval_seconds":86400,"percent_used":0.1234}

# Operator (also Admin): rotate a key with a 600 s grace period -> 204
curl -X POST -H "Authorization: Bearer $KELVRAN_ADMIN_OPERATOR_TOKEN" -H "Content-Type: application/json" \
  $ADMIN/admin/virtual_keys/team-alpha/rotate \
  -d '{"new_key_hash":"5ea70b0b8699869f6b55abd0003876cde62aa65eb1023ef3f01dc9dfffa76edf","grace_period_seconds":600}'

# Operator (also Admin): reweight a deployment -> 204
curl -X POST -H "Authorization: Bearer $KELVRAN_ADMIN_OPERATOR_TOKEN" -H "Content-Type: application/json" \
  $ADMIN/admin/deployments/gpt4o-primary/weight -d '{"weight":3}'

# Admin only: create a key -> 204
curl -X POST -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  $ADMIN/admin/virtual_keys/team-beta \
  -d '{"key_hash":"b58fb5ccf291f4f4c149a9d22ced4a497aabf51c7fb6578a429b9d751b1c82b7","budget_usd":50,"rate_limit":{"burst":20,"refill_per_second":10}}'
```

The `new_key_hash` above is the SHA-256 of `do-not-use-team-alpha-rotated-secret` and the `key_hash` is the SHA-256 of `do-not-use-team-beta-secret`. Make a real one with `printf '%s' '<secret>' | sha256sum | cut -d' ' -f1` (`shasum -a 256` on macOS). `key_hash` and `new_key_hash` are required; `grace_period_seconds` of `0` or less rotates with no grace period. `GET /admin/config` returns the whole loaded config as JSON with Go field names (PascalCase); it holds environment-variable names and key hashes, never secret values. Request and response bodies for every route are in [Admin API reference](../reference/admin-api.md).

### 4. Read the audit trail

Every successful admin call except `GET /admin/audit` and the `/admin/debug/pprof/*` routes writes one structured log line at Info level whose `authorized_by` field is the tier that authenticated (`admin`, `viewer`, `cost_viewer` or `operator`). With `audit_log_path` set, the same event is also appended as one JSON line to that file, and `GET /admin/audit` (Admin or Viewer) queries it.

```bash
curl -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN" \
  "$ADMIN/admin/audit?field=name&value=team-alpha&since=2026-10-01T00:00:00Z"
# [{"time":"2026-10-08T12:00:00.482913-04:00","msg":"admin_virtual_key_rotated","fields":{"authorized_by":"operator","grace_period_seconds":"600","name":"team-alpha"}}]
```

Query parameters are optional and combine: `msg` (exact event name), `field` plus `value` (exact match on one field), `since` and `until` (RFC 3339, half-open range). No parameters returns every entry. A bad timestamp answers `400`. Field values are always strings. `time` is RFC 3339 with fractional seconds in the gateway process's local zone, so it ends in `Z` only when the process runs in UTC.

Event names: `admin_config_read`, `admin_virtual_keys_read`, `admin_virtual_key_spend_read`, `admin_virtual_key_inflight_read`, `admin_prompts_read`, `admin_virtual_key_upserted`, `admin_virtual_key_deleted`, `admin_virtual_key_rotated`, `admin_deployment_weight_updated`, `admin_cache_entry_erased`, `admin_backup_completed`, `admin_prompt_upserted`, `admin_prompt_deleted`, `admin_prompt_label_set`, `admin_prompt_label_deleted`. Secrets, key hashes, prompt bodies and message content are never logged; entries carry identifiers (`name`, `id`, `virtual_key_id`, `label`) and a few scalar fields (`weight`, `grace_period_seconds`, `model`, `version`, `count`, `files`, `l1_found`, `l2_found`). See [Metrics and logs](../reference/metrics-and-logs.md).

## Variants

### Require a client certificate (mTLS)

```yaml
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  mtls:
    ca_cert_path: "/etc/kelvran/admin-ca.pem"
    server_cert_path: "/etc/kelvran/admin-server.pem"
    server_key_path: "/etc/kelvran/admin-server-key.pem"
```

All three paths are required together; a block missing any one fails config load. The admin server then requires and verifies a client certificate against `ca_cert_path` and presents `server_cert_path` and `server_key_path`. Bearer tokens still apply on top. Call it over `https://` with a client certificate signed by that CA. Requires gateway/v0.16.0 or later; an earlier gateway ignores the block and keeps serving plain HTTP.

### Reach the admin port from another host

Set `listen_addr` to a non-loopback address on purpose. The shipped Kubernetes manifests expose only port 8080, the NetworkPolicy never opens 8081, and no manifest or compose file sets an admin token variable. If you open the port, put mTLS or a network boundary in front of it. See [Deploy with Kustomize](deploy/kubernetes-kustomize.md) and [Security model](../explanation/security-model.md).

### Give finance a spend-only credential

Set `token_env` and `cost_viewer_token_env` only. The CostViewer token answers `401` on every route except `GET /admin/virtual_keys/{name}/spend`, and that response carries no key hash, model list or rate-limit shape.

### Turn the audit trail off

`enable_audit_log: false` disables both the log line and the JSONL copy at once. There is no way to keep one without the other.

### Profile a running gateway

`enable_pprof: true` mounts `/admin/debug/pprof/` on the same listener for the Admin token only.

## Verify it worked

Each check is a command and the output to expect.

```bash
# 1. A valid token of the wrong tier is refused with 401, not 403
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $KELVRAN_ADMIN_OPERATOR_TOKEN" $ADMIN/admin/config
# 401

# 2. The key created in step 3 is listed
curl -s -H "Authorization: Bearer $KELVRAN_ADMIN_VIEWER_TOKEN" $ADMIN/admin/virtual_keys | grep -o '"id":"team-beta"'
# "id":"team-beta"

# 3. The create was recorded, attributed to the admin tier
curl -s -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  "$ADMIN/admin/audit?msg=admin_virtual_key_upserted&field=name&value=team-beta"
# [{"time":"...","msg":"admin_virtual_key_upserted","fields":{"authorized_by":"admin","name":"team-beta"}}]
```

If check 3 answers `404`, `audit_log_path` is not set (the route is not registered). If it returns `[]`, `enable_audit_log` is `false`, the append failed (look for `admin_audit_durable_append_failed`), or the file was rotated out from under the process (P7).

## Before you rely on it

- Mutations apply in memory first and answer `204` even if the durable write fails. On the next restart a `DELETE` or rotate whose write failed resurrects the old credential. Alert on `kelvran.persistence.failed` and re-drive the mutation before restarting. See R12 in [Failure modes](../operations/FAILURE-MODES.md).
- The admin server sets a 10 s read-header timeout and no write timeout, request span or access log. A stalled persistence disk can hold an admin request open indefinitely (P6).
- The JSONL audit file is created with mode `0600`, is append-only, is never truncated or rotated, and is not part of `POST /admin/backup`. A failed append logs `admin_audit_durable_append_failed` and the request still succeeds (P7). An unopenable path is fatal at startup. Retention is in [SECURITY.md](../../SECURITY.md).
- Admin-token rotation itself needs a restart. See [Rotate credentials](rotate-credentials.md).
- The admin routes, their JSON bodies and the four tiers are public, stable surface under [Versioning](../VERSIONING.md).

## Not available today

- No OpenAPI or Swagger description of the admin API.
- No `403`. A valid credential of an insufficient tier gets `401`.
- No hot-reload or rotation of the admin tokens themselves. They are read once at startup; only a restart picks up a new value.
- No per-route or per-resource roles beyond the four fixed tiers, and no API to create tiers.
- No read access for Operator, and nothing but `/spend` for CostViewer.
- No audit entry for `GET /admin/audit` or for any `/admin/debug/pprof/*` request.
- No shipped deploy manifest wires an admin token or exposes 8081.
- No rotation or size cap for the JSONL audit file.
- No metric for audit-append failures; a log line only.
- No admin route that resets a key's spend without deleting it.
- No request span, access log or write timeout on the admin server.

## Release history

- Viewer tier: gateway/v0.3.0 (2026-09-09).
- CostViewer tier, `POST .../rotate`, and bbolt persistence of admin-created keys: gateway/v0.11.0 (2026-09-16).
- Operator tier: gateway/v0.14.0 (2026-09-21).
- Durable audit store and `GET /admin/audit`: gateway/v0.15.0 (2026-09-23).
- Admin mTLS and `GET /admin/virtual_keys/{name}/inflight`: gateway/v0.16.0 (2026-09-28). Both were committed 2026-09-23, after the v0.15.0 tag.

## Related

- [Admin API reference](../reference/admin-api.md), [Configuration reference](../reference/config.md), [Error codes](../reference/error-codes.md)
- [Virtual keys and budgets](virtual-keys-and-budgets.md), [Rotate credentials](rotate-credentials.md), [Backup and restore](backup-and-restore.md), [Prompt management](prompt-management.md), [Caching](caching.md)
- [Security model](../explanation/security-model.md), [SECURITY.md](../../SECURITY.md), [THREAT_MODEL.md](../../THREAT_MODEL.md)
- RFCs: [admin API](../rfcs/2026-09-05-gateway-admin-api.md), [viewer role](../rfcs/2026-09-09-gateway-admin-viewer-role.md), [risk-tiered RBAC](../rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md)
