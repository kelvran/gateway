# Use the OpenAI Python SDK with Kelvran

This page shows a developer who already uses the `openai` Python package how to point it at a Kelvran gateway, make a buffered and a streaming chat completion, list models, and handle the errors the gateway returns. It installs nothing new: the gateway speaks the OpenAI Chat Completions wire shape, and the SDK only needs a different `base_url` and a virtual-key secret as its `api_key`.

Use this when you have a running gateway and a virtual key, and you want your existing `openai` code to go through Kelvran instead of straight to a provider.

## Prerequisites

- A gateway that answers `GET /healthz` with `{"status":"ok"}`. The [quickstart](../../tutorials/quickstart.md) gets you there; its minimal config listens on `:8080` and serves the canonical model `gpt-4o`.
- A virtual key: the raw secret in an environment variable, its SHA-256 as `key_hash` in `config.yaml`. See [Virtual keys and budgets](../virtual-keys-and-budgets.md) and the [first virtual key tutorial](../../tutorials/first-virtual-key-and-budget.md). The examples below use the variable name `KELVRAN_KEY`. Keep the secret in a variable that is not `OPENAI_API_KEY`: on a host that also runs the gateway, `OPENAI_API_KEY` is usually the gateway's own upstream credential (`api_key_env` in [`gateway/config.example.yaml`](../../../gateway/config.example.yaml)), and the two must not be confused.
- Only for a local try-out, `config.example.yaml` ships a public example key: secret `example-team-alpha-secret-do-not-use`, `key_hash` `6701a1ecc6b08958fa24e13f267aac7233d47f390e92e71f8cc8fb3144672cf1`. Never use it for anything real.
- The `openai` package installed. Client-library behaviour described below as "check your client version" is not pinned by anything in this repository.

## Steps

### 1. Point the client at the gateway

The base URL is the gateway's `listen_addr` plus `/v1`. Authentication is `Authorization: Bearer <raw secret>`, which is exactly what the SDK sends from `api_key`.

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",   # listen_addr ":8080" from config.yaml, plus /v1
    api_key=os.environ["KELVRAN_KEY"],     # the raw virtual-key secret, never its key_hash
)
```

The gateway accepts only the `Bearer ` scheme. A missing or malformed header is `401` with `type` `authentication_error` and `code` null; a secret whose hash matches no configured key is `401` with `code` `invalid_api_key`.

### 2. Make one buffered chat completion

```python
resp = client.chat.completions.create(
    model="gpt-4o",   # a canonical model: name from config.yaml, not the provider's upstream_model
    messages=[{"role": "user", "content": "Say hello in five words."}],
    max_tokens=64,
)
print(resp.id, resp.object, resp.created, resp.model)
print(resp.choices[0].message.content)
print(resp.usage.prompt_tokens, resp.usage.completion_tokens, resp.usage.total_tokens)
```

The gateway reads these request fields: `model`, `messages`, `temperature`, `max_tokens`, `tools`, `tool_choice`, `stream`, `response_format`, plus Kelvran's own extensions. Every other field the SDK lets you pass (`n`, `top_p`, `stop`, `seed`, `user`, `logprobs`, `frequency_penalty`, `presence_penalty`, `logit_bias`, `parallel_tool_calls`, `max_completion_tokens`, `store`, `metadata`, `reasoning_effort`) is silently dropped: the request succeeds and the field has no effect.

`messages[].content` must be a string. OpenAI's array-of-parts `content` is rejected with `400` `invalid_json` (see "Not available today").

The response is `200` with `id`, `object` (`chat.completion`), `created`, `model`, `choices[]` and `usage`. `id`, `object` and `created` on every response are present since gateway/v0.18.0. In gateway/v0.17.0 and earlier, no completion from any provider carries `object` or `created` (the fields did not exist), and a Bedrock-served completion additionally arrives with an empty `id`; OpenAI, Anthropic and Gemini ids pass through unchanged in both versions.

### 3. Make one streaming call

```python
stream = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Count to five."}],
    stream=True,
)
for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
    if chunk.usage:
        print("\ntotal_tokens:", chunk.usage.total_tokens)
```

On the wire this is `Content-Type: text/event-stream`, one `data: {...}` frame per chunk (`object` `chat.completion.chunk`, one `id` and `created` for the whole stream), ending with `data: [DONE]`. Do not pass `stream_options`; the gateway ignores it and always asks OpenAI and OpenAI-compatible upstreams for usage, which then appears on that provider's final frame (`choices` null). Whether `chunk.usage` is ever set depends on the deployment's provider: `openai`/`openaicompat` on the final frame, `gemini` on any frame whose usage metadata is non-zero (use the last `usage` seen), `anthropic`/`bedrock` never on a live stream (the gateway reads their usage for billing but does not forward it); a cache-hit replay always ends with one usage frame. Do not assume the last chunk carries it.

A failure after the first chunk arrives as one in-band `data: {"error":{...}}` frame and the stream ends without `[DONE]`; the HTTP status stays `200`. The gateway's own design note expects the SDK's stream parser to raise its API error class for that frame rather than fail to parse (check your client version). The in-band frame ships since gateway/v0.18.0; in gateway/v0.17.0 and earlier a failure after the first chunk wrote a plain-text error line into the open stream, also without `[DONE]`. More on the SSE contract: [Streaming](../streaming.md).

### 4. List the models the key may call

```python
for m in client.models.list():
    print(m.id, m.owned_by)
```

`GET /v1/models` returns one entry per canonical `model:` name the calling key is allowed to use (its `allowed_models` filter applies), sorted by `id`, with no upstream call. It exists since gateway/v0.18.0; in gateway/v0.17.0 and earlier the path is a `404`.

## Variants

### Read the headers Kelvran adds

```python
raw = client.chat.completions.with_raw_response.create(
    model="gpt-4o", messages=[{"role": "user", "content": "hi"}], max_tokens=8,
)
print(raw.headers.get("X-Kelvran-Overhead-Duration-Ms"))   # buffered responses only
resp = raw.parse()
```

`X-Kelvran-Overhead-Duration-Ms` is the gateway's own added latency in whole milliseconds: handler wall time minus the measured upstream round-trip. It is set on buffered chat responses only, never on streams. `with_raw_response` is the SDK's documented way to reach headers (check your client version).

### Send an Idempotency-Key or an end-user id

```python
resp = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "hi"}],
    extra_headers={
        "Idempotency-Key": "7f3c9a2e-0001",
        "X-Kelvran-End-User-Id": "user-123",
    },
)
```

`Idempotency-Key` is honoured on both chat paths: scoped to the virtual key, fingerprinted on the decoded request (sha256 of the re-serialised honoured fields, so whitespace, key order and any dropped field such as `n` or `seed` do not count), kept for 10 minutes; a concurrent duplicate waits and receives the stored response. Reusing a key with a different body fails with `422` `invalid_request_error` / `idempotency_key_reused` since gateway/v0.19.0 (`502` `upstream_error` before), not OpenAI's `400`. The store is in-process, so it only deduplicates within one gateway instance.

`X-Kelvran-End-User-Id` only matters for a virtual key with `cache_scope_to_end_user: true`, where it partitions the response cache per end user; see [Caching](../caching.md).

### Tools and tool_choice

Buffered `tool_calls` use OpenAI's native nesting, so `resp.choices[0].message.tool_calls[0].function.arguments` works as usual. The SDK's default `tool_choice` strings `"auto"`, `"required"`, `"none"` and the object `{"type":"function","function":{"name":...}}` are accepted since gateway/v0.18.0; in gateway/v0.17.0 and earlier the string form was `400` and the object form `502`. Any other shape is `400` with `code` `invalid_tool_choice` and `param` `tool_choice`, as is a forced tool that `tools[]` does not define.

Streaming tool-call deltas are not OpenAI-shaped: each element is flat `{index, id, name, arguments_json}` with no `function` nesting, so the SDK's `delta.tool_calls[].function` is not populated. Read the fragments from the raw chunk (`chunk.model_dump()`) and concatenate `arguments_json` per `index` (check your client version).

### Kelvran request extensions

Pass fields the SDK does not know through `extra_body`, for example `{"prompt_id": "...", "prompt_variables": {...}}` for server-side prompts ([Prompt management](../prompt-management.md)) or `"thinking_binding_mode"`. For images and documents, add a `parts` array to the message dict itself (`{"type": "image", "media_type": "image/png", "data": "<base64>"}` or `"url"`); the SDK forwards message dicts as given (check your client version). Structured output uses the standard `response_format={"type": "json_schema", ...}`; see [Structured output](../structured-output.md).

### Embeddings

`POST /v1/embeddings` exists for `openai` and `bedrock` deployments. Its response is `{model, data[{index, embedding}], usage}` and omits OpenAI's `object` fields, so check your client version before relying on the typed `client.embeddings.create` result; the reference shape is in the [data-plane API](../../reference/data-plane-api.md). Sending a chat model's name here is `400` `not_an_embedding_model`; a missing `model` or empty `input` is `400` `missing_required_parameter` with `param` naming the field. The SDK's default `encoding_format: "base64"` is ignored by the gateway, which always returns float arrays; the SDK passes those through, so `.data[i].embedding` works and only the `object` attributes are absent.

## How errors surface in this client

Every data-plane error body is `{"error":{"message","type","param","code"}}`, all four keys always present. The SDK picks its exception class from the HTTP status; `.status_code` is the status, `.code`, `.type` and `.param` are copied from the envelope's `error` object, `.body` is that `error` object itself (so the gateway's text is `.body["message"]`), and `.message` is the SDK's own `Error code: <status> - <body>` string rather than the envelope's `message`; response headers are on `.response.headers` (check your client version). Only `type` and `code` are stable; `message` text is not. The JSON envelope ships since gateway/v0.18.0; in gateway/v0.17.0 and earlier the same statuses carry a `text/plain` body and `.code` is empty.

| Status | `type` | `code` | Meaning | `Retry-After` |
|---|---|---|---|---|
| 401 | `authentication_error` | null / `invalid_api_key` | Header missing or malformed / secret matches no key | no |
| 403 | `permission_error` | `model_not_allowed` | Key may not call this model | no |
| 403 | `permission_error` | `source_ip_not_allowed` | Client IP outside `allowed_source_cidrs` | yes (recorded gap: this local rejection falls into the upstream-error outcome) |
| 400 | `invalid_request_error` | `model_not_found` | Unknown `model`; Kelvran uses 400, not OpenAI's 404 | no |
| 400 | `invalid_request_error` | `invalid_json`, `invalid_request`, `invalid_tool_choice`, `empty_messages`, `content_policy_violation`, `invalid_prompt_reference` | Request-shape problems and guardrail blocks | no, except `invalid_prompt_reference`, which carries one (recorded gap: the prompt-reference errors fall into the upstream-error outcome) |
| 400 | `invalid_request_error` | `response_format_unsupported` | `response_format` with no capable deployment (`param` `response_format`; since gateway/v0.19.0, `502` before) | no |
| 413 | `invalid_request_error` | `request_too_large` | Body over 32 MiB | no |
| 422 | `invalid_request_error` | `idempotency_key_reused` | `Idempotency-Key` reused with a different body (`param` `Idempotency-Key`; since gateway/v0.19.0, `502` before) | no |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | Key throttled; transient | yes |
| 429 | `insufficient_quota` | `insufficient_quota` | Key's `budget_usd` exhausted; permanent | no |
| 501 | `server_error` | `streaming_not_configured`, `embeddings_not_configured` | Feature not configured on this gateway | `streaming_not_configured`: yes (recorded gap; unreachable in a `cmd/gateway` build, where the stream caller is always wired); `embeddings_not_configured`: no |
| 502 | `server_error` | `upstream_error` | Provider answered non-2xx or a transport failure | yes |
| 503 | `server_error` | `deployment_capacity_exceeded` | Deployment at capacity | yes |

An upstream `502` message is redacted: `upstream provider returned status N` when the provider answered (`N=401` almost always means the gateway's own upstream credential is wrong or unset), or `upstream call failed for model "<model>"` for a transport or decode failure (that redaction first shipped in gateway/v0.18.0). The `400` `response_format_unsupported` and `422` `idempotency_key_reused` bodies (local rejections, `502` before gateway/v0.19.0) keep their own Kelvran-authored text. The provider's full error text is only in the gateway log. Non-streaming upstream calls are cut off after 60 s and surface as this `502`. The full table is in [Error codes](../../reference/error-codes.md).

### 429: rate_limit_error versus insufficient_quota

Both arrive as the same `429` exception class, so branch on `code`:

```python
import time
from openai import OpenAI, RateLimitError

client = OpenAI(base_url="http://localhost:8080/v1", api_key=os.environ["KELVRAN_KEY"], max_retries=0)
try:
    resp = client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
except RateLimitError as e:                      # 429, check your client version
    if e.code == "insufficient_quota":
        raise                                     # budget exhausted: do not retry
    time.sleep(int(e.response.headers.get("Retry-After", "1")))
    # retry once here
```

- `rate_limit_exceeded` and `concurrency_limit_exceeded` are transient. `Retry-After` is an integer number of seconds (minimum 1) from a per-key exponential backoff with equal jitter: the Nth consecutive rejection waits between half and all of 500 ms × 2^(N-1), capped at 30 s. Sleep for it, then retry. (Only a `502` can carry a larger value: there the provider's own `Retry-After`, capped at 60 s, acts as a floor.)
- `insufficient_quota` means the key's `budget_usd` is spent. No `Retry-After` is sent. Retrying only produces the same answer until the budget window resets or an operator raises the cap. LangChain and LiteLLM stop retrying on this `type`; the OpenAI SDK's default policy retries every `429` (check your client version), which is why the example sets `max_retries=0` and inspects `code` itself.

## What Kelvran adds that the client ignores

The SDK parses what it knows and keeps the rest; read extras with `model_dump()` or `getattr` (check your client version).

- `usage.cache_read_tokens`, `usage.cache_creation_tokens`, `usage.reasoning_tokens` (present only when non-zero).
- `choices[].message.reasoning_blocks[]`: opaque extended-thinking blocks from Anthropic and Bedrock models. Echo them back unchanged, in order, on the assistant message of the next turn, or those providers return a hard `400`. Building the follow-up turn from `resp.choices[0].message.model_dump()` keeps them; a hand-written dict with only `role` and `content` drops them.
- `input_transformations[]` on Anthropic responses, and `reasoning_blocks[]` on streaming deltas.
- `/v1/models` entries also carry `kind` (`chat` or `embedding`), `display_name`, `description`, `type` and `created_at`.
- Headers: `X-Kelvran-Overhead-Duration-Ms` on buffered responses, `Retry-After` on the statuses in the table above.

## Verify it worked

Run the buffered call through `with_raw_response` and print the status, the gateway header and the envelope field:

```python
raw = client.chat.completions.with_raw_response.create(
    model="gpt-4o", messages=[{"role": "user", "content": "hi"}], max_tokens=8,
)
print(raw.status_code, raw.headers.get("X-Kelvran-Overhead-Duration-Ms"), raw.parse().object)
```

Expected output is `200`, a small integer such as `3`, and `chat.completion`. On gateway/v0.17.0 the third value prints `None` (no `object` field yet). Then prove the key check is live with a wrong secret:

```python
from openai import AuthenticationError
try:
    OpenAI(base_url="http://localhost:8080/v1", api_key="wrong").models.list()
except AuthenticationError as e:                 # 401, check your client version
    print(e.status_code, e.code, e.body["message"])   # e.message is the SDK's own "Error code: 401 - {...}" string
```

Expected: `401 invalid_api_key dataplane: auth: identity: invalid virtual key` (only the status and `code` are stable; the message prefix is not a contract). On gateway/v0.17.0 `/v1/models` is a `404` before any key check, so this call raises `NotFoundError` there rather than `AuthenticationError`; send a wrong-key `chat.completions.create` instead, where the body is `text/plain`, `e.code` is `None` and `e.body` is the bare message string (print `e.body`, not `e.body["message"]`). The same two checks from the shell are in [curl](curl.md).

## Not available today

- OpenAI's array-of-parts `content` on inbound messages. `content` is a string; multimodal input goes in Kelvran's `parts`. An SDK multimodal request fails with `400` `invalid_json`.
- The dropped request fields listed in step 2 (`n`, `top_p`, `stop`, `seed`, `user`, `logprobs`, penalties, `logit_bias`, `max_completion_tokens`, `parallel_tool_calls`, `stream_options`, `store`, `metadata`, `reasoning_effort`). They do not error; they do nothing.
- OpenAI-shaped streaming tool-call deltas (`delta.tool_calls[].function.{name,arguments}`).
- The Responses API, the legacy Completions API, and the images, audio, files, batches and fine-tuning routes: only `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz` and `GET /readyz` exist. Anything else is a `404`.
- `tool_choice` types `allowed_tools` and `custom`.
- OpenAI's `404` for an unknown model; Kelvran returns `400` `model_not_found` by recorded decision.
- A Redis-backed `Idempotency-Key` store; deduplication is per gateway instance.
- `x-api-key` authentication and the Anthropic Messages API, so the Anthropic SDK and Claude Code cannot use the gateway as a base URL.
- A first-party Kelvran SDK and an OpenAPI document for `/v1/*`; the `base_url` override described here is the integration path ([Why no SDK](../../explanation/why-no-sdk.md)).
- A CI compatibility matrix that runs the official `openai` package against the gateway.

## Related pages

- [Data-plane API reference](../../reference/data-plane-api.md), [Error codes](../../reference/error-codes.md), [Compatibility](../../reference/compatibility.md), [Config reference](../../reference/config.md).
- [Streaming](../streaming.md), [Structured output](../structured-output.md), [Caching](../caching.md), [Troubleshooting](../troubleshooting.md).
- Other clients: [openai-node](openai-node.md), [curl](curl.md).
- [Failure modes](../../operations/FAILURE-MODES.md) for what each status means on the gateway side; [Versioning](../../VERSIONING.md) for which parts of the wire contract are stable.
