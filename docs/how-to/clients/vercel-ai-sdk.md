# Use Vercel AI SDK with Kelvran

Use this when you already write TypeScript or Node.js against the Vercel AI SDK (`ai` with `@ai-sdk/openai` or `@ai-sdk/openai-compatible`) and want `generateText` and `streamText` to go through a Kelvran gateway instead of straight to a provider. This page is for application developers. It covers pointing a provider at the gateway, one buffered call, one streaming call, how Kelvran's errors surface in this SDK, the traps a live recording found, and what Kelvran adds that the SDK ignores. There is nothing new to install.

## Prerequisites

- A running gateway with at least one deployment, for example the one from [the quickstart](../../tutorials/quickstart.md). The examples assume `listen_addr: ":8080"` and a deployment whose canonical `model` is `gpt-4o`; substitute your own `model:` names from `deployments`. Model names are Kelvran's canonical names, not provider ids, and this SDK does not check them client-side: the recording routes a name no provider catalogue knows.
- A virtual key secret in the environment variable `KELVRAN_KEY`. The gateway stores only the SHA-256 of that secret; the client sends the raw secret.
- `ai` plus `@ai-sdk/openai` (or `@ai-sdk/openai-compatible`) already installed in your project. The behaviour below was recorded on 2026-10-09 with `ai` 7.0.134, `@ai-sdk/openai` 4.0.90, `@ai-sdk/openai-compatible` 3.0.66 and Node v26.9.0 against a real gateway. Anything this page does not pin to that recording is marked "check your client version".

## Steps

### 1. Point the provider at the gateway

Set `baseURL` to the gateway address plus `/v1` and `apiKey` to the virtual key secret. The provider sends it as `Authorization: Bearer <secret>`, the authentication every `/v1` route accepts (`GET /v1/models` also takes `x-api-key` as the bearer's alias; the other routes answer it with 401).

```ts
import { createOpenAI } from "@ai-sdk/openai";

const openai = createOpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: process.env.KELVRAN_KEY,
});
```

Always build models with `openai.chat("<model>")`. The provider's default factory, `openai("<model>")`, targets the OpenAI Responses API: the request goes to `<baseURL>/responses`, which the gateway does not serve, and the SDK throws `APICallError` with `statusCode` 404 and `url` ending in `/v1/responses`. Under `/v1` the gateway serves exactly `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models` and `POST /v1/messages` (the Anthropic Messages shape); any other path is a plain-text 404 with no envelope, and a wrong method on one of those four paths is a 405 envelope with `code: method_not_allowed`. Route details: [Data-plane API](../../reference/data-plane-api.md).

The alternative provider works the same way; the recording passed the key as an explicit header (its `apiKey` option sends the identical `Authorization: Bearer` header; check your client version):

```ts
import { createOpenAICompatible } from "@ai-sdk/openai-compatible";

const kelvran = createOpenAICompatible({
  name: "kelvran",
  baseURL: "http://localhost:8080/v1",
  headers: { Authorization: `Bearer ${process.env.KELVRAN_KEY}` },
});
// then: kelvran.chatModel("gpt-4o")
```

Both `generateText` and `streamText` work through `kelvran.chatModel(...)` in the recording. The rest of this page uses `openai.chat(...)`; swap in `kelvran.chatModel(...)` one for one.

### 2. Make a buffered call

```ts
import { generateText } from "ai";

const result = await generateText({
  model: openai.chat("gpt-4o"),
  prompt: "Say hello in five words.",
});

console.log(result.text);
console.log(result.finishReason); // "stop"
console.log(result.usage);
```

`result.usage` in the recording is the SDK's normalized object with Kelvran's wire usage attached under `raw`:

```json
{"inputTokens":13,"inputTokenDetails":{"noCacheTokens":13,"cacheReadTokens":0},
 "outputTokens":11,"outputTokenDetails":{"textTokens":11,"reasoningTokens":0},
 "totalTokens":24,"raw":{"prompt_tokens":13,"completion_tokens":11,"total_tokens":24}}
```

Of OpenAI's request fields the gateway reads only `model`, `messages`, `temperature`, `max_tokens`, `top_p`, `stop`, `tools`, `tool_choice`, `stream` and `response_format`; every other field the SDK may send (`seed`, `user`, `stream_options`, `max_completion_tokens` and so on) is dropped silently, not rejected. Field by field: [Compatibility](../../reference/compatibility.md).

### 3. Make a streaming call

```ts
import { streamText } from "ai";

const s = streamText({
  model: openai.chat("gpt-4o"),
  prompt: "Count to five, one number per line.",
});

for await (const delta of s.textStream) {
  process.stdout.write(delta);
}
// finishReason "stop"; totalTokens is an integer for openai/openaicompat
// deployments and for cache replays. A live anthropic or bedrock stream has
// no usage frame (what the SDK reports then: check your client version).
console.log("\n", await s.finishReason, (await s.usage).totalTokens);
```

The gateway answers with `Content-Type: text/event-stream`, one `data: {...}` frame per chunk, then `data: [DONE]`. Whether a usage-only frame (`"choices": []` plus `usage`) precedes `[DONE]` depends on the path: a live stream from an `openai` or `openaicompat` deployment carries one; a live stream from an `anthropic` or `bedrock` deployment never carries `usage` in any frame (`gemini` puts it on content frames); and a cache hit or `Idempotency-Key` replay always ends with one, whatever the provider. The SDK resolves `s.usage` and `s.finishReason` after the stream ends, taking `usage` from whichever frame carried it; on a live `anthropic` or `bedrock` stream no frame does, so `s.usage` carries no token counts there (what the SDK reports in that case: check your client version). When the request hits the gateway's response cache, the recording shows the whole answer arriving as a single delta followed by that usage-only frame; the recording's deployment was Bedrock, so the frame came from the replay, not the provider. The usage-only frame carries `"choices": []` since gateway/v0.18.0; gateway/v0.17.0 and earlier send `"choices": null` there, which this SDK's frame schema rejects, so on gateway/v0.17.0 `streamText` fails with `AI_TypeValidationError` at the last frame, after the text deltas have already been delivered, on exactly those paths: OpenAI-shaped live streams and every cache or idempotency replay. Frame-by-frame detail: [Streaming](../streaming.md).

### 4. List models

The recording does not exercise model listing through this SDK, and the SDK has no model-listing call to point at the gateway (check your client version). List the models your key may call with `GET /v1/models` directly; it takes the same bearer token and returns one entry per canonical model, filtered by the key's `allowed_models`, sorted by `id`; a call from outside the key's `allowed_source_cidrs` is a 403 `source_ip_not_allowed` instead. `GET /v1/models` first shipped in gateway/v0.18.0. The call is shown in [curl](curl.md).

## How errors surface

Every error the three OpenAI-shaped routes return (`POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`) is the OpenAI envelope (`POST /v1/messages` answers in Anthropic's) `{"error":{"message","type","param","code"}}` with all four keys present. A request to a path the gateway does not serve, such as the `/v1/responses` call made by `openai("<model>")`, `/v1/completions`, or `/v1/models/` with a trailing slash, is Go's plain-text `404 page not found` with no envelope, so never assume `responseBody` is JSON; the sample below parses it defensively. This SDK does not pick an exception class per status: it throws one `APICallError` whose `statusCode` is the HTTP status and whose `responseBody` is the envelope as text. Only `type` and `code` are a stable contract; message text can change between releases.

| Status | `type` | `code` | Meaning | What this SDK throws |
|---|---|---|---|---|
| 401 | `authentication_error` | `invalid_api_key` (null when the header is missing) | Wrong or missing virtual key secret | `APICallError`, `statusCode` 401, `responseBody` carries `error.code "invalid_api_key"` |
| 403 | `permission_error` | `model_not_allowed`, `source_ip_not_allowed` | This key may not call the model, or its `allowed_source_cidrs` excludes the address you are calling from | `APICallError`, `statusCode` 403, `error.code "model_not_allowed"` or `"source_ip_not_allowed"` |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | The key's rate limit or concurrency cap. `Retry-After` is set: wait that many seconds, then retry | Not exercised in the recording; expect `APICallError` with `statusCode` 429 and the envelope in `responseBody` |
| 429 | `insufficient_quota` | `insufficient_quota` | The key's budget is spent. No `Retry-After`. Do not retry; nothing changes until an operator raises the budget | Not exercised in the recording; same class and status as the row above, so branch on the envelope's `type` |

Branch on the body, not the class:

```ts
import { APICallError } from "ai"; // import path: check your client version

try {
  await generateText({ model: openai.chat("gpt-4o"), prompt, maxRetries: 0 });
} catch (err) {
  if (!(err instanceof APICallError)) throw err;
  // The envelope is JSON only on gateway/v0.18.0 or later and only from
  // the three OpenAI-shaped routes; gateway/v0.17.0 bodies and an unregistered
  // path's 404 are plain text, so never let the parse mask the real error.
  let envelope: { type?: string; code?: string } = {};
  try {
    envelope = JSON.parse(err.responseBody ?? "{}").error ?? {};
  } catch {
    // plain-text body: branch on err.statusCode alone
  }
  if (err.statusCode === 429 && envelope.type === "insufficient_quota") {
    throw err; // budget exhausted: retrying cannot succeed
  }
  if (err.statusCode === 429) {
    // rate_limit_error: read Retry-After from err.responseHeaders (check
    // your client version), sleep that many seconds, retry once.
  }
  throw err;
}
```

`maxRetries: 0` turns off the SDK's own retries, which otherwise hide the first failure and would also retry a budget 429. The envelope itself first shipped in gateway/v0.18.0; in gateway/v0.17.0 and earlier the status codes are the same but `responseBody` is a plain-text line, not this JSON, so parse it defensively as above and branch on `statusCode` alone. Full table, including 400 `model_not_found` for an unknown model (not OpenAI's 404), 502 `upstream_error` and 503 `deployment_capacity_exceeded`: [Error codes](../../reference/error-codes.md).

## Gotchas

- `openai("<model>")` is the Responses API. It 404s against the gateway (`/v1/responses`). Use `openai.chat("<model>")` or `kelvran.chatModel("<model>")`.
- `streamText` on gateway/v0.17.0 fails on the last frame with `AI_TypeValidationError` whenever that release sends its usage-only frame with `"choices": null`: on every live stream from an `openai` or `openaicompat` deployment and on every cache hit or `Idempotency-Key` replay for any provider. A live `anthropic` or `bedrock` stream has no such frame and completes. Since gateway/v0.18.0 the frame carries `"choices": []`. Until you run that release or later, `generateText` is the path that works everywhere.
- `createOpenAICompatible` accepts the virtual key either as `apiKey` (sent as `Authorization: Bearer <apiKey>`, the same header `createOpenAI` sends) or as an explicit `headers: { Authorization: "Bearer ..." }`; the recording used the header form. If both are set, `headers` wins (check your client version).
- By default the SDK retries 408, 409, 429 and 5xx on its own and honours `Retry-After` up to 60 s (check your client version), which suits `rate_limit_error` but also retries a budget 429 that cannot succeed. Pass `maxRetries: 0` and retry yourself when you need to classify errors or avoid those wasted attempts; otherwise keep the default and accept them.
- A cache hit or `Idempotency-Key` replay reports the stored response's own `usage` unchanged, not a recount, so repeating one prompt does not move `usage.totalTokens`. The recording's 24, 20 and 26 came from three different prompts (the 20 from a replayed stream), not from one prompt replayed; do not expect a replay's count to match a fresh call's for a different prompt or provider path.
- The SDK normalizes usage; `usage.raw` is the gateway's own `usage` object as sent. Kelvran's extra usage fields (`cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`, present only when non-zero) appear there, not in the normalized fields (check your client version).

## What Kelvran adds that this client ignores

- Response header `X-Kelvran-Overhead-Duration-Ms`, the gateway's own added latency in milliseconds, on buffered responses only, never on streams. `generateText` exposes response headers as `result.response.headers` (header names are lower-cased by the SDK's fetch layer, so read `result.response.headers?.["x-kelvran-overhead-duration-ms"]`; not exercised in the recording, check your client version). `streamText` never sees it because the gateway does not set it on streams.
- `choices[].message.reasoning_blocks[]` (and `delta.reasoning_blocks[]` on chunks): opaque reasoning content from Anthropic, Bedrock, Gemini and openaicompat deployments. The SDK drops it; it is only visible on the raw response. Anthropic and Bedrock reject a follow-up turn with 400 when a thinking-enabled response's `reasoning_blocks` are not echoed back unchanged on the assistant message. This SDK's message types have no such field, so a multi-turn conversation built from `result.response.messages` drops them (not exercised in the recording; check your client version); against a thinking-enabled Anthropic or Bedrock deployment, keep multi-turn on a non-thinking deployment or call `/v1/chat/completions` directly for those turns.
- A top-level `stop_reason` and, when a stop sequence matched, `stop_sequence` on Anthropic and Bedrock responses, and, inside `choices[]`, on the streaming chunk that sets `finish_reason` (since gateway/v0.19.0); `finish_reason` is unchanged.
- Request extensions the gateway reads from the JSON body (`prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables`, `thinking_binding_mode`, `messages[].parts`, `messages[].cache_control`) and request headers it honours (`Idempotency-Key`, 10-minute window; `X-Kelvran-End-User-Id`; W3C `traceparent` and `baggage`). The recording sends none of them through this SDK; whether a provider-level `headers` option or a per-call option carries them is for you to check (check your client version).

## Not available today

- `POST /v1/responses`: the Responses API, which is where `openai("<model>")` sends requests.
- `POST /v1/completions`: the legacy Completions API. Only chat completions, embeddings and models exist under `/v1`.
- The Anthropic Messages API (`/v1/messages`; `x-api-key` is read on `GET /v1/models` only), so an Anthropic-shaped provider has nothing to call on this gateway yet.
- A first-party Kelvran SDK or provider package. The OpenAI-shaped providers above are the integration path.
- OpenAI-shaped streaming tool-call deltas: the gateway streams flat `{ index, id, name, arguments_json }` elements with no `function` nesting. Tool calling through this SDK is not exercised in the recording.
- OpenAI content arrays on inbound messages (400 `invalid_json`); multimodal input uses Kelvran's own `parts` field. Image or file parts through this SDK are not exercised in the recording.
- Model listing and embeddings through this SDK: not exercised in the recording. The gateway serves `GET /v1/models` and `POST /v1/embeddings`; use [curl](curl.md) for them.
- `X-Kelvran-Overhead-Duration-Ms` on streaming responses.

## Related

- [openai-node](openai-node.md), [openai-python](openai-python.md) and [curl](curl.md) for the same calls in other clients.
- [Streaming](../streaming.md) for every frame the gateway sends, including the usage frame and the in-band error frame.
- [Compatibility](../../reference/compatibility.md), [Error codes](../../reference/error-codes.md), [Data-plane API](../../reference/data-plane-api.md).
- [Quickstart](../../tutorials/quickstart.md) and the [documentation index](../../README.md).
