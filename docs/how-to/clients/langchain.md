# Use LangChain with Kelvran

This page shows a developer who already uses LangChain's `langchain-openai` package how to point `ChatOpenAI` at a Kelvran gateway, make one buffered and one streaming call, and recognise the gateway's errors as they surface in this client. Nothing new is installed: the gateway speaks the OpenAI Chat Completions wire shape, so the client only needs a different `base_url` and a virtual-key secret as its `api_key`. Every client behaviour below comes from a recording made on 2026-10-09 against a real gateway with langchain-openai 1.7.0 (langchain-core 1.6.8, openai 3.26.1, Python 3.14); client details the recording does not show are marked "check your client version".

Use this when you have a running gateway and a virtual key, and you want an existing LangChain application to reach its models through Kelvran instead of straight to a provider.

## Prerequisites

- A gateway that answers `GET /healthz` with `{"status":"ok"}`. The [quickstart](../../tutorials/quickstart.md) gets you there; its minimal config listens on `:8080` and serves the canonical model `gpt-4o`. Substitute your own `model:` names from `deployments`.
- A virtual key: the raw secret in an environment variable, its SHA-256 as `key_hash` in `config.yaml`. See [Virtual keys and budgets](../virtual-keys-and-budgets.md) and the [first virtual key tutorial](../../tutorials/first-virtual-key-and-budget.md). The examples use the variable name `KELVRAN_KEY`. Do not reuse `OPENAI_API_KEY`: on a host that also runs the gateway, that variable is usually the gateway's own upstream credential (`api_key_env` in [`gateway/config.example.yaml`](../../../gateway/config.example.yaml)).
- The `langchain-openai` package installed in your project.

## Steps

### 1. Point the client at the gateway

The base URL is the gateway's `listen_addr` plus `/v1`. The client sends `api_key` as `Authorization: Bearer <secret>`, the authentication every `/v1` route accepts (`GET /v1/models`, `POST /v1/messages` and `POST /v1/messages/count_tokens` also take `x-api-key` as the bearer's alias).

```python
import os
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    model="gpt-4o",                        # a canonical model: the name from config.yaml, not the provider's upstream_model
    base_url="http://localhost:8080/v1",   # listen_addr ":8080" plus /v1
    api_key=os.environ["KELVRAN_KEY"],     # the raw virtual-key secret, never its key_hash
)
```

`ChatOpenAI` passes the model string through unchanged, so a Kelvran canonical name that OpenAI has never heard of works; the recording's deployment is Bedrock-backed and named `extractor`. A secret whose hash matches no configured key is `401` with `code` `invalid_api_key`; a missing or malformed header is `401` with `code` null.

### 2. Make one buffered call

```python
from langchain_core.messages import HumanMessage   # import path: check your client version

msg = llm.invoke([HumanMessage("Say hello in five words.")])
print(msg.content)
print(msg.usage_metadata)
print(sorted(msg.response_metadata))
```

`invoke` returns an `AIMessage`. `.content` is the text. `.usage_metadata` is `{'input_tokens': 13, 'output_tokens': 11, 'total_tokens': 24, 'input_token_details': {}, 'output_token_details': {}}` for a short answer: the client maps the gateway's `usage.prompt_tokens`, `completion_tokens` and `total_tokens`. `.response_metadata` has the keys `finish_reason`, `id`, `logprobs`, `model_name`, `model_provider`, `system_fingerprint` and `token_usage`.

The gateway reads these request fields: `model`, `messages`, `temperature`, `max_tokens`, `top_p`, `stop`, `tools`, `tool_choice`, `stream`, `response_format`, plus Kelvran's own extensions. Every other OpenAI field (`n`, `seed`, `user`, `logprobs`, `frequency_penalty`, `presence_penalty`, `logit_bias`, `parallel_tool_calls`, `max_completion_tokens`, `store`, `metadata`, `reasoning_effort`, `stream_options`) is silently dropped: the request succeeds and the field has no effect. `messages[].content` must be a string. OpenAI's `tool_choice` strings `"auto"`, `"required"`, `"none"` and the object `{"type":"function","function":{"name":...}}` are accepted since `gateway/v0.18.0` (in `gateway/v0.17.0` and earlier the string form was `400` and the object form `502`); tool calls are not exercised in the recording (check your client version).

### 3. Make one streaming call

```python
llm_usage = ChatOpenAI(model="gpt-4o", base_url="http://localhost:8080/v1",
                       api_key=os.environ["KELVRAN_KEY"], stream_usage=True)
last = None
for chunk in llm_usage.stream("Count to five, one number per line."):
    print(chunk.content, end="", flush=True)
    last = chunk
print("\n", last.usage_metadata)
```

`stream` yields `AIMessageChunk` objects; the recording's three-line answer arrived as five chunks (the prompt above asks for five lines, so the count you see will not match). On the wire this is `Content-Type: text/event-stream`, one `data: {...}` frame per chunk (`object` `chat.completion.chunk`, one `id` and `created` for the whole stream) and a final `data: [DONE]`. `object` and `created` on frames, and a gateway-minted `id` for Bedrock frames, are present since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier no frame carried `object` or `created` and a Bedrock stream's frames had an empty `id` and `model`. More in [Streaming](../streaming.md).

Usage on a stream needs two things. First, `stream_usage=True` on the model: without it the last chunk's `.usage_metadata` is `None` even when the gateway sends a usage frame. Second, a usage frame has to arrive, which depends on the provider behind the deployment: `openai` and `openaicompat` send one as the final frame (the gateway always asks these upstreams for usage and ignores any `stream_options` the client sends), `gemini` carries usage on content frames, `anthropic` and `bedrock` never do on a live stream, and a cache or `Idempotency-Key` replay always ends with one. That usage-only frame carries `"choices": []` since `gateway/v0.18.0`; `gateway/v0.17.0` and earlier encode the same frame as `"choices": null`, and the gateway's changelog records that LangChain tolerates the null form (check your client version).

A failure after the first chunk is one in-band `data: {"error":{...}}` frame and the stream ends without `[DONE]`, with the HTTP status still `200`; how this client surfaces that frame is not exercised in the recording. The in-band frame is present since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier a failure after the first chunk appended the plain-text error message to the open SSE body with no `data:` prefix, also without `[DONE]`.

### 4. List the models the key may call

The recording exercises no model-listing call through LangChain. Use the route directly: `GET /v1/models` with the same bearer returns one entry per canonical model the key's `allowed_models` permit, sorted by `id`, with no upstream call; the [curl page](curl.md) shows the request. The route exists since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier the path is a `404`.

## How errors surface in this client

Every data-plane error body is `{"error":{"message","type","param","code"}}`, all four keys always present; only `type` and `code` are a stable contract, message text is not. The client raises LangChain-named subclasses of the `openai` exception classes with `.status_code` and `.code` filled from the response; the recording shows two of them. The JSON envelope is present since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier the same statuses carry a `text/plain` body with no `type` or `code`: the exception classes below are unchanged there, but `.code` is `None`, so the `code` column and any `if e.code == ...` dispatch apply only from `gateway/v0.18.0` on (check your client version). Construct the model with `max_retries=0` while exploring errors so that a `429` or `5xx` is not retried before you see it (a `401` or `403` is never retried; check your client version).

| Status | `type` | `code` | `Retry-After` | In this client (recorded 2026-10-09) |
|---|---|---|---|---|
| 401 | `authentication_error` | `invalid_api_key` | no | `langchain_openai.chat_models.base.OpenAIAuthenticationError`, a subclass of `openai.AuthenticationError`; `.status_code == 401`, `.code == "invalid_api_key"` |
| 403 | `permission_error` | `model_not_allowed` | no | `OpenAIPermissionDeniedError`; `.status_code == 403`, `.code == "model_not_allowed"` |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | yes, integer seconds, minimum 1 | not exercised |
| 429 | `insufficient_quota` | `insufficient_quota` | no | not exercised |

```python
import openai

probe = ChatOpenAI(model="gpt-4o", base_url="http://localhost:8080/v1", api_key="wrong", max_retries=0)
try:
    probe.invoke([HumanMessage("hi")])
except openai.AuthenticationError as e:               # also catches the LangChain subclass
    print(type(e).__name__, e.status_code, e.code)    # OpenAIAuthenticationError 401 invalid_api_key  (gateway/v0.17.0: OpenAIAuthenticationError 401 None)
```

The two `429` cases share a status, so the class alone cannot tell them apart. `rate_limit_error` is transient: sleep for `Retry-After` (a per-key exponential backoff, capped at 30 s) and retry. `insufficient_quota` means the key's `budget_usd` is spent and no `Retry-After` is sent; retrying produces the same answer until the budget window resets or an operator raises the cap. The gateway labels the budget case `type: insufficient_quota` so that LangChain's retry logic treats it as permanent; that is the gateway's recorded design intent, not something the recording exercises (check your client version). Other statuses (`400` `model_not_found` for an unknown model, `413` `request_too_large`, `502` `upstream_error`, `503` `deployment_capacity_exceeded`) follow the same class-by-status pattern and are not exercised; the full table is in [Error codes](../../reference/error-codes.md).

## Gotchas

- Streaming usage is `None` by default. Set `stream_usage=True` on the model, and remember that `anthropic` and `bedrock` deployments send no usage frame on a live stream at all (step 3).
- Client retries mask `429` and `5xx`, not `401` or `403`. `ChatOpenAI` hands `max_retries` to the `openai` client it wraps, whose retry predicate retries only `408`, `409`, `429` and `5xx`, so a wrong key or a disallowed model raises on the first response even with the default; a `429`, including a budget `insufficient_quota`, is retried `max_retries` times (default 2) with the SDK's own backoff before it surfaces, which is the delay `max_retries=0` removes (check your client version).
- Catch either class. `OpenAIAuthenticationError` is a subclass of `openai.AuthenticationError`, so an `except openai.AuthenticationError` written for the plain SDK keeps working; the recording does not name the parent of `OpenAIPermissionDeniedError` (check your client version).
- `response_metadata` carries `logprobs` and `system_fingerprint` keys. The gateway's completion has neither field (it sends `id`, `object`, `created` — the last two since `gateway/v0.18.0` — `model`, `choices`, `usage` and, for Anthropic, `input_transformations`), so those values come from the client's defaults, not from Kelvran (check your client version).
- `usage_metadata` detail dicts are empty. Kelvran's extra usage fields (`cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`, present only when non-zero) use Kelvran's names rather than OpenAI's `prompt_tokens_details`, so they are not mapped into `input_token_details` (check your client version). Read them from the raw response if you need them.
- Only ten request fields reach the provider. Any `ChatOpenAI` option that maps to a dropped OpenAI field (step 2) is accepted by the client and ignored by the gateway. Whether `ChatOpenAI(max_tokens=…)` reaches the wire as `max_tokens` (read) or as `max_completion_tokens` (dropped) is not covered by the recording: capture one request body and check your client version before relying on a token cap.
- Multimodal content blocks fail. The gateway requires `messages[].content` to be a string and rejects OpenAI's array-of-parts form with `400` `invalid_json`; a message whose content the client serialises as a list hits this (check your client version). Kelvran's own `parts` field is the multimodal path; see [Compatibility](../../reference/compatibility.md).
- Streaming tool-call deltas are flat `{index, id, name, arguments_json}` with no `function` nesting, so a client-side accumulator that expects `delta.tool_calls[].function.arguments` does not reassemble them (check your client version). Buffered `tool_calls` use OpenAI's nesting.

## What Kelvran adds that this client ignores

- `X-Kelvran-Overhead-Duration-Ms` on buffered `200` chat completions: the gateway's own added latency in whole milliseconds, never set on streams, on errors or on `/v1/embeddings`. The client ignores it; it is reachable only through a raw-response hook on the underlying `openai` client (check your client version).
- `choices[].message.reasoning_blocks[]` and `delta.reasoning_blocks[]`: opaque extended-thinking blocks. The client drops them unless they are read from the raw response. Anthropic and Bedrock require them echoed back unchanged on the next turn's assistant message and reject a later turn that omits them, so a multi-turn conversation with extended thinking through this client needs the raw blocks carried by hand.
- `usage.cache_read_tokens`, `usage.cache_creation_tokens`, `usage.reasoning_tokens`, and `input_transformations[]` on Anthropic responses.
- A top-level `stop_reason` and, when a stop sequence matched, `stop_sequence` on Anthropic and Bedrock responses, and, inside `choices[]`, on the streaming chunk that sets `finish_reason` (since gateway/v0.19.0); `finish_reason` is unchanged.
- Request extensions the gateway reads when present in the body: `prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables`, `thinking_binding_mode`, `messages[].parts`, `messages[].cache_control`, the canonical `tool_choice` object `{mode, tool_name, disable_parallel_tool_use}`; and the request headers `Idempotency-Key`, `X-Kelvran-End-User-Id`, `traceparent` and `baggage`. Whether and how the client lets you attach them is a client detail (check your client version).

## Verify it worked

Run step 2. Expected: one line with the model's greeting, then a `usage_metadata` dict whose `total_tokens` is a positive integer, then the seven `response_metadata` keys listed in step 2. Run the `401` probe from "How errors surface in this client". Expected: `OpenAIAuthenticationError 401 invalid_api_key`. On gateway/v0.17.0 the probe prints `OpenAIAuthenticationError 401 None`, because the error body there is `text/plain` and carries no `code` (the `openai` package sets `.code` only from a JSON body; check your client version). To see the gateway's own header, repeat the buffered request with curl and `-D -`: the response carries `X-Kelvran-Overhead-Duration-Ms: <integer>` ([curl](curl.md)).

## Not available today

- `POST /v1/responses`. Only `POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz`, `GET /readyz` and `HEAD /api/hello` exist; a client mode that targets the Responses API gets a `404` (check your client version for when yours does).
- `POST /v1/completions`, so any completion-style (non-chat) LangChain model class is a `404` (check your client version).
- Exact token counts (`POST /v1/messages/count_tokens` answers `404` until the passthrough leg adds the `anthropic` branch) and the raw-body passthrough to `anthropic` deployments. `POST /v1/messages` itself is served since gateway/v0.19.0 (`x-api-key` read on it and on `GET /v1/models`), so LangChain's Anthropic integration can use Kelvran as a base URL; `ChatOpenAI` remains the documented path for every provider on this page.
- Embeddings and model listing through LangChain: not exercised. `POST /v1/embeddings` exists for `openai` and `bedrock` deployments and its response omits OpenAI's `object` fields ([data-plane API](../../reference/data-plane-api.md)).
- OpenAI's `404` for an unknown model; Kelvran returns `400` `model_not_found`.
- A first-party Kelvran SDK or LangChain integration package; `ChatOpenAI` with `base_url` is the integration path ([Why no SDK](../../explanation/why-no-sdk.md)).
- An automated test in this repository that runs `langchain-openai` against the gateway; this page rests on the 2026-10-09 recording.

## Related

- Other clients: [OpenAI Python](openai-python.md), [OpenAI Node](openai-node.md), [curl](curl.md).
- [Streaming](../streaming.md), [Structured output](../structured-output.md), [Troubleshooting](../troubleshooting.md), [Virtual keys and budgets](../virtual-keys-and-budgets.md).
- [Compatibility](../../reference/compatibility.md), [Error codes](../../reference/error-codes.md), [Data-plane API](../../reference/data-plane-api.md).
- [Quickstart](../../tutorials/quickstart.md), [Documentation index](../../README.md).
