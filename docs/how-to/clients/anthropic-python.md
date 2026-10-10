# Use the Anthropic Python SDK with Kelvran

This page shows a developer who already uses the `anthropic` Python package how to point it at a Kelvran gateway, make a buffered and a streaming message, list models, and handle the errors the gateway returns. It installs nothing new: the gateway serves the Anthropic Messages API on `POST /v1/messages` since `gateway/v0.19.0` (item 11, [RFC-1](../../rfcs/2026-10-09-gateway-anthropic-messages-ingress.md)), and the SDK's `base_url` override is the only client-side change. Behind the route each message becomes the same canonical request the OpenAI route produces, so virtual keys, budgets, rate limits, guardrails, the cache, `Idempotency-Key` and routing apply unchanged; the gateway translates to whichever provider the model's deployment names.

Use this when you have a running gateway and a virtual key and you want your existing `anthropic` code to go through Kelvran. For Claude Code, see [Use Claude Code with Kelvran](claude-code.md).

## Prerequisites

- A gateway that answers `GET /healthz` with `{"status":"ok"}`. The [quickstart](../../tutorials/quickstart.md) gets you there; its minimal config serves the canonical model `gpt-4o` through an OpenAI deployment, which this page's examples use — the Anthropic wire shape is translated to the deployment's provider, so the model name is whatever the gateway's config calls it, not necessarily an Anthropic id.
- A virtual key: the raw secret in an environment variable, its SHA-256 as `key_hash` in `config.yaml`. See [Virtual keys and budgets](../virtual-keys-and-budgets.md). The examples use `KELVRAN_KEY`.
- The `anthropic` package installed. Client-library behaviour described below as "the SDK does X" is the SDK's published behaviour as of writing; nothing in this repository pins a client version.

## Steps

### 1. Point the client at the gateway

The base URL is the gateway's `listen_addr` with no path — the SDK appends `/v1/messages` itself. The SDK sends `api_key` as `x-api-key`, which the gateway reads as the bearer's alias on this route; `auth_token` sends `Authorization: Bearer` instead. Set one, not both: when both arrive with different values the gateway verifies `Authorization` alone and a wrong one is a `401` with no second attempt.

```python
import os
from anthropic import Anthropic

client = Anthropic(
    base_url="http://localhost:8080",      # listen_addr ":8080" from config.yaml; the SDK adds /v1/messages
    api_key=os.environ["KELVRAN_KEY"],     # sent as x-api-key; the raw virtual-key secret, never its key_hash
)
```

### 2. Make one buffered message

```python
msg = client.messages.create(
    model="gpt-4o",                        # a canonical model name from the gateway's config
    max_tokens=256,
    messages=[{"role": "user", "content": "Say hello in one sentence."}],
)
print(msg.content[0].text, msg.stop_reason, msg.usage.input_tokens, msg.usage.output_tokens)
```

The response is an Anthropic `message`: `id` (gateway-issued), `type: message`, `role: assistant`, `model` (the canonical name), `content` blocks, `stop_reason` (`end_turn`, `max_tokens`, `tool_use`, `stop_sequence`, or the provider's native reason) and `usage` with the cache counters. The response headers carry `X-Kelvran-Overhead-Duration-Ms`, which the SDK ignores.

### 3. Make one streaming call

```python
with client.messages.stream(
    model="gpt-4o",
    max_tokens=256,
    messages=[{"role": "user", "content": "Count from one to five, words only."}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)
    final = stream.get_final_message()
print("\n", final.stop_reason, final.usage.output_tokens)
```

The gateway emits the Anthropic event sequence — `message_start`, `content_block_start`/`content_block_delta`/`content_block_stop`, `message_delta` with the final usage, `message_stop` — as the provider's chunks arrive, plus an `event: ping` after 15 s of upstream silence. A failure after the first event is one `event: error` and no `message_stop`; the SDK raises on it. A response the gateway cut off (its runaway ceiling, a mid-stream budget top-up) ends with `stop_reason: max_tokens`.

### 4. List the models the key may call

```python
for m in client.models.list():
    print(m.id, m.display_name)
```

`GET /v1/models` answers in the SDK's list shape (`data[].id|type|display_name|created_at`, `first_id`/`last_id`/`has_more`) and lists only the models the key's `allowed_models` permits.

## Variants

### Send an Idempotency-Key or an end-user id

```python
msg = client.messages.create(
    model="gpt-4o", max_tokens=64,
    messages=[{"role": "user", "content": "hi"}],
    metadata={"user_id": "end-user-42"},                    # fills the end-user cache scope when the header is absent
    extra_headers={"Idempotency-Key": "order-7f3a"},        # same key + same body within 10 minutes replays the stored answer
)
```

`metadata.user_id` affects response-cache partitioning only for keys with `cache_scope_to_end_user: true`; an `X-Kelvran-End-User-Id` header, when present, wins over it. On this route the `Idempotency-Key` fingerprint is the body as received (plus the normalised `anthropic-beta` set), so a reused key with a different body is `422` `idempotency_key_reused`.

### Tools, thinking, system prompts

`tools`, `tool_choice` (`auto`, `any`, `tool`, `none`), `system` (string or block array with `cache_control`; a `role: system` entry inside `messages` is parsed the same way — on a `bedrock`, `anthropic` or `gemini` deployment it is hoisted into the provider's system prompt after the other system blocks, on an `openai` or `openaicompat` deployment it stays in place as a `role: system` message), `thinking`, `output_config.effort`, `stop_sequences`, `temperature`, `top_p`, `top_k` and image/document blocks are canonical and translate. A member the gateway's schema cannot hold (a pre-release beta body field, `service_tier`, an unknown block type) is recorded by JSON pointer and the request is served only by an `anthropic` deployment or one whose config sets `accept_lossy_anthropic_ingress: true`; otherwise it is `400` `lossy_ingress_rejected` with the pointers in `param`. The `anthropic-beta` values the SDK sends are folded into the cache key (two calls differing only there never share an entry) and are stripped on the way to a Bedrock deployment until `anthropic_beta_policy: forward_known` is applied by the upstream leg.

### Count tokens

```python
from anthropic import NotFoundError
try:
    client.messages.count_tokens(model="gpt-4o", messages=[{"role": "user", "content": "how many?"}])
except NotFoundError as e:
    print(e.body["error"]["code"])   # count_tokens_unavailable
```

`POST /v1/messages/count_tokens` is served and answers `404` `not_found_error` (`code` `count_tokens_unavailable`) for every deployment until the passthrough leg adds the `anthropic` branch. The call authenticates, applies the key's allowlists and consumes one RPM token; it debits no budget.

## How errors surface in this client

Every error is Anthropic's envelope with Kelvran's `code` and `param` kept, so `e.body["error"]["code"]` tells the cases apart; the SDK maps the status to its exception classes and retries `429` and `5xx` on its own schedule, honouring `Retry-After` ([error-codes.md](../../reference/error-codes.md)):

| Status | Envelope `type` | `code` | SDK exception (check your version) |
|---|---|---|---|
| 400 | `invalid_request_error` | `model_not_found`, `lossy_ingress_rejected`, `missing_required_parameter`, `invalid_request`, `invalid_json`, `content_policy_violation` | `BadRequestError` |
| 401 | `authentication_error` | `invalid_api_key`, `key_expired` or none | `AuthenticationError` |
| 403 | `permission_error` | `model_not_allowed`, `source_ip_not_allowed` | `PermissionDeniedError` |
| 404 | `not_found_error` | `count_tokens_unavailable` | `NotFoundError` |
| 413 | `request_too_large` | `request_too_large` | `APIStatusError` |
| 422 | `invalid_request_error` | `idempotency_key_reused` (`param` `Idempotency-Key`) | `UnprocessableEntityError` |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` (with `Retry-After`) or `insufficient_quota` (budget spent, no `Retry-After`) | `RateLimitError` |
| 502 / 503 | `api_error` | `upstream_error`, `deployment_capacity_exceeded` | `InternalServerError` / `APIStatusError` |

A budget 429 is `rate_limit_error` because Anthropic's vocabulary has no quota type; read `code` to stop retrying on `insufficient_quota`. An upstream over-long-prompt rejection carries `capability_rejected: prompt_too_long` at the start of `message`.

## What Kelvran adds that the client ignores

`code` and `param` in the error body (read them from `e.body`), `X-Kelvran-Overhead-Duration-Ms`, the `kelvran.ingress.*` span attributes and the `chat_completion` log line's `ingress_format`/`dropped_fields` fields.

## Verify it worked

Run step 2, then look at the gateway's log: one `chat_completion` line with `ingress_format: anthropic-messages`, `passthrough: true` when an `anthropic` deployment served it (`false` on a translate hop), the model, tokens and cost.

## Not available today

- The response relay for `anthropic` deployments (item 11 slice S11b): the request body already reaches an `anthropic` deployment as received, but the response is re-encoded from the canonical shadow and an upstream error body is redacted. Every other deployment is a translate hop and drops the members the schema cannot hold (reported in `dropped_fields`).
- Exact token counts (`count_tokens` answers `404`; see above).
- Forwarding `anthropic-beta` values to Bedrock (`anthropic_beta_policy: forward_known` is applied by the upstream leg).
- The `anthropic-ratelimit-unified-*` and `x-should-retry` response headers: the gateway synthesises none.
- The Batches, Files and Admin APIs: only `/v1/messages`, `/v1/messages/count_tokens` and `/v1/models` exist for this SDK; anything else is a plain 404.

## Related pages

- [Data-plane API: `POST /v1/messages`](../../reference/data-plane-api.md#post-v1messages)
- [Compatibility](../../reference/compatibility.md) — the `anthropic` SDK row
- [Error codes](../../reference/error-codes.md) — the Anthropic envelope
- [Use Claude Code with Kelvran](claude-code.md)
