# Call the Kelvran gateway with curl

This page shows how to drive the gateway's data plane from a shell with `curl`: one buffered chat completion, one streamed completion, a model listing, and the exact way errors and rate limits show up in a terminal. It is for a developer who already uses `curl` against OpenAI-shaped APIs and wants the Kelvran-specific details without reading Go.

**Use this when** you want to test a virtual key, a model name or a deployment by hand, script a smoke check, or see the raw headers and body a client library would otherwise hide.

## Prerequisites

- A running gateway. The default `listen_addr` in [`gateway/config.example.yaml`](../../../gateway/config.example.yaml) is `:8080`; this page uses `http://localhost:8080`. To bring one up, follow the [quickstart](../../tutorials/quickstart.md).
- The raw secret of a virtual key whose SHA-256 is a `virtual_keys.<name>.key_hash` in the gateway's config. The client sends the secret, never the hash. See [virtual keys and budgets](../virtual-keys-and-budgets.md).
- A model name that one of your `deployments.<name>.model` entries serves. The examples use `gpt-4o`; replace it with your own. See [the config reference](../../reference/config.md).
- `curl`. Nothing else is installed or imported; the error-handling loop below uses only POSIX shell plus `grep`, `head`, `awk`, `cut`, `mktemp` and `sleep`.

## Steps

### 1. Point curl at the gateway

The base URL ends in `/v1`. Every `/v1` route sits under it and every one of them requires `Authorization: Bearer <raw virtual-key secret>`; `x-api-key` is accepted on `GET /v1/models` and `POST /v1/messages`, as the bearer's alias. `GET /healthz`, `GET /readyz` and `HEAD /api/hello` sit outside `/v1` and take no credentials.

```bash
export KELVRAN_BASE_URL="http://localhost:8080/v1"
export KELVRAN_KEY="<raw virtual-key secret>"
```

If the gateway runs the unmodified example config, use the `team-beta` key: its secret is `example-team-beta-secret-do-not-use`. Do not use `team-alpha` from a shell: that key carries `allowed_source_cidrs: 10.0.0.0/8` in the example, so a request from `localhost` (`127.0.0.1` or `::1`), or from any other address outside `10.0.0.0/8`, is rejected with `403 source_ip_not_allowed`. Both secrets are public (they are printed in the config's own comments) and must never guard anything real.

### 2. Send one buffered chat completion

`-D -` prints the response headers before the body so you can see what the gateway adds.

```bash
curl -s -D - "$KELVRAN_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Say hello in five words."}],"max_tokens":64}'
```

What you should see: `HTTP/1.1 200 OK`, `Content-Type: application/json`, an `X-Kelvran-Overhead-Duration-Ms: <integer>` header, then one JSON line shaped like this (values vary):

```json
{"id":"chatcmpl-…","object":"chat.completion","created":1760000000,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there, nice to meet you."},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":8,"total_tokens":21}}
```

The gateway reads these request fields: `model`, `messages[].role`, `messages[].content` (a string), `messages[].tool_calls`, `messages[].tool_call_id`, `temperature`, `max_tokens`, `top_p`, `stop`, `tools`, `tool_choice`, `stream`, `response_format`. Any other OpenAI field (`n`, `seed`, `user`, `max_completion_tokens`, `stream_options`, `messages[].name`, …) is dropped silently; the request still succeeds. The full field table is in [compatibility](../../reference/compatibility.md). `object` and `created`, and a gateway-minted `id` for providers that return none, first shipped in gateway/v0.18.0.

### 3. Stream a completion

`-N` turns off curl's output buffering so frames appear as they arrive. The body is identical except for `"stream":true`.

```bash
curl -sN "$KELVRAN_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

What you should see: `Content-Type: text/event-stream` (add `-D -` to confirm), then one `data: {…}` frame per chunk and a final `data: [DONE]`:

```text
data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1760000000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}

data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":1760000000,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

Every frame of one stream shares one `id` and `created`. A `usage` object appears on a chunk only when the provider sends it; do not assume the last chunk carries it. Streaming responses do not carry `X-Kelvran-Overhead-Duration-Ms`. Frame-by-frame detail, including tool-call deltas, is in [streaming](../streaming.md).

### 4. List the models this key may call

```bash
curl -s "$KELVRAN_BASE_URL/models" -H "Authorization: Bearer $KELVRAN_KEY"
```

What you should see: one document with one entry per canonical `model` name the key is allowed to use (filtered by the key's `allowed_models`), sorted by `id`:

```json
{"object":"list","data":[{"id":"gpt-4o","object":"model","created":1760000000,"owned_by":"openai","type":"model","created_at":"2026-10-08T12:00:00Z","display_name":"gpt-4o","description":"","kind":"chat"}],"first_id":"gpt-4o","last_id":"gpt-4o","has_more":false}
```

`limit` defaults to every model and is clamped to 1000; `after_id` and `before_id` page through the list and cannot be combined. The path is exact: `$KELVRAN_BASE_URL/models/` with a trailing slash is a 404. `GET /v1/models` first shipped in gateway/v0.18.0.

## Variants

### Replay-safe retries with `Idempotency-Key`

Add `-H "Idempotency-Key: <your unique token>"` to a chat request (buffered or streamed). A second request with the same key and the same body within 10 minutes replays the stored response; the same key with a different body fails with `422` `idempotency_key_reused` (since gateway/v0.19.0; `502` `upstream_error` before). Details are in the [data-plane API reference](../../reference/data-plane-api.md).

### Per-end-user cache scope

Add `-H "X-Kelvran-End-User-Id: <opaque id>"`. It only matters for a virtual key with `cache_scope_to_end_user: true`; see [caching](../caching.md).

### Trace correlation

Add `-H "traceparent: <W3C trace context>"` or `-H "baggage: agent_run_id=run-42"`. The `agent_run_id` baggage member becomes the `kelvran.agent_run_id` span attribute. See [TELEMETRY](../../operations/TELEMETRY.md).

## How errors surface in curl

`curl` exits 0 on any HTTP status, so a script must look at the status itself. The two usual ways are `--fail-with-body` (non-zero exit on 4xx/5xx, body still printed) and `-w '%{http_code}'` (status appended to the output). Every error body from the three OpenAI-shaped `/v1/*` routes is the OpenAI envelope (`POST /v1/messages` answers in Anthropic's), written compactly with no trailing newline. A path the gateway does not serve (for example `/v1/models/` with a trailing slash, or `/v1/completions`) is Go's plain-text `404 page not found`, with no envelope:

```text
HTTP/1.1 429 Too Many Requests
Content-Type: application/json; charset=utf-8
X-Content-Type-Options: nosniff
Retry-After: 1

{"error":{"message":"dataplane: rate limit exceeded","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}
```

All four keys are always present (`null` when unknown). Only `type` and `code` are a stable contract; the `message` text is not, so grep on `"code":"…"` rather than on message words. The JSON envelope ships since gateway/v0.18.0; in gateway/v0.17.0 and earlier the same statuses carried a `text/plain` body. Statuses you will meet from a shell:

| Status | `type` | `code` | Meaning | `Retry-After` |
|---|---|---|---|---|
| 401 | `authentication_error` | `null` | `Authorization` header missing or not `Bearer …` | no |
| 401 | `authentication_error` | `invalid_api_key` | secret's SHA-256 matches no `key_hash` | no |
| 403 | `permission_error` | `model_not_allowed` | key's `allowed_models` rejected this `model` | no |
| 403 | `permission_error` | `source_ip_not_allowed` | key's `allowed_source_cidrs` rejected the address you are calling from | yes (recorded gap) |
| 400 | `invalid_request_error` | `model_not_found` | no deployment serves this `model` (not OpenAI's 404) | no |
| 400 | `invalid_request_error` | `invalid_json` / `invalid_request` / `invalid_tool_choice` / `empty_messages` | body shape problem; `param` names the field when known | no |
| 405 | `invalid_request_error` | `method_not_allowed` | wrong method; `Allow` header lists the right one | no |
| 413 | `invalid_request_error` | `request_too_large` | body over 32 MiB | no |
| 429 | `rate_limit_error` | `rate_limit_exceeded` / `concurrency_limit_exceeded` | key throttled; transient | yes |
| 429 | `insufficient_quota` | `insufficient_quota` | key's `budget_usd` spent; permanent until raised | no |
| 502 | `server_error` | `upstream_error` | provider answered non-2xx (`upstream provider returned status N`) or the call failed before an answer (`upstream call failed for model "<m>"`) | yes |
| 503 | `server_error` | `deployment_capacity_exceeded` | deployment at its own ceiling | yes |

The full table, including the 501 and prompt-reference codes, is in [error codes](../../reference/error-codes.md).

On a stream, a failure before the first chunk is an ordinary status plus the envelope. A failure after the first chunk arrives as one in-band frame and the stream ends without `[DONE]`; the HTTP status stays 200. The in-band frame ships since gateway/v0.18.0; in gateway/v0.17.0 and earlier a mid-stream failure appended the plain-text error message to the open SSE body with no `data:` prefix (and still no `[DONE]`):

```text
data: {"error":{"message":"upstream provider returned status 503","type":"server_error","param":null,"code":"upstream_error"}}
```

A stream that ends without `data: [DONE]` is therefore never complete. A 502 `upstream_error` never echoes the provider's body; the full text is only in the gateway's `chat_completion` log line ([FAILURE-MODES](../../operations/FAILURE-MODES.md)).

## What curl shows that OpenAI's API does not have

Unlike a client library, curl prints everything, so expect these Kelvran additions and leave them alone if your script compares bodies against OpenAI's:

- `X-Kelvran-Overhead-Duration-Ms` response header on buffered chat completions: the gateway's own added latency in milliseconds (total handler time minus the upstream round-trip).
- `usage.cache_read_tokens`, `usage.cache_creation_tokens`, `usage.reasoning_tokens`: present only when non-zero.
- `choices[].message.reasoning_blocks[]` (and `delta.reasoning_blocks[]` on streams): provider reasoning content. For Anthropic and Bedrock, echo it back unchanged on later turns.
- `choices[].message.refusal` (openai and openaicompat only) and a top-level `input_transformations[]` (Anthropic only).
- A top-level `stop_reason` and, when a stop sequence matched, `stop_sequence` on Anthropic and Bedrock responses, and, inside `choices[]`, on the streaming chunk that sets `finish_reason` (since gateway/v0.19.0); `finish_reason` is unchanged.
- On `GET /v1/models`: `kind` (`chat` or `embedding`) on every entry, plus the Anthropic-dialect fields `type`, `created_at`, `display_name`, `description`, `first_id`, `last_id`, `has_more` in the same document.
- Streaming tool-call deltas are flat `{index, id, name, arguments_json}` objects, not OpenAI's `function.{name,arguments}` nesting.

## What to do on 429: `rate_limit_error` versus `insufficient_quota`

Both are HTTP 429. Read `error.type` before retrying:

- `rate_limit_error` (`rate_limit_exceeded`, `concurrency_limit_exceeded`): transient. `Retry-After` is whole seconds (minimum 1): a per-key backoff whose ceiling starts at 500 ms, doubles per consecutive rejection and caps at 30 s, jittered between half and all of that ceiling and rounded up to the next second (on a `502` that carries the provider's own `Retry-After`, the larger of the two is sent, with the provider's value capped at 60 s). Sleep for it, then retry.
- `insufficient_quota`: the key's `budget_usd` is spent. There is no `Retry-After` and retrying cannot succeed; stop and raise the budget or use another key ([virtual keys and budgets](../virtual-keys-and-budgets.md)).

`curl --retry N` treats every 429 as transient and retries budget rejections too, so do not use it unattended against the gateway. Which statuses and whether `Retry-After` is honoured depend on your curl build: check your client version. This loop makes the distinction explicit:

```bash
body=$(mktemp)
headers=$(mktemp)
for attempt in 1 2 3 4 5; do
  status=$(curl -s -o "$body" -D "$headers" -w '%{http_code}' \
    "$KELVRAN_BASE_URL/chat/completions" \
    -H "Authorization: Bearer $KELVRAN_KEY" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}')
  if [ "$status" = "200" ]; then cat "$body"; echo; break; fi
  err_type=$(grep -o '"type":"[^"]*"' "$body" | head -1 | cut -d'"' -f4)
  if [ "$status" = "429" ] && [ "$err_type" = "insufficient_quota" ]; then
    echo "budget exhausted for this key; stop retrying" >&2; break
  fi
  if [ "$status" = "429" ]; then
    wait=$(awk 'tolower($1)=="retry-after:" {print $2+0}' "$headers")
    echo "attempt $attempt: 429 $err_type, sleeping ${wait:-1}s" >&2
    sleep "${wait:-1}"; continue
  fi
  echo "attempt $attempt: HTTP $status" >&2; cat "$body"; echo; break
done
rm -f "$body" "$headers"
```

The gateway sets the same `Retry-After` on `502 upstream_error` and `503 deployment_capacity_exceeded`, which are also retryable; the loop above stops on them. To retry those too, change the sleep branch's `if [ "$status" = "429" ]` to `if [ "$status" = "429" ] || [ "$status" = "502" ] || [ "$status" = "503" ]` (keeping the `insufficient_quota` check before it) and print `$status` in place of the literal `429` in its message.

## Verify it worked

Confirm the key, the base URL and the route in one call. On gateway/v0.18.0 and later use the models route:

```bash
curl -s -o /dev/null -w '%{http_code}\n' "$KELVRAN_BASE_URL/models" -H "Authorization: Bearer $KELVRAN_KEY"
```

What it returns: `200`. A `401` means the secret does not hash to any configured `key_hash`; a `403` means the key's `allowed_source_cidrs` excludes the address you are calling from; a `404` means either `KELVRAN_BASE_URL` is wrong (it must end in `/v1` with no trailing slash) or the gateway is `gateway/v0.17.0`, which has no `GET /v1/models`; connection refused means the gateway is not listening where `KELVRAN_BASE_URL` says. On `gateway/v0.17.0` use the chat route instead and expect `200`:

```bash
curl -s -o /dev/null -w '%{http_code}\n' "$KELVRAN_BASE_URL/chat/completions" -H "Authorization: Bearer $KELVRAN_KEY" -H "Content-Type: application/json" -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":1}'
```

Then confirm the error path is the envelope, with a deliberately wrong key:

```bash
curl -s -i "$KELVRAN_BASE_URL/chat/completions" \
  -H "Authorization: Bearer wrong-do-not-use" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

What it returns on gateway/v0.18.0 and later: `HTTP/1.1 401 Unauthorized`, `Content-Type: application/json; charset=utf-8`, and the body `{"error":{"message":"dataplane: auth: identity: invalid virtual key","type":"authentication_error","param":null,"code":"invalid_api_key"}}`. On gateway/v0.17.0 the status is the same but the body is `text/plain; charset=utf-8` with the single line `dataplane: auth: identity: invalid virtual key` and no JSON.

## Not available today

- No `POST /v1/messages/count_tokens` yet (Claude Code falls back to a character-based estimate). Seven routes exist: `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `POST /v1/messages` (the Anthropic Messages shape; `x-api-key` read on it and on `GET /v1/models`), `GET /healthz`, `GET /readyz`, `HEAD /api/hello`.
- No OpenAI content-array messages (`"content":[{"type":"text",…}]`); `content` must be a string. Images and documents go in Kelvran's own `parts` array; sending the OpenAI array form is a `400 invalid_json`.
- No OpenAI-shaped streaming tool-call deltas; the flat shape above is what you get.
- No `X-Kelvran-Overhead-Duration-Ms` on streaming responses.
- No OpenAI Responses, Completions, images, audio or files routes.
- No OpenAPI document to generate requests from.
- No 404 for an unknown model; it is `400 model_not_found` by design.

## Related

- [Data-plane API reference](../../reference/data-plane-api.md) for every header and body field.
- [Error codes](../../reference/error-codes.md) for the complete `type`/`code` table.
- [Compatibility](../../reference/compatibility.md) for which OpenAI fields are honoured, ignored or rejected.
- [Config reference](../../reference/config.md) for `listen_addr`, `virtual_keys`, `deployments`, `allowed_models`, `budget_usd` and `cache_scope_to_end_user`.
- [Structured output](../structured-output.md) for `response_format` with curl.
- [Troubleshooting](../troubleshooting.md) when a status in the table above is not the one you expected.
- [OpenAI Python](openai-python.md) and [OpenAI Node](openai-node.md) for the same calls through a client library.
- [docs/VERSIONING.md](../../VERSIONING.md) for what in this page is a stable contract.
