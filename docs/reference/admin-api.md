# Admin API reference

This page lists every route on the gateway's admin HTTP surface: the credential tier each route accepts, the request and response JSON, the status codes, the side effects (persistence, cross-replica propagation, audit log), and the configuration keys that enable and secure the listener. It is for operators who run the gateway and for anyone writing automation against the admin port. For a guided setup, read [Configure the Admin API and its credential tiers](../how-to/admin-api-rbac.md); this page is the lookup table behind it.

The route set and bodies below are those of `gateway/v0.17.0` (2026-10-07) and of `main` as of 2026-10-08. The handler is `gateway/internal/admin/admin.go`; the listener is started by `gateway/cmd/gateway/main.go`.

## Conventions

| Item | Value |
|---|---|
| Listener | A separate `net/http` server on its own port, never the client-facing mux. Default `127.0.0.1:8081`. |
| Transport | Plain HTTP unless `admin.mtls` is configured; then TLS, and every client must present a certificate signed by the configured CA. |
| Authentication | `Authorization: Bearer <token>` on every request. The token is compared with a constant-time comparison. |
| Request bodies | JSON. The `Content-Type` request header is not checked. |
| Success bodies | JSON with `Content-Type: application/json`, or `204 No Content` for mutations that return nothing. |
| Error bodies | Plain text written by `http.Error` (`text/plain; charset=utf-8`). Admin routes never return a JSON error envelope. The data-plane error codes in [error-codes.md](error-codes.md) do not apply here. |
| Timeouts | `ReadHeaderTimeout` of 10 s. No other server timeout is set. |
| Identifiers | A virtual-key name, prompt id or label is at most 256 bytes on write routes (upsert, label set). Content is not validated. Read, delete and rotate routes do not check the length. |
| Durations | Every duration field is an integer number of seconds. |
| Money | In responses, `budget_usd` and `spent_usd` are decimal strings, never floats. The upsert request accepts `budget_usd` as a decimal string or a JSON number. |

## Configuration

The `admin:` block of `config.yaml`. Every key is optional. Omit the whole block for no admin server. The full file is [gateway/config.example.yaml](../../gateway/config.example.yaml); every config key is in [config.md](config.md).

| Key | Type | Default | Meaning |
|---|---|---|---|
| `admin.listen_addr` | string | `127.0.0.1:8081` | Bind address of the admin server. The default is loopback only. |
| `admin.token_env` | string | unset | Name of the environment variable holding the Admin token. Unset: the admin server does not start. Set but resolving to an empty value: the gateway refuses to start. |
| `admin.viewer_token_env` | string | unset | Name of the environment variable holding the Viewer token. Set but empty: refuse to start. |
| `admin.cost_viewer_token_env` | string | unset | Name of the environment variable holding the CostViewer token. Set but empty: refuse to start. |
| `admin.operator_token_env` | string | unset | Name of the environment variable holding the Operator token. Set but empty: refuse to start. |
| `admin.persist_path` | string | unset | bbolt file for the identity store. Admin-created and rotated keys survive a restart only when this or `admin.redis_addr` is set. |
| `admin.redis_addr` | string | unset | Redis `host:port` for the identity store. When both this and `admin.persist_path` are set, Redis wins and a warning is logged. |
| `admin.redis_password_env`, `admin.redis_username`, `admin.redis_tls` | string, string, bool | unset | AUTH and TLS settings for `admin.redis_addr`. Meaningless when `admin.redis_addr` is unset. |
| `admin.enable_pprof` | bool | `false` | Mounts `net/http/pprof` under `/admin/debug/pprof/` on the admin mux, Admin tier only. |
| `admin.backup_dir` | string | unset | Directory for `POST /admin/backup`. Unset: that route answers 501. |
| `admin.enable_audit_log` | bool | `true` | Gates every admin audit-log line, both the `slog` line and the durable JSONL copy. The default is `true` whether or not the `admin:` block exists. |
| `admin.audit_log_path` | string | unset | Append-only JSONL file that receives every audit entry and backs `GET /admin/audit`. Unset: `GET /admin/audit` is not registered. An unopenable path exits at startup. Ignored when `admin.token_env` is unset. |
| `admin.on_corrupt_store` | `fail` or `reset` | `fail` | What happens when a configured identity, budget or prompt `persist_path` exists but bbolt cannot open it. Any other value is a load error. |
| `admin.mtls.ca_cert_path` | string | unset | PEM CA the server verifies client certificates against. |
| `admin.mtls.server_cert_path` | string | unset | PEM certificate the admin server presents. |
| `admin.mtls.server_key_path` | string | unset | PEM private key for `server_cert_path`. |

Validation rules for `admin.mtls`: the three paths are required together; a block missing any one of them is a load error. The files are read at startup; a load failure is a startup error. `ClientAuth` is `RequireAndVerifyClientCert`: a client without a certificate signed by the CA fails the TLS handshake. mTLS is in addition to the bearer tiers, never a replacement.

Startup logs `admin server listening` with `addr` and `mtls` (true or false).

### Minimal enabling block

```yaml
admin:
  listen_addr: "127.0.0.1:8081"
  token_env: "KELVRAN_ADMIN_TOKEN"
  viewer_token_env: "KELVRAN_ADMIN_VIEWER_TOKEN"
  operator_token_env: "KELVRAN_ADMIN_OPERATOR_TOKEN"
  persist_path: "/var/lib/kelvran-gateway/identity.db"
  audit_log_path: "/var/lib/kelvran-gateway/admin-audit.jsonl"
```

The token values live only in the named environment variables.

## Credential tiers

Four tiers. Admin is required; the other three are optional. Admin is a strict superset of every other tier. A tier whose environment variable is not configured matches nothing. The tier that authenticated a request is recorded as `authorized_by` in the audit entry.

| Tier | Config key | `authorized_by` | Scope |
|---|---|---|---|
| Admin | `admin.token_env` | `admin` | Every route. |
| Viewer | `admin.viewer_token_env` | `viewer` | Every `GET` route except pprof. No write route. |
| CostViewer | `admin.cost_viewer_token_env` | `cost_viewer` | Only `GET /admin/virtual_keys/{name}/spend`. |
| Operator | `admin.operator_token_env` | `operator` | Only `POST /admin/virtual_keys/{name}/rotate`, `POST /admin/deployments/{name}/weight`, `POST /admin/cache/erase`. |

Design records: [2026-09-05 admin API RFC](../rfcs/2026-09-05-gateway-admin-api.md), [2026-09-09 viewer role RFC](../rfcs/2026-09-09-gateway-admin-viewer-role.md), [2026-09-20 RBAC risk-tiering RFC](../rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md).

Admin tokens and client virtual keys are separate credential spaces. A virtual key never authenticates on the admin port; an admin token never authenticates on `/v1/*`.

### Authentication failures

| Status | Body | Cause |
|---|---|---|
| 401 | `missing or malformed Authorization header` | No `Authorization` header, or a value that is not `Bearer ` followed by at least one character. |
| 401 | `invalid admin token` | The presented token matches no tier that this route accepts. |

With `admin.mtls` set, a client without a valid certificate fails at the TLS handshake before any HTTP response.

## Route index

| Method | Path | Tiers | Success | Errors | Audit event |
|---|---|---|---|---|---|
| GET | `/admin/config` | Admin, Viewer | 200 JSON | 500 | `admin_config_read` |
| GET | `/admin/audit` | Admin, Viewer | 200 JSON | 400, 500 | none |
| GET | `/admin/virtual_keys` | Admin, Viewer | 200 JSON | | `admin_virtual_keys_read` |
| POST | `/admin/virtual_keys/{name}` | Admin | 204 | 400 | `admin_virtual_key_upserted` |
| DELETE | `/admin/virtual_keys/{name}` | Admin | 204 | 400, 404, 409, 500 | `admin_virtual_key_deleted` |
| POST | `/admin/virtual_keys/{name}/rotate` | Admin, Operator | 204 | 400, 404 | `admin_virtual_key_rotated` |
| GET | `/admin/virtual_keys/{name}/spend` | Admin, Viewer, CostViewer | 200 JSON | 404 | `admin_virtual_key_spend_read` |
| GET | `/admin/virtual_keys/{name}/inflight` | Admin, Viewer | 200 JSON | 404 | `admin_virtual_key_inflight_read` |
| GET | `/admin/prompts` | Admin, Viewer | 200 JSON | | `admin_prompts_read` |
| GET | `/admin/prompts/{id}` | Admin, Viewer | 200 JSON | 404 | `admin_prompts_read` |
| GET | `/admin/prompts/{id}/versions/{version}` | Admin, Viewer | 200 JSON | 400, 404 | `admin_prompts_read` |
| POST | `/admin/prompts/{id}` | Admin | 200 JSON | 400 | `admin_prompt_upserted` |
| DELETE | `/admin/prompts/{id}` | Admin | 204 | 400, 404, 500 | `admin_prompt_deleted` |
| PUT | `/admin/prompts/{id}/labels/{label}` | Admin | 200 JSON | 400, 404 | `admin_prompt_label_set` |
| DELETE | `/admin/prompts/{id}/labels/{label}` | Admin | 204 | 404, 500 | `admin_prompt_label_deleted` |
| POST | `/admin/backup` | Admin | 200 JSON | 500, 501 | `admin_backup_completed` |
| POST | `/admin/deployments/{name}/weight` | Admin, Operator | 204 | 400, 404, 500 | `admin_deployment_weight_updated` |
| POST | `/admin/cache/erase` | Admin, Operator | 200 JSON | 400, 404, 500 | `admin_cache_entry_erased` |
| GET | `/admin/debug/pprof/` and named profiles | Admin | pprof output | | none |
| POST | `/admin/debug/pprof/symbol` | Admin | pprof output | | none |

Every route also answers 401 as described above. `GET /admin/audit` exists only when `admin.audit_log_path` is set. The pprof routes exist only when `admin.enable_pprof` is `true`.

## GET /admin/config

Returns the loaded static configuration as JSON, unredacted. The config struct has no JSON tags, so keys are Go field names in PascalCase: `ListenAddr`, `VirtualKeys`, `Deployments`, `PriceTable`, `Models` (on `main` since 2026-10-08; absent in `gateway/v0.17.0`), `Telemetry`, `Budget`, `Prompt`, `RateLimit`, `ConfigPropagation`, `Cache`, `Guardrails`, `Admin`, `HealthProbe`, `Alerting`, `CredentialReload`. Nested structs follow the same rule.

The document contains environment-variable names and key hashes, never secret values. It is a read of the whole deployment topology and every key's budget and model shape, which is why the read is audit-logged.

| Status | Body |
|---|---|
| 200 | The config document. |
| 500 | `encoding config` |

## GET /admin/audit

Queries the durable JSONL audit trail at `admin.audit_log_path`. Registered only when that key is set. With no query parameters, every recorded entry is returned.

| Parameter | Type | Meaning |
|---|---|---|
| `msg` | string | Exact match on the event name, for example `admin_virtual_key_upserted`. |
| `field` and `value` | string, string | Exact match of one entry field: `fields[field] == value`. Use both together, for example `field=name&value=team-alpha`. |
| `since` | RFC 3339 | Lower bound, inclusive. |
| `until` | RFC 3339 | Upper bound, exclusive. |

All parameters are optional and combine with AND.

Response: a JSON array of `{"time": <RFC 3339>, "msg": <string>, "fields": {<string>: <string>}}`. `fields` is omitted when empty. Every field value is a string, including counts and versions.

| Status | Body |
|---|---|
| 200 | The array (possibly `[]`). |
| 400 | `since "<value>" is not a valid RFC 3339 timestamp: ...` or the `until` equivalent. |
| 500 | `querying audit log: ...` |

This read is not itself audit-logged.

## GET /admin/virtual_keys

Returns every virtual key the gateway currently accepts, sorted by `id`. `key_hash` is never included.

Each element:

| Field | Type | Present | Meaning |
|---|---|---|---|
| `id` | string | always | The key name. |
| `budget_usd` | string | always | Decimal cap. `"0"` means unlimited. |
| `budget_reset_interval_seconds` | int | always | `0` means a lifetime cap with no rolling window. |
| `budget_warn_percent` | number | always | See `POST /admin/virtual_keys/{name}`. |
| `allowed_models` | string[] | when non-empty | Sorted. |
| `allowed_regions` | string[] | when non-empty | Sorted. |
| `allowed_source_cidrs` | string[] | when non-empty | Sorted CIDR strings. |
| `cache_scope_to_end_user` | bool | when `true` | |
| `rate_limit_burst` | number | when non-zero | Key-level RPM bucket capacity. |
| `rate_limit_refill_per_second` | number | when non-zero | Key-level RPM refill. |
| `billing_subject_id` | string | when non-empty | Settable only through `virtual_keys.<name>.billing_subject_id` in `config.yaml`. |

Per-model and TPM rate-limit settings are not part of this response.

## POST /admin/virtual_keys/{name}

Creates the key `{name}` or replaces it. An upsert is a full replace of every field, never a merge: a field omitted from the body is reset, including `rate_limit` and its `per_model` map. Field names mirror the `virtual_keys.<name>` section of `config.yaml`.

Request body:

| Field | Type | Required | Validation | Meaning |
|---|---|---|---|---|
| `key_hash` | string | yes | 64 hex characters (a SHA-256 digest), any case; must not equal another key's current or grace-period hash | Hex SHA-256 of the client secret, stored lowercased. The secret itself is never sent. |
| `budget_usd` | decimal string or number | no | not negative | Spend cap. `0` or omitted: unlimited. |
| `budget_reset_interval_seconds` | int | no | not negative | Rolling budget window. `0`: lifetime cap. |
| `budget_warn_percent` | number | no | `0 <= value <= 100` | Fraction of `budget_usd` at which a `budget_warn_threshold_crossed` warning is logged; `0.8` means 80 %. `0`: disabled. The validation range is only a sign and magnitude bound: a value above `1` sets a threshold above the cap, which never fires before the hard cutoff. |
| `allowed_models` | string[] | no | | Canonical model names this key may call. Omitted or empty: no model allow-list. |
| `allowed_regions` | string[] | no | | Deployment regions this key may route to. Omitted or empty: no region allow-list. |
| `allowed_source_cidrs` | string[] | no | each entry parses as a CIDR | Client source networks this key may call from. Omitted or empty: no source allow-list. |
| `cache_scope_to_end_user` | bool | no | | Scope this key's cache entries by the request's end-user identifier. See [caching.md](../how-to/caching.md). |
| `rate_limit` | object | no | | See below. Omitted: `burst` and `refill_per_second` resolve to the defaults. |

`rate_limit` object:

| Field | Type | Validation | Meaning |
|---|---|---|---|
| `burst` | number | see note | Key-level RPM bucket capacity. |
| `refill_per_second` | number | see note | Key-level RPM refill rate. |
| `tpm_capacity` | number | | Key-level tokens-per-minute bucket. `<= 0`: no TPM limit. |
| `tpm_refill_per_second` | number | | Key-level TPM refill. |
| `per_model` | object | | Map of model name to a per-model override object. |

Note on `burst` and `refill_per_second`: when both are `<= 0` (including when `rate_limit` is omitted), they resolve to the gateway defaults of `20` burst and `10` refill per second. When either is positive, both are stored as given.

`max_concurrent_requests` (the key's concurrency cap in `config.yaml`) is not accepted here; see "Not available today".

`per_model.<model>` object:

| Field | Type | Validation |
|---|---|---|
| `burst` | number | `> 0`, required |
| `refill_per_second` | number | `> 0`, required |
| `tpm_capacity` | number | set together with `tpm_refill_per_second`, or neither |
| `tpm_refill_per_second` | number | set together with `tpm_capacity`, or neither |

Status codes:

| Status | Body |
|---|---|
| 204 | none |
| 400 | `virtual key name is required` |
| 400 | `virtual key name exceeds 256 characters` |
| 400 | `invalid request body: ...` |
| 400 | `key_hash is required` |
| 400 | `dataplane: applyVirtualKeyUpsert: identity: NewVerifier: virtual key "<name>": key_hash "<value>" is not valid hex: ...` or `... must decode to 32 bytes (a SHA-256 digest), got <n>` |
| 400 | `dataplane: applyVirtualKeyUpsert: identity: duplicate virtual key hash in config: virtual key "<id>"`, with a ` (previous_key_hash)` suffix when the collision is with a grace-period hash |
| 400 | `budget_usd must not be negative` |
| 400 | `budget_reset_interval_seconds must not be negative` |
| 400 | `budget_warn_percent must be between 0 and 100` |
| 400 | `allowed_source_cidrs entry "<entry>": ...` |
| 400 | `rate_limit.per_model.<model> must set positive burst and refill_per_second` |
| 400 | `rate_limit.per_model.<model>.tpm_capacity/tpm_refill_per_second must both be set, or neither` |
| 400 | any error from the pipeline apply step, as raw text |

Side effects:

- The rate limiter entry is registered before the new key becomes resolvable, never after.
- A name that did not exist before also clears any leftover budget-spend state recorded under that id. An update of an existing name keeps its accumulated spend.
- `billing_subject_id` is not a field of this body; after an upsert the key has none.
- The key is written to the identity store when `admin.persist_path` or `admin.redis_addr` is set. Without either, the key is lost at restart.
- A `virtual_key_upsert` event is published when `config_propagation` is configured.
- Audit entry `admin_virtual_key_upserted` with `name` and `authorized_by=admin`. The hash is never logged.

## DELETE /admin/virtual_keys/{name}

Removes the key, live. The last remaining key cannot be deleted.

| Status | Body |
|---|---|
| 204 | none |
| 400 | `virtual key name is required` |
| 404 | `dataplane: virtual key not found: "<name>"` |
| 409 | `dataplane: cannot delete the last remaining virtual key: identity: NewVerifier: at least one virtual key is required` |
| 500 | any other error, as raw text |

Side effects: the key's budget-spend record is removed from memory and from the budget store (see [DATA-SUBJECT-REQUESTS.md](../operations/DATA-SUBJECT-REQUESTS.md)); the key's rate-limiter bucket is left in memory and never reached again; a `virtual_key_delete` event is published when `config_propagation` is configured; audit entry `admin_virtual_key_deleted` with `name` and `authorized_by=admin`.

## POST /admin/virtual_keys/{name}/rotate

Issues a new secret for the key while keeping the old one valid for a grace period. The id, budget, allow-lists and rate limits are unchanged.

Request body:

| Field | Type | Required | Meaning |
|---|---|---|---|
| `new_key_hash` | string | yes | Hex SHA-256 of the new secret: 64 hex characters, any case, stored lowercased; must not equal another key's current or grace-period hash. A malformed hash is a 400 with `dataplane: RotateVirtualKey: identity: NewVerifier: ...` as raw text; a hash equal to another key's current or grace-period hash is a 400 with `dataplane: RotateVirtualKey: identity: duplicate virtual key hash in config: virtual key "<id>"` (with a ` (previous_key_hash)` suffix when the collision is with a grace-period hash). |
| `grace_period_seconds` | int | no | How long the previous secret keeps working. `<= 0`: the old secret stops immediately. |

| Status | Body |
|---|---|
| 204 | none |
| 400 | `virtual key name is required` |
| 400 | `invalid request body: ...` |
| 400 | `new_key_hash is required` |
| 400 | any other pipeline error, as raw text |
| 404 | `dataplane: virtual key not found: "<name>"` |

Semantics:

- The old secret is accepted until the grace period elapses, then rejected.
- One previous secret is kept. A second rotation during a pending grace period replaces that slot; the earlier old secret stops working at once.
- The rotation is persisted to the identity store when configured, and published to other replicas as a `virtual_key_upsert` event carrying the resulting key state.
- Audit entry `admin_virtual_key_rotated` with `name`, `grace_period_seconds` and `authorized_by` (`admin` or `operator`). Neither hash is logged.

See [rotate-credentials.md](../how-to/rotate-credentials.md) for the procedure.

## GET /admin/virtual_keys/{name}/spend

Returns the key's current spend against its cap. The only route the CostViewer tier may call.

Response:

| Field | Type | Meaning |
|---|---|---|
| `spent_usd` | string | Decimal spend within the current window. |
| `budget_usd` | string | Decimal cap. `"0"` means unlimited. |
| `budget_reset_interval_seconds` | int | The key's window. |
| `percent_used` | number | `spent_usd / budget_usd` as a fraction (`0.5` means 50 %). `0` when `budget_usd` is not positive. |

| Status | Body |
|---|---|
| 200 | The object. |
| 404 | `virtual key "<name>" not found` |

Audit entry `admin_virtual_key_spend_read` with `name` and `authorized_by`; the figures are not logged. In Redis budget mode a backend error makes `spent_usd` read as `0`; see row R8 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md).

## GET /admin/virtual_keys/{name}/inflight

Returns the key's current in-flight request count. Observability only; no admission decision reads this value.

Response:

| Field | Type | Meaning |
|---|---|---|
| `total_in_flight` | int | Requests in flight under this key on this replica. |
| `by_agent_run_id` | object | Map of `agent_run_id` to count. |

| Status | Body |
|---|---|
| 200 | The object. |
| 404 | `virtual key "<name>" not found` |

Audit entry `admin_virtual_key_inflight_read` with `name` and `authorized_by`.

## Prompt routes

Prompts are global, operator-managed templates stored by the gateway and referenced from data-plane requests by `prompt_id` with `prompt_version` or `prompt_label`. Design record: [2026-09-13 prompt management RFC](../rfcs/2026-09-13-gateway-prompt-management.md). Procedures: [prompt-management.md](../how-to/prompt-management.md).

Prompt object, returned by every prompt read and write route:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Prompt id. |
| `version` | int | Version number, starting at 1. |
| `messages` | Message[] | The stored messages. |
| `created_at` | RFC 3339 | When this version was created. |

Message object (the data-plane message shape):

| Field | Type | Present |
|---|---|---|
| `role` | string | always; one of `system`, `user`, `assistant`, `tool` |
| `content` | string | when non-empty |
| `parts` | ContentPart[] | when non-empty |
| `tool_calls` | ToolCall[] | when non-empty |
| `tool_call_id` | string | when non-empty |
| `cache_control` | object | when set |
| `refusal` | string | when non-empty; a response-only field that no provider accepts on a request |
| `reasoning_blocks` | ReasoningBlock[] | when non-empty; opaque provider reasoning blocks (`sequence`, `redacted`, `text`, `data`, `signature`) echoed verbatim |

ContentPart: `type` (`text`, `image`, `document`), `text`, `media_type`, `data` (base64), `url`, `cache_control`; all but `type` omitted when empty.

### GET /admin/prompts

Returns a JSON array of the latest version of every prompt, sorted by `id`. Audit entry `admin_prompts_read` with `authorized_by`.

### GET /admin/prompts/{id}

Returns the latest version of `{id}`.

| Status | Body |
|---|---|
| 200 | The prompt object. |
| 404 | `prompt "<id>" not found` |

Audit entry `admin_prompts_read` with `id` and `authorized_by`.

### GET /admin/prompts/{id}/versions/{version}

Returns one historical version.

| Status | Body |
|---|---|
| 200 | The prompt object. |
| 400 | `invalid version "<version>": must be a positive integer` |
| 404 | `prompt "<id>" version <n> not found` |

Audit entry `admin_prompts_read` with `id`, `version` and `authorized_by`.

### POST /admin/prompts/{id}

Creates the next version of `{id}`. The new version number is the count of existing versions plus one; a new id gets version 1. Existing versions are never modified.

Request body: `{"messages": Message[]}`.

| Status | Body |
|---|---|
| 200 | The new prompt object. |
| 400 | `prompt id is required` |
| 400 | `prompt id exceeds 256 characters` |
| 400 | `invalid request body: ...` |
| 400 | `messages is required and must be non-empty` |
| 400 | a content-part validation error: invalid base64 in `parts[].data`, or a declared `media_type` whose category does not match the detected content |
| 400 | the message-count error when `messages` has more than 2000 elements |
| 400 | any store error, as raw text, including a persistence failure (see below) |

Audit entry `admin_prompt_upserted` with `id`, `version` and `authorized_by=admin`. Message content is never logged.

### DELETE /admin/prompts/{id}

Removes every version of `{id}` and its labels.

| Status | Body |
|---|---|
| 204 | none |
| 400 | `prompt id is required` |
| 404 | `prompt: prompt not found: "<id>"` |
| 500 | any other error, as raw text |

Audit entry `admin_prompt_deleted` with `id` and `authorized_by=admin`.

### PUT /admin/prompts/{id}/labels/{label}

Points `{label}` at one version of `{id}`. This is the single promote and rollback primitive: pointing a label at a newer version promotes, at an older version rolls back. The label is created if absent.

Request body: `{"version": int}`. A `version` of `0` or less means the current latest version, resolved to a concrete number at call time and stored as that number.

Response:

| Field | Type |
|---|---|
| `prompt_id` | string |
| `label` | string |
| `version` | int |
| `updated_at` | RFC 3339 |

| Status | Body |
|---|---|
| 200 | The label object. |
| 400 | `prompt id and label are both required` |
| 400 | `label exceeds 256 characters` |
| 400 | `invalid request body: ...` |
| 400 | any other store error, as raw text |
| 404 | `prompt: prompt not found: "<id>"` or `prompt: prompt not found: "<id>" version <n>` |

Audit entry `admin_prompt_label_set` with `id`, `label`, `version` and `authorized_by=admin`.

### DELETE /admin/prompts/{id}/labels/{label}

Removes the label. Versions are untouched.

| Status | Body |
|---|---|
| 204 | none |
| 404 | `prompt: prompt not found: "<id>" label "<label>"` |
| 500 | any other error, as raw text |

Audit entry `admin_prompt_label_deleted` with `id`, `label` and `authorized_by=admin`.

## POST /admin/backup

Copies every bbolt-backed store that has a `persist_path` configured (identity under `admin.persist_path`, budget under `budget.persist_path`, prompt under `prompt.persist_path`) into `admin.backup_dir`, one file per store, while the gateway keeps serving. A store with no `persist_path` is skipped silently. A failure on one store does not stop the others, but the response is then 500 with every store's error joined, and the files that were written are not listed; look in `admin.backup_dir`.

Request body: none.

Response: `{"files": [<string>, ...]}`, the file names written; `{"files": null}` when no configured store is bbolt-backed.

| Status | Body |
|---|---|
| 200 | The object. |
| 500 | the backup error, as raw text |
| 501 | `admin.backup_dir is not configured` |

The audit JSONL file at `admin.audit_log_path` is not a bbolt store and is not included. Redis-backed stores are not included. Audit entry `admin_backup_completed` with `files` and `authorized_by=admin`. Restore is an offline operation; see [backup-and-restore.md](../how-to/backup-and-restore.md).

## POST /admin/deployments/{name}/weight

Sets the routing weight of one deployment, live.

Request body: `{"weight": int}`.

| Status | Body |
|---|---|
| 204 | none |
| 400 | `deployment name is required` |
| 400 | `invalid request body: ...` |
| 400 | `weight must be non-negative, got <n>` |
| 404 | `dataplane: deployment not found: "<name>"` |
| 500 | any other error, as raw text |

Semantics:

- `0` is accepted and means unset, the same meaning `weight` has in `config.yaml`.
- The change is in memory only. A restart reverts to the weight in `config.yaml`.
- A `deployment_weight` event is published when `config_propagation` is configured.
- Audit entry `admin_deployment_weight_updated` with `name`, `weight` and `authorized_by` (`admin` or `operator`).

Weighted routing itself is described in [routing-and-failover.md](../how-to/routing-and-failover.md).

## POST /admin/cache/erase

Removes one cached response from the L1 (exact-match) and L2 (normalized-match) layers of this replica. The caller must already know the original request's defining fields; the cache has no per-tenant or per-subject index. The procedure around a data-subject request is in [DATA-SUBJECT-REQUESTS.md](../operations/DATA-SUBJECT-REQUESTS.md).

Request body: one flat JSON object.

| Field | Type | Required | Meaning |
|---|---|---|---|
| `virtual_key_id` | string | yes | The key the original request was made under. Must name a configured key. |
| `end_user_id` | string | no | The end-user identifier the original request carried, for keys with `cache_scope_to_end_user`. Omitted: the tenant-scoped entry. |
| the data-plane chat request fields, flattened | | | `model`, `messages`, `temperature`, `max_tokens`, `tools`, `tool_choice`, `stream`, `response_format`, `prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables`, `thinking_binding_mode`. Send exactly the ones the original request set. |

Which of those fields take part in the cache key is described in [caching.md](../how-to/caching.md). `tools` and `tool_choice` are part of the L1 and L2 key on `main` since 2026-10-08, not in `gateway/v0.17.0`.

Response:

| Field | Type | Meaning |
|---|---|---|
| `l1_found` | bool | An L1 entry existed and was removed. |
| `l2_found` | bool | An L2 entry existed and was removed. |
| `l3_skipped` | bool | Always `true`. L3 is not touched. |

| Status | Body |
|---|---|
| 200 | The object. `l1_found` and `l2_found` both `false` is a successful "nothing cached" answer, not an error. |
| 400 | `invalid request body: ...` |
| 400 | `virtual_key_id is required` |
| 400 | `dataplane: request sets both prompt_id and messages` |
| 400 | `dataplane: request sets both prompt_label and prompt_version` |
| 400 | `dataplane: failed to resolve prompt_id: ...` |
| 404 | `virtual key "<id>" not found` |
| 500 | any other error, as raw text |

Semantics:

- The get-then-delete against each layer is not atomic. A concurrent identical request can write a fresh entry under the same key between the two steps.
- The erasure acts on this replica only. In a multi-replica deployment, call every replica.
- Audit entry `admin_cache_entry_erased` with `virtual_key_id`, `model`, `l1_found`, `l2_found` and `authorized_by` (`admin` or `operator`). Message content is never logged.

## pprof routes

Present only when `admin.enable_pprof: true`. Admin tier only; the Viewer tier is refused.

| Method | Path | Handler |
|---|---|---|
| GET | `/admin/debug/pprof/` | index |
| GET | `/admin/debug/pprof/cmdline` | cmdline |
| GET | `/admin/debug/pprof/profile` | CPU profile |
| GET, POST | `/admin/debug/pprof/symbol` | symbol |
| GET | `/admin/debug/pprof/trace` | execution trace |
| GET | `/admin/debug/pprof/goroutine`, `/heap`, `/threadcreate`, `/block`, `/mutex`, `/allocs` | named profiles |

These routes write no audit entry.

## Audit log

Every successful call to a route with an audit event in the route index writes one `slog` Info line in JSON. When `admin.audit_log_path` is set, the same entry is appended to the JSONL file and becomes queryable through `GET /admin/audit`. `admin.enable_audit_log: false` disables both outputs together.

| Event | Fields besides `authorized_by` |
|---|---|
| `admin_config_read` | none |
| `admin_virtual_keys_read` | `count` |
| `admin_virtual_key_upserted` | `name` |
| `admin_virtual_key_deleted` | `name` |
| `admin_virtual_key_rotated` | `name`, `grace_period_seconds` |
| `admin_virtual_key_spend_read` | `name` |
| `admin_virtual_key_inflight_read` | `name` |
| `admin_prompts_read` | none for the list; `id` for one prompt; `id`, `version` for one version |
| `admin_prompt_upserted` | `id`, `version` |
| `admin_prompt_deleted` | `id` |
| `admin_prompt_label_set` | `id`, `label`, `version` |
| `admin_prompt_label_deleted` | `id`, `label` |
| `admin_backup_completed` | `files` |
| `admin_deployment_weight_updated` | `name`, `weight` |
| `admin_cache_entry_erased` | `virtual_key_id`, `model`, `l1_found`, `l2_found` |

No entry ever carries a token, a key hash, prompt content, or spend figures.

When the durable append fails, the request still succeeds, the `slog` line is the only copy, and a Warn line `admin_audit_durable_append_failed` with `error` is written. There is no metric for this failure. `GET /admin/audit` has no marker for the gap. See row P7 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md). Log and metric names across the gateway are in [metrics-and-logs.md](metrics-and-logs.md).

## Persistence and propagation

### What survives a restart

| Mutation | Durable when | Store |
|---|---|---|
| Virtual-key upsert, delete, rotate | `admin.persist_path` or `admin.redis_addr` is set | identity store |
| Budget spend changes caused by upsert (new id) or delete | `budget.persist_path` or `budget.redis_addr` is set | budget store |
| Prompt upsert, delete, label set, label delete | `prompt.persist_path` is set | prompt store |
| Deployment weight | never | in memory only |

At startup a persisted key is authoritative over the same id in `config.yaml`; a persisted key whose id is not in `config.yaml` is loaded as a new key. A persisted key's rate limit is rebuilt only from its stored `burst` and `refill_per_second`; a `per_model` or key-level TPM override that was set through this API does not survive a restart.

### Persistence failures

A store write that fails during a virtual-key mutation does not change the HTTP response: the caller gets 204, the change is live in memory, and the gateway logs `identity_persist_failed` or `budget_persist_failed` and increments `kelvran.persistence.failed` with `kelvran.persistence.store_kind` of `identity` or `budget`. On the next restart an upsert vanishes and a delete or rotate resurrects the old credential. Re-drive the mutation before any restart. Row R12 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md) has the operator procedure.

A prompt-store write that fails answers 400 on `POST /admin/prompts/{id}` and 500 on `DELETE /admin/prompts/{id}`, with the raw error text, after the in-memory change has already applied. No log line and no metric record it. Row P6 of the same document describes the recovery.

### Cross-replica propagation

With `config_propagation.redis_addr` and `config_propagation.signing_secret_env` set, four admin routes publish three event types to the other replicas as HMAC-signed events:

| Admin route | Event type |
|---|---|
| `POST /admin/virtual_keys/{name}` | `virtual_key_upsert` |
| `POST /admin/virtual_keys/{name}/rotate` | `virtual_key_upsert` (the resulting key state plus this replica's currently registered rate-limit config, so a replica that never saw the original upsert still registers a working limiter) |
| `DELETE /admin/virtual_keys/{name}` | `virtual_key_delete` |
| `POST /admin/deployments/{name}/weight` | `deployment_weight` |

A replica ignores its own events and applies the others with last-writer-wins ordering. A publish failure leaves the local mutation applied and the other replicas stale; it is logged as `configpropagation_publish_failed` and, on `main` since 2026-10-08 and not in `gateway/v0.17.0`, counted by `kelvran.configpropagation.publish_failed`. Rows R13 and R14 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md) cover publish and subscribe outages.

## Not available today

- A route that resets a virtual key's spend without deleting the key. `DELETE` then `POST` is the only lever, and it also removes legitimate spend.
- Erasure of L3 (lexical near-duplicate) cache entries. `POST /admin/cache/erase` reports `l3_skipped: true` on every call; an L3 entry expires only through its TTL.
- Propagation of prompt mutations across replicas. Only virtual-key and deployment-weight events are published. Prompts are per replica: send every prompt mutation to every replica, or stop a replica and restart it from a copy of another replica's `prompt.persist_path` file. A bbolt file cannot be shared by two running replicas; the second fails to start within one second with `another process holds the file lock` (row P2 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md)).
- A JSON error envelope on admin routes. Every error is plain text.
- An admin web UI, by recorded decision.
- A dedicated read route for deployments or for one deployment's current weight. `GET /admin/config` returns the configured `Deployments`; the live weight set through this API is not exposed by any route.
- Per-entry deletion from the audit log. Only `admin.enable_audit_log: false` exists, and it stops new entries only.
- Setting `billing_subject_id` or `rate_limit.max_concurrent_requests` (the key's concurrency cap) through this API. Neither is a field of the upsert body (an unknown body field is ignored). `billing_subject_id` is reported by `GET /admin/virtual_keys` when set; `max_concurrent_requests` is reported by no route; an unknown body field is ignored. An upsert stores the key with no concurrency cap: the live limiter on this replica keeps the cap registered at startup until the next restart, after which a persisted key is reloaded with none.

## Examples

Tokens are read from the environment variables named in the `admin:` block. The key hashes below are the example hashes from [gateway/config.example.yaml](../../gateway/config.example.yaml); do-not-use.

Create or replace a key (Admin):

```bash
curl -s -X POST http://127.0.0.1:8081/admin/virtual_keys/team-gamma \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"key_hash":"6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1","budget_usd":"50","budget_reset_interval_seconds":2592000,"allowed_models":["gpt-4o"],"rate_limit":{"burst":20,"refill_per_second":10}}'
```

Response: `204 No Content`.

Rotate a key with a one-hour grace period (Admin or Operator):

```bash
curl -s -X POST http://127.0.0.1:8081/admin/virtual_keys/team-gamma/rotate \
  -H "Authorization: Bearer $KELVRAN_ADMIN_OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"new_key_hash":"8e43f8e74a4151a23f77bd21474a038434c56bad8c5d8bc7bb2af0d14fa95095","grace_period_seconds":3600}'
```

Response: `204 No Content`.

Read spend (CostViewer):

```bash
curl -s http://127.0.0.1:8081/admin/virtual_keys/team-gamma/spend \
  -H "Authorization: Bearer $KELVRAN_ADMIN_COST_VIEWER_TOKEN"
```

Response:

```json
{"spent_usd":"0.0123","budget_usd":"50","budget_reset_interval_seconds":2592000,"percent_used":0.000246}
```

Query the audit trail for one key since a date (Admin or Viewer):

```bash
curl -s 'http://127.0.0.1:8081/admin/audit?field=name&value=team-gamma&since=2026-10-01T00:00:00Z' \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
```

Re-weight a deployment (Admin or Operator):

```bash
curl -s -X POST http://127.0.0.1:8081/admin/deployments/gpt4o-canary/weight \
  -H "Authorization: Bearer $KELVRAN_ADMIN_OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"weight":3}'
```

Response: `204 No Content`.

Erase one cached response (Admin or Operator):

```bash
curl -s -X POST http://127.0.0.1:8081/admin/cache/erase \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"virtual_key_id":"team-gamma","model":"gpt-4o","messages":[{"role":"user","content":"the exact original prompt"}]}'
```

Response:

```json
{"l1_found":true,"l2_found":true,"l3_skipped":true}
```

## Related pages

- [Configure the Admin API and its credential tiers](../how-to/admin-api-rbac.md)
- [Virtual keys and budgets](../how-to/virtual-keys-and-budgets.md)
- [Configuration reference](config.md)
- [Error codes](error-codes.md) (data plane)
- [Security model](../explanation/security-model.md) and [THREAT_MODEL.md](../../THREAT_MODEL.md)
- [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md)
