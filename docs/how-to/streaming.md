# Stream a chat completion over SSE

This page shows how to request a streamed chat completion from the Kelvran gateway and how to consume every kind of frame it sends: content deltas, the usage frame, the `[DONE]` sentinel and the in-band error frame. It is for application developers who write or configure an SSE consumer against `POST /v1/chat/completions`, and for operators who need to know what the gateway does when a stream fails before or after its first byte.

**Use this when** your client needs tokens as they are generated instead of one buffered JSON response.

## Prerequisites

- A running gateway. The default `listen_addr` in [`gateway/config.example.yaml`](../../gateway/config.example.yaml) is `:8080`; this page uses `http://localhost:8080`.
- A virtual key secret in the environment variable `KELVRAN_KEY`. Never paste the secret into a file or a shell history. See [virtual keys and budgets](virtual-keys-and-budgets.md).
- A model name that one of your `deployments[].model` entries serves. The examples use `gpt-4o`; replace it with your own. See [the config reference](../reference/config.md).
- A client that reads the response body incrementally. With curl that is the `-N` flag.

## Steps

### 1. Send `stream: true`

Streaming uses the standard OpenAI request field. Nothing else in the body changes.

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

The gateway validates the whole body first, then branches on `stream`. Two optional request headers also apply to streams: `X-Kelvran-End-User-Id` (scopes the cache to an end user, see [caching](caching.md)) and `Idempotency-Key` (replays a completed request, see [Cache hit or `Idempotency-Key` replay](#cache-hit-or-idempotency-key-replay) under Variants). Header names are listed in the [data-plane API reference](../reference/data-plane-api.md).

### 2. Read the response headers

On a stream the gateway sets three headers before anything is written:

```text
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

The status is `200` once the first frame is flushed. `X-Kelvran-Overhead-Duration-Ms` is set on buffered responses only; a stream never carries it, because the value is only known after the pipeline returns, by which time the first flushed chunk has already committed the response headers.

### 3. Parse frames

Every frame is one line, `data: <json>`, followed by a blank line, flushed immediately. The gateway writes no `event:`, `id:` or `retry:` fields and no comment keep-alives, so a parser needs to handle only `data:` lines.

Each JSON payload is a `chat.completion.chunk` with fields in this order: `id`, `object`, `created`, `model`, `choices`, `usage`.

- `id`, `object` and `created` are the same on every frame of one stream. A provider id is kept when present; otherwise the gateway mints `chatcmpl-` followed by 32 lowercase hex characters. This per-frame envelope is on main since 2026-10-08, not in gateway/v0.17.0, where Bedrock and Gemini frames carry an empty `id` and an empty `model`, and no frame of any provider carries `object` or `created`.
- `choices[].finish_reason` is always present and is `null` until the provider sets it.
- `choices[].delta` carries `role` (first frame only), `content`, `tool_calls`, `reasoning_blocks` and `refusal`. All are omitted when empty, so an empty delta is `{}`.
- `usage` is omitted unless this frame carries it.

A successful stream from an OpenAI deployment looks like this (values illustrative):

```text
data: {"id":"chatcmpl-3f9e...","object":"chat.completion.chunk","created":1759900000,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"hello "},"finish_reason":null}]}

data: {"id":"chatcmpl-3f9e...","object":"chat.completion.chunk","created":1759900000,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-3f9e...","object":"chat.completion.chunk","created":1759900000,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}

data: [DONE]
```

### 4. Handle the usage frame per provider

Whether a live stream carries `usage` depends on the provider behind the deployment:

| Provider | `usage` on a live stream |
|---|---|
| `openai`, `openaicompat` | On the provider's final frame, with `"choices":[]` (the gateway always asks these upstreams for usage). On main since 2026-10-09; `gateway/v0.17.0` sends `"choices":null` there, which LlamaIndex's `stream_chat` and the Vercel AI SDK reject |
| `gemini` | On every content frame whose Gemini `usageMetadata` carries a non-zero count; the gateway does not restrict it to the last frame, so use the last `usage` seen |
| `anthropic`, `bedrock` | Never in any frame; usage is used for billing only |

Do not assume the last frame carries `usage`. Billing does not depend on the client seeing it: when the provider sends none, the gateway bills an estimate and increments `kelvran.streaming.cost_estimated` (see [metrics and logs](../reference/metrics-and-logs.md)).

### 5. Stop on `data: [DONE]`

A successful stream ends with exactly one `data: [DONE]` frame after the last chunk. Treat a stream that ends without it as incomplete.

### 6. Handle errors in both phases

**Before the first chunk.** Any rejection (missing or invalid key, model not allowed, rate limit, budget, concurrency, no deployment for the model, an upstream that never sent a byte) is an ordinary HTTP error. The status comes from the same mapping as buffered requests, for example `400`, `401`, `403`, `429`, `502`, `503`; `400` `model_not_found` is the routing miss, and `503` is a deployment at its own capacity (an all-unhealthy pool fails open rather than rejecting, row U3 of [FAILURE-MODES](../operations/FAILURE-MODES.md)). On main, not in gateway/v0.17.0, the body is the OpenAI-shaped envelope with `Content-Type: application/json; charset=utf-8` and `X-Content-Type-Options: nosniff`, replacing the `text/event-stream` header set in step 2:

```text
HTTP/1.1 429 Too Many Requests
Content-Type: application/json; charset=utf-8
X-Content-Type-Options: nosniff

{"error":{"message":"...","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}
```

In gateway/v0.17.0 the same statuses apply and the body is `text/plain`. `Retry-After` (whole seconds, minimum 1) is present when the error is classified rate-limited, deployment-capacity or upstream-error: `429` `rate_limit_exceeded` and `concurrency_limit_exceeded`, `502` and `503` carry it (on `502` the upstream's own `Retry-After`, capped at 60 s, floors the value); `429` `insufficient_quota` (the example above) does not. Only the `429` case is part of the stable surface in [docs/VERSIONING.md](../VERSIONING.md); the per-code column and the `type`/`code` vocabulary are in [error codes](../reference/error-codes.md). Message text is not a stable contract.

**After the first chunk.** The `200` is already committed. On main since 2026-10-08, not in gateway/v0.17.0, the gateway writes exactly one in-band frame and ends the stream without `[DONE]`:

```text
data: {"error":{"message":"upstream call failed for model \"gpt-4o\"","type":"server_error","param":null,"code":"upstream_error"}}
```

The frame carries the same `type` and `code` a buffered error would. Messages are redacted: a provider's own in-band error frame becomes `<provider>: upstream provider returned a mid-stream error`; a read error (including the idle timeout) or a decode failure becomes `upstream call failed for model "<model>"`. `upstream provider returned status N` only occurs before the first chunk: a non-2xx upstream status is detected before any chunk is read. In gateway/v0.17.0 and earlier there is no error frame: the handler falls through to the buffered error writer, which appends the error message as a bare text line after the last chunk (no `data:` prefix, so SSE parsers ignore it; for transport failures it is the unredacted dial or timeout text) and the stream ends without `data: [DONE]`, which is the only reliable signal.

### 7. Reassemble tool calls and reasoning blocks

Tool-call deltas use Kelvran's flat shape, keyed by `index`:

```json
{"index":0,"id":"call_1","name":"get_weather","arguments_json":"{\"city\":"}
```

`id` and `name` arrive on the frame that introduces the call; later frames append fragments of `arguments_json`, which you concatenate in order. This is not OpenAI's nested `function: {name, arguments}` shape; a consumer written for that shape must read `name` and `arguments_json` instead.

Reasoning content streams as `delta.reasoning_blocks[]` with `index`, `text`, `signature`, `redacted` and `data`. `text` fragments concatenate per `index`; `signature` and `data` arrive whole.

## Variants

### Cache hit or `Idempotency-Key` replay

When the request hits the response cache (L1, L2 or L3) or replays a completed `Idempotency-Key`, the gateway synthesizes the stream from the stored response: one frame per choice carrying the full `content`, `role`, `tool_calls`, `reasoning_blocks` and `finish_reason` together, then one usage-only frame with `"choices":null`, then `[DONE]`. The `id` and `created` are the stored ones. An entry cached by gateway/v0.17.0 or earlier has no stored `id` or `created`; each replay of it mints a fresh `chatcmpl-` id and uses the replay time, until the entry expires. See [caching](caching.md) and [the cache gate](../explanation/cache-gate.md).

### Long outputs: the runaway and top-up guards

Two guards can end a live stream early. Both end it as a normal truncated stream with `[DONE]`, never with an error frame, and no frame sets `finish_reason` (`null` on every live frame; a cached replay of the truncated response carries `""`).

- Runaway guard: output is cut once accumulated characters exceed `max_tokens * 4 * 10`, or 750,000 characters when `max_tokens` is unset. Log event: `streaming_runaway_guard_triggered`.
- Reservation top-up: on every decoded batch the gateway raises this request's TPM and budget reservations to match the estimated output. When the key has no headroom left the stream is cut. Log event: `streaming_midstream_reservation_topup_exhausted`.

A stream cut by either guard is still written to the response cache (only `finish_reason: "length"` blocks the write) and is replayed, truncated, to later cache hits for the entry's TTL; treat an empty `finish_reason` on a replay as a possible guard truncation. The U5 known-gaps note in [FAILURE-MODES](../operations/FAILURE-MODES.md) records this.

If the Redis-backed limiter or budget tracker errors during a top-up, the stream keeps its prior reservation and continues. On main since 2026-10-08, not in gateway/v0.17.0, this is logged (`ratelimit_tpm_backend_unavailable` / `budget_backend_unavailable` with `op=mid_stream_topup`) and counted (`kelvran.ratelimit.fail_open` / `kelvran.budget.fail_open`) once per stream. Row R4 of [FAILURE-MODES](../operations/FAILURE-MODES.md) covers this. Key limits are configured per [virtual keys and budgets](virtual-keys-and-budgets.md).

### Idle timeout and fallback

The upstream stream call is bounded by a 60-second idle window, reset on every byte of progress, not by a whole-call deadline. It is a constant in the gateway binary, not a `config.yaml` key. An upstream that fails before any chunk reaches the client is retried on the next deployment (the legacy single hop, or the `fallback_chains` walk); the check runs before every hop. Once one chunk is out, no further hop is attempted. See [routing and failover](routing-and-failover.md) and rows U1 and U5 of [FAILURE-MODES](../operations/FAILURE-MODES.md).

### Client disconnects

A stream that fails after its first chunk, including a client disconnect, is still billed: from provider usage if it arrived, otherwise from an estimate of 4 characters per token over the delivered content and the serialized prompt. Row U5 of [FAILURE-MODES](../operations/FAILURE-MODES.md) records the disclosed gap where a decode error or a failed write bills nothing.

## Verify it worked

Check the headers and status:

```bash
curl -sN -D - -o /dev/null http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

Expected: a `HTTP/1.1 200 OK` line, `Content-Type: text/event-stream` and `Cache-Control: no-cache`.

Check the sentinel:

```bash
curl -sN http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  | grep -v '^$' | tail -n 1
```

Expected: `data: [DONE]`. A last line starting with `data: {"error":` means the upstream failed mid-stream; see step 6. A `{"error":...}` body with no `data:` prefix means the request was rejected before the first chunk; the HTTP status says why.

## Not available today

- No client-controllable `stream_options` or `include_usage`: the request has no such field, and a `stream_options` object sent anyway is ignored, not rejected. The gateway forces `include_usage` upstream for OpenAI-shaped providers.
- No usage frame on live Anthropic or Bedrock streams. Only cache and idempotency replays always carry one.
- No mid-stream fallback, retry or resume once a chunk has been written.
- No configurable streaming idle timeout; the 60-second idle window is fixed.
- No `X-Kelvran-Overhead-Duration-Ms` header on streams.
- No `event:`, `id:` or `retry:` SSE fields and no keep-alive comment frames.
- No OpenAI-compatible nested `function` shape for streamed tool-call deltas; `arguments_json` is the only shape.
- No WebSocket or bidirectional realtime transport. [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md) records SSE-only as the design.

## Stability

The `data: {...}` framing, the in-band `data: {"error":...}` frame, the `data: [DONE]` sentinel and the `type`/`code` vocabulary are part of the public surface in [docs/VERSIONING.md](../VERSIONING.md). Error message text is not. SSE streaming shipped in gateway/v0.1.0 (2026-09-03). The latest tagged release is gateway/v0.17.0 (2026-10-07); everything marked "on main" above is unreleased at the time of writing. See [versioning](../explanation/versioning.md) and [compatibility](../reference/compatibility.md).

## Related pages

- [curl client](clients/curl.md), [OpenAI Python client](clients/openai-python.md), [OpenAI Node client](clients/openai-node.md)
- [Structured output](structured-output.md) for `response_format` on streams
- [Troubleshooting](troubleshooting.md)
- [Data-plane API reference](../reference/data-plane-api.md), [error codes](../reference/error-codes.md), [config reference](../reference/config.md), [metrics and logs](../reference/metrics-and-logs.md)
- [TELEMETRY](../operations/TELEMETRY.md) for every `kelvran.*` counter
