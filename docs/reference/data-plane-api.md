# Data-plane API reference

This page lists every client-facing HTTP route of the Kelvran gateway: method, path, authentication, request and response JSON shapes, request and response headers, SSE framing and status codes. It is for developers who call the gateway from an application, an SDK or `curl`, and for operators who wire health probes. It describes `gateway/v0.18.0` (2026-10-10), the latest tagged release; behaviour marked "since gateway/v0.18.0" is absent from `gateway/v0.17.0` and earlier. The admin surface is a separate server and is documented in [admin-api.md](admin-api.md).

## Routes

The gateway registers exactly five routes on one `http.ServeMux`. Every pattern is an exact path. Any other path, including a trailing-slash variant such as `/v1/models/`, is Go's default plain-text 404, never a 301.

| Method | Path | Auth | Success | Wrong method |
|---|---|---|---|---|
| `POST` | `/v1/chat/completions` | `Authorization: Bearer` | `200`, `application/json`; or `200`, `text/event-stream` when `stream` is `true` | `405`, `Allow: POST`, JSON error envelope |
| `POST` | `/v1/embeddings` | `Authorization: Bearer` | `200`, `application/json` | `405`, `Allow: POST`, JSON error envelope |
| `GET` | `/v1/models` | `Authorization: Bearer` | `200`, `application/json` | `405`, `Allow: GET`, JSON error envelope |
| `GET` | `/healthz` | none | `200`, `application/json` | `405`, plain text, no `Allow` header |
| `GET` | `/readyz` | none | `200` or `503`, `application/json` | `405`, plain text, no `Allow` header |

Not available today: `POST /v1/messages`, `POST /v1/messages/count_tokens`, `POST /v1/completions`, `POST /v1/responses`, and a `/metrics` route. None is registered. The Anthropic Messages ingress is in progress as item 11 under `docs/rfcs/2026-10-09-gateway-anthropic-messages-ingress.md` (decided 2026-10-10): its parser exists, no route yet; see [mcp-a2a-status.md](../explanation/mcp-a2a-status.md) for the status of other planned surfaces.

`listen_addr` is a required `config.yaml` key with no default; [config.example.yaml](../../gateway/config.example.yaml) sets it to `:8080`, and the examples below use `http://127.0.0.1:8080`.

## Authentication

| Rule | Behaviour |
|---|---|
| Header | `Authorization: Bearer <raw virtual-key secret>`. The scheme prefix is the literal string `Bearer ` (one space). |
| Lookup | The gateway computes SHA-256 of the presented token and looks the hex digest up against the configured `key_hash` values. The raw secret is never stored. |
| Missing or malformed header | `401`, `type` `authentication_error`, `code` `null`. |
| Unknown token | `401`, `type` `authentication_error`, `code` `invalid_api_key`. |
| Expired key | `401`, `type` `authentication_error`, `code` `key_expired`: the token matches a key whose `expires_at` (RFC 3339, inclusive) has passed. The message never names the key. Since gateway/v0.18.0. |
| Rotation grace | After a rotation with a grace period, the previous secret authenticates only until its expiry instant; afterwards it is rejected as `invalid_api_key`. |
| Source-IP allowlist | A key with `allowed_source_cidrs` accepts only requests whose TCP peer address (`RemoteAddr`) falls inside one of the CIDRs. Other sources get `403`, `type` `permission_error`, `code` `source_ip_not_allowed`. The check runs once per request, immediately after bearer verification, on all four `/v1/*` handlers (buffered chat, streaming chat, embeddings, models). |
| `X-Forwarded-For` | Ignored. The allowlist reads the TCP peer only; there is no configuration knob to trust a proxy header. |
| `x-api-key` | Not accepted. Only the `Bearer ` prefix is recognised. |
| Model allowlist | A key with `allowed_models` may call only those canonical names; any other model is `403`, `code` `model_not_allowed`. `GET /v1/models` lists only the allowed names. |

Handler-level validation (method, body size, JSON shape; see each route) runs before authentication. An oversized body therefore gets `413` even without a valid key. Authentication is the first pipeline step after that.

Key configuration is covered in [virtual-keys-and-budgets.md](../how-to/virtual-keys-and-budgets.md) and [config.md](config.md).

## Request headers

| Header | Routes that read it | Meaning |
|---|---|---|
| `Authorization` | `/v1/chat/completions`, `/v1/embeddings`, `/v1/models` | Required. See Authentication. |
| `Content-Type` | none | Not inspected. The body is decoded as JSON regardless of the declared type. |
| `Idempotency-Key` | `/v1/chat/completions` (buffered and streaming) | Opaque client-chosen key. See Idempotency-Key below. Not read by `/v1/embeddings` or `/v1/models`. |
| `X-Kelvran-End-User-Id` | `/v1/chat/completions` (buffered and streaming) | Caller-supplied, unauthenticated end-user identifier. Affects response-cache partitioning only for keys with `cache_scope_to_end_user: true`. See below. Not read by `/v1/embeddings` or `/v1/models`. |
| `traceparent`, `tracestate` | all five routes (otelhttp middleware) | W3C Trace Context. The caller's span becomes the parent of the `gateway.http` server span on every route, and of the dataplane span on `/v1/chat/completions`. |
| `baggage` | `/v1/chat/completions`, `/v1/embeddings` | W3C Baggage. The member `agent_run_id` is read and recorded on the request's telemetry and in the per-agent-run in-flight breakdown. Never fabricated when absent. |
| `X-Forwarded-For` | none | Ignored for source-IP allowlists. |

Every route, `GET /v1/models`, `/healthz` and `/readyz` included, is wrapped by the otelhttp server middleware, which extracts `traceparent`, `tracestate` and `baggage` and opens one `gateway.http` server span per request. Only the two `/v1/chat/completions` paths open a dataplane span of their own (`chat <model>`); `GET /v1/models`, like `/v1/embeddings`, does not.

## Response headers

| Header | When present | Value |
|---|---|---|
| `Content-Type` | every response | `application/json` on success bodies; `application/json; charset=utf-8` on every JSON error envelope; `text/event-stream` on a stream; `text/plain; charset=utf-8` on the `405` from `/healthz` and `/readyz` and on the `/readyz` `500`. |
| `X-Content-Type-Options` | every JSON error envelope; `GET /v1/models` success; the plain-text `405` from `/healthz` and `/readyz` and the `/readyz` `500` (set by `http.Error`) | `nosniff` |
| `X-Kelvran-Overhead-Duration-Ms` | buffered `POST /v1/chat/completions` `200` only | Decimal integer string: total handler wall time minus the measured upstream round-trip, in milliseconds. Not set on streaming responses (the stream's headers are committed before the pipeline runs; a proposal is [2026-10-08-gateway-streaming-overhead-measurement.md](../rfcs/2026-10-08-gateway-streaming-overhead-measurement.md)). Not set on errors. Design: [2026-09-14-gateway-overhead-duration-header.md](../rfcs/2026-09-14-gateway-overhead-duration-header.md). |
| `Retry-After` | `POST /v1/chat/completions` errors whose outcome is rate-limited, upstream-error or deployment-capacity | Integer seconds. See Retry-After below. Never set by `/v1/embeddings` or `/v1/models`. |
| `Allow` | `405` from the three `/v1/*` routes | `POST` or `GET`. `/healthz` and `/readyz` send no `Allow` header. |
| `Cache-Control`, `Connection` | streaming `POST /v1/chat/completions` | `no-cache` and `keep-alive`. |

JSON success bodies on `/v1/chat/completions` (buffered), `/v1/embeddings` and `/v1/models` are written with `json.Encoder` and end with one newline; `/healthz` and `/readyz` write their JSON with a single `Write` and no trailing newline. Error envelopes are written with a single `Write` and no trailing newline.

## POST /v1/chat/completions

Buffered and streaming chat completions in OpenAI Chat Completions shape, plus Kelvran extension fields. Only `POST` is accepted.

### Limits and timeouts

| Bound | Value | Result when exceeded |
|---|---|---|
| Request body | 32 MiB (`maxRequestBodyBytes = 32 << 20`) | `413`, `code` `request_too_large` |
| `messages` length | 2000 | `400`, `code` `invalid_request` |
| `tools` length | 256 | `400`, `code` `invalid_request` |
| Any single `tools[].function.parameters` or `response_format.json_schema.schema` | depth 32 levels; 10000 JSON tokens | `400`, `code` `invalid_request` |
| Any single `content` string or `parts[].data` string | 8 MiB | `400`, `code` `invalid_request` |
| Non-streaming upstream call | 60 s whole call (`upstreamHTTPTimeout`) | `502`, `code` `upstream_error` |
| Streaming upstream call | 60 s idle gap, reset on every byte (`streamIdleTimeout`) | error envelope or in-band error frame |
| Server read-header timeout | 10 s | connection closed by the server |

None of these is a configuration key. They are constants in `gateway/cmd/gateway/main.go` and `gateway/internal/adapter/validate.go`.

### Handler-level validation order

Checks run in this order, before authentication. The first failure wins. All use `type` `invalid_request_error`.

| Order | Condition | Status | `code` | `param` |
|---|---|---|---|---|
| 1 | method is not `POST` | `405` | `method_not_allowed` | `null` |
| 2 | body larger than 32 MiB | `413` | `request_too_large` | `null` |
| 3 | body cannot be read | `400` | `invalid_body` | `null` |
| 4 | body is not syntactically valid JSON | `400` | `invalid_json` | `null` |
| 5 | `tool_choice` has an unrecognised shape | `400` | `invalid_tool_choice` | `tool_choice` |
| 6 | a field has the wrong JSON type (for example `content` as an array) | `400` | `invalid_json` | `null` |
| 7 | more than 2000 `messages` | `400` | `invalid_request` | `null` |
| 8 | more than 256 `tools`, or a tool `parameters` schema deeper than 32 levels or longer than 10000 JSON tokens | `400` | `invalid_request` | `null` |
| 9 | `tool_choice` mode `tool` names a tool that `tools[]` does not define | `400` | `invalid_tool_choice` | `tool_choice` |
| 10 | a `content` or `parts[].data` string longer than 8 MiB | `400` | `invalid_request` | `null` |
| 11 | a `parts[].data` value is not valid standard (padded) base64, or its sniffed MIME category (the part before `/`) differs from the declared `media_type` category; a sniff of `application/octet-stream` is inconclusive and never rejected | `400` | `invalid_request` | `null` |
| 12 | `response_format.json_schema.schema` deeper than 32 levels or longer than 10000 JSON tokens | `400` | `invalid_request` | `null` |

Rows 5 and 6 are both raised while the body is decoded, and their relative order is not a contract: a body that has both a malformed `tool_choice` and a wrong-type field reports whichever error the JSON engine meets first in document order on the shipped binary (built with Go 1.27, whose `encoding/json` runs on json/v2), while the classic engine reported the `tool_choice` sentinel first. Rows 1 to 4 always precede both, and rows 7 to 12 always follow.

Unknown top-level fields (`n`, `seed`, `top_k`, and so on) are ignored: the body is decoded with plain `json.Unmarshal`, not forwarded and not rejected.

### Request body

| Field | Type | Required | Meaning |
|---|---|---|---|
| `model` | string | yes | Canonical model name as configured under `deployments.*.model`. A name no deployment serves is `400`, `code` `model_not_found`. |
| `messages` | `Message[]` | yes, unless `prompt_id` supplies them | At most 2000. A request that resolves to zero messages is `400`, `code` `empty_messages`, `param` `messages`. Setting both `prompt_id` and a non-empty `messages` is `400`, `code` `invalid_prompt_reference`. |
| `temperature` | number | no | Forwarded to the provider. |
| `max_tokens` | integer | no | Forwarded to the provider. |
| `top_p` | number | no | Forwarded to the provider (since gateway/v0.19.0; dropped silently before). Sonnet 4.5 and Haiku 4.5 reject `temperature` and `top_p` together; that is the provider's own `400`. |
| `stop` | string or string array | no | Forwarded to the provider as an array (since gateway/v0.19.0; dropped silently before). Provider limits (OpenAI: four) are the provider's own `400`. |
| `tools` | `ToolDef[]` | no | At most 256. OpenAI nested shape; see below. |
| `tool_choice` | string or object | no | See `tool_choice` below. |
| `stream` | boolean | no, default `false` | `true` selects the SSE response. |
| `response_format` | object | no | `{"type":"json_schema","json_schema":{...}}`; see below. |
| `prompt_id` | string | no | Kelvran extension. Names a server-side prompt to resolve into messages before routing. Unknown id is `400`, `code` `invalid_prompt_reference`. |
| `prompt_version` | integer | no | Kelvran extension. Pins `prompt_id` to a version; `0` or negative means latest. Mutually exclusive with `prompt_label` (`400`, `code` `invalid_prompt_reference`). |
| `prompt_label` | string | no | Kelvran extension. Resolves `prompt_id` through an operator-managed label. |
| `prompt_variables` | object, string to string | no | Kelvran extension. `{{name}}` substitutions for the resolved prompt. |
| `thinking_binding_mode` | string | no | Kelvran extension. `""` (default) or `"non_strict"` requests Anthropic's non-strict preserved-thinking mode; `"strict"` requests the strict mode. A no-op for every other provider and model. |

Prompt fields are covered in [prompt-management.md](../how-to/prompt-management.md); `response_format` in [structured-output.md](../how-to/structured-output.md).

### `Message`

| Field | Type | Direction | Meaning |
|---|---|---|---|
| `role` | string | both | `system`, `user`, `assistant` or `tool`. |
| `content` | string | both | Text. Must be a JSON string; an OpenAI-style content array is `400`, `code` `invalid_json`. Multi-modal content goes in `parts`. |
| `parts` | `ContentPart[]` | request | Multi-modal parts alongside or instead of `content`. On `role: tool` messages (since gateway/v0.19.0): carried to `anthropic` (a `tool_result` block array) and `bedrock` (`toolResult.content` blocks); text parts only on `openai`/`openaicompat`, none on `gemini`. The request is routed to a deployment in the pool that can carry them; when none can it is `400`, `code` `tool_result_parts_unsupported`, `param` `messages`. |
| `tool_calls` | `ToolCall[]` | both | Tool calls the assistant requested. OpenAI nested shape; see below. |
| `tool_call_id` | string | request | On `role: tool` messages, the id of the tool call this message answers. |
| `cache_control` | `CacheControl` | request | Opts this message into provider-side prompt caching. |
| `refusal` | string | response | The model's refusal text. Populated only by `openai` and `openaicompat` deployments. |
| `reasoning_blocks` | `ReasoningBlock[]` | both | Opaque thinking blocks. Echo the exact slice back unmodified on the next turn. |

### `ContentPart`

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `text`, `image` or `document`. |
| `text` | string | Set when `type` is `text`. |
| `media_type` | string | MIME type, for example `image/png` or `application/pdf`. Set when `type` is not `text`. |
| `data` | string | Base64-encoded inline bytes. Exactly one of `data` and `url` is set when `type` is not `text`. At most 8 MiB. Its sniffed MIME category must match `media_type`. |
| `url` | string | Remote reference passed to the provider verbatim. The gateway never fetches it. |
| `cache_control` | `CacheControl` | Opts this part into provider-side prompt caching independently of its message. |

### `ToolCall` (wire shape)

```json
{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}
```

`function.arguments` is always a JSON-encoded string. The gateway re-nests the flat internal fields into this OpenAI shape on both input and output.

### `ToolDef` (wire shape)

```json
{"type":"function","function":{"name":"get_weather","description":"...","parameters":{...}},"cache_control":{"ttl":"1h"},"strict":true}
```

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `function`. |
| `function.name` | string | Tool name. |
| `function.description` | string | Optional. |
| `function.parameters` | JSON Schema object | Optional. Bounded to depth 32 and 10000 JSON tokens. |
| `cache_control` | `CacheControl` | Optional. A sibling of `function`, not nested inside it. |
| `strict` | boolean | Optional. A sibling of `function`. Requests strict grammar-backed validation of the tool's input schema. |

### `tool_choice`

Three inbound forms decode into the same canonical value:

| Form | Example | Meaning |
|---|---|---|
| OpenAI bare string | `"auto"`, `"required"`, `"none"` | Provider default, must call a tool, must not call a tool. |
| OpenAI function object | `{"type":"function","function":{"name":"get_weather"}}` | Must call the named tool. |
| Kelvran canonical object | `{"mode":"tool","tool_name":"get_weather","disable_parallel_tool_use":true}` | `mode` is `auto`, `required`, `none` or `tool`; `tool_name` is read only when `mode` is `tool`; `disable_parallel_tool_use` is honoured by direct Anthropic only. |

Any other shape (Anthropic's `{"type":"auto"}`, OpenAI `allowed_tools`, a mix of `mode` and `type`) is `400`, `code` `invalid_tool_choice`, `param` `tool_choice`. A forced tool that `tools[]` does not define is rejected the same way. The OpenAI string and function-object forms are accepted since gateway/v0.18.0; gateway/v0.17.0 and earlier reject them. Re-encoding always produces the canonical object, which is what the `Idempotency-Key` fingerprint hashes.

### `response_format`

```json
{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{...}}}
```

| Field | Type | Meaning |
|---|---|---|
| `type` | string | `json_schema`. |
| `json_schema.name` | string | Required by the providers. |
| `json_schema.strict` | boolean | Optional. Requests the provider's strictest enforcement. |
| `json_schema.schema` | JSON Schema object | Forwarded as-is. The gateway bounds its depth (32) and token count (10000) and does not otherwise parse or validate it. |

### `CacheControl`

| Field | Type | Meaning |
|---|---|---|
| `ttl` | string | `""` for the provider default, or `"1h"`. Read by Anthropic (direct and Bedrock Anthropic-family) only. |
| `key` | string | Stable caller-supplied identity (session or conversation id). Read by the OpenAI adapter only, as its `prompt_cache_key`. |

### `ReasoningBlock`

| Field | Type | Meaning |
|---|---|---|
| `sequence` | integer | Position relative to `tool_calls`: `N` means immediately before `tool_calls[N]`. |
| `redacted` | boolean | `true` for provider-encrypted blocks. |
| `text` | string | Plaintext reasoning when `redacted` is `false`. |
| `data` | string | Opaque ciphertext when `redacted` is `true`. |
| `signature` | string | Provider signature over `text`. Replay verbatim. |

### Buffered response (`stream` absent or `false`)

`200`, `Content-Type: application/json`, header `X-Kelvran-Overhead-Duration-Ms`.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Provider id when the provider sent one. Otherwise a gateway-issued `chatcmpl-` followed by 32 lowercase hex characters (Bedrock sends none). |
| `object` | string | `chat.completion`. |
| `created` | integer | Unix seconds. Stamped by the gateway when the provider left it empty. |
| `model` | string | Canonical model name. |
| `choices[]` | array | `{"index", "message", "finish_reason"}`. `message` is a `Message` with `role`, `content`, and when present `tool_calls`, `refusal`, `reasoning_blocks`. `finish_reason` is a string; its vocabulary is not documented here. |
| `usage` | object | See `Usage`. |
| `input_transformations[]` | array | Anthropic only. Each entry is `{"type", "path", "reason"}`: `type` is `thinking_dropped` or `thinking_mismatch_allowed`; `path` is the affected block's location in the request, verbatim from Anthropic (for example `messages.3.content.0`); `reason` is `prefix_binding_mismatch` or `model_binding_mismatch`. Omitted when empty. |
| `stop_reason` | string | Set on responses served by `anthropic` and `bedrock` deployments when the provider reported a stop reason (since gateway/v0.19.0): the native vocabulary (`end_turn`, `tool_use`, `stop_sequence`, `max_tokens`, …). `finish_reason` stays the canonical vocabulary. Absent for other providers, on a stream a guard cut short, and on cached entries written before gateway/v0.19.0. |
| `stop_sequence` | string | Anthropic only: the matched stop sequence when `stop_reason` is `stop_sequence`; absent otherwise. Set `stop` on the request (honoured since gateway/v0.19.0) to see it. |

A cache hit or an `Idempotency-Key` replay returns the `id` and `created` the completion was stored with. The `id`, `object` and `created` envelope is present since gateway/v0.18.0; gateway/v0.17.0 and earlier return `"id": ""` for Bedrock and no `object` or `created` for any provider.

### `Usage`

| Field | Type | Meaning |
|---|---|---|
| `prompt_tokens` | integer | Total input tokens charged. Already includes the two cache counts below. |
| `completion_tokens` | integer | Output tokens. Already includes `reasoning_tokens`. |
| `total_tokens` | integer | `prompt_tokens + completion_tokens`. |
| `cache_read_tokens` | integer | Subset of `prompt_tokens` read from the provider's prompt cache. Omitted when `0`. |
| `cache_creation_tokens` | integer | Subset of `prompt_tokens` spent writing a cache entry. Omitted when `0`. |
| `reasoning_tokens` | integer | Subset of `completion_tokens` spent on reasoning. Omitted when `0`; `0` also means "not reported by this provider". |

### Streaming response (`stream: true`)

Response headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`. The status is committed by the first flushed byte.

Frame format: one `data: <json>\n\n` frame per chunk, flushed immediately, then the terminal sentinel `data: [DONE]\n\n`. No `event:`, `id:` or `retry:` fields are sent.

Chunk shape:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | One id for the whole stream (provider id, or a gateway-issued `chatcmpl-` id). |
| `object` | string | `chat.completion.chunk`. |
| `created` | integer | Unix seconds, identical on every chunk of one stream. |
| `model` | string | Canonical model name when the provider sent none; OpenAI chunks keep the provider's snapshot name, so buffered and streamed `model` can differ for that provider. |
| `choices[]` | array | `{"index", "delta", "finish_reason"}`. `finish_reason` is `null` until the final chunk. The chunk that sets `finish_reason` also carries `stop_reason` and, for Anthropic when a stop sequence matched, `stop_sequence` on `anthropic` and `bedrock` deployments (since gateway/v0.19.0). |
| `usage` | object | Present only on the chunk where the provider supplied usage, typically the last. A provider that never streams usage leaves it absent on every chunk. |

`delta` fields, each omitted when empty:

| Field | Type | Meaning |
|---|---|---|
| `role` | string | First chunk of a message only. |
| `content` | string | Incremental text. |
| `tool_calls[]` | array | Flat `{"index", "id", "name", "arguments_json"}`. `id` and `name` appear on the chunk that introduces the call; `arguments_json` fragments concatenate in order. This is not OpenAI's `function.{name,arguments}` nesting; see Not available today. |
| `reasoning_blocks[]` | array | `{"index", "text", "signature", "redacted", "data"}`. `text` fragments concatenate; `signature` and `data` arrive whole. |
| `refusal` | string | Incremental refusal text (`openai`, `openaicompat` only). |

Failure handling:

| When the failure happens | What the client receives |
|---|---|
| Before the first chunk is flushed (auth, rate limit, routing, an upstream that never sent a byte) | The normal status code and JSON error envelope. `Retry-After` when eligible. |
| After the first chunk is flushed | Exactly one in-band frame `data: {"error":{"message","type","param","code"}}\n\n` with the same `type` and `code` a buffered error would carry, then the stream ends without `data: [DONE]`. The HTTP status stays `200`. No fallback to another deployment is attempted at this point. |

The in-band error frame is present since gateway/v0.18.0. Billing of a truncated stream is described in [FAILURE-MODES.md](../operations/FAILURE-MODES.md), row U5. Client guidance is in [streaming.md](../how-to/streaming.md).

### `Idempotency-Key`

| Rule | Behaviour |
|---|---|
| Routes | `POST /v1/chat/completions`, buffered and streaming. Ignored elsewhere. |
| Scope | Per virtual key. Two keys sending the same header value never collide. |
| Fingerprint | SHA-256 of the canonical re-encoding of the request body (`json.Marshal` of the decoded request). |
| Claim lifetime | 10 minutes from the claim (`idempotencyKeyTTL`). Expired claims are swept when a later claim runs. |
| Concurrent duplicate | Waits for the owning request to finish, then receives the stored response verbatim (same `id`, same `created`). |
| Same key, different body | `422`, `type` `invalid_request_error`, `code` `idempotency_key_reused`, `param` `Idempotency-Key`, no `Retry-After` (since gateway/v0.19.0; `502` `upstream_error` with `Retry-After` before). |
| Store | In-process only. Not shared between replicas; a second instance does not see the first instance's claims. |

### `X-Kelvran-End-User-Id`

| Rule | Behaviour |
|---|---|
| Routes | `POST /v1/chat/completions`, buffered and streaming. |
| Effect | None unless the virtual key has `cache_scope_to_end_user: true`. With the flag on, the header value is folded into the key's L1 and L2 response-cache partition. |
| Flag on, header absent | The request gets a request-unique cache scope: it is never served from, and never populates, any shared entry. |
| Trust | The header is caller-supplied and unauthenticated. It is a partitioning signal, not a credential. |

Cache behaviour is covered in [caching.md](../how-to/caching.md).

### Example: buffered request

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <raw-virtual-key-secret>' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 7f3c9a2e-0001' \
  -H 'baggage: agent_run_id=run-42' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}],"max_tokens":64}' \
  -D -
```

`-D -` prints the response headers, including `X-Kelvran-Overhead-Duration-Ms`.

### Example: streaming request

```bash
curl -sN http://127.0.0.1:8080/v1/chat/completions \
  -H 'Authorization: Bearer <raw-virtual-key-secret>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}],"stream":true}'
# data: {"id":"chatcmpl-…","object":"chat.completion.chunk","created":…,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"He"},"finish_reason":null}]}
# …
# data: [DONE]
```

## POST /v1/embeddings

Embeddings in OpenAI shape. Only `POST` is accepted. There is no streaming variant.

### Request body

| Field | Type | Required | Meaning |
|---|---|---|---|
| `model` | string | yes | Canonical name of a `kind: embedding` deployment. Missing or empty is `400`, `code` `missing_required_parameter`, `param` `model`. |
| `input` | string or string[] | yes | One or more texts. A bare string is normalised to a one-element array. Missing, `null` or an empty array is `400`, `code` `missing_required_parameter`, `param` `input`; a bare `""` is accepted as a one-element array and forwarded. Any other JSON type is `400`, `code` `invalid_json`. |
| `dimensions` | integer | no | Requested output size. `0` or absent means the provider's default. |

The same 32 MiB body cap and the same `405`, `413`, `invalid_body` and `invalid_json` handling as chat apply. Unknown fields are ignored.

### Pipeline

After handler validation the request runs: bearer verification, source-IP allowlist, model allowlist, per-key rate limit, budget reservation, pre-call guardrails on the input text, routing (`400`, `code` `model_not_found` when no deployment serves `model`), a best-effort reroute to an in-region deployment when the key has `allowed_regions` (never an error: with no in-region deployment the first pick is used), and the deployment-kind check. A `model` that resolves to a chat deployment is `400`, `code` `not_an_embedding_model`. A deployment whose provider has no embedding adapter is `501`, `code` `embeddings_not_configured`. Embedding deployments exist only for providers `openai` and `bedrock`; `kind: embedding` on any other provider is a configuration load error.

### Response body

`200`, `Content-Type: application/json`.

| Field | Type | Meaning |
|---|---|---|
| `model` | string | Canonical model name as requested. |
| `data[]` | array | `{"index", "embedding"}`, one per `input` entry, in input order. `embedding` is an array of numbers. |
| `usage` | `Usage` | `completion_tokens` is always `0`. |

### Differences from chat

| Capability | `/v1/embeddings` |
|---|---|
| Streaming | Not available today. |
| Fallback to another deployment | Not available today. The first upstream failure is the final answer: `502`, `code` `upstream_error`. |
| `Retry-After` | Never set, on any status, including `429`. The retry-after attachment runs only on the two chat paths. |
| `Idempotency-Key` | Not read. |
| `X-Kelvran-End-User-Id` | Not read. |
| `X-Kelvran-Overhead-Duration-Ms` | Not set. |

### Example

```bash
curl -s http://127.0.0.1:8080/v1/embeddings \
  -H 'Authorization: Bearer <raw-virtual-key-secret>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"text-embedding-3-small","input":["first text","second text"],"dimensions":256}'
```

## GET /v1/models

Lists the canonical models the calling virtual key may use. Present since gateway/v0.18.0; in gateway/v0.17.0 and earlier the path is a 404. Only `GET` is accepted. Bearer and source-IP checks are identical to the other `/v1/*` routes. No rate limit, budget or guardrail runs; the route is an in-memory read with no upstream call.

### Query parameters

| Parameter | Type | Default | Validation |
|---|---|---|---|
| `limit` | integer | all models | Integer `>= 1`, clamped to `1000`. A non-integer or a value `< 1` is `400`, `code` `invalid_request`, `param` `limit`. |
| `after_id` | string | none | Returns the page after this id. An id the key cannot see is `400`, `code` `invalid_request`, `param` `after_id`. |
| `before_id` | string | none | Returns the page before this id. An id the key cannot see is `400`, `code` `invalid_request`, `param` `before_id`. |
| `after_id` and `before_id` together | | | `400`, `code` `invalid_request`, `param` `after_id`. |

`limit` and the `after_id`/`before_id` conflict are checked before authentication, so those `400`s are returned even without a valid key; the unknown-cursor `400` is checked after it.

### Response body

`200`, `Content-Type: application/json`, `X-Content-Type-Options: nosniff`. One superset document carrying the fields the OpenAI SDK (`object`, `data[].id|object|created|owned_by`), the Anthropic SDK (`data[].id|type|display_name|created_at`, `first_id`/`last_id`/`has_more`) and Claude Code's provider discovery (`data[].id|display_name|description`) each read, per their published shapes; no test in this repository drives those clients against the route, and Claude Code cannot use the gateway for chat today because its chat turns are `POST /v1/messages` (see Not available today).

| Field | Type | Meaning |
|---|---|---|
| `object` | string | `list`. |
| `data[]` | array | One entry per canonical model, sorted by `id`. Never one per deployment. Filtered by the key's `allowed_models`. |
| `first_id` | string or null | `id` of the first entry; `null` when `data` is empty. |
| `last_id` | string or null | `id` of the last entry; `null` when `data` is empty. |
| `has_more` | boolean | Whether more entries exist beyond this page in the paging direction. |

Entry fields:

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Canonical model name, the value to send as `model`. |
| `object` | string | `model`. |
| `created` | integer | Unix seconds when this instance loaded its catalog. Replicas restarted at different times report different values. |
| `owned_by` | string | Comma-joined, sorted provider names whose deployments serve the model. Reported, never selectable. |
| `type` | string | `model`. |
| `created_at` | string | The same catalog load time, RFC 3339 UTC. |
| `display_name` | string | From the optional top-level `models:` config section; falls back to `id`. |
| `description` | string | From `models:`; `""` when unset. |
| `kind` | string | `chat` or `embedding`. |

### Example

```bash
curl -s 'http://127.0.0.1:8080/v1/models?limit=100' -H 'Authorization: Bearer <raw-virtual-key-secret>'
# then: ?limit=100&after_id=<last_id from the previous page>
```

## GET /healthz

Liveness. No authentication. Independent of provider reachability.

| Method | Status | Body |
|---|---|---|
| `GET` | `200` | `{"status":"ok"}`, `Content-Type: application/json` |
| other | `405` | plain text `method not allowed`, no `Allow` header, no JSON envelope |

## GET /readyz

Readiness. No authentication.

| Method | Status | Body |
|---|---|---|
| `GET` | `200` when every configured canonical model has at least one probe-healthy deployment; `503` otherwise | `{"ready":<bool>,"models":{"<canonical model>":<bool>,...}}`, `Content-Type: application/json` |
| `GET` | `500` | plain text `internal error`, only if the body cannot be encoded |
| other | `405` | plain text `method not allowed`, no `Allow` header |

A deployment that has never been probed reads healthy. Health probing is off unless `health_probe.interval_seconds` is greater than `0`; with probing off every deployment reads healthy and `/readyz` is `200` whenever the process serves. Probe thresholds and the router's behaviour when every deployment of a model is unhealthy are in [FAILURE-MODES.md](../operations/FAILURE-MODES.md), row U3, and [routing-and-failover.md](../how-to/routing-and-failover.md).

```bash
curl -s http://127.0.0.1:8080/healthz   # 200 {"status":"ok"}
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz   # 200, or 503 when a model has no healthy deployment
```

## Error envelope

Every error on `/v1/chat/completions`, `/v1/embeddings` and `/v1/models` is one JSON object:

```json
{"error":{"message":"...","type":"...","param":null,"code":"..."}}
```

| Property | Value |
|---|---|
| Headers | `Content-Type: application/json; charset=utf-8`, `X-Content-Type-Options: nosniff`; `Allow` on `405`; `Retry-After` when eligible (chat only). |
| Keys | All four are always present. `param` and `code` are `null` when unknown. |
| Body | Written with one `Write`, no trailing newline. |
| `type` vocabulary | `invalid_request_error`, `authentication_error`, `permission_error`, `rate_limit_error`, `insufficient_quota`, `server_error`. |
| `message` | Human-readable. Not a stable contract; only `type` and `code` are covered by [VERSIONING.md](../VERSIONING.md). |
| Fallback | If an error matches no known sentinel, `type` is derived from the status (`401` authentication, `403` permission, `429` rate limit, other 4xx invalid request, `502` server with `code` `upstream_error`, otherwise server with `code` `null`). |

The JSON envelope is present since gateway/v0.18.0. In gateway/v0.17.0 and earlier the same status codes and the same message text come as a `text/plain` body. `/healthz` and `/readyz` are unchanged: their success bodies were already JSON, and their `405` stays plain text.

Upstream failures are redacted. A provider HTTP status reads `upstream provider returned status N`. A transport or decode failure reads `upstream call failed for model "<model>"` (since gateway/v0.18.0; gateway/v0.17.0 and earlier returned the dial or timeout text verbatim). The full provider text reaches only the gateway's own log line.

### Status codes

| Status | `type` | `code` | Raised when | `Retry-After` (chat only) |
|---|---|---|---|---|
| `400` | `invalid_request_error` | `invalid_body` | request body cannot be read | no |
| `400` | `invalid_request_error` | `invalid_json` | body is not JSON, or a field has the wrong JSON type | no |
| `400` | `invalid_request_error` | `invalid_request` | a handler-level bound is exceeded (messages, tools, schema, field size, content part); `GET /v1/models` `limit`, `after_id` or `before_id` invalid (`param` names the parameter) | no |
| `400` | `invalid_request_error` | `invalid_tool_choice` | `tool_choice` shape unknown, or a forced tool not in `tools[]` (`param` `tool_choice`) | no |
| `400` | `invalid_request_error` | `missing_required_parameter` | `/v1/embeddings` without `model` or `input` (`param` names the field) | no |
| `400` | `invalid_request_error` | `model_not_found` | no deployment serves `model` | no |
| `400` | `invalid_request_error` | `empty_messages` | request resolves to zero messages (`param` `messages`) | no |
| `400` | `invalid_request_error` | `content_policy_violation` | a guardrail block-tier verdict | no |
| `400` | `invalid_request_error` | `invalid_prompt_reference` | `prompt_id` and `messages` both set; `prompt_label` and `prompt_version` both set; unknown prompt id, version or label; resolved prompt content fails the content-part check | yes |
| `400` | `invalid_request_error` | `streaming_not_supported` | the resolved provider has no streaming implementation | yes |
| `400` | `invalid_request_error` | `not_an_embedding_model` | `/v1/embeddings` routed to a chat deployment | no |
| `400` | `invalid_request_error` | `response_format_unsupported` | `response_format` set and no deployment in the pool can enforce it (`param` `response_format`); since gateway/v0.19.0, previously `502` | no |
| `401` | `authentication_error` | `null` | `Authorization` missing or not `Bearer ` | no |
| `401` | `authentication_error` | `invalid_api_key` | token matches no key, or a rotated-out key past its grace period | no |
| `401` | `authentication_error` | `key_expired` | token matches a key whose `expires_at` has passed (since gateway/v0.18.0) | no |
| `403` | `permission_error` | `model_not_allowed` | key's `allowed_models` excludes `model` | no |
| `403` | `permission_error` | `source_ip_not_allowed` | key's `allowed_source_cidrs` excludes the TCP peer | yes |
| `405` | `invalid_request_error` | `method_not_allowed` | wrong method on a `/v1/*` route; `Allow` header set | no |
| `413` | `invalid_request_error` | `request_too_large` | body over 32 MiB | no |
| `422` | `invalid_request_error` | `idempotency_key_reused` | `Idempotency-Key` reused within its window with a different body (`param` `Idempotency-Key`); since gateway/v0.19.0, previously `502` | no |
| `429` | `rate_limit_error` | `rate_limit_exceeded` | per-key rate limit | yes |
| `429` | `rate_limit_error` | `concurrency_limit_exceeded` | per-key in-flight cap | yes |
| `429` | `insufficient_quota` | `insufficient_quota` | per-key budget exhausted | no |
| `501` | `server_error` | `streaming_not_configured` | the pipeline has no streaming upstream caller | yes |
| `501` | `server_error` | `embeddings_not_configured` | the pipeline or provider has no embedding upstream | no |
| `502` | `server_error` | `upstream_error` | default for every unrecognised error: every upstream failure after fallback (an upstream `429` is reported as `502`) | yes |
| `503` | `server_error` | `deployment_capacity_exceeded` | a deployment-scoped capacity ceiling rejected the request | yes |

The status is decided by one switch in `gateway/cmd/gateway/main.go` (`errorStatus`); `type` and `code` are added by `gateway/cmd/gateway/error_envelope.go`. Client-side handling per code is in [error-codes.md](error-codes.md) and [troubleshooting.md](../how-to/troubleshooting.md).

## Retry-After

| Rule | Value |
|---|---|
| Routes | `POST /v1/chat/completions` only (buffered and streaming, before the first chunk). Never on `/v1/embeddings` or `/v1/models`. |
| Eligible outcomes | Rate limited (`429` `rate_limit_exceeded`, `429` `concurrency_limit_exceeded`), upstream error (`502` `upstream_error`), deployment capacity (`503` `deployment_capacity_exceeded`). |
| Not eligible | Budget `429` `insufficient_quota`; `401` (no key to track a streak for); `403` `model_not_allowed`; `400` `model_not_found`, `empty_messages` and `content_policy_violation`; every handler-level `400`, `405` and `413`. Any other pipeline error takes the default upstream-error outcome and therefore does carry it, including `403` `source_ip_not_allowed`, `400` `invalid_prompt_reference`, `400` `streaming_not_supported` and `501` `streaming_not_configured`. |
| Form | Integer delay-seconds, rounded up to the next whole second, minimum `1`. |
| Local backoff | Per virtual key, equal-jitter exponential: base 500 ms, doubling on each consecutive eligible rejection, capped at 30 s. A successful request resets the key's streak. |
| Upstream floor | When the final upstream response carried its own `Retry-After`, that value, capped at 60 s, is a floor on the delay. The local backoff is never lowered by it. |

Design: [2026-09-07-gateway-retry-storm-mitigation.md](../rfcs/2026-09-07-gateway-retry-storm-mitigation.md). Upstream-header parsing is logged with the uncapped value (up to 24 h) so an operator can see what the provider asked for; see [metrics-and-logs.md](metrics-and-logs.md).

## Not available today

| Capability | Status |
|---|---|
| Anthropic Messages ingress (`POST /v1/messages`, `/v1/messages/count_tokens`) | Not registered. In progress as item 11 (RFC-1, `docs/rfcs/2026-10-09-gateway-anthropic-messages-ingress.md`, decided 2026-10-10): the parser exists, no route yet. |
| `x-api-key` authentication | Not accepted; only `Authorization: Bearer`. |
| Trusting `X-Forwarded-For` for source-IP allowlists | Not implemented; no configuration knob exists. |
| OpenAI-shaped streaming `tool_calls` deltas (`function.{name,arguments}`) | Not implemented. Streaming deltas are flat `{index, id, name, arguments_json}`; buffered `tool_calls` are OpenAI-nested. |
| Streaming on `/v1/embeddings` | No streaming concept. |
| Fallback chains or provider-derived `Retry-After` on `/v1/embeddings` | Not implemented; a bare `502`. |
| A Redis-backed `Idempotency-Key` store | Not implemented; the store is in-process, single instance. |
| `X-Kelvran-Overhead-Duration-Ms` on streaming responses | Not set; [2026-10-08-gateway-streaming-overhead-measurement.md](../rfcs/2026-10-08-gateway-streaming-overhead-measurement.md) is a pending proposal. |
| A dataplane span on `GET /v1/models` | Not opened; the route has only the `gateway.http` server span every route gets. |
| A `/metrics` route on the data plane | Not registered. Telemetry export is described in [TELEMETRY.md](../operations/TELEMETRY.md). |

## Related pages

- [error-codes.md](error-codes.md): per-code client guidance.
- [config.md](config.md): every `config.yaml` key, including `virtual_keys.*.allowed_models`, `allowed_source_cidrs`, `cache_scope_to_end_user`, `health_probe` and `models`.
- [compatibility.md](compatibility.md): which OpenAI and Anthropic SDK behaviours the shapes above satisfy.
- [metrics-and-logs.md](metrics-and-logs.md): the telemetry each route emits.
- [admin-api.md](admin-api.md): the separate admin server.
- [curl.md](../how-to/clients/curl.md), [openai-python.md](../how-to/clients/openai-python.md), [openai-node.md](../how-to/clients/openai-node.md): client recipes.
- [FAILURE-MODES.md](../operations/FAILURE-MODES.md): route behaviour under each failure.
- [VERSIONING.md](../VERSIONING.md): which parts of this surface are a compatibility promise.
- [2026-09-02-streaming-support.md](../rfcs/2026-09-02-streaming-support.md): the streaming design.
