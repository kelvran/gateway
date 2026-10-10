# Error codes

This page lists every error the gateway's data plane can return: the JSON envelope, the `type` and `code` vocabulary, the HTTP status each pair carries, when it occurs, whether `Retry-After` accompanies it, and what the `message` may contain. It is for client developers handling errors from `POST /v1/chat/completions`, `POST /v1/embeddings` and `GET /v1/models`, and for operators reading those errors in logs. The routes themselves are described in [`data-plane-api.md`](data-plane-api.md); what each failure means operationally is in [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md).

## Scope and version

| Item | Value |
|---|---|
| Routes covered | `POST /v1/chat/completions` (buffered and SSE), `POST /v1/embeddings`, `GET /v1/models` |
| Routes not covered | `GET /healthz`, `GET /readyz`, every `/admin/*` route, unknown paths (see [Routes outside the envelope](#routes-outside-the-envelope)) |
| JSON error envelope | since `gateway/v0.18.0`. `gateway/v0.17.0` returns a `text/plain` body with no `type` or `code`, with the same status codes and the same message text except for the `tool_choice` statuses and the message changes listed two rows below |
| `GET /v1/models` | since `gateway/v0.18.0` |
| `invalid_tool_choice` code (in `gateway/v0.17.0` OpenAI's `{"type":"function",…}` `tool_choice` was a 502 and its string form a 400 `invalid request body`); the `upstream call failed for model "<model>"` redaction (`gateway/v0.17.0` returned transport failures verbatim); the `deployment at capacity (<reason>)` message (`gateway/v0.17.0` said `deployment "<name>" at capacity (<reason>)`); the `not_an_embedding_model` message ending `: model "<model>"` (`gateway/v0.17.0` ended `: deployment "<name>"`) | since `gateway/v0.18.0` |
| Stable contract | `type` and `code` ([`docs/VERSIONING.md`](../VERSIONING.md)) |
| Not a contract | `message` text. It may change in any release. Match on `type` and `code`, never on `message` |
| Source of truth | `gateway/cmd/gateway/error_envelope.go` (envelope, `type`, `code`) and `errorStatus` in `gateway/cmd/gateway/main.go` (status) |

## Envelope

Every data-plane error body has this shape. All four keys are always present; `param` and `code` are `null` when there is no value.

```json
{"error":{"message":"dataplane: rate limit exceeded","type":"rate_limit_error","param":null,"code":"rate_limit_exceeded"}}
```

| Key | JSON type | Always present | Meaning |
|---|---|---|---|
| `error.message` | string | yes | Human-readable text. Not a contract. Upstream provider bodies and transport details are redacted (see [Message redaction](#message-redaction)) |
| `error.type` | string | yes | One of the six values in [Error types](#error-types) |
| `error.param` | string or `null` | yes | The offending request field, when one is known (see [`param`](#param)) |
| `error.code` | string or `null` | yes | Machine-readable code (see the code tables below). `null` for a missing or malformed `Authorization` header and for the encoding fallback |

### Response headers on every error

| Header | Value |
|---|---|
| `Content-Type` | `application/json; charset=utf-8` |
| `X-Content-Type-Options` | `nosniff` |
| `Content-Length` | cleared before the body is written |
| `Retry-After` | integer seconds, only on the responses listed under [`Retry-After`](#retry-after) |
| `Allow` | only on 405, the permitted method list (`POST` for `/v1/chat/completions` and `/v1/embeddings`, `GET` for `/v1/models`) |

The body is written in one write with no trailing newline.

### Encoding fallback

If marshalling the envelope ever fails, this fixed body is written instead, with the status the error already had:

```json
{"error":{"message":"internal error while encoding the error response","type":"server_error","param":null,"code":null}}
```

## Error types

| `type` | Meaning | Statuses it appears with |
|---|---|---|
| `invalid_request_error` | The request itself is the problem: shape, size, an unknown model, a blocked prompt, a bad prompt reference, an unenforceable `response_format`, a reused `Idempotency-Key` | 400, 405, 413, 422 |
| `authentication_error` | No usable virtual key in `Authorization: Bearer` | 401 |
| `permission_error` | The key is valid but not allowed to do this | 403 |
| `rate_limit_error` | A transient per-key throttle. Clients may retry after `Retry-After` | 429 |
| `insufficient_quota` | The key's `budget_usd` is spent. Not a throttle: it clears only when an operator raises the budget or, for a key with `budget_reset_interval_seconds`, when the current window resets | 429 |
| `server_error` | The gateway or an upstream provider could not complete the request | 501, 502, 503 |

## Pipeline codes

These are produced after the request body is accepted, by the shared error writer for all three routes. `Retry-After` is as described under [`Retry-After`](#retry-after); the column states the outcome for `/v1/chat/completions`. `/v1/embeddings` and `/v1/models` never set it.

`/v1/models` can produce only the three 401 codes and 403 `source_ip_not_allowed`. `/v1/embeddings` never produces `concurrency_limit_exceeded`, `deployment_capacity_exceeded`, `empty_messages`, `invalid_prompt_reference`, `streaming_not_supported`, `streaming_not_configured`, `response_format_unsupported`, `idempotency_key_reused` or `tool_result_parts_unsupported`; `/v1/chat/completions` never produces `not_an_embedding_model` or `embeddings_not_configured`; every other row applies to both POST routes.

| Status | `type` | `code` | `param` | `Retry-After` (chat) | Raised when | Message begins with (not a contract) |
|---|---|---|---|---|---|---|
| 401 | `authentication_error` | `null` | `null` | no | `Authorization: Bearer` header missing or malformed | `dataplane: auth: identity: missing or malformed Authorization header` |
| 401 | `authentication_error` | `invalid_api_key` | `null` | no | The bearer token hashes to no configured virtual key | `dataplane: auth: identity: invalid virtual key` |
| 401 | `authentication_error` | `key_expired` | `null` | no | The bearer token matches a configured virtual key whose `expires_at` has passed (RFC 3339 instant, inclusive). The message never names the key; on `/v1/chat/completions` and `/v1/embeddings` the gateway's log line carries `virtual_key_id` and `key_expired_at` (`/v1/models` writes no request log line). Since `gateway/v0.18.0` | `dataplane: auth: identity: virtual key expired` |
| 429 | `rate_limit_error` | `rate_limit_exceeded` | `null` | yes | The key's RPM or TPM bucket is empty | `dataplane: rate limit exceeded` |
| 429 | `rate_limit_error` | `concurrency_limit_exceeded` | `null` | yes | The key already has its `rate_limit.max_concurrent_requests` requests outstanding | `dataplane: concurrency limit exceeded` |
| 429 | `insufficient_quota` | `insufficient_quota` | `null` | no | The key has spent its `budget_usd` | `dataplane: budget exceeded` |
| 403 | `permission_error` | `model_not_allowed` | `null` | no | The key has an `allowed_models` list that excludes the requested model | `dataplane: model not allowed for this virtual key` |
| 403 | `permission_error` | `source_ip_not_allowed` | `null` | yes | The key has an `allowed_source_cidrs` list that excludes the client IP | `dataplane: source IP not allowed for this virtual key` |
| 400 | `invalid_request_error` | `model_not_found` | `null` | no | No deployment serves the requested model. No upstream call is made | `dataplane: no deployment configured for requested model` |
| 400 | `invalid_request_error` | `content_policy_violation` | `null` | no | A Block-tier guardrail verdict rejected the request (pre-call or post-call), or a Block-tier detector errored | `dataplane: request blocked by guardrail policy` |
| 400 | `invalid_request_error` | `empty_messages` | `"messages"` | no | The request resolves to zero messages (sent `messages: []` with no `prompt_id`, or the prompt resolved to an empty list) | `dataplane: request resolved to zero messages` |
| 400 | `invalid_request_error` | `invalid_prompt_reference` | `null` | yes | `prompt_id` and `messages` both set; `prompt_label` and `prompt_version` both set; unknown `prompt_id`, `prompt_version` or `prompt_label`; or the resolved prompt's own content fails the content-part check | `dataplane: request sets both prompt_id and messages`, `dataplane: request sets both prompt_label and prompt_version`, `dataplane: failed to resolve prompt_id`, or `dataplane: resolved prompt content failed validation` |
| 400 | `invalid_request_error` | `streaming_not_supported` | `null` | yes | `stream: true` to a provider adapter with no streaming implementation | `dataplane: streaming not supported for this provider` |
| 400 | `invalid_request_error` | `not_an_embedding_model` | `null` | no (embeddings route) | `/v1/embeddings` names a model whose deployment is `kind: chat` | `dataplane: requested model is not an embedding deployment` |
| 400 | `invalid_request_error` | `response_format_unsupported` | `"response_format"` | no | `response_format` is set and no deployment in the model's pool can enforce it (Bedrock models outside the structured-output whitelist). No upstream call is made. Since `gateway/v0.19.0`; before, 502 `upstream_error` with `Retry-After` | `adapter: response_format is not supported by this model and no capable deployment was found` |
| 400 | `invalid_request_error` | `tool_result_parts_unsupported` | `"messages"` | no | A `role: tool` message carries `parts` the model's pool cannot carry — image or document parts on `openai`/`openaicompat`, any parts on `gemini` — and no deployment in the model's pool can carry them. No upstream call is made. Since `gateway/v0.19.0`; before, `anthropic`/`bedrock`/`gemini` answered 502 `upstream_error`, and `openai`/`openaicompat` forwarded an image part as `image_url` for the provider's own 400 and answered 502 `upstream_error` for a document part | `adapter: a tool result carries content parts this provider cannot represent and no capable deployment is available` |
| 422 | `invalid_request_error` | `idempotency_key_reused` | `"Idempotency-Key"` | no | The `Idempotency-Key` was used within its 10-minute window with a request body that hashes differently. No upstream call is made. Since `gateway/v0.19.0`; before, 502 `upstream_error` with `Retry-After` | `dataplane: idempotency: idempotency: key already claimed with a different request body` |
| 501 | `server_error` | `streaming_not_configured` | `null` | yes | The pipeline has no streaming upstream configured and the request is a streaming cache miss | `dataplane: streaming is not configured for this pipeline` |
| 501 | `server_error` | `embeddings_not_configured` | `null` | no (embeddings route) | The deployment's provider has no embedding adapter, or no embedding upstream is configured | `dataplane: embeddings are not configured for this deployment` |
| 503 | `server_error` | `deployment_capacity_exceeded` | `null` | yes | The deployment's own aggregate RPM, TPM or concurrency ceiling rejected the call and fallback did not succeed. The caller may be nowhere near its own limits | `deployment at capacity (<reason>)` |
| 502 | `server_error` | `upstream_error` | `null` | yes | Any error with no dedicated case above. See [Errors that share `502 upstream_error`](#errors-that-share-502-upstream_error) | varies; see [Message redaction](#message-redaction) |

Wrapped errors classify the same way as bare ones: the mapping uses `errors.Is` and `errors.As`, so a sentinel wrapped with `%w` keeps its status, `type` and `code`.

### Status-derived fallback

A pipeline error with no dedicated `type`/`code` case takes its `type` from the status the status switch chose:

| Status | `type` | `code` |
|---|---|---|
| 401 | `authentication_error` | `null` |
| 403 | `permission_error` | `null` |
| 429 | `rate_limit_error` | `null` |
| other 4xx | `invalid_request_error` | `null` |
| 502 | `server_error` | `upstream_error` |
| anything else | `server_error` | `null` |

Every sentinel the status switch knows also has a dedicated `type`/`code` case, so today only the 502 row is reachable.

## Handler-level codes

These are produced before the pipeline runs, while the handler reads and validates the request. All carry `type: invalid_request_error`. None carries `Retry-After`.

| Status | `code` | `param` | Routes | Raised when | Message (not a contract) |
|---|---|---|---|---|---|
| 405 | `method_not_allowed` | `null` | all three | Wrong HTTP method. The `Allow` header lists the permitted method | `method not allowed` |
| 413 | `request_too_large` | `null` | chat, embeddings | Request body exceeds 32 MiB | `request body too large` |
| 400 | `invalid_body` | `null` | chat, embeddings | The body could not be read for a reason other than size | `reading request body` |
| 400 | `invalid_json` | `null` | chat, embeddings | The body is not valid JSON for the request type | `invalid request body: <decoder error>` |
| 400 | `invalid_tool_choice` | `"tool_choice"` | chat | `tool_choice` is not `"auto"`, `"required"`, `"none"`, `{"type":"function","function":{"name":...}}` or the canonical `{"mode":...}` object; `type` is `allowed_tools` or `custom`; `mode` and `type` both set; `{"type":"function"}` without a `function.name`; `mode: "tool"` without `tool_name`; or the forced tool is not defined in `tools[]` | `adapter: invalid tool_choice: <detail>` |
| 400 | `invalid_request` | `null` | chat | More than 2000 messages; more than 256 tool definitions; one message `content` or content-part `data` field over 8 MiB; a content part whose base64 does not decode; a content part whose declared `media_type` category differs from the sniffed type; or a `response_format.json_schema.schema` or `tools[].parameters` schema that is not valid JSON or exceeds 32 levels of nesting or 10000 JSON tokens | names the bound or check that failed, for example `adapter: messages exceeds this gateway's per-request message-count bound: 2001 messages, max 2000` |
| 400 | `invalid_request` | `"limit"` | models | `limit` is not a positive integer | `limit must be a positive integer` |
| 400 | `invalid_request` | `"after_id"` | models | `after_id` and `before_id` are both set | `after_id and before_id cannot be combined` |
| 400 | `invalid_request` | `"after_id"` or `"before_id"` | models | The cursor names a model this key cannot see | `<param> does not name a model this key can see` |
| 400 | `missing_required_parameter` | `"model"` | embeddings | `model` is empty | `model is required` |
| 400 | `missing_required_parameter` | `"input"` | embeddings | `input` is missing or empty | `input is required and must be non-empty` |

## `param`

`param` is `null` except in these cases.

| `code` | `param` | Route |
|---|---|---|
| `empty_messages` | `"messages"` | chat |
| `response_format_unsupported` | `"response_format"` | chat |
| `idempotency_key_reused` | `"Idempotency-Key"` (a request header, not a body field) | chat |
| `invalid_tool_choice` | `"tool_choice"` | chat |
| `invalid_request` | `"limit"`, `"after_id"` or `"before_id"` | models |
| `missing_required_parameter` | `"model"` or `"input"` | embeddings |

In a mid-stream error frame `param` is always `null`; the three pipeline codes that set it (`empty_messages`, `response_format_unsupported`, `idempotency_key_reused`) all fail before the first chunk and so never appear in a frame.

## `Retry-After`

`Retry-After` is attached by the chat pipeline, after the key is authenticated, when the error's outcome is one of: rate limited, deployment capacity, or upstream error. The upstream-error outcome is the default for every error with no dedicated outcome of its own, so some local errors fall into it.

| Property | Value |
|---|---|
| Format | a plain integer number of seconds (RFC 9110 delay-seconds form) |
| Value | the key's per-key retry backoff: 500 ms doubled per consecutive rejection with equal jitter, capped at 30 s, rounded up to a whole second, floor 1 (so 1-30 s). When the final upstream response carried its own `Retry-After`, that value (capped at 60 s) is a floor on the delay |
| Routes | `/v1/chat/completions` only. `/v1/embeddings` and `/v1/models` never set it |
| Before authentication | never (401 responses carry no `Retry-After`) |

| Carries `Retry-After` on chat | Never carries `Retry-After` |
|---|---|
| 429 `rate_limit_exceeded` | 429 `insufficient_quota` |
| 429 `concurrency_limit_exceeded` | 401 (both codes) |
| 503 `deployment_capacity_exceeded` | 403 `model_not_allowed` |
| 502 `upstream_error` (every cause) | 400 `model_not_found` |
| 403 `source_ip_not_allowed` | 400 `content_policy_violation` |
| 400 `invalid_prompt_reference` | 400 `empty_messages` |
| 400 `streaming_not_supported` | every handler-level code (400, 405, 413) |
| 501 `streaming_not_configured` | everything on `/v1/embeddings` and `/v1/models` |
| | 400 `response_format_unsupported` (since gateway/v0.19.0; its 502 before carried one) |
| | 422 `idempotency_key_reused` (since gateway/v0.19.0; its 502 before carried one) |

The 403, 400 and 501 entries in the left column are local errors that carry the header because they have no dedicated outcome; they also count as `error.type=upstream_error` in telemetry. [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md) records this as a known gap. The design is [`docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md`](../rfcs/2026-09-07-gateway-retry-storm-mitigation.md).

## Message redaction

The `message` never contains an upstream provider's response body, a deployment's internal `base_url` or `host:port`, or the operator's deployment name. The unredacted text reaches only the gateway's `chat_completion` / `embeddings` log line.

| Condition | Client-facing `message` |
|---|---|
| Upstream provider returned a non-2xx status (including a provider 401, 403 or 429) | `upstream provider returned status <N>` |
| Upstream provider sent an in-band error on an already-2xx stream | `<provider>: upstream provider returned a mid-stream error` |
| Transport or decode failure that falls into the 502 default (refused dial, timeout, mid-stream decode error) | `upstream call failed for model "<model>"` |
| Deployment-level capacity rejection | `deployment at capacity (<reason>)` |
| Every Kelvran-authored sentinel | its own text, as listed in the tables above |

The status code is never changed by redaction; only the text is.

## Streaming responses

| Moment of failure | What the client receives |
|---|---|
| Before the first SSE chunk | A normal envelope with the real status (401, 429, 400, 502, ...). The response is not a stream |
| After the first SSE chunk | HTTP 200 is already committed. Exactly one in-band frame is written, then the stream ends. No `data: [DONE]` follows |

The in-band frame:

```text
data: {"error":{"message":"upstream call failed for model \"gpt-4o\"","type":"server_error","param":null,"code":"upstream_error"}}

```

The frame's `type`, `code` and redacted `message` are those the same error would carry in a buffered response. `param` is always `null` in a frame. There is no fallback to another deployment once the first chunk has gone out. The OpenAI SDKs' stream parsers surface this frame as an `APIError`. See [`../how-to/streaming.md`](../how-to/streaming.md).

## Errors that share `502 upstream_error`

Every error with no dedicated case takes the 502 default, `type: server_error`, `code: upstream_error`, with `Retry-After` on chat. The cases below are reachable today and cannot be told apart by `type` or `code`.

| Cause | Reached a provider | `message` |
|---|---|---|
| Provider answered with a non-2xx status after fallback was exhausted (a provider 429 is surfaced this way, never as 429) | yes | `upstream provider returned status <N>` |
| Transport failure before any response: refused connection, dial failure, timeout waiting for the response | no | `upstream call failed for model "<model>"` |
| Read timeout or decode error after the provider started answering | yes | `upstream call failed for model "<model>"` |
| URL-based image or document content part sent to a Bedrock deployment (Bedrock accepts inline base64 only) | no | `upstream call failed for model "<model>"` (the adapter's own text reaches only the server log) |
| Provider-side content-policy or safety block that the fallback chain did not absorb | yes | `upstream provider returned status <N>` for a non-2xx rejection; `upstream call failed for model "<model>"` for a 2xx body that the adapter classified as a safety block (today Gemini's `promptFeedback.blockReason`) |
| Bedrock event stream ended before a `messageStop` event | yes | `upstream call failed for model "<model>"`, as a mid-stream frame when a chunk had already been sent |

## Routes outside the envelope

| Route | Error bodies |
|---|---|
| `GET /healthz`, `GET /readyz` | A wrong method answers 405 `text/plain` `method not allowed` with no `Allow` header. `/readyz` answers 500 `text/plain` `internal error` if it cannot encode its body. Success and not-ready (503) bodies are JSON |
| `/admin/*` | `text/plain` bodies from `http.Error`: 401 `missing or malformed Authorization header` or `invalid admin token`, and route-specific 400, 404, 409, 500 and 501 texts. A wrong method on a known admin path is net/http's own `text/plain` 405 `Method Not Allowed` with an `Allow` header, sent before the token is checked. See [`admin-api.md`](admin-api.md) |
| Unknown path | net/http's default `text/plain` 404. The data-plane routes are registered on their exact paths, so `/v1/models/` (trailing slash) is a 404, not a redirect |

## Not available today

- OpenAI's 404 for an unknown model. An unknown model is deliberately 400 `model_not_found`: it is treated as a request-shape mistake, not a missing resource.
- A way for the client to tell a Block-tier guardrail detector error from a real guardrail finding. Both are 400 `content_policy_violation`.
- `Retry-After` on `/v1/embeddings` or `/v1/models`, for any error.
- An upstream provider's 429 surfaced as 429. It is 502 `upstream_error`.

## Client library behaviour

The envelope follows OpenAI's shape so that the official OpenAI SDKs choose their exception class from the HTTP status and populate `.code` and `.type` from the body, and so that LangChain and LiteLLM stop retrying on `insufficient_quota` while continuing to retry `rate_limit_error`. Per-client notes are in [`../how-to/clients/openai-python.md`](../how-to/clients/openai-python.md), [`../how-to/clients/openai-node.md`](../how-to/clients/openai-node.md) and [`../how-to/clients/curl.md`](../how-to/clients/curl.md).

## Related pages

- [`data-plane-api.md`](data-plane-api.md): the three routes, their request and response shapes.
- [`metrics-and-logs.md`](metrics-and-logs.md): the `error.type` attribute and the `chat_completion` / `embeddings` log lines that carry the unredacted text.
- [`../how-to/troubleshooting.md`](../how-to/troubleshooting.md): what to do about each error.
- [`../how-to/virtual-keys-and-budgets.md`](../how-to/virtual-keys-and-budgets.md): the limits behind 429, 403 and `insufficient_quota`.
- [`../operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md): operator view of every failure, including the known gaps above.
- [`../VERSIONING.md`](../VERSIONING.md): what in the envelope is a stable contract.
- [`../../THREAT_MODEL.md`](../../THREAT_MODEL.md): why upstream bodies and transport details are redacted.
