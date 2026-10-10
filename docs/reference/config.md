# Configuration reference: `config.yaml`

This page lists every key the gateway's configuration parser reads from `config.yaml`: its type, default, validation rule, and the subsystem that consumes it, grouped by top-level section, followed by the parser's own constraints, the CLI flags that act on the file, and what refuses to start. It is for operators writing or reviewing a gateway configuration and for anyone checking what a key does before relying on it. The authoritative key list is the parser in `gateway/internal/gateway/controlplane/config.go`; the annotated example is [`gateway/config.example.yaml`](../../gateway/config.example.yaml), which omits several keys listed here.

The page describes the parser as of gateway/v0.18.0. The latest release is gateway/v0.18.0 (2026-10-10). The items that first shipped in gateway/v0.18.0 are marked where they appear. Everything else is also in gateway/v0.17.0.

## Type vocabulary

The parser keeps every scalar as its raw source text and converts it per field. The types below name the conversion that field applies.

| Type | Conversion | Malformed value |
|---|---|---|
| `string` | Raw text; a single layer of matching `"…"` or `'…'` quotes is removed | n/a |
| `bool` | Exactly one of `true`, `True`, `TRUE`, `false`, `False`, `FALSE` | Load error (`yes`, `no`, `on`, `off`, `1`, `0` are strings, not booleans) |
| `int` | `strconv.Atoi` on the raw text | Treated as unset (0). A value that overflows a 64-bit integer is also treated as unset, not clamped |
| `float` | `strconv.ParseFloat` on the raw text | Treated as unset (0), including out-of-range literals such as `1e400`, except `tpm_accounting.output_token_multiplier`, where a present-but-unparseable value is a load error |
| `decimal` | `decimal.NewFromString` on the raw text; never routed through a float | Treated as unset, except where the table says the key is required |
| `mapping` | A nested block of `key: value` lines indented two more spaces | A scalar where a mapping is expected is a load error only for the required `virtual_keys` and `deployments` sections and for a named entry under `virtual_keys`, `deployments`, `price_table`, `models` or `per_model` (`… must be a mapping`); every other mapping-typed key given a scalar (`telemetry: otlp`, `rate_limit: 5`, `tls: yes`) is silently ignored, its defaults apply, and `-validate` reports `config is valid`. A mapping where a scalar is expected is treated as unset, except that a required key, a `bool` field, a `fallback_chains` value or a `category_overrides` value is a load error |
| `name → bool` | A mapping whose values must each be a `bool`; only `true` entries are kept | Load error on any non-boolean value |
| `csv string` | A `string` split on `,`, each part trimmed, empty parts dropped; order is preserved | n/a |

"Treated as unset" means the field keeps its zero value and the default in its table row applies. The parser does not report the malformed value.

## File format and parser rules

The gateway parses `config.yaml` with a hand-rolled YAML subset, not a YAML library. The rules below are exact.

| Rule | Behaviour |
|---|---|
| Supported syntax | `key: value` scalars and nested mappings only. Indentation is two spaces per level |
| Lists | Not supported. Every naturally list-shaped field is a mapping keyed by name (`virtual_keys`, `deployments`, `allowed_models`, `allowed_regions`, `allowed_source_cidrs`, `category_overrides`, `price_table`, `models`, `per_model`), and ordered sequences are `csv string` values (`fallback_chains`) |
| Flow style, anchors, aliases, multi-line strings | Not supported |
| Comments | `#` starts a comment anywhere on a line, including inside a quoted value. A value cannot contain a literal `#`; the text from `#` onward is dropped |
| Tab in leading whitespace | Load error (`leading tab characters are not valid YAML indentation`) |
| Duplicate key at one nesting level | Load error (`duplicate key … at this nesting level`). This includes two deployments or two virtual keys with the same name |
| Nesting depth | More than 64 nested mapping levels is a load error |
| Quoted keys | A key may be quoted; a colon inside the quotes does not split the key (for example `"10.0.0.0/8": true`, `"my:deployment":`) |
| Unknown keys | Silently ignored at every level. A misspelled key configures nothing and produces no warning. There is no strict mode |
| Boolean fields | Only the six literals in the type table are booleans. A present-but-unrecognised value (`sticky: yes`) is a load error on every `bool` field |
| Numeric fields | Converted per field (see the type vocabulary). A malformed or overflowing number is treated as unset, not reported, except `tpm_accounting.output_token_multiplier` (present but unparseable is a load error) and the two required `price_table` decimals |
| Blank lines | Ignored |

## Load order and required keys

`Load` reads the whole file, then processes sections in this order: `listen_addr`, `virtual_keys`, `deployments`, embedding-group consistency, `telemetry`, `budget`, `prompt`, `rate_limit`, `alerting`, `config_propagation`, `cache`, `guardrails`, `admin`, `health_probe`, `credential_reload`, `price_table`, `models`. The first validation failure stops the load. `virtual_keys` and `deployments` are sorted by name after parsing, so file order never matters.

| Key | Required | Rule |
|---|---|---|
| `listen_addr` | Yes | Non-empty `string` |
| `virtual_keys` | Yes | `mapping` with at least one entry |
| `deployments` | Yes | `mapping` with at least one entry |
| Every other top-level key | No | Absent sections leave their defaults in place |

`-validate` (see [CLI flags](#cli-flags)) runs `Load` plus two cross-deployment checks: every `provider` must be a registered adapter name, and every `fallback_chains` target must be a configured deployment. The same two checks run at normal startup as the first step of pipeline construction, after `Load` and telemetry initialisation.

## Top-level keys

| Key | Type | Default | Meaning | Read by |
|---|---|---|---|---|
| `listen_addr` | `string` | required | Address the client-facing HTTP server binds to, for example `":8080"` | `cmd/gateway` |
| `virtual_keys` | `mapping` | required | Tenant credentials, one entry per key name. See [virtual_keys](#virtual_keysname) | identity, rate limit, budget |
| `deployments` | `mapping` | required | Upstream routes, one entry per deployment name. See [deployments](#deploymentsname) | router, dataplane |
| `price_table` | `mapping` | empty | Per-model USD prices. See [price_table](#price_tablemodel) | cost accounting |
| `models` | `mapping` | absent | Display metadata for `GET /v1/models`. See [models](#modelsmodel). Since `gateway/v0.18.0` | `GET /v1/models` |
| `telemetry` | `mapping` | stdout exporter | See [telemetry](#telemetry) | telemetry |
| `attribution` | `mapping` | identifier capture on | See [attribution](#attribution). Since `gateway/v0.18.0` | attribution middleware, dataplane |
| `budget` | `mapping` | in-memory | See [budget](#budget) | budget tracker |
| `prompt` | `mapping` | in-memory | See [prompt](#prompt) | prompt store |
| `rate_limit` | `mapping` | in-memory | See [rate_limit](#rate_limit) | per-key limiter |
| `alerting` | `mapping` | off | See [alerting](#alerting) | alert notifier |
| `config_propagation` | `mapping` | off | See [config_propagation](#config_propagation) | config propagation |
| `cache` | `mapping` | defaults | See [cache](#cache) | L1/L2/L3 caches |
| `guardrails` | `mapping` | default policy | See [guardrails](#guardrails) | guardrail engine |
| `admin` | `mapping` | admin server off | See [admin](#admin) | admin HTTP server, identity store |
| `health_probe` | `mapping` | probing off | See [health_probe](#health_probe) | router health |
| `credential_reload` | `mapping` | 60 s | See [credential_reload](#credential_reload) | credential reload loops |

## `virtual_keys.<name>`

Each entry is a tenant credential issued by Kelvran. The entry name is the key's ID. Clients send the raw secret as `Authorization: Bearer <secret>` (or, on `GET /v1/models`, as `x-api-key: <secret>`); the file holds only the SHA-256 hash of that secret.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `key_hash` | `string` | required | Hex-encoded SHA-256 digest of the raw secret (64 hex characters, case-insensitive). Empty or absent is a load error. A value that is not valid hex, or does not decode to 32 bytes, passes `-validate` and fails startup (`constructing identity verifier: … key_hash …`) |
| `budget_usd` | `decimal` | 0 (unlimited) | Cumulative spending cap in USD. 0 or absent means no cap |
| `budget_reset_interval_seconds` | `int` | 0 | 0 keeps `budget_usd` a lifetime cap. A positive value makes it a rolling window of that many seconds (2592000 for 30 days) |
| `budget_warn_percent` | `float` | 0 (off) | Fraction of `budget_usd` (for example `0.8`) at or above which every billable completion logs `budget_warn_threshold_crossed`; when `alerting.webhook_url_env` is set, one `budget_warn_threshold_crossed` webhook event is also sent per budget window. Never blocks. Meaningless when `budget_usd` is 0 |
| `rate_limit` | `mapping` | gateway default | See the next table |
| `allowed_models` | `name → bool` | empty (all models) | Canonical model names this key may call. A request for a model not in a non-empty list is rejected before routing |
| `allowed_regions` | `name → bool` | empty (no constraint) | Deployment `region` values this key may be routed to. When non-empty, a deployment with no `region` never satisfies the constraint (fails closed), which today means every non-Bedrock deployment unless it sets `region` |
| `allowed_source_cidrs` | `name → bool` | empty (no constraint) | CIDR blocks the client source IP must fall within (`"10.0.0.0/8"`, `"203.0.113.4/32"`). Each `true` entry must parse with `net.ParseCIDR` at load time; a `false` entry is not syntax-checked |
| `billing_subject_id` | `string` | empty | Opaque external billing identifier. Never read by an enforcement path |
| `cache_scope_to_end_user` | `bool` | `false` | Folds the caller's `X-Kelvran-End-User-Id` header into this key's L1/L2 cache partition |
| `attribution_capture_ids` | `bool` | `true` | `false` keeps this key's requests from carrying the Claude Code identifiers (session, agent, parent-agent, prompt ids, agent type) onto the request span; the bounded attribution fields (client tool, request class) are always captured. Identifiers are only ever recorded for authenticated requests. A non-boolean value fails startup. Since `gateway/v0.18.0` |
| `expires_at` | `string` | absent (never expires) | RFC 3339 instant (for example `"2099-01-01T00:00:00Z"`, quoted or bare) from which the gateway rejects this key with 401 `key_expired`, inclusive. A value that is not RFC 3339 (a date alone, an epoch integer, an empty string) is a load error `-validate` detects; a past instant loads, so an already-expired key never stops the gateway. Since `gateway/v0.18.0` |

### `virtual_keys.<name>.rate_limit`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `burst` | `float` | 20 | Token-bucket capacity for requests. Must be set together with `refill_per_second` or not at all; neither may be negative. When both are 0 or absent the gateway applies its default of 20 burst / 10 per second |
| `refill_per_second` | `float` | 10 | Requests added to the bucket per second. Pair rule as above |
| `tpm_capacity` | `float` | 0 (no TPM limit) | Tokens-per-minute bucket capacity for this key, in raw tokens. Must be set together with `tpm_refill_per_second` or not at all; neither may be negative |
| `tpm_refill_per_second` | `float` | 0 | Tokens added per second. Pair rule as above |
| `max_concurrent_requests` | `int` | 0 (unlimited) | Maximum in-flight requests for this key. Values at or below 0 mean unlimited. Enforced in memory per replica regardless of `rate_limit.redis_addr` |
| `per_model` | `mapping` | absent | Per-model overrides, one entry per canonical model name. See the next table |

The key-level RPM and TPM buckets, and the `per_model` RPM and TPM buckets, are enforced in both in-memory mode and Redis mode (`rate_limit.redis_addr`).

### `virtual_keys.<name>.rate_limit.per_model.<model>`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `burst` | `float` | required | Must be greater than 0, else load error. There is no fallback default for a per-model entry |
| `refill_per_second` | `float` | required | Must be greater than 0, else load error |
| `tpm_capacity` | `float` | 0 (use the key-level TPM bucket) | Optional per-model TPM override. Must be set together with `tpm_refill_per_second` or not at all; neither may be negative |
| `tpm_refill_per_second` | `float` | 0 | Pair rule as above |

## `deployments.<name>`

Each entry routes one canonical, client-facing `model` to one provider endpoint. Several deployments may share a `model`; the router weights across them. The entry name is the deployment's ID, referenced by `fallback_chains` and by the admin API.

### Identity and routing

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `model` | `string` | required | Canonical model name clients send. Missing is a load error |
| `provider` | `string` | required | Adapter name. Must be one of `openai`, `anthropic`, `gemini`, `bedrock`, `openaicompat`; any other value fails `-validate` and startup |
| `upstream_model` | `string` | required | Provider-side model identifier sent on the wire |
| `base_url` | `string` | required | Full upstream endpoint URL. Must parse as a URL and use the `https` scheme unless `allow_insecure_http` is `true` |
| `allow_insecure_http` | `bool` | `false` | Permits a non-`https` `base_url` for this one deployment |
| `region` | `string` | empty | AWS region for SigV4 signing. Required when `provider` is `bedrock`. Also the value `allowed_regions` matches against for any provider |
| `weight` | `int` | 0 (resolves to 1) | Routing share among deployments of the same `model`. Negative is a load error |
| `cost_tier` | `int` | 0 (no tier) | Operator-assigned cost band, lower is cheaper. Negative is a load error. Tier preference activates for a model only when every deployment of that model has a non-zero tier |
| `sticky` | `bool` | `false` | Marks this deployment as the canary side of a stable/canary pair within its model group; the same virtual key is deterministically kept on the same side |
| `kind` | `string` | `"chat"` | `""` or `chat` for `/v1/chat/completions`; `embedding` for `/v1/embeddings`. `embedding` is allowed only with `provider` `openai` or `bedrock`. Any other value is a load error. Two `embedding` deployments sharing one `model` must have the same `provider` and `upstream_model` |
| `disable_cache_control_auto_populate` | `bool` | `false` | Opts this deployment out of adding a cache-control marker to an unmarked system message. Only the `anthropic` and `bedrock` adapters read the result |
| `shared_across_tenants` | `bool` | `false` | Declares the upstream credential is shared by more than one tenant. When `true`, cache-control auto-populate is forced off regardless of the key above |
| `anthropic_beta_policy` | `string` | `"strip"` | What a `bedrock` deployment does with the `anthropic-beta` values a `/v1/messages` request carries once that route is served (RFC-1 §5): `strip` drops them; `forward_known` forwards only the live-proven allow-list (`adapter.BedrockForwardKnownAnthropicBetas`: `claude-code-20250219`, `thinking-token-count-2026-05-13`, `context-management-2025-06-27`, `interleaved-thinking-2025-05-14`, each a Converse `200` on 2026-10-10; `prompt-caching-scope-2026-01-05` is rejected by Converse and never forwarded). Any other value is a load error; `forward_known` on a non-`bedrock` deployment is a load error (`anthropic` forwards every `anthropic-*` header verbatim, the other providers have no beta transport) |
| `accept_lossy_anthropic_ingress` | `bool` | `false` | Lets a non-`anthropic` deployment serve a `/v1/messages` request whose body carries members the canonical schema cannot hold, dropping them (RFC-1 §6, decision Q5). `false` makes this deployment ineligible for such a request; when no deployment in the model's pool is eligible the request is a `400` (`lossy_ingress_rejected`) once the route is served: the message names the top-level unknown members (plain identifiers only) and counts the rest, and `param` carries every member's JSON pointer (cut on a pointer boundary at 512 bytes with a trailing `…+N` when a body carries that many). `kelvran doctor` warns about every non-`anthropic` chat deployment that leaves it `false` |
| `fallback_chains` | `mapping` | absent (router single fallback) | See [fallback_chains](#deploymentsnamefallback_chains) |
| `tls` | `mapping` | absent (shared transport) | See [tls](#deploymentsnametls) |
| `rate_limit` | `mapping` | absent (no ceiling) | See [deployment rate_limit](#deploymentsnamerate_limit) |

### Credentials

The file never holds a secret value. Each credential key names either an environment variable (`*_env`) or a file path (`*_file`). When both are set for one credential the `*_file` value wins and the `*_env` sibling is not consulted.

| Key | Type | Required when | Meaning |
|---|---|---|---|
| `api_key_env` | `string` | `provider` is not `bedrock`, unless `api_key_file` is set | Name of the environment variable holding the provider API key. Resolved once at startup |
| `api_key_file` | `string` | alternative to `api_key_env` | Path to a file holding the provider API key. Re-read every `credential_reload.interval_seconds` |
| `access_key_id_env` | `string` | `provider` is `bedrock`, unless `access_key_id_file` is set | Environment variable holding the AWS access key ID |
| `access_key_id_file` | `string` | alternative to `access_key_id_env` | File holding the AWS access key ID; hot-reloaded |
| `secret_access_key_env` | `string` | `provider` is `bedrock`, unless `secret_access_key_file` is set | Environment variable holding the AWS secret access key |
| `secret_access_key_file` | `string` | alternative to `secret_access_key_env` | File holding the AWS secret access key; hot-reloaded |
| `session_token_env` | `string` | never | Environment variable holding an AWS session token for STS credentials |
| `session_token_file` | `string` | never | File holding the AWS session token; hot-reloaded |

Validation: a `bedrock` deployment needs an access key ID source, a secret access key source, and `region`, else load error (the error text names only the `*_env` spellings; the check accepts either). Any other provider needs `api_key_env` or `api_key_file`, else load error.

Resolution: an environment variable that resolves empty at startup logs a warning; calls to that deployment fail until the process is restarted with the variable set. A `*_file` path that cannot be read at startup logs `credential file could not be read or is empty; calls will fail` (`credential file could not be read; calls will fail` in gateway/v0.17.0), the deployment starts with an empty credential, and the reload loop retries every interval. A `*_file` that is empty or whitespace-only is treated the same as unreadable; this rule applies since `gateway/v0.18.0`. On a reload tick that fails, the last-known-good value is kept. `-validate` never opens credential files or reads environment variables.

### `deployments.<name>.tls`

Gives this one deployment its own HTTP transport. Paths point at PEM files; no certificate material is inline. The files are read once at startup; a path that cannot be read or parsed is a startup failure (`deployment "<name>" tls config: …`), not a load error, so it passes `-validate`.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `ca_cert_path` | `string` | empty | CA bundle used to verify the upstream's certificate |
| `client_cert_path` | `string` | empty | Client certificate presented for mTLS. Must be set together with `client_key_path` |
| `client_key_path` | `string` | empty | Private key for `client_cert_path`. Must be set together with `client_cert_path` |

A `tls` block with none of the three keys set is a load error. `ca_cert_path` alone, `client_cert_path` + `client_key_path` alone, or all three together are valid.

### `deployments.<name>.fallback_chains`

Maps an error class to an ordered list of other deployment names to try when a call to this deployment fails with that class. Values are `csv string`, not YAML lists.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `content_policy` | `csv string` | absent | Deployments to try after a content-policy rejection, in order |
| `context_window_exceeded` | `csv string` | absent | Deployments to try after a context-window error, in order |
| `generic` | `csv string` | absent | Deployments to try after any other error, in order |

Any other key under `fallback_chains` is a load error. A value that is not a string is a load error. Every named target must be a configured deployment; this is checked by `-validate` and at startup, not by `Load` alone. A deployment with no `fallback_chains` block keeps the router's single-fallback behaviour.

### `deployments.<name>.rate_limit`

A deployment-scoped ceiling, aggregated across every virtual key that reaches this deployment, including fallback hops. Unlike a virtual key's bucket, 0 here means no ceiling, never the gateway default. All deployment buckets are in memory per replica regardless of `rate_limit.redis_addr`.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `burst` | `float` | 0 (no ceiling) | Token-bucket capacity for requests against this deployment. Must be set together with `refill_per_second` or not at all; neither may be negative |
| `refill_per_second` | `float` | 0 | Pair rule as above |
| `tpm_capacity` | `float` | 0 (no ceiling) | Tokens-per-minute bucket capacity, enforced per hop with reserve-then-reconcile bookkeeping (enforced since gateway/v0.17.0; parsed but not enforced before it). Pair rule with `tpm_refill_per_second` |
| `tpm_refill_per_second` | `float` | 0 | Pair rule as above |
| `tpm_accounting` | `mapping` | absent (raw tokens) | See the next table. Requires `tpm_capacity` to be set, else load error |
| `max_concurrent_requests` | `int` | 0 (unlimited) | Maximum in-flight requests against this deployment across all keys, checked per hop |

### `deployments.<name>.rate_limit.tpm_accounting`

Makes the deployment TPM ceiling count tokens the way the provider's quota does. The weighted count for one call is `PromptTokens - CacheReadTokens (if excluded) + CompletionTokens * output_token_multiplier`. The per-virtual-key TPM dimension is never weighted.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `output_token_multiplier` | `float` | 0 (means 1) | Weight applied to completion tokens. Must be `>= 1` when set; a value in `(0, 1)` or below 0 is a load error. A present-but-unparseable value is a load error |
| `exclude_cache_read_tokens` | `bool` | `false` | Subtracts cache-read tokens from the prompt count |

## `price_table.<model>`

USD price per token, keyed by canonical model name. Every deployment sharing a `model` is billed at that one price. Values are parsed as `decimal` from their source text.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `prompt_per_token` | `decimal` | required | Price of one prompt token. Missing or malformed is a load error; negative is a load error |
| `completion_per_token` | `decimal` | required | Price of one completion token. Same rules |
| `cache_read_per_token` | `decimal` | absent (priced at `prompt_per_token`) | Price of one cache-read token. Negative is a load error |
| `cache_creation_per_token` | `decimal` | absent (priced at `prompt_per_token`) | Price of one cache-write token. Negative is a load error |

An entry that is not a mapping is a load error. A model with no entry is not a load error and produces no warning: every call to that model is costed at `0`, so `budget_usd` is never consumed and `budget_warn_percent` never fires for it.

## `models.<model>`

First shipped in `gateway/v0.18.0`. Display metadata for `GET /v1/models`, keyed by canonical model name.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `display_name` | `string` | empty (the route uses the model id) | Human-readable name |
| `description` | `string` | empty | Free-text description |

The model name must match some deployment's `model`, else load error. Any key other than `display_name` and `description` inside an entry is a load error. An entry that is not a mapping is a load error.

## `telemetry`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `exporter` | `string` | `"stdout"` | One of `stdout`, `otlp`, `none`. Empty or absent means `stdout`. Any other value fails startup (`unknown exporter`) |
| `otlp_endpoint` | `string` | empty | Read only when `exporter` is `otlp`. A `host:port` value; the scheme defaults to HTTPS. Set the environment variable `OTEL_EXPORTER_OTLP_INSECURE=true` for a plain-HTTP collector |

Exporter construction never dials, so an unreachable collector does not stop startup. Details in [TELEMETRY.md](../operations/TELEMETRY.md) and [metrics-and-logs.md](metrics-and-logs.md).

## `attribution`

First shipped in `gateway/v0.18.0`. The whole section is optional; when present it must be a mapping: `attribution: false`, `attribution: {}` and `attribution: null` all fail startup (the configuration's YAML subset has no flow style and no null, so each is read as a scalar), while a bare `attribution:` line with no keys means the defaults.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `capture_ids` | `bool` | `true` | `false` keeps every request from carrying the Claude Code identifiers (`x-claude-code-session-id`, `-agent-id`, `-parent-agent-id`, `-prompt-id`, `-agent-type`) onto the request span. The bounded attribution values — the normalised `User-Agent` client tool and the request class — are always captured as metric dimensions and log fields (normalised to `other` / `none` when absent) and as span attributes when present (the request class reaches the span only when its header was sent). A request that fails authentication never carries identifiers, whatever this switch says. A non-boolean value fails startup. Per-key opt-out: `virtual_keys.<name>.attribution_capture_ids`. |

See [the attribution and spend-ledger RFC](../rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md) and [metrics-and-logs.md](metrics-and-logs.md).

## `budget`

Durability for virtual-key spend. With neither key set, spend is in memory and resets on restart.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `persist_path` | `string` | empty (in-memory) | Path of the bbolt file for single-process durability |
| `redis_addr` | `string` | empty | Redis `host:port` for a cross-replica budget backend. When both keys are set, `redis_addr` wins and a warning (`budget_redis_addr_and_persist_path_both_set`) is logged |
| `redis_password_env`, `redis_username`, `redis_tls` | see [Redis auth keys](#redis-auth-keys) | | |

The Redis client dials lazily; an unreachable Redis does not stop startup. Opening the bbolt file follows the rules in [Persistence files](#persistence-files).

## `prompt`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `persist_path` | `string` | empty (in-memory) | Path of the bbolt file holding prompt templates. Empty means templates revert on restart |

## `rate_limit`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `redis_addr` | `string` | empty (in-memory) | Redis `host:port`. When set, the per-virtual-key RPM, TPM and `per_model` buckets are Redis-backed and shared across replicas. Deployment ceilings and `max_concurrent_requests` stay in memory per replica |
| `redis_password_env`, `redis_username`, `redis_tls` | see [Redis auth keys](#redis-auth-keys) | | |

The Redis client dials lazily; an unreachable Redis does not stop startup.

## `alerting`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `webhook_url_env` | `string` | empty (off) | Name of the environment variable holding the webhook URL. Required for the notifier to activate. If the variable resolves empty, the gateway logs `alerting_webhook_url_env_unset` and runs with alerting disabled; this is not a startup failure |
| `signing_secret_env` | `string` | empty (unsigned) | Name of the environment variable holding an HMAC-SHA256 signing secret (Standard Webhooks). A value prefixed `whsec_` is base64-decoded after the prefix; any other value is used as raw bytes and logged once as non-compliant |

## `config_propagation`

Push-based propagation of admin mutations between replicas over Redis.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `redis_addr` | `string` | empty (off) | Redis `host:port` every replica shares. Deliberately separate from `rate_limit.redis_addr` |
| `signing_secret_env` | `string` | required when `redis_addr` is set | Name of the environment variable holding the shared HMAC secret. If `redis_addr` is set and this key is missing, or the variable resolves empty, the process refuses to start |
| `redis_password_env`, `redis_username`, `redis_tls` | see [Redis auth keys](#redis-auth-keys) | | |

## `cache`

Three in-process cache layers. Every layer is capacity-bounded; there is no unbounded mode. Jitter is added on top of the TTL and cannot be disabled: a value at or below 0 resolves to the 10 % default.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `ttl_seconds` | `int` | 300 | L1 exact-match TTL. At or below 0 means default |
| `max_entries` | `int` | 10000 | L1 capacity. At or below 0 means default |
| `jitter_fraction` | `float` | 0.10 | Fraction of TTL added as random jitter. At or below 0 means default |
| `l2` | `mapping` | defaults | See the next table |
| `l3` | `mapping` | defaults | See the next table |

### `cache.l2` and `cache.l3`

| Key | Type | L2 default | L3 default | Meaning |
|---|---|---|---|---|
| `ttl_seconds` | `int` | 75 | 300 | Layer TTL. At or below 0 means default |
| `max_entries` | `int` | 10000 | 10000 per tenant | Layer capacity. At or below 0 means default |
| `jitter_fraction` | `float` | 0.10 | 0.10 | At or below 0 means default |

Unknown keys under `cache` are ignored like everywhere else, and because flow style is unsupported `cache: {backend: …}` makes the whole `cache` value a string, so the section is skipped and every layer keeps its default. See [caching](../how-to/caching.md) and [cache-gate](../explanation/cache-gate.md).

## `guardrails`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `policy_version` | `string` | `"v1"` | Stamped into every cache key. Change it when detectors or policy change so entries written under the old policy miss |
| `category_overrides` | `mapping` | absent (default policy) | See the next table |
| `bedrock_guardrails` | `mapping` | absent (off) | See [bedrock_guardrails](#guardrailsbedrock_guardrails) |
| `embed_sim` | `mapping` | absent (off) | See [embed_sim](#guardrailsembed_sim) |

### `guardrails.category_overrides.<category>`

A flat mapping of category name to action string. The value must be a `string`; a nested mapping (`credential:\n  action: block`) is a load error. An unknown category or action is logged at startup (`guardrail_config_unknown_category` / `guardrail_config_unknown_action`) and skipped; it is not a startup failure. An override sets both the finding action and the detector-error action for that category.

| Category | Default action |
|---|---|
| `credential` | `block` |
| `financial_id` | `block` |
| `government_id` | `block` |
| `contact_info` | `warn` |
| `network_id` | `warn` |
| `prompt_injection` | `warn` |

Allowed values: `block`, `warn`.

### `guardrails.bedrock_guardrails`

AWS Bedrock Guardrails as an additional prompt-attack detector. Once the block exists, every non-optional key is required; a missing one is a load error.

| Key | Type | Required | Meaning |
|---|---|---|---|
| `region` | `string` | yes | AWS region |
| `access_key_id_env` / `access_key_id_file` | `string` | one of the two | Access key ID source; `*_file` wins and is hot-reloaded |
| `secret_access_key_env` / `secret_access_key_file` | `string` | one of the two | Secret access key source; `*_file` wins and is hot-reloaded |
| `session_token_env` / `session_token_file` | `string` | no | Session token source for STS credentials |
| `guardrail_id` | `string` | yes | Bedrock guardrail identifier |
| `guardrail_version` | `string` | yes | Bedrock guardrail version |

Bedrock Guardrails makes no call at startup; bad credentials surface per request under the `prompt_injection` policy.

### `guardrails.embed_sim`

Embedding-similarity prompt-injection detector backed by Bedrock embeddings.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `region` | `string` | yes | | AWS region |
| `access_key_id_env` / `access_key_id_file` | `string` | one of the two | | Access key ID source; `*_file` wins and is hot-reloaded |
| `secret_access_key_env` / `secret_access_key_file` | `string` | one of the two | | Secret access key source; `*_file` wins and is hot-reloaded |
| `session_token_env` / `session_token_file` | `string` | no | | Session token source |
| `similarity_threshold` | `float` | no | 0.82 | Minimum cosine similarity to report a finding. At or below 0 means default; above 1 passes `-validate` and fails startup (`constructing embedsim detector: embedsim: SimilarityThreshold must be in (0,1]`) |
| `corpus_path` | `string` | no | bundled corpus | JSON file replacing the bundled injection corpus. Read once at startup; a file that cannot be read, is not a JSON array, or has zero entries is a startup failure |

embed_sim embeds its corpus when the detector is constructed, so credentials that fail at startup are fatal (`constructing embedsim detector: …`).

## `admin`

The admin HTTP server starts only when `token_env` is set. It listens on its own address, never on `listen_addr`. Route and role details are in [admin-api.md](admin-api.md) and [admin-api-rbac](../how-to/admin-api-rbac.md).

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `listen_addr` | `string` | `"127.0.0.1:8081"` | Bind address of the admin server. Loopback-only by default |
| `token_env` | `string` | empty (admin server off) | Name of the environment variable holding the Admin bearer token. If set and the variable resolves empty, the process refuses to start |
| `viewer_token_env` | `string` | empty (no Viewer tier) | Environment variable holding the read-only Viewer token. If set and empty, refuses to start |
| `cost_viewer_token_env` | `string` | empty (no CostViewer tier) | Environment variable holding the CostViewer token. If set and empty, refuses to start |
| `operator_token_env` | `string` | empty (no Operator tier) | Environment variable holding the Operator token. If set and empty, refuses to start |
| `persist_path` | `string` | empty (in-memory) | bbolt file for admin-created and rotated virtual keys |
| `redis_addr` | `string` | empty | Redis `host:port` for a cross-replica identity store. When both are set, `redis_addr` wins and a warning (`identity_redis_addr_and_persist_path_both_set`) is logged. Unlike the other Redis consumers, the identity store is loaded at startup: an unreachable Redis is a startup failure |
| `redis_password_env`, `redis_username`, `redis_tls` | see [Redis auth keys](#redis-auth-keys) | | |
| `enable_pprof` | `bool` | `false` | Mounts `net/http/pprof` under `/admin/debug/pprof/` behind the Admin token |
| `backup_dir` | `string` | empty (route answers 501) | Directory `POST /admin/backup` writes into. The route is always registered; with `backup_dir` empty it returns `501 Not Implemented` (`admin.backup_dir is not configured`) |
| `enable_audit_log` | `bool` | `true` | Gates every admin-route audit event, both the log line and the durable trail. Defaults to `true` even when the `admin` section is absent |
| `audit_log_path` | `string` | empty (no durable trail) | JSONL file for admin audit events; enables `GET /admin/audit`. Governed by `enable_audit_log`. If the file cannot be opened the process refuses to start. Not read when `token_env` is unset |
| `on_corrupt_store` | `string` | `"fail"` | `fail` or `reset`. Any other value is a load error. Applies to every `persist_path` (identity, budget, prompt). Defaults to `fail` even when the `admin` section is absent. See [Persistence files](#persistence-files) |
| `mtls` | `mapping` | absent (plain HTTP) | See the next table |

### `admin.mtls`

Requires every admin client to present a certificate signed by the given CA, on top of the bearer tokens. All three keys are required together; a block missing any of them is a load error naming the missing keys.

| Key | Type | Meaning |
|---|---|---|
| `ca_cert_path` | `string` | PEM CA the server verifies client certificates against |
| `server_cert_path` | `string` | PEM certificate the admin server presents |
| `server_key_path` | `string` | PEM private key for `server_cert_path` |

## `health_probe`

Active probing of deployments. There is no default interval: probing is off unless `interval_seconds` is greater than 0.

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `interval_seconds` | `int` | 0 (off) | Seconds between probe passes. At or below 0 disables probing; every deployment stays eligible for routing. The first pass runs one interval after start. Each probe call is bounded to 5 s |
| `unhealthy_threshold` | `int` | 3 | Consecutive failed probes before a deployment is excluded. At or below 0 means default |
| `healthy_threshold` | `int` | 2 | Consecutive successful probes before an excluded deployment returns. At or below 0 means default |
| `recovery_ramp_steps` | `int` | 4 | Further consecutive successes over which the returned deployment's weight ramps to full. At or below 0 means default |
| `recovery_ramp_initial_percent` | `int` | 20 | Effective-weight percentage a recovered deployment starts at. At or below 0 means default; above 100 is clamped to 100 |

Probe behaviour and the fail-open rule when every deployment of a model is unhealthy are in [FAILURE-MODES.md](../operations/FAILURE-MODES.md). Routing details are in [routing-and-failover](../how-to/routing-and-failover.md).

## `credential_reload`

| Key | Type | Default | Meaning and validation |
|---|---|---|---|
| `interval_seconds` | `int` | 60 | Seconds between re-reads of every `*_file` credential: deployment credentials, `guardrails.bedrock_guardrails` and `guardrails.embed_sim`. At or below 0 means the 60 s default; there is no disabled state. The loop is a no-op when no `*_file` key is configured |

Rotation procedures are in [rotate-credentials](../how-to/rotate-credentials.md).

## Redis auth keys

Each of the four Redis-backed sections (`budget`, `rate_limit`, `admin`, `config_propagation`) accepts the same three optional keys next to its `redis_addr`. They are meaningless when that section's `redis_addr` is empty.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `redis_password_env` | `string` | empty (no AUTH) | Name of the environment variable holding the Redis password |
| `redis_username` | `string` | empty | Redis ACL username sent with the password. Not a secret; a plain value |
| `redis_tls` | `bool` | `false` | Enables TLS with the system CA store and a minimum of TLS 1.2 |

Not available today: a custom CA bundle or a client certificate for Redis TLS.

## Persistence files

`admin.persist_path`, `budget.persist_path` and `prompt.persist_path` are bbolt files, all governed by `admin.on_corrupt_store`.

| Situation | Behaviour |
|---|---|
| Path absent | Created fresh, regardless of `on_corrupt_store` |
| File exists and opens | Hydrated at startup. A hydration error is a startup failure |
| File exists and fails to open under `fail` | Startup failure |
| File exists and fails to open as corrupt under `reset` | The open failure is logged at Error (`persist_store_open_failed`), the file is renamed to `<path>.corrupt-<unix-seconds>-<nanoseconds>` (`persist_store_reset`, Warn; a failed rename is `persist_store_corrupt_backup_failed` and the original open error is fatal), and a fresh file is created at the path |
| Another process holds the lock | Fails within one second with `another process holds the file lock`; never treated as corruption, so `reset` never renames a locked file. Since `gateway/v0.18.0` |

Backup and offline restore are in [backup-and-restore](../how-to/backup-and-restore.md).

## CLI flags

| Flag | Default | Meaning |
|---|---|---|
| `-config <path>` | `config.yaml` | Path of the configuration file |
| `-validate` | off | Loads the file, runs the provider-name and `fallback_chains` checks, prints `config is valid` and exits 0, or prints `config error: …` and exits 1. Never dials Redis, opens a store or credential file, or reads an environment variable |
| `-version` | off | Prints the build identity on one line and exits 0. Since `gateway/v0.18.0` |
| `-restore-store identity\|budget\|prompt` | off | Offline restore into that store's configured `persist_path`; exits without starting a server. The gateway must be stopped. Requires `-restore-from` |
| `-restore-from <file>` | | Backup file to restore from, for example one written by `POST /admin/backup` |
| `-restore-force` | off | Overwrite an existing destination file; refused by default |

`-validate` catches config-shape errors only. The startup failures in the next section that depend on an environment variable, a file or a network address pass `-validate`.

## What refuses to start

The process exits 1 before binding a listener when any of the following holds. The first column says whether `-validate` detects it.

| Detected by `-validate` | Condition |
|---|---|
| yes | Any load error in this page's tables (missing required key, pair rule, negative value, bad boolean, duplicate key, tab indentation, unknown `kind`, unknown `fallback_chains` class, bad CIDR, non-RFC-3339 `expires_at`, non-`https` `base_url` without `allow_insecure_http`, `tls`/`mtls` block rules, `tpm_accounting` without `tpm_capacity`, `models` entry for an unserved model) |
| yes | A `provider` that is not one of the five adapter names |
| yes | A `fallback_chains` target that is not a configured deployment |
| yes | `deployments.<name>.anthropic_beta_policy` set to a value other than `strip` or `forward_known`, or `forward_known` on a deployment whose `provider` is not `bedrock` |
| no | `admin.token_env`, `viewer_token_env`, `cost_viewer_token_env` or `operator_token_env` set but resolving to an empty variable |
| no | `config_propagation.redis_addr` set without `signing_secret_env`, or the secret resolving empty |
| no | `admin.redis_addr` set and Redis unreachable or the key hydration failing |
| no | A `persist_path` bbolt file that cannot be opened (locked, corrupt under `fail`, or unopenable) or hydrated |
| no | `admin.audit_log_path` that cannot be opened |
| no | `guardrails.embed_sim` cannot be constructed: `similarity_threshold` above 1, a `corpus_path` that cannot be read or parsed or has zero entries, or a corpus that cannot be embedded (bad or expired credentials) |
| no | `telemetry.exporter` set to a value other than `stdout`, `otlp`, `none` |
| no | `admin.mtls` certificate files that cannot be loaded |
| no | `deployments.<name>.tls` PEM files that cannot be read or parsed (`ca_cert_path`, `client_cert_path`/`client_key_path`) |
| no | A `key_hash` that is not valid hex or does not decode to 32 bytes |

What does not stop startup: an empty provider credential (warning, calls fail), an unreadable `*_file` (warning, retried), an unreachable Redis for `budget`, `rate_limit` or `config_propagation` (lazy dial), an unreachable OTLP collector, an empty `alerting.webhook_url_env` (warning, alerting off), an unknown `category_overrides` category or action (warning, skipped). The per-dependency table is [FAILURE-MODES.md](../operations/FAILURE-MODES.md).

## Not available today

- A `schema_version` key. [VERSIONING.md](../VERSIONING.md) names it as a condition for gateway/v1.0.0; the parser reads none.
- A strict mode that rejects unknown keys. Unknown keys are ignored at every level.
- YAML lists, anchors, aliases, flow style or multi-line strings.
- Hot reload of `config.yaml` by `SIGHUP` or environment-variable overrides. The file is read once at process start; only `*_file` credentials and admin-API mutations change at runtime.
- Per-deployment pricing. `price_table` is keyed by canonical model, so every deployment sharing a `model` is billed at one price.
- A `cache.backend` or remote-cache key. The three cache layers are in-process; the gRPC remote cache in the tree is a dormant stub with no config key.
- A custom CA bundle or client certificate for Redis TLS (`redis_tls` uses the system CA store only).

## Examples

Secrets appear only as environment-variable names. The two `key_hash` values are the SHA-256 digests of the public example secrets `example-team-alpha-secret-do-not-use` and `example-team-beta-secret-do-not-use` from [`gateway/config.example.yaml`](../../gateway/config.example.yaml); never use them for a real key.

### Minimal valid file

Every required key and nothing else.

```yaml
listen_addr: ":8080"
virtual_keys:
  team-alpha:
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
```

### Virtual key with every rate-limit dimension

```yaml
virtual_keys:
  team-alpha:
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
    budget_usd: 100.0
    budget_reset_interval_seconds: 2592000
    budget_warn_percent: 0.8
    rate_limit:
      burst: 20
      refill_per_second: 10
      tpm_capacity: 100000
      tpm_refill_per_second: 1000
      max_concurrent_requests: 10
      per_model:
        gpt-4o:
          burst: 5
          refill_per_second: 1
          tpm_capacity: 50000
          tpm_refill_per_second: 500
    allowed_models:
      gpt-4o: true
    allowed_source_cidrs:
      "10.0.0.0/8": true
    cache_scope_to_end_user: true
  team-beta:
    key_hash: "8e43f8e74a4151a23f77bd21474a038434c56bad8c5d8bc7bb2af0d14fa95095"
```

### Bedrock deployment with file-based credentials and a provider-weighted TPM ceiling

```yaml
deployments:
  claude-bedrock-primary:
    model: "claude-bedrock"
    provider: "bedrock"
    upstream_model: "anthropic.claude-3-5-sonnet-20241022-v2:0"
    base_url: "https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse"
    region: "us-east-1"
    access_key_id_file: "/var/run/secrets/kelvran/access-key-id"
    secret_access_key_file: "/var/run/secrets/kelvran/secret-access-key"
    session_token_file: "/var/run/secrets/kelvran/session-token"
    rate_limit:
      tpm_capacity: 3000000
      tpm_refill_per_second: 50000
      tpm_accounting:
        output_token_multiplier: 5
        exclude_cache_read_tokens: true
credential_reload:
  interval_seconds: 60
```

### Fallback chains

Comma-separated strings, not lists. Every target must be a configured deployment.

```yaml
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"
    fallback_chains:
      context_window_exceeded: "claude-opus-primary"
      content_policy: "claude-opus-primary,gemini-flash-primary"
      generic: "gpt4o-secondary"
```

### Redis-backed multi-replica state

All four Redis consumers, each with its own address and auth keys.

```yaml
rate_limit:
  redis_addr: "redis:6379"
  redis_password_env: "REDIS_PASSWORD"
  redis_tls: true
budget:
  redis_addr: "redis:6379"
  redis_password_env: "REDIS_PASSWORD"
  redis_tls: true
admin:
  token_env: "KELVRAN_ADMIN_TOKEN"
  redis_addr: "redis:6379"
  redis_password_env: "REDIS_PASSWORD"
  redis_tls: true
config_propagation:
  redis_addr: "redis:6379"
  signing_secret_env: "KELVRAN_CONFIG_PROPAGATION_SIGNING_SECRET"
  redis_password_env: "REDIS_PASSWORD"
  redis_tls: true
```

### Price table and model metadata

```yaml
price_table:
  gpt-4o:
    prompt_per_token: 0.0000025
    completion_per_token: 0.00001
models:
  gpt-4o:
    display_name: "GPT-4o"
    description: "General-purpose chat, routed to OpenAI."
```

The `models` section first shipped in `gateway/v0.18.0`.

### Validate without starting

```sh
kelvran-gateway -config /etc/kelvran-gateway/config.yaml -validate
```

Prints `config is valid` and exits 0, or `config error: …` and exits 1.

## Related pages

- [`gateway/config.example.yaml`](../../gateway/config.example.yaml): the annotated example file.
- [provider-credentials](../how-to/provider-credentials.md), [rotate-credentials](../how-to/rotate-credentials.md): the `*_env` and `*_file` credential keys in practice.
- [virtual-keys-and-budgets](../how-to/virtual-keys-and-budgets.md): the `virtual_keys` section in practice.
- [routing-and-failover](../how-to/routing-and-failover.md): `weight`, `cost_tier`, `sticky`, `fallback_chains`, `health_probe`.
- [caching](../how-to/caching.md): the `cache` section.
- [admin-api-rbac](../how-to/admin-api-rbac.md), [admin-api.md](admin-api.md): the `admin` token tiers and routes.
- [backup-and-restore](../how-to/backup-and-restore.md): `persist_path`, `backup_dir`, `-restore-store`.
- [troubleshooting](../how-to/troubleshooting.md), [FAILURE-MODES.md](../operations/FAILURE-MODES.md): what each dependency failure does.
- [TELEMETRY.md](../operations/TELEMETRY.md), [metrics-and-logs.md](metrics-and-logs.md): the `telemetry` section and the log events named on this page.
- [error-codes.md](error-codes.md): the client-facing error codes that rate limits, budgets and guardrails produce.
- [security-model](../explanation/security-model.md): why secrets are environment-variable names and why `admin` fails closed.
- [VERSIONING.md](../VERSIONING.md): which config keys are a stable public surface.
