# Configure virtual keys and budgets

This page shows an operator how to issue a Kelvran virtual key, cap its spend, rate-limit it, restrict what it may call, and manage it live through the admin API. It covers the single-process case first, then the knobs that keep keys and budgets correct across more than one gateway replica.

Use this when you need to hand a team or an application its own credential with its own dollar cap and throughput limits, without giving it any upstream provider key.

## Prerequisites

- A gateway binary or image and a `config.yaml` with at least one deployment. Start with the [quickstart](../tutorials/quickstart.md) if you have neither.
- `openssl` and `sha256sum` (or `shasum -a 256` on older macOS) to generate a secret and its hash — or the `kelvran` CLI, whose `keys create` does both (since `gateway/v0.18.0`).
- For the admin-API steps: `curl` and an environment variable holding the admin token, for example `KELVRAN_ADMIN_TOKEN`.
- For the multi-replica variant: a reachable Redis.

Every field named below is described in the [config reference](../reference/config.md); every route is in the [admin API reference](../reference/admin-api.md).

## Steps

### 1. Generate the secret and its hash

The gateway never generates or stores a raw secret. You generate it, hand the raw value to the client, and put only its SHA-256 hash in config.

`kelvran keys create team-alpha --budget 100 --reset monthly --warn 0.8` (since `gateway/v0.18.0`) creates step 2's `team-alpha` live through the admin API when `KELVRAN_ADMIN_TOKEN` is exported; with `--config config.yaml` and no admin token in the environment it writes the same entry into the file for the next start instead ([reference](../reference/kelvran-cli.md#kelvran-keys)). The by-hand path below is the documented `key_hash` contract:

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

- `budget_usd` is a decimal cap. Omitted or `0` means unlimited. The reservation is not request-aware: a single request can overshoot the cap by its own cost — at the tier multiple for a long prompt (`long_context`: 5× on Haiku 5.5, 2× input / 1.5× output on the GPT-6 and GPT-5.6 lines) or for a premium OpenAI `service_tier`; see [Size a budget for long prompts](#size-a-budget-for-long-prompts). Reservations are sized from the key's billing history in both modes: every request before the window's first billed settlement reserves the whole remaining headroom (so a brand-new key's first concurrent burst is admitted one at a time until one of them settles with a billed cost), every later one the window's mean billed cost — in Redis mode too since 2026-10-16 (on `main`, not in `gateway/v0.20.0`; before, Redis mode reserved the whole headroom on every call and a capped key admitted one in-flight request at a time: [FAILURE-MODES.md](../operations/FAILURE-MODES.md) R19).
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
      tokens_per_day: 2000000       # optional: rolling 24-hour token allowance
      max_concurrent_requests: 10   # optional; <= 0 = unlimited
      per_model:
        gpt-4o:
          burst: 5                  # both required and positive
          refill_per_second: 1
          tpm_capacity: 50000       # optional pair
          tpm_refill_per_second: 500
```

- `burst` and `refill_per_second` must both be set or both omitted, and may not be negative. Omitted resolves to the gateway default of 20 burst and 10 per second, on both the YAML and the admin-API path.
- `tpm_capacity` and `tpm_refill_per_second` follow the same pair rule and add a tokens-per-minute dimension. TPM is reserved before the upstream call from past usage (in Redis mode too since gateway/v0.20.0; before, Redis mode reserved the whole capacity, one request per key at a time) and reconciled to real usage afterwards. It is enforced in both in-memory and Redis mode; the Redis path uses GCRA and ships in gateway/v0.14.0 (2026-09-21) and later.
- `tokens_per_day` adds a third dimension: a rolling 24-hour token allowance — a bucket of that capacity refilling at `tokens_per_day/86400` tokens per second, so a key that spends it all at 09:00 is whole again at 09:00 the next day (never a midnight reset). It is checked with RPM and TPM in one rate-limit gate (one Redis call since 2026-10-17, round-5 item 10 phase 2b; a third call before) on the same reserve-then-reconcile path — a bucket with no usage history reserves at most one hour's share of the day (`tokens_per_day/24`), or the request's minute reservation when smaller, so one request never claims the whole day; in Redis mode the usage history expires 24 h after the last settle, in memory it lasts until the key is re-registered — and a refusal reserves nothing: since 2026-10-17 the gate reserves nothing unless all three dimensions admit, so a refused request consumes no RPM token and leaves no reservation in either mode (before, it had consumed the RPM token and returned the TPM reservation at once). Like TPM it counts chat and messages turns only (embeddings and `count_tokens` draw RPM alone) and cache hits release their reservation. A single value, valid alone or beside the TPM pair; negative or non-numeric fails startup; key-level only — written under `per_model` or on a deployment it is a load error, never silently ignored. Its 429 is the same `rate_limit_exceeded`, with `Retry-After` floored on the bucket's own wait until a request of the key's typical size fits again — that reservation's share of the day, `reservation / (tokens_per_day/86400)` seconds, so a 2,000-token request on a 2,000,000-a-day key waits about 86 s — capped at 24 h; the per-key backoff still applies when it is larger, and a refusal decided before the key has any usage history carries the backoff alone. Enforced in both in-memory (per replica) and Redis mode. On `main` since 2026-10-15, not in `gateway/v0.20.0`.
- `max_concurrent_requests` caps the key's simultaneously in-flight requests. It is counted in memory per gateway instance. The concurrency check runs after the RPM, TPM and tokens-per-day checks, so a request rejected for concurrency has already spent one RPM token; its TPM and tokens-per-day reservations are returned when the request finalizes.
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

Per request, after authentication, the gateway checks in this order: source-IP allow-list, model allow-list, idempotency claim, prompt resolution, the rate-limit gate — RPM, TPM, then tokens-per-day in one call, nothing taken unless all three admit (since 2026-10-17; before, three checks and a tokens-per-day refusal returning the TPM reservation) — concurrency slot, budget reservation, then the cache lookup.

## Variants

### Know what a budget cannot see on an `anthropic` passthrough hop

A budget prices every request from the token counts the provider reports, at the deployment's configured rates. Since 2026-10-13 (brainstorm item (i)) the gateway also reads the members Anthropic bills beyond flat tokens and prices each one when the model's `price_table` entry carries the matching key: the 1-hour cache-write slice (`cache_creation_1h_per_token`; on Bedrock, every write of a request that carried a 1h breakpoint the adapter forwarded), server-side web searches (`web_search_per_request`) and US-only inference (`us_inference_multiplier`). Set all three on every Anthropic model an `anthropic` or `bedrock` deployment serves — the example config shows the pricing page's values (2× base input, 0.01, 1.1) — because an absent key keeps the earlier behaviour for that member, which under-counts it.

What a budget still cannot see on an `anthropic` deployment serving `POST /v1/messages` (the request reaches Anthropic as sent since gateway/v0.19.0): fast mode (`speed: "fast"` on Claude Opus 5.5 / 5 / 4.8 is a separate, higher price row) and code execution's container-hours past the monthly allowance (an organisation-level figure no per-request count carries); Claude Haiku 5.5's higher rates once a prompt exceeds 100,000 tokens stopped being a gap on 2026-10-14 (`long_context`, below). Priority tier is not a gap — it is a capacity commitment, not a per-request price — and web fetch is free. Since 2026-10-14 a third gap: `usage.iterations` entries from an advisor or server-side fallback model (`advisor_message`/`fallback_message`) are summed but priced at the serving model's rates; the `usage_iterations_foreign_model` warning names the model and the key. Size such a key's budget with those three in mind, or keep Anthropic-bound traffic on a Bedrock deployment, which has no server tools, no fast mode and no `inference_geo`; its one member is the 1h write, which is priced. Details: `THREAT_MODEL.md`, Tampering row.

On an `openai` deployment (since `gateway/v0.20.0`) a client's `service_tier` is forwarded and the tier the response reports is priced through `price_table.<model>.service_tier_multipliers`: set the map for every OpenAI model (`kelvran init` writes the pricing page's values) or a `fast` request bills at Standard while OpenAI charges 2×; `service_tier_policy: strip` on the deployment closes the lever for keys you do not want choosing a tier. A silent response with a requested premium tier is billed at that tier; a served non-base tier that a priced model's map lacks bills Standard and logs `service_tier_unpriced` (on a silent response a requested tier the map lacks bills Standard with no line; a model with no price row bills $0 and never warns). The budget reservation is not request-aware, so the one-request overshoot past `budget_usd` that a long `max_tokens` call can cause is, for a premium tier, that request's cost at the tier multiple (2× to 2.5× through Chat Completions); for shared or low-cap keys set `service_tier_policy: strip` until the per-key allowed-tiers follow-up lands.

### Size a budget for long prompts

Since 2026-10-14 (round-5 item 5), when a model's `price_table` row carries `long_context` — on any provider: the tier is the model's own schedule — a request whose prompt — cache reads and cache writes included — exceeds `above_tokens` is billed at the tier's two multipliers for every token of the request: Claude Haiku 5.5 over 100,000 tokens at 5× on every token category (the per-search fee is unscaled), the GPT-6 and GPT-5.6 lines over 272K at 2× on the input side and 1.5× on the output. `kelvran init` writes both; a row without the block bills every prompt length at Standard, which under-counts a tiered model. The span attribute `kelvran.pricing.long_context_tier` marks a request billed at the tier. The budget reservation is not request-aware and the mid-stream top-up prices the completion at Standard, so one long-prompt request can overshoot `budget_usd` by its tier multiple; size a key that sends long prompts to Haiku 5.5 with the 5× in mind.

### Keep spend across restarts on one host

Without persistence, spend resets on every restart. For a single process, point the budget tracker at a bbolt file:

```yaml
budget:
  persist_path: "/var/lib/kelvran-gateway/budget.db"
```

A bbolt store takes an exclusive file lock. Since `gateway/v0.18.0` the open fails within one second with `another process holds the file lock` and the process exits 1; on v0.17.0 and earlier it blocks startup forever with no log line. Either way each `persist_path` must be a distinct file owned by one process. Two replicas sharing a file never see each other's spend. Backups and offline restore are in [Back up and restore](backup-and-restore.md).

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
  redis_addr: "localhost:6379"               # optional: RPM, TPM and tokens-per-day decided in Redis via Lua
```

- Set `budget.redis_addr`, `admin.redis_addr` and `config_propagation.redis_addr` together. Without them a key's effective cap becomes N times `budget_usd` across N replicas, and a live admin mutation stays invisible to the other replicas until each restarts. [`deploy/k8s/README.md`](../../deploy/k8s/README.md) explains why.
- Add `rate_limit.redis_addr` when RPM, TPM and tokens-per-day must be shared across replicas; otherwise each process keeps its own buckets (a daily cap enforced per replica admits the cap once per replica). With `budget.redis_addr` on the same Redis (same address, username, resolved password and TLS) the rate limiter, the budget and the ledger share one client (on `main` since 2026-10-23, not in `gateway/v0.20.0`), and a chat completion's budget, token and ledger writes after the call are one Redis command (on `main` since 2026-10-30, not in `gateway/v0.20.0`).
- `config_propagation.signing_secret_env` is required once its `redis_addr` is set; startup fails without it. Events are HMAC-signed and published on `kelvran:config:mutations`. Pub/sub does not replay: re-apply any mutation made during a Redis outage.
- If a section sets both `redis_addr` and `persist_path`, `redis_addr` wins and a warning is logged.
- Per-key concurrency, deployment ceilings, idempotency, prompts and the response cache stay per replica regardless.
- On the request path a Redis failure fails open for rate limiting and budget: the request is admitted and `kelvran.ratelimit.fail_open` or `kelvran.budget.fail_open` increments. An unreachable identity Redis at startup fails closed: the process exits before listening. Row-by-row detail is in [FAILURE-MODES.md](../operations/FAILURE-MODES.md).

### See what a key spent, by model, tool or session

Since item 13d every billable call is recorded in the spend ledger. With the admin listener on and a token that may read spend (Admin, Viewer, or CostViewer with `key=`):

```bash
curl -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  "$ADMIN/admin/spend?group_by=model&key=team-alpha&since=2026-10-10T00:00:00Z&until=2026-10-11T00:00:00Z"
```

Rows come sorted by spend, highest first, `spend_usd` as a decimal string. `group_by=session` shows the Claude Code session ids (or the `agent_run_id` Baggage member) a key ran; a session named `other` is what the caps folded. The window is clamped to `ledger.retention_hours` (168 h by default) — the ledger's own window; the per-request decision log line keeps whatever retention its log sink gives it ([SECURITY.md](../../SECURITY.md)). See [GET /admin/spend](../reference/admin-api.md#get-adminspend) and the [`ledger` section](../reference/config.md#ledger) of the configuration reference.

### Manage keys live through the admin API

The admin API is off until `admin.token_env` names an environment variable, and that variable must resolve non-empty or the gateway refuses to start. It listens on its own address, `127.0.0.1:8081` by default:

```yaml
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  persist_path: "/var/lib/kelvran-gateway/identity.db"   # or redis_addr, see above
```

Create or replace a key. The body mirrors the YAML names with three exceptions: allow-lists are JSON arrays; `max_concurrent_requests` is not accepted (an unknown field is silently ignored, so the key gets no concurrency cap; `billing_subject_id` and `expires_at` are accepted since `gateway/v0.18.0`); and an upsert is a full replace, never a merge:

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
    "rate_limit": {"burst": 20, "refill_per_second": 10, "tpm_capacity": 100000, "tpm_refill_per_second": 1000, "tokens_per_day": 2000000}
  }'
```

Success is `204`. The handler answers `400` for a name over 256 characters, a negative `budget_usd` or `budget_reset_interval_seconds`, a `budget_warn_percent` outside `[0, 100]`, a per-model entry without positive `burst` and `refill_per_second` or with only one of its two TPM values, a negative `tokens_per_day`, an unparsable CIDR, or a malformed `key_hash`. Unlike the YAML loader, it does not check the key-level `tpm_capacity`/`tpm_refill_per_second` pair: a lone `tpm_capacity` is accepted and creates a TPM bucket that never refills, so always send both.

Delete a key. This also purges its budget state, in memory and in the persisted or Redis store:

```bash
curl -sS -X DELETE http://127.0.0.1:8081/admin/virtual_keys/team-gamma \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"     # 204; 404 if unknown; 409 if it is the last key
```

Rotation (`POST /admin/virtual_keys/{name}/rotate` with `new_key_hash` and `grace_period_seconds`) is covered in [Rotate credentials](rotate-credentials.md). The viewer, cost_viewer and operator token tiers are in [Admin API and RBAC](admin-api-rbac.md).

Two restart rules apply once `admin.persist_path` or `admin.redis_addr` is set:

- A persisted admin mutation is authoritative over `config.yaml` on restart. Config is seed state only, and a persisted key with no config entry is added.
- `per_model` and TPM overrides are not persisted for any key that has ever been upserted or rotated through the admin API, including a key declared in `config.yaml`: on restart the persisted record wins and its rate limit is rebuilt from `burst`/`refill_per_second` only, so the config file's `per_model`/`tpm_*`/`tokens_per_day` for that key are ignored. Re-apply them with another upsert after every restart.

Every admin mutation is audit-logged unless `admin.enable_audit_log: false`. With `admin.audit_log_path` set, entries also go to a JSONL file readable through `GET /admin/audit`.

### Keep Claude Code identifiers off a key's spans

Every request is attributed by the client tool normalised from `User-Agent` and by the `x-claude-code-*` headers Claude Code sends (since `gateway/v0.18.0`). The bounded values (client tool, request class) are always recorded; the identifiers (session, agent, parent-agent and prompt ids, agent type) land on the request span by default. `attribution_capture_ids: false` on a key keeps them off that key's spans; the top-level `attribution.capture_ids: false` does it for every key. A request that fails authentication never carries identifiers. See [the configuration reference](../reference/config.md) and [metrics and logs](../reference/metrics-and-logs.md).

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

   Returns a JSON array with one entry per key. `id`, `budget_usd`, `budget_reset_interval_seconds` and `budget_warn_percent` are always present; `allowed_models`, `allowed_regions`, `allowed_source_cidrs`, `cache_scope_to_end_user`, `rate_limit_burst`, `rate_limit_refill_per_second`, `billing_subject_id`, `attribution_capture_ids_disabled` and `expires_at` appear only when set. Key hashes are never included.

3. Spend is tracked (admin, viewer or cost_viewer token). `kelvran spend` lists every key's spend as a table (since `gateway/v0.18.0`); the per-key route by hand:

   ```bash
   curl -sS http://127.0.0.1:8081/admin/virtual_keys/team-alpha/spend -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN"
   ```

   Returns `{"spent_usd":"...","budget_usd":"100","budget_reset_interval_seconds":2592000,"percent_used":...}`. `percent_used` is `0` when the budget is unlimited; `spent_usd` is tracked for an unlimited key in every mode (in Redis mode since `gateway/v0.20.0`), and the spend ledger, `kelvran spend --by model --key team-alpha`, has the same key's spend by model, tool or session.

4. Limits are enforced. Send a request to `POST /v1/chat/completions` with the raw secret as the bearer token and observe the rejection you provoke:

   | Condition | Status | Envelope `code` | `Retry-After` header |
   |---|---|---|---|
   | Wrong or unknown secret | `401` | `invalid_api_key` | no |
   | Expired key (`expires_at` passed) | `401` | `key_expired` | no |
   | Model not in `allowed_models` | `403` | `model_not_allowed` | no |
   | Client IP outside `allowed_source_cidrs` | `403` | `source_ip_not_allowed` | yes |
   | RPM, TPM or tokens-per-day exhausted | `429` | `rate_limit_exceeded` (type `rate_limit_error`) | yes (for tokens-per-day, the day bucket's own wait, up to 24 h) |
   | `max_concurrent_requests` reached | `429` | `concurrency_limit_exceeded` (type `rate_limit_error`) | yes |
   | Budget exhausted | `429` | `insufficient_quota` (type `insufficient_quota`) | no |

   Full envelope shapes are in the [error-code reference](../reference/error-codes.md); counters and log events are in [Metrics and logs](../reference/metrics-and-logs.md).

## Not available today

- Team, organisation or hierarchical budgets. Only flat per-key budgets exist.
- A distributed per-key concurrency limiter. `max_concurrent_requests` is counted per gateway instance even with Redis configured.
- An admin route that resets a key's spend without deleting the key. The levers are `DELETE` then re-create (which also zeroes legitimate spend) or, in Redis budget mode only, `DEL budget:<url.QueryEscape(key id)>` directly in Redis; see row R7 of [FAILURE-MODES.md](../operations/FAILURE-MODES.md).
- Persistence of `per_model`, TPM and `tokens_per_day` overrides across restarts for any key ever upserted or rotated through the admin API (config-declared keys included).
- Setting `max_concurrent_requests` through the admin API. It is config.yaml-only (`billing_subject_id` and `expires_at` are body fields since `gateway/v0.18.0`). An admin upsert of a config-declared key records `max_concurrent_requests` as 0; with `admin.persist_path` or `admin.redis_addr` set, that key's cap is unlimited after the next restart.
- Server-side generation of virtual-key secrets. You generate the secret and supply the hash.
- Propagation of prompt mutations across replicas; only virtual-key and deployment-weight mutations propagate.
- A custom CA or client certificate for Redis connections. `redis_tls` uses the system CA only.
- A hosted or rendered copy of the admin API's OpenAPI description (the document `gateway/openapi/kelvran-gateway.openapi.yaml` covers it; [reference](../reference/openapi.md)).
- A retention or deletion window for budget-spend history other than the rolling budget window. See [DATA-SUBJECT-REQUESTS.md](../operations/DATA-SUBJECT-REQUESTS.md).

## Related

- [Tutorial: your first virtual key and budget](../tutorials/first-virtual-key-and-budget.md)
- [Config reference](../reference/config.md), [Admin API reference](../reference/admin-api.md), [Error codes](../reference/error-codes.md)
- [Rotate credentials](rotate-credentials.md), [Admin API and RBAC](admin-api-rbac.md), [Back up and restore](backup-and-restore.md)
- [Security model](../explanation/security-model.md) and the original [virtual keys and budgets RFC](../rfcs/2026-09-02-virtual-keys-budgets.md)
