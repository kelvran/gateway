# Configure the openai Node.js client for Kelvran

Use this when you already call OpenAI from Node.js with the official `openai` npm package and want the same code to go through a Kelvran gateway. This page is for application developers. It covers pointing the client at the gateway, one buffered call, one streaming call, listing models, how Kelvran's errors surface in this client, and what Kelvran adds that the client ignores. There is nothing new to install.

## Prerequisites

- A running gateway with at least one deployment, for example the one from [the quickstart](../../tutorials/quickstart.md). The examples assume `listen_addr: ":8080"` and a deployment whose canonical `model` is `gpt-4o`; substitute your own `model:` names from `deployments`.
- A virtual key secret in the environment variable `KELVRAN_KEY`. The gateway stores only `virtual_keys.<name>.key_hash`, the SHA-256 of that secret; the client sends the raw secret. See [Virtual keys and budgets](../virtual-keys-and-budgets.md). If you run the unedited [`gateway/config.example.yaml`](../../../gateway/config.example.yaml), use its `team-beta` key, whose public example secret is `example-team-beta-secret-do-not-use` (no `allowed_models`, no `allowed_source_cidrs`). Its `team-alpha` key is restricted by `allowed_source_cidrs` to `10.0.0.0/8` and answers 403 `source_ip_not_allowed` to a client on `localhost`. Never use either secret outside a local test.
- Node.js with the `openai` package already installed in your project. Client-library details that the Kelvran repository does not confirm are marked "check your client version".

## Steps

### 1. Point the client at the gateway

Set `baseURL` to the gateway address plus `/v1` and `apiKey` to the virtual key secret. The SDK sends it as `Authorization: Bearer <secret>`, the authentication every `/v1` route accepts (`GET /v1/models` and `POST /v1/messages` also take `x-api-key` as the bearer's alias).

```js
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: process.env.KELVRAN_KEY,
});
```

Under `/v1` the gateway serves exactly four routes: `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models` and `POST /v1/messages` (the Anthropic Messages shape, not an OpenAI SDK method). Any other SDK method (`client.responses`, `client.completions`, `client.images`, `client.files` and so on) hits an unregistered path and gets a 404. Route details: [Data-plane API](../../reference/data-plane-api.md).

### 2. Make a buffered chat completion

```js
const completion = await client.chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Say hello in five words." }],
  max_tokens: 64,
});

console.log(completion.choices[0].message.content);
console.log(completion.usage); // { prompt_tokens, completion_tokens, total_tokens, ... }
```

The response carries `id`, `object: "chat.completion"`, `created`, `model`, `choices[]` and `usage`, so the SDK's response types fit. Two request rules differ from OpenAI:

- `messages[].content` must be a string. The content-array form (`content: [{ type: "text", ... }, { type: "image_url", ... }]`) fails to decode and returns 400 with `code: "invalid_json"`. Multimodal input uses Kelvran's own `parts` field; see [Compatibility](../../reference/compatibility.md).
- Of OpenAI's request fields, only `model`, `messages`, `temperature`, `max_tokens`, `top_p`, `stop`, `tools`, `tool_choice`, `stream` and `response_format` are read (Kelvran's own extensions are listed under "What Kelvran adds that this client ignores"). `max_completion_tokens`, `n`, `seed`, `user`, `logprobs`, `frequency_penalty`, `presence_penalty`, `logit_bias`, `stream_options`, `store`, `metadata`, `parallel_tool_calls`, `service_tier` and `reasoning_effort` are dropped silently. Use `max_tokens`, not `max_completion_tokens`.

### 3. Stream

```js
const stream = await client.chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Count to five." }],
  stream: true,
});

for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");
  if (chunk.usage) console.error("\nusage:", chunk.usage);
}
```

The gateway answers with `Content-Type: text/event-stream`, one `data: {...}` frame per chunk and a final `data: [DONE]`, which the SDK's stream iterator consumes. Every chunk has `object: "chat.completion.chunk"` and all chunks of one stream share one `id` and `created`. `usage` appears on the final chunk when the provider supplies it: the gateway always requests it from OpenAI and OpenAI-compatible upstreams and ignores any `stream_options` you send. Since gateway/v0.18.0, a failure after the first chunk arrives as one in-band `data: {"error":{...}}` frame with no `[DONE]`; the SDK's stream parser surfaces it as an `APIError` instead of a truncated completion (check your client version; see Version notes). More in [Streaming](../streaming.md).

### 4. List models

```js
const page = await client.models.list();
for (const m of page.data) {
  console.log(m.id, m.owned_by);
}
```

`GET /v1/models` requires the same bearer token and returns only the canonical model names this key may call (the key's `allowed_models` filter applies), one entry per model, sorted by `id`. The document is `{ object: "list", data: [...], first_id, last_id, has_more }`. Each entry carries the OpenAI fields (`id`, `object: "model"`, `created`, `owned_by`) plus fields this client ignores (`type`, `created_at`, `display_name`, `description`, `kind`). `page.data` is the SDK's view of that document (check your client version). `GET /v1/models` first shipped in gateway/v0.18.0.

## How errors surface in this client

Every error from `/v1/*` is the OpenAI envelope `{"error":{"message","type","param","code"}}` with all four keys present. The SDK picks the exception class from the HTTP status and fills `err.status`, `err.type`, `err.code`, `err.param` and `err.message` from the body. Only `type` and `code` are a stable contract; message text can change between releases ([docs/VERSIONING.md](../../VERSIONING.md)).

| Status | `type` | `code` | Meaning | SDK class (check your client version) |
|---|---|---|---|---|
| 401 | `authentication_error` | `invalid_api_key`, or null when the header is missing | Wrong or missing virtual key secret | `AuthenticationError` |
| 403 | `permission_error` | `model_not_allowed`, `source_ip_not_allowed` | This key may not use the model or call from this IP | `PermissionDeniedError` |
| 400 | `invalid_request_error` | `model_not_found`, `invalid_json`, `invalid_tool_choice`, `empty_messages`, `content_policy_violation`, `invalid_prompt_reference`, `invalid_request` | Request-shape problem. An unknown model is 400, not OpenAI's 404 | `BadRequestError` |
| 400 | `invalid_request_error` | `response_format_unsupported` | `response_format` on a model pool with no capable deployment (`param` `response_format`; since gateway/v0.19.0, `502` before) | `BadRequestError` |
| 400 | `invalid_request_error` | `tool_result_parts_unsupported` | A `role: tool` message carries image or document `parts` and no deployment in the pool can carry them (`param` `messages`; since gateway/v0.19.0, `502` before) | `BadRequestError` |
| 413 | `invalid_request_error` | `request_too_large` | Body over 32 MiB | generic `APIError` |
| 422 | `invalid_request_error` | `idempotency_key_reused` | `Idempotency-Key` reused with a different body (`param` `Idempotency-Key`; since gateway/v0.19.0, `502` before) | `UnprocessableEntityError` |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | The key's rate limit or concurrency cap. `Retry-After` is set | `RateLimitError` |
| 429 | `insufficient_quota` | `insufficient_quota` | The key's `budget_usd` is spent. No `Retry-After` | `RateLimitError` |
| 501 | `server_error` | `streaming_not_configured`, `embeddings_not_configured` | Pipeline built without a stream or embedding upstream; not reachable from the `gateway` binary, which always wires both | `InternalServerError` |
| 502 | `server_error` | `upstream_error` | Provider error or transport failure. `Retry-After` is set | `InternalServerError` |
| 503 | `server_error` | `deployment_capacity_exceeded` | Deployment at capacity. `Retry-After` is set | `InternalServerError` |

Full table: [Error codes](../../reference/error-codes.md). A 502 message is redacted to `upstream provider returned status N` or `upstream call failed for model "<model>"`; the provider's own text is only in the gateway's log line.

### 429: rate limit versus budget

Both cases are HTTP 429, so this client raises the same class for both. Branch on `err.type`:

```js
try {
  await client.chat.completions.create({ model: "gpt-4o", messages });
} catch (err) {
  if (!(err instanceof OpenAI.APIError)) throw err;
  if (err.status === 429 && err.type === "insufficient_quota") {
    // Budget spent. Retrying never helps until an operator raises the
    // budget or its window resets. Fail the operation or queue it.
    throw err;
  }
  if (err.status === 429) {
    // rate_limit_exceeded or concurrency_limit_exceeded: wait the
    // Retry-After seconds, then retry. err.headers carries the response
    // headers; its shape differs between client versions.
  }
  throw err;
}
```

By default this client retries 429 and 5xx responses on its own and honours `Retry-After` (check your client version). That suits `rate_limit_error`, 502 and 503, but it also retries a budget 429, which cannot succeed. To avoid that, construct the client with `maxRetries: 0` and retry yourself only when `err.type !== "insufficient_quota"`, or keep the default and accept the wasted attempts. How limits and budgets are set: [Virtual keys and budgets](../virtual-keys-and-budgets.md) and [Config reference](../../reference/config.md).

## What Kelvran adds that this client ignores

- Response header `X-Kelvran-Overhead-Duration-Ms`: the gateway's own added latency in milliseconds, on buffered responses only, never on streams. Read it with `.withResponse()`:

  ```js
  const { data, response } = await client.chat.completions
    .create({ model: "gpt-4o", messages })
    .withResponse();
  console.log(response.headers.get("x-kelvran-overhead-duration-ms")); // check your client version
  ```

  Background: [TELEMETRY.md](../../operations/TELEMETRY.md) and the [overhead header RFC](../../rfcs/2026-09-14-gateway-overhead-duration-header.md).
- Extra `usage` fields, present only when non-zero: `cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`. They are subsets of `prompt_tokens` and `completion_tokens`, never additions.
- A top-level `stop_reason` and, when a stop sequence matched, `stop_sequence` on Anthropic and Bedrock responses, and, inside `choices[]`, on the streaming chunk that sets `finish_reason` (since gateway/v0.19.0); `finish_reason` is unchanged.
- `choices[].message.reasoning_blocks[]` (and `delta.reasoning_blocks[]` on chunks): opaque reasoning content, emitted by Anthropic, Bedrock, Gemini and openaicompat deployments when the model produces it. When you continue the conversation, echo that array back unchanged on the same assistant message; Anthropic and Bedrock reject the turn with 400 if it is dropped. Also `input_transformations[]` on Anthropic responses and `choices[].message.refusal` from openai and openaicompat deployments.
- Request extensions the gateway reads when you add them to the params object: `prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables` ([Prompt management](../prompt-management.md)); `messages[].parts` and `messages[].cache_control`; `thinking_binding_mode`; the canonical `tool_choice` object `{ mode, tool_name, disable_parallel_tool_use }`. TypeScript rejects unknown keys at compile time, so cast the params or use `@ts-expect-error` (check your client version).
- Request headers the gateway reads, sent through the constructor's `defaultHeaders` or a per-call `{ headers }` option (check your client version): `Idempotency-Key` (10-minute window; the same key with a different body returns 422 `idempotency_key_reused` since gateway/v0.19.0, 502 `upstream_error` before), `X-Kelvran-End-User-Id` (cache partitioning for keys with `cache_scope_to_end_user: true`), and W3C `traceparent` and `baggage`.

## Verify it worked

Save the client construction from step 1 followed by the buffered example from step 2 as `hello.mjs` and run it:

```sh
node hello.mjs
```

What you see: one line with the model's greeting, then a `usage` object whose `total_tokens` is a positive integer. Confirm the gateway answered, not the provider directly, by reading the response headers with curl:

```sh
curl -s -D - -o /dev/null http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

The headers include `HTTP/1.1 200 OK`, `Content-Type: application/json` and `X-Kelvran-Overhead-Duration-Ms: <integer>`. A wrong secret makes the Node example throw `AuthenticationError` with `err.status === 401` and `err.code === "invalid_api_key"`.

## Version notes

These behaviours first shipped in gateway/v0.18.0; gateway/v0.17.0 and earlier lack them:

- The JSON error envelope. gateway/v0.17.0 returns `text/plain` error bodies with the same status codes, so this client raises the same classes but `err.code` and `err.type` are empty.
- `GET /v1/models`, so `client.models.list()`.
- `id`, `object` and `created` on every completion and chunk. In gateway/v0.17.0 no completion or chunk from any provider carries `object` or `created`, and Bedrock completions additionally arrive with `"id": ""` (and chunks with an empty `id` and `model`).
- OpenAI's `tool_choice` forms `"auto"`, `"required"`, `"none"` and `{ type: "function", function: { name } }`. gateway/v0.17.0 rejects the string form with 400 and the object form with 502.
- Redaction of transport failures to `upstream call failed for model "<model>"`.
- The in-band `data: {"error":{...}}` frame after a mid-stream failure. gateway/v0.17.0 ends the stream with no error frame and no `[DONE]`, so this client sees a truncated stream, not an `APIError`.

## Not available today

- `POST /v1/messages/count_tokens` and the raw-body passthrough to `anthropic` deployments. `POST /v1/messages` itself is served since gateway/v0.19.0 (`x-api-key` read on it and on `GET /v1/models`), so the Anthropic SDK can use Kelvran as a base URL.
- The Responses API, the Completions API, images, audio, files and fine-tuning routes. Only chat completions, embeddings, models and the Anthropic `messages` route exist under `/v1`.
- OpenAI content-array `content` on inbound messages (400 `invalid_json`).
- OpenAI-shaped streaming tool-call deltas. The gateway streams flat `{ index, id, name, arguments_json }` elements with no `type` and no `function` nesting, so this client's tool-call stream accumulators do not reassemble arguments (check your client version). Buffered tool calls use the OpenAI nesting.
- Client-side `stream_options`, `n`, `seed`, `user`, `logprobs`, `max_completion_tokens` and the other dropped fields listed in step 2.
- `X-Kelvran-Overhead-Duration-Ms` on streaming responses.
- A first-party Kelvran SDK or an OpenAPI document. The `openai` package with `baseURL` is the integration path by recorded decision; see [Why no SDK](../../explanation/why-no-sdk.md).
- A CI compatibility test against `openai-node`. No Node client code exists in this repository; the examples follow the gateway's wire contract and the package's public API shape.

## Related

- [curl](curl.md) and [openai-python](openai-python.md) for the same calls in other clients.
- [Data-plane API](../../reference/data-plane-api.md), [Error codes](../../reference/error-codes.md), [Compatibility](../../reference/compatibility.md), [Config reference](../../reference/config.md).
- [Structured output](../structured-output.md), [Troubleshooting](../troubleshooting.md), [FAILURE-MODES.md](../../operations/FAILURE-MODES.md), [README.md](../../../README.md).
