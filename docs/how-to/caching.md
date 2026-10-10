# Configure the response cache

This page shows an operator how to tune the gateway's three in-process response-cache layers, scope cached responses per end user, invalidate the whole cache on a policy change, and erase one cached response through the admin API. It is for people who run the gateway and edit its `config.yaml`; client authors only need the "What a hit looks like from the client" note below.

Use this when you want to change how long or how much the gateway caches, stop two end users behind one virtual key from sharing responses, or remove a specific cached response.

## Before you start

- A running gateway built from `gateway/v0.18.0` or `gateway/v0.17.0`. Behaviour that differs between the two is marked.
- Write access to the gateway's `config.yaml` and a way to restart the process. The cache section is read at startup only.
- For the erasure step: the admin listener enabled (`admin.listen_addr`, default `127.0.0.1:8081`) and an Admin or Operator bearer token (`admin.token_env` or `admin.operator_token_env`). See [Admin API and RBAC](admin-api-rbac.md).
- Model and key names below come from [`gateway/config.example.yaml`](../../gateway/config.example.yaml): virtual key `team-alpha` (secret `example-team-alpha-secret-do-not-use`) and model `gpt-4o`.

## How the cache behaves (read once)

- The cache is always on, in-process, and empty after every restart. There is no knob to turn it off.
- Every request is checked in order: L1 exact match, then L2 normalized match (a hit is promoted into L1), then L3-lite lexical near-duplicate, then a real upstream call.
- The lookup happens after authentication, model and source-IP allow-lists, rate limits and the budget reservation. A hit still has to pass the RPM and TPM checks and the budget reservation, and holds a concurrency slot while it is served; it consumes one RPM token, but its TPM and budget reservations are released when the request finishes, so a hit is never billed and does not count against TPM.
- L2 normalization is exactly three operations: outer whitespace trim, Unicode NFC, and stripping one trailing `.`, `!` or `?` from the last message. No case folding, no internal-whitespace collapse.
- L3-lite is MinHash/Jaccard over 3-word shingles (128-value signature, top 5 candidates), with a 0.9 similarity floor and a 24-hour staleness limit. It is lexical, not embedding-based.
- A response is written to all three layers after the post-call guardrail passes. A response with any choice `finish_reason: length` is never written; a post-call guardrail block is never written. Streaming responses are served from and written to the cache too.
- Concurrent byte-identical buffered misses are coalesced into one upstream call; followers are not billed. The streaming path has no coalescing. See [Streaming](streaming.md).
- Every layer is partitioned per virtual key with its own LRU cap. One tenant cannot evict another. There is no global cap: resident entries scale with active tenants x `max_entries` x 3 layers.

For the design reasoning see [The cache gate](../explanation/cache-gate.md) and the Cache Subsystem section of [`gateway/ARCHITECTURE.md`](../../gateway/ARCHITECTURE.md).

## Step 1: Set TTLs, capacity and jitter

Add a `cache:` section to `config.yaml`. These values are the defaults; omitting the section, a block, or a key (or setting it to `0`) gives the same result.

```yaml
cache:
  ttl_seconds: 300            # L1 exact match
  max_entries: 10000          # per virtual key
  jitter_fraction: 0.10       # <= 0 also means 0.10; cannot be disabled
  l2:
    ttl_seconds: 75           # L2 normalized match
    max_entries: 10000        # per virtual key
    jitter_fraction: 0.10
  l3:
    ttl_seconds: 300          # L3-lite lexical near-duplicate
    max_entries: 10000        # per virtual key
    jitter_fraction: 0.10
```

- `ttl_seconds` is the base lifetime of an entry in that layer.
- `max_entries` is the LRU cap per virtual key in that layer, not a global cap.
- `jitter_fraction` adds up to that fraction of the TTL as random extra lifetime on each write, so entries written together do not expire together. `0` or a negative value resolves to `0.10`.

Restart the gateway. Every key in this block is described in [Configuration reference](../reference/config.md).

### Variant: many tenants on one instance

Lower `max_entries` on each layer rather than the TTL. The cap is per virtual key, so an instance serving 200 keys at the default holds up to 200 x 10,000 entries per layer. The L3 layer also scans a tenant's candidate set on every lookup, so a smaller `cache.l3.max_entries` bounds that scan.

### Variant: shorter L1 for fast-moving content

Set `cache.ttl_seconds` lower. L2 and L3 keep their own TTLs, so lower `cache.l2.ttl_seconds` and `cache.l3.ttl_seconds` with it if the content must not be served from any layer. The 24-hour L3 staleness limit and the volatile-query bypass are compiled in and are not affected by these values.

## Step 2: Scope the cache per end user (optional)

By default every end user behind one virtual key shares that key's cache. To split it, set the flag on the key and have clients send the end-user header.

```yaml
virtual_keys:
  team-alpha:
    key_hash: "6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1"
    cache_scope_to_end_user: true
```

Restart the gateway. To change the flag without a restart, `POST /admin/virtual_keys/team-alpha` with the Admin token (not Operator) and a body that repeats the key's full definition (`key_hash` and every other field you want to keep) with `"cache_scope_to_end_user": true`; the upsert replaces the whole key. See [Admin API reference](../reference/admin-api.md). Whether a live upsert survives the next restart is covered in [Virtual keys and budgets](virtual-keys-and-budgets.md).

Clients then send `X-Kelvran-End-User-Id: <opaque id>` on every request. The value is folded into the L1, L2 and L3 partitions, so two end users never share an entry.

The header is caller-supplied and not authenticated. When the flag is on and a request arrives without the header, the request gets a unique, never-shared scope: it cannot read or populate any other request's entry. Enable the flag only once every client sends the header. Each header-less request creates a new one-entry partition in all three layers that nothing ever reads or evicts, so resident memory grows by three stored responses per such request until the next restart. With the flag on, the `max_entries` cap applies per (virtual key, end user) partition, so the capacity arithmetic in "How the cache behaves" and "Variant: many tenants on one instance" scales with active end-user scopes, not virtual keys. The flag is off by default and off for every key configured before it existed. See [Virtual keys and budgets](virtual-keys-and-budgets.md).

## Step 3: Invalidate everything after a guardrail or policy change

`guardrails.policy_version` is part of every L1 and L2 key and is an exact-match gate on L3. Bumping it makes every existing entry a miss at every layer.

```yaml
guardrails:
  policy_version: "v2"      # default is "v1"
```

Restart the gateway. Because the cache is in-process, a restart alone also empties it; the version bump matters for keeping entries written under an old policy out of the way for as long as the process lives.

## Step 4: Erase one cached response

`POST /admin/cache/erase` removes one request's L1 and L2 entries. It accepts an Admin or Operator token. Supply the virtual key and the request-defining fields the original request used: `model`; either `messages` or the prompt reference (`prompt_id` with `prompt_version` or `prompt_label`, plus the same `prompt_variables`), never both; and whichever of `temperature`, `max_tokens`, `response_format`, `thinking_binding_mode`, `tools` and `tool_choice` were set. A request that used `prompt_id` is hashed after variable substitution, so the same `prompt_variables` must be sent. If the key uses `cache_scope_to_end_user`, also send `end_user_id` with the header value the original request carried.

```bash
curl -sS -X POST http://127.0.0.1:8081/admin/cache/erase \
  -H "Authorization: Bearer $KELVRAN_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "virtual_key_id": "team-alpha",
    "model": "gpt-4o",
    "messages": [{"role": "user", "content": "the exact original prompt"}]
  }'
```

Response:

```json
{"l1_found":true,"l2_found":true,"l3_skipped":true}
```

- `200` with both `false` means the key is known and nothing was cached under that exact request.
- `400` when the body is invalid, `virtual_key_id` is empty, the prompt reference fields conflict, or the prompt reference does not resolve to a stored prompt; `500` for any other error.
- `404` when `virtual_key_id` matches no configured virtual key.
- `l3_skipped` is always `true`. The L3 layer is never erased; a near-identical follow-up can still be served from L3 until its TTL expires.
- The lookup and delete are not atomic. A concurrent identical request can write a fresh entry under the same key immediately afterwards.
- The call acts on the memory of the one instance it reaches. Repeat it against every replica.

The full data-subject procedure and its limits are in [Data subject requests](../operations/DATA-SUBJECT-REQUESTS.md). The route is documented in [Admin API reference](../reference/admin-api.md).

## Verify it worked

Send the same request twice through the data plane, then read the gateway's log.

```bash
for i in 1 2; do
  curl -sS http://127.0.0.1:8080/v1/chat/completions \
    -H "Authorization: Bearer example-team-alpha-secret-do-not-use" \
    -H 'Content-Type: application/json' \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Name one prime number."}]}' > /dev/null
done
```

The gateway writes JSON log lines, one `chat_completion` line per request. The first carries `"cache_hit":false`. The second carries `"cache_hit":true`, `"cache_layer":"L1"` and `"cache_age_ms":<milliseconds since the write>`. Each L1 and L2 check, and each L3 check that actually searched (not a volatile-word bypass or a search error), also emits a `cache_cross_instance_check` line with `cache_layer`, `hit`, `ttl_ms` and `instance_id`; the L3 line's `tenant_id` is the virtual key ID, while the L1 and L2 lines carry the cache scope (the same value unless `cache_scope_to_end_user` is on, when it is a hash).

Unless `telemetry.exporter` is `none`, the counter `kelvran.cache.lookup` gains one point (printed to stdout under the default `stdout` exporter, shipped to the collector under `otlp`) with `kelvran.cache.lookup_outcome=hit` and `kelvran.cache.layer=L1`; `kelvran.cache.savings_usd` grows by the notional cost of the call. Metric and log fields are listed in [Metrics and logs reference](../reference/metrics-and-logs.md) and [TELEMETRY.md](../operations/TELEMETRY.md).

To confirm an erasure, repeat the request after Step 4 and check that the `chat_completion` line reports `"cache_hit":false` or `"cache_layer":"L3"`; the second outcome is the L3 limitation described above.

To confirm Step 2, send the request above twice with `-H 'X-Kelvran-End-User-Id: alice'` and once more with `-H 'X-Kelvran-End-User-Id: bob'`. The second `alice` request logs `"cache_hit":true` and `"cache_layer":"L1"`; the `bob` request logs `"cache_hit":false`. If `bob` hits, the flag is not active on the running process.

### What a hit looks like from the client

Nothing in the HTTP response marks a hit. The body is the stored completion. The only request header the cache reads is `X-Kelvran-End-User-Id`.

## What is in the key

Two requests share an L1 entry only when all of these match: virtual key, model, the serialized messages, `temperature`, `max_tokens`, `guardrails.policy_version`, `response_format`, the resolved prompt reference, the end-user scope, `thinking_binding_mode`, the `tools` plus `tool_choice` definitions and, since gateway/v0.19.0, the thinking configuration and the sampling fields (`top_p`, `top_k`, `stop`, `effort`). L2 uses the same fields over normalized messages. L3 gates by exact equality on model, `guardrails.policy_version`, `response_format`, the resolved prompt reference, the reasoning-blocks history, `thinking_binding_mode`, `tools` plus `tool_choice`, the thinking configuration and the sampling fields; it does not gate on `temperature` or `max_tokens`, which is why a truncated response is never written.

The L3 layer additionally refuses a candidate unless the entity/number/date fingerprint and the negation fingerprint are identical, the candidate is at most 24 hours old, the similarity is at least 0.9, and the model matches. Any user message containing one of the words `weather`, `price`, `stock`, `score`, `today`, `current`, `currently`, `now` or `latest` bypasses L3 entirely. None of this is configurable. See [`THREAT_MODEL.md`](../../THREAT_MODEL.md) for why.

## Upgrade note: tools and tool_choice in the key

Folding `tools` and `tool_choice` into the L1 and L2 keys and the L3 gate first shipped in `gateway/v0.18.0`. The RFC is [2026-10-08-gateway-cache-key-tools-fingerprint.md](../rfcs/2026-10-08-gateway-cache-key-tools-fingerprint.md). On `gateway/v0.17.0`, two requests with identical messages but different `tool_choice` can share a cached response.

Because the field is hashed even when empty, every L1 and L2 key differs from an earlier build's. The in-process cache is empty after the restart an upgrade requires, so there is nothing to flush. See [Upgrade](upgrade.md).

## Upgrade note: the thinking configuration, the sampling fields and the passthrough fingerprint in the key

gateway/v0.19.0 folds the request's thinking configuration — the `thinking` object a `/v1/messages` request carries; a `/v1/chat/completions` request has none, and a `thinking` key in its body is ignored as before — and its sampling fields (`top_p` and `stop`, which `/v1/chat/completions` now forwards instead of dropping; `top_k` and `effort`, `/v1/messages` fields) into the L1 and L2 keys and the L3 gate, so a reply produced under one configuration is never served to a request that asked for another. The RFC is [2026-10-09-gateway-anthropic-messages-ingress.md](../rfcs/2026-10-09-gateway-anthropic-messages-ingress.md) §3. As with the tools fold, the field is hashed even when empty, so every L1 and L2 key differs from a gateway/v0.18.0 build's: one cold cache on upgrade, nothing to flush. The same release folds the Anthropic ingress's passthrough fingerprint — the members of a `/v1/messages` body the canonical schema cannot hold, and its `is_error` tool results — which is empty for every `/v1/chat/completions` request, so it adds no further key change.

## Not available today

- Disabling the cache, disabling one layer, or opting a virtual key out. There is no such key in `config.yaml`.
- A client-side bypass or no-cache header. The only `X-Kelvran-*` request header is `X-Kelvran-End-User-Id`; `X-Kelvran-Overhead-Duration-Ms` is a response header on buffered completions and does not affect caching.
- Disabling TTL jitter. A `jitter_fraction` of `0` or less resolves to `0.10`.
- Erasing an L3 entry. The lexical cache has no delete operation; `l3_skipped` is always `true`.
- Finding every cached entry for one person. The cache is keyed by a hash of the request, not indexed by subject.
- A shared or Redis-backed response cache across replicas. The design exists as [an RFC](../rfcs/2026-09-11-gateway-redis-backed-cache-design.md); the gateway always builds the in-process caches.
- Embedding-based semantic matching. L3-lite is lexical near-duplicate matching only.
- Coalescing of concurrent identical streaming misses.
- An atomic get-and-delete for erasure.

## Related

- [Configuration reference](../reference/config.md) for `cache.*`, `guardrails.policy_version` and `cache_scope_to_end_user`
- [Admin API reference](../reference/admin-api.md) for `POST /admin/cache/erase`
- [Data-plane API reference](../reference/data-plane-api.md) for `X-Kelvran-End-User-Id`
- [The cache gate](../explanation/cache-gate.md) and [Architecture](../explanation/architecture.md)
- [`gateway/config.example.yaml`](../../gateway/config.example.yaml)
