# Configure virtual keys and budgets

This page shows an operator how to issue a Kelvran virtual key, cap its spend, rate-limit it, restrict what it may call, and manage it live through the admin API. It covers the single-process case first, then the knobs that keep keys and budgets correct across more than one gateway replica.

Use this when you need to hand a team or an application its own credential with its own dollar cap and throughput limits, without giving it any upstream provider key.

## Prerequisites

- A gateway binary or image and a `config.yaml` with at least one deployment. Start with the [quickstart](../tutorials/quickstart.md) if you have neither.
- `openssl` and `sha256sum` (or `shasum -a 256` on older macOS) to generate a secret and its hash.
- For the admin-API steps: `curl` and an environment variable holding the admin token, for example `KELVRAN_ADMIN_TOKEN`.
- For the multi-replica variant: a reachable Redis.

Every field named below is described in the [config reference](../reference/config.md); every route is in the [admin API reference](../reference/admin-api.md).

## Steps

### 1. Generate the secret and its hash

The gateway never generates or stores a raw secret. You generate it, hand the raw value to the client, and put only its SHA-256 hash in config:

```bash
export KELVRAN_KEY=$(openssl rand -hex 32)                 # the raw secret: clients send it as a bearer token
printf '%s' "$KELVRAN_KEY" | sha256sum | cut -d' ' -f1     # the 64-hex digest that goes into key_hash
```

`printf '%s'` matters: a trailing newline changes the digest. `key_hash` must decode to 32 bytes (64 hex characters); it is lower-cased at startup by the verifier (`-validate` does not check it), and two keys with the same hash are a startup error.

### 2. Declare the key with a budget

Config must declare at least one key under `virtual_keys`, and every key needs `key_hash`:

```yaml
virtual_keys:
  team-alpha:
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
    budget_usd: 100.0                        # omit or 0 = unlimited
    budget_reset_interval_seconds: 2592000   # optional; 30-day rolling window
    budget_warn_percent: 0.8                 # optional; a FRACTION of budget_usd
  team-beta:
    key_hash: "8e43f8e74a4151a23f77bd21474a038434c56bad8c5d8bc7bb2af0d14fa95095"
```

The two hashes are the SHA-256 digests of the public example secrets `example-team-alpha-secret-do-not-use` and `example-team-beta-secret-do-not-use` from [`gateway/config.example.yaml`](../../gateway/config.example.yaml). Replace them with your own.

- `budget_usd` is a decimal cap. Omitted or `0` means unlimited.
- `budget_reset_interval_seconds` greater than 0 turns the cap into a rolling window. The window is checked lazily on each request; there is no scheduler. `0` means the cap lasts for the life of the process, or for the life of the store when spend is persisted.
- `budget_warn_percent` is a fraction: `0.8` means 80 %. The gateway multiplies `budget_usd` by this value, so `80` would place the threshold at eighty times the budget. At or above the threshold every billable completion logs `budget_warn_threshold_crossed`, and once per window the alerting webhook fires if one is configured. Separately, a fixed 50/75/90/100 % ladder emits `kelvran.budget.threshold_crossed` and a `budget_threshold_crossed` log line once per bucket per window.

`team-beta` has no budget, the gateway's default rate limit, and access to every configured model.

### 3. Add rate limits

All rate-limit fields sit under `rate_limit:` inside the key:

```yaml
    rate_limit:
      burst: 20                     # both or neither; omitted = 20 burst / 10 per second
      refill_per_second: 10
      tpm_capacity: 100000          # optional pair: tokens per minute
      tpm_refill_per_second: 1000
      max_concurrent_requests: 10   # optional; <= 0 = unlimited
      per_model:
        gpt-4o:
          burst: 5                  # both required and positive
          refill_per_second: 1
          tpm_capacity: 50000       # optional pair
          tpm_refill_per_second: 500
```

- `burst` and `refill_per_second` must both be set or both omitted, and may not be negative. Omitted resolves to the gateway default of 20 burst and 10 per second, on both the YAML and the admin-API path.
- `tpm_capacity` and `tpm_refill_per_second` follow the same pair rule and add a tokens-per-minute dimension. TPM is reserved before the upstream call from past usage and reconciled to real usage afterwards. It is enforced in both in-memory and Redis mode; the Redis path uses GCRA and ships in gateway/v0.14.0 (2026-09-21) and later.
- `max_concurrent_requests` caps the key's simultaneously in-flight requests. It is counted in memory per gateway instance. The concurrency check runs after the RPM and TPM checks, so a request rejected for concurrency has already spent one RPM token; its TPM reservation is returned when the request finalizes.
- `per_model.<model>` gives one model its own RPM bucket and, optionally, its own TPM pair. Every other model keeps the key's default bucket.

### 4. Restrict models and source IPs

Allow-lists are YAML mappings of value to `true`. They are not lists; the parser has no list support and rejects a non-boolean value at load:

```yaml
    allowed_models:            # mapping, NOT a list
      gpt-4o: true
      claude-opus-4: true
    allowed_source_cidrs:
      "10.0.0.0/8": true
```

- A request for a model outside `allowed_models` gets `403` with code `model_not_allowed`.
- A client outside `allowed_source_cidrs` gets `403` with code `source_ip_not_allowed`. The source IP is the TCP peer address; `X-Forwarded-For` is deliberately not trusted. CIDR syntax is checked at load, so `-validate` catches a typo.
- `allowed_regions` restricts the key to deployments with a matching `region`. It fails closed: a deployment with no `region` (every non-Bedrock deployment) never satisfies it.

### 5. Validate and start

```bash
/tmp/kelvran-gateway -validate -config config.yaml && echo valid
```

`-validate` exits 0 when the file loads and 1 otherwise. It checks shape, the rate-limit pair rules and CIDR syntax; it does not resolve environment variables and never dials Redis. It does not check `key_hash` format or duplicates: a malformed or duplicate hash passes `-validate` and fails at startup with `constructing identity verifier`. Then start the gateway as described in the [deployment runbook](../operations/DEPLOY.md).

Per request, after authentication, the gateway checks in this order: source-IP allow-list, model allow-list, idempotency claim, prompt resolution, RPM then TPM, concurrency slot, budget reservation, then the cache lookup.

## Variants

### Keep spend across restarts on one host

Without persistence, spend resets on every restart. For a single process, point the budget tracker at a bbolt file:

```yaml
budget:
  persist_path: "/var/lib/kelvran-gateway/budget.db"
```

A bbolt store takes an exclusive file lock. On builds after gateway/v0.17.0 the open fails within one second with `another process holds the file lock` and the process exits 1; on v0.17.0 and earlier it blocks startup forever with no log line. Either way each `persist_path` must be a distinct file owned by one process. Two replicas sharing a file never see each other's spend. Backups and offline restore are in [Back up and restore](backup-and-restore.md).

### Run more than one replica

Four Redis addresses exist, each in its own section, each accepting `redis_password_env`, `redis_username` and `redis_tls` (system CA, TLS 1.2 or later):

```yaml
budget:
  redis_addr: "localhost:6379"               # atomic cap check-and-debit across replicas
  # redis_password_env: "REDIS_BUDGET_PASSWORD"
admin:
  redis_addr: "localhost:6379"               # keys created or rotated live hydrate from here on (re)start
config_propagation:
  redis_addr: "localhost:6379"               # live upsert/delete/rotate reach running replicas
  signing_secret_env: "KELVRAN_CONFIG_PROPAGATION_SIGNING_SECRET"   # REQUIRED with redis_addr
rate_limit:
  redis_addr: "localhost:6379"               # optional: RPM and TPM decided in Redis via Lua
```

- Set `budget.redis_addr`, `admin.redis_addr` and `config_propagation.redis_addr` together. Without them a key's effective cap becomes N times `budget_usd` across N replicas, and a live admin mutation stays invisible to the other replicas until each restarts. [`deploy/k8s/README.md`](../../deploy/k8s/README.md) explains why.
- Add `rate_limit.redis_addr` when RPM and TPM must be shared across replicas; otherwise each process keeps its own buckets.
- `config_propagation.signing_secret_env` is required once its `redis_addr` is set; startup fails without it. Events are HMAC-signed and published on `kelvran:config:mutations`. Pub/sub does not replay: re-apply any mutation made during a Redis outage.
- If a section sets both `redis_addr` and `persist_path`, `redis_addr` wins and a warning is logged.
- Per-key concurrency, deployment ceilings, idempotency, prompts and the response cache stay per replica regardless.
- On the request path a Redis failure fails open for rate limiting and budget: the request is admitted and `kelvran.ratelimit.fail_open` or `kelvran.budget.fail_open` increments. An unreachable identity Redis at startup fails closed: the process exits before listening. Row-by-row detail is in [FAILURE-MODES.md](../operations/FAILURE-MODES.md).

### Manage keys live through the admin API

The admin API is off until `admin.token_env` names an environment variable, and that variable must resolve non-empty or the gateway refuses to start. It listens on its own address, `127.0.0.1:8081` by default:

```yaml
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  persist_path: "/var/lib/kelvran-gateway/identity.db"   # or redis_addr, see above
```

Create or replace a key. The body mirrors the YAML names with three exceptions: allow-lists are JSON arrays; `max_concurrent_requests` and `billing_subject_id` are not accepted (an unknown field is silently ignored, so the key gets no concurrency cap and no billing subject); and an upsert is a full replace, never a merge:

```bash
curl -sS -X POST http://127.0.0.1:8081/admin/virtual_keys/team-gamma \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "key_hash": "<64-hex sha256>",
    "budget_usd": 50,
    "budget_reset_interval_seconds": 2592000,
    "budget_warn_percent": 0.8,
    "allowed_models": ["gpt-4o"],
    "rate_limit": {"burst": 20, "refill_per_second": 10, "tpm_capacity": 100000, "tpm_refill_per_second": 1000}
  }'
```

Success is `204`. The handler answers `400` for a name over 256 characters, a negative `budget_usd` or `budget_reset_interval_seconds`, a `budget_warn_percent` outside `[0, 100]`, a per-model entry without positive `burst` and `refill_per_second` or with only one of its two TPM values, an unparsable CIDR, or a malformed `key_hash`. Unlike the YAML loader, it does not check the key-level `tpm_capacity`/`tpm_refill_per_second` pair: a lone `tpm_capacity` is accepted and creates a TPM bucket that never refills, so always send both.

Delete a key. This also purges its budget state, in memory and in the persisted or Redis store:

```bash
curl -sS -X DELETE http://127.0.0.1:8081/admin/virtual_keys/team-gamma \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"     # 204; 404 if unknown; 409 if it is the last key
```

Rotation (`POST /admin/virtual_keys/{name}/rotate` with `new_key_hash` and `grace_period_seconds`) is covered in [Rotate credentials](rotate-credentials.md). The viewer, cost_viewer and operator token tiers are in [Admin API and RBAC](admin-api-rbac.md).

Two restart rules apply once `admin.persist_path` or `admin.redis_addr` is set:

- A persisted admin mutation is authoritative over `config.yaml` on restart. Config is seed state only, and a persisted key with no config entry is added.
- `per_model` and TPM overrides are not persisted for any key that has ever been upserted or rotated through the admin API, including a key declared in `config.yaml`: on restart the persisted record wins and its rate limit is rebuilt from `burst`/`refill_per_second` only, so the config file's `per_model`/`tpm_*` for that key are ignored. Re-apply them with another upsert after every restart.

Every admin mutation is audit-logged unless `admin.enable_audit_log: false`. With `admin.audit_log_path` set, entries also go to a JSONL file readable through `GET /admin/audit`.

### Scope the response cache to end users

`cache_scope_to_end_user: true` on a key partitions its response cache by the caller-supplied `X-Kelvran-End-User-Id` header. When the header is absent the request gets its own private scope. See [Caching](caching.md). `billing_subject_id` is opaque metadata on the decision event and is never read by enforcement.

## Verify it worked

1. The config loads:

   ```bash
   /tmp/kelvran-gateway -validate -config config.yaml; echo "exit $?"
   ```

   Prints `exit 0`. A bad CIDR prints an error naming the key and exits 1; a list-shaped allow-list fails earlier with a parser error giving only the line number (`expected "key: value" or "key:", got "- ..."`). A list item that happens to contain a colon, such as `- gpt-4o: true`, passes and allows a model literally named `- gpt-4o`, so check the `GET /admin/virtual_keys` output in step 2 for stray `- ` prefixes.

2. The gateway knows the key (admin or viewer token):

   ```bash
   curl -sS http://127.0.0.1:8081/admin/virtual_keys -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
   ```

   Returns a JSON array with one entry per key. `id`, `budget_usd`, `budget_reset_interval_seconds` and `budget_warn_percent` are always present; `allowed_models`, `allowed_regions`, `allowed_source_cidrs`, `cache_scope_to_end_user`, `rate_limit_burst`, `rate_limit_refill_per_second` and `billing_subject_id` appear only when set. Key hashes are never included.

3. Spend is tracked (admin, viewer or cost_viewer token):

   ```bash
   curl -sS http://127.0.0.1:8081/admin/virtual_keys/team-alpha/spend -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
   ```

   Returns `{"spent_usd":"...","budget_usd":"100","budget_reset_interval_seconds":2592000,"percent_used":...}`. `percent_used` is `0` when the budget is unlimited.

4. Limits are enforced. Send a request to `POST /v1/chat/completions` with the raw secret as the bearer token and observe the rejection you provoke:

   | Condition | Status | Envelope `code` | `Retry-After` header |
   |---|---|---|---|
   | Wrong or unknown secret | `401` | `invalid_api_key` | no |
   | Model not in `allowed_models` | `403` | `model_not_allowed` | no |
   | Client IP outside `allowed_source_cidrs` | `403` | `source_ip_not_allowed` | yes |
   | RPM or TPM exhausted | `429` | `rate_limit_exceeded` (type `rate_limit_error`) | yes |
   | `max_concurrent_requests` reached | `429` | `concurrency_limit_exceeded` (type `rate_limit_error`) | yes |
   | Budget exhausted | `429` | `insufficient_quota` (type `insufficient_quota`) | no |

   Full envelope shapes are in the [error-code reference](../reference/error-codes.md); counters and log events are in [Metrics and logs](../reference/metrics-and-logs.md).

## Not available today

- Team, organisation or hierarchical budgets. Only flat per-key budgets exist.
- A distributed per-key concurrency limiter. `max_concurrent_requests` is counted per gateway instance even with Redis configured.
- An admin route that resets a key's spend without deleting the key. The levers are `DELETE` then re-create (which also zeroes legitimate spend) or, in Redis budget mode only, `DEL budget:<url.QueryEscape(key id)>` directly in Redis; see row R7 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md).
- Persistence of `per_model` and TPM overrides across restarts for any key ever upserted or rotated through the admin API (config-declared keys included).
- Setting `max_concurrent_requests` or `billing_subject_id` through the admin API. Both are config.yaml-only. An admin upsert of a config-declared key records `max_concurrent_requests` as 0; with `admin.persist_path` or `admin.redis_addr` set, that key's cap is unlimited after the next restart.
- Server-side generation of virtual-key secrets. You generate the secret and supply the hash.
- Propagation of prompt mutations across replicas; only virtual-key and deployment-weight mutations propagate.
- A custom CA or client certificate for Redis connections. `redis_tls` uses the system CA only.
- An OpenAPI spec for the admin API.
- A retention or deletion window for budget-spend history other than the rolling budget window. See [DATA-SUBJECT-REQUESTS.md](../operations/DATA-SUBJECT-REQUESTS.md).

## Related

- [Tutorial: your first virtual key and budget](../tutorials/first-virtual-key-and-budget.md)
- [Config reference](../reference/config.md), [Admin API reference](../reference/admin-api.md), [Error codes](../reference/error-codes.md)
- [Rotate credentials](rotate-credentials.md), [Admin API and RBAC](admin-api-rbac.md), [Back up and restore](backup-and-restore.md)
- [Security model](../explanation/security-model.md) and the original [virtual keys and budgets RFC](../rfcs/2026-09-02-virtual-keys-budgets.md)
