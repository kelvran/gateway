# Use LlamaIndex with Kelvran

Use this when you already build with LlamaIndex in Python and want its LLM calls to go through a Kelvran gateway instead of straight to a provider. The gateway speaks the OpenAI Chat Completions wire shape, so LlamaIndex's OpenAI-compatible LLM class needs a different base URL, a virtual-key secret as its API key, and `is_chat_model=True` so it uses the chat route rather than the legacy completions route the gateway does not serve. This page covers pointing the client at the gateway, one buffered call, one streaming call, listing models, how Kelvran's errors surface in this client, and the traps specific to it.

## Prerequisites

- A gateway that answers `GET /healthz` with `{"status":"ok"}`. The [quickstart](../../tutorials/quickstart.md) gets you there; its minimal config listens on `:8080` and serves the canonical model `gpt-4o`. Substitute your own `model:` names from `deployments`.
- A virtual key: the raw secret in the environment variable `KELVRAN_KEY`, its SHA-256 as `key_hash` in `config.yaml`. See [Virtual keys and budgets](../virtual-keys-and-budgets.md) and the [first virtual key tutorial](../../tutorials/first-virtual-key-and-budget.md). Keep the secret out of `OPENAI_API_KEY`: on a host that also runs the gateway, that variable is usually the gateway's own upstream credential.
- The `llama-index-llms-openai-like` package installed, together with `llama-index-core`. Client behaviour on this page comes from one recording made on 2026-10-09 with `llama-index-core` 0.14.25, `llama-index-llms-openai` 0.8.2 and `llama-index-llms-openai-like` 0.8.1 against a real gateway; anything beyond that recording is marked "check your client version".

## Steps

### 1. Point the client at the gateway

Use `OpenAILike`, not `OpenAI` (see Gotchas for why). The base URL is the gateway's `listen_addr` plus `/v1`; the API key is the raw virtual-key secret, which the client sends as `Authorization: Bearer <secret>`, the only scheme the gateway accepts.

```python
import os
from llama_index.llms.openai_like import OpenAILike

llm = OpenAILike(
    model="gpt-4o",                        # a canonical model: name from config.yaml, not the provider's upstream_model
    api_base="http://localhost:8080/v1",   # listen_addr ":8080" from config.yaml, plus /v1
    api_key=os.environ["KELVRAN_KEY"],     # the raw virtual-key secret, never its key_hash
    is_chat_model=True,                    # required; without it LlamaIndex calls /v1/completions, which the gateway does not serve
)
```

Under `/v1` the gateway serves exactly three routes: `POST /v1/chat/completions`, `POST /v1/embeddings` and `GET /v1/models`. Any other path is a 404. Route details: [Data-plane API](../../reference/data-plane-api.md).

### 2. Make one buffered chat call

```python
from llama_index.core.llms import ChatMessage   # import path: check your client version

resp = llm.chat([ChatMessage(role="user", content="Say hello in five words.")])
print(resp.message.content)
print(resp.raw.usage)   # CompletionUsage(completion_tokens=11, prompt_tokens=13, total_tokens=24, ...)
```

`chat()` returns a `ChatResponse`. `.message.content` is the answer; `.raw` is the completion the underlying `openai` package parsed, so `.raw.usage` is its `CompletionUsage` with `prompt_tokens`, `completion_tokens` and `total_tokens`. `complete()` also works with `is_chat_model=True`: `llm.complete("Say hello in five words.")` returns a `CompletionResponse` whose `.text` is the answer.

The gateway reads `model`, `messages`, `temperature`, `max_tokens`, `tools`, `tool_choice`, `stream` and `response_format`, plus its own extensions, and silently drops every other request field; nothing else the client sends changes the result. Field-by-field: [Compatibility](../../reference/compatibility.md).

### 3. Make one streaming call

```python
last = None
for last in llm.stream_chat([ChatMessage(role="user", content="Count to five, one number per line.")]):
    pass
print(last.message.content)
```

`stream_chat()` yields one `ChatResponse` per delta (four for a three-line answer in the recording). Each carries the text accumulated so far in `.message.content`, so the last one holds the whole answer; the per-delta increment is on `.delta` (check your client version).

On the wire this is `Content-Type: text/event-stream`, one `data: {...}` frame per chunk, ending with `data: [DONE]`. Whether a usage-only frame precedes `[DONE]` depends on the deployment's provider and on cache replays ([Streaming](../streaming.md)). That frame carries `"choices": []` on main since 2026-10-09, not in gateway/v0.17.0, which sends `"choices": null` there and breaks this call; see Gotchas.

### 4. List the models the key may call

The LlamaIndex LLM classes expose no model-listing call in the recording. Read `GET /v1/models` with curl or the `openai` package instead:

```sh
curl -s http://localhost:8080/v1/models -H "Authorization: Bearer $KELVRAN_KEY"
```

It returns one entry per canonical `model:` name the calling key may use (its `allowed_models` filter applies), sorted by `id`, with no upstream call. The route is on main since 2026-10-08, not in gateway/v0.17.0, where the path is a 404. Same call in other clients: [curl](curl.md), [openai-python](openai-python.md).

## How errors surface in this client

Every data-plane error body is `{"error":{"message","type","param","code"}}`, all four keys always present; only `type` and `code` are a stable contract. `OpenAILike` calls through the `openai` Python package, which picks its exception class from the HTTP status and copies the envelope's `code` onto the exception. The JSON envelope is on main since 2026-10-08, not in gateway/v0.17.0, where the same statuses carry a `text/plain` body and `.code` is empty.

| Status | `type` | `code` | Meaning | This client |
|---|---|---|---|---|
| 401 | `authentication_error` | `invalid_api_key` | Secret matches no configured key | `openai.AuthenticationError`, `.code == "invalid_api_key"`, status 401 |
| 403 | `permission_error` | `model_not_allowed` | Key may not call this model | not exercised for this client; the same status-to-class rule gives `openai.PermissionDeniedError` with `.code == "model_not_allowed"` (check your client version) |
| 429 | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | Key throttled; transient. `Retry-After` is set (integer seconds, minimum 1): sleep for it, then retry | not exercised |
| 429 | `insufficient_quota` | `insufficient_quota` | Key's `budget_usd` is spent. No `Retry-After`; do not retry | not exercised |

The 401 case from the recording:

```python
from openai import AuthenticationError

probe = OpenAILike(
    model="gpt-4o", api_base="http://localhost:8080/v1", api_key="wrong",
    is_chat_model=True, max_retries=0,
)
try:
    probe.chat([ChatMessage(role="user", content="hi")])
except AuthenticationError as e:
    print(e.status_code, e.code)   # 401 invalid_api_key (attribute names: check your client version)
```

`max_retries=0` turns off the `openai` package's own retry loop, whose default policy retries every 429, including a budget `insufficient_quota` that cannot succeed (check your client version). With retries off, branch on `.code` yourself: wait `Retry-After` seconds and retry for `rate_limit_exceeded` and `concurrency_limit_exceeded`; stop for `insufficient_quota` and raise the budget or use another key. Full table, including 400 `model_not_found` for a model no deployment serves (a key with `allowed_models` gets 403 `model_not_allowed` first when the requested model is outside its allowlist; a listed name that no deployment serves is 400 `model_not_found`) and the redacted 502 `upstream_error`: [Error codes](../../reference/error-codes.md).

## Gotchas

1. **`llama_index.llms.openai.OpenAI` rejects Kelvran model names.** `OpenAI(model="extractor", api_base=..., api_key=...)` raises `ValueError: Unknown model 'extractor'. Please provide a valid OpenAI model name in: o1, ...` before any request leaves the process: the class validates the name against OpenAI's own catalogue. Kelvran's canonical names are whatever `config.yaml` says, so use `OpenAILike` for every deployment, whether or not a name happens to match an OpenAI model.
2. **`is_chat_model=True` is mandatory.** Without it LlamaIndex treats the model as a completion model and sends `POST /v1/completions`, a route the gateway does not register. The call fails with a 404 (`openai.NotFoundError`, check your client version) and nothing reaches a provider.
3. **`stream_chat()` fails on gateway/v0.17.0.** The usage-only final frame of a stream has no choices. On main since 2026-10-09 the gateway writes it as `"choices": []`, OpenAI's shape, and `stream_chat()` completes. gateway/v0.17.0 writes `"choices": null` there, and `stream_chat()` raises `TypeError` inside `llama_index/llms/openai/base.py` when it reaches that frame, after the content deltas have arrived. The frame appears on `openai` and `openaicompat` deployments and on every cache or `Idempotency-Key` replay; live `anthropic` and `bedrock` streams send no usage frame ([Streaming](../streaming.md)). On gateway/v0.17.0, use `chat()` instead of `stream_chat()`, or catch the exception knowing the preceding delta already holds the full text.
4. **Only `type` and `code` are stable in errors.** The `message` text, such as `dataplane: auth: identity: invalid virtual key`, can change between releases; match on `e.code`, not on `str(e)`.

## What Kelvran adds that this client ignores

- Response header `X-Kelvran-Overhead-Duration-Ms`: the gateway's own added latency in whole milliseconds, set on buffered responses only, never on streams. `ChatResponse` exposes no response headers in the recording; read the header with `curl -D -` ([curl](curl.md)).
- `choices[].message.reasoning_blocks[]`: opaque extended-thinking blocks from Anthropic and Bedrock deployments. Anthropic and Bedrock require them echoed back unchanged, in order, on the next turn's assistant message, or return a hard 400; the gateway passes them through opaquely and does not check them. A `ChatMessage` rebuilt from `resp.message` carries the text only, so a multi-turn conversation with extended thinking through this client drops them (check your client version). `resp.raw` is the parsed completion; whether extension fields survive on it is a client detail (check your client version).
- Extra `usage` fields, present only when non-zero: `cache_read_tokens`, `cache_creation_tokens`, `reasoning_tokens`. Also `input_transformations[]` on Anthropic responses and `delta.reasoning_blocks[]` on streamed chunks.
- Request extensions the gateway reads when present in the body (`prompt_id`, `prompt_version`, `prompt_label`, `prompt_variables`, `messages[].parts`, `messages[].cache_control`, `thinking_binding_mode`) and request headers it honours (`Idempotency-Key`, `X-Kelvran-End-User-Id`, W3C `traceparent` and `baggage`). How to attach them through this client is a client detail (check your client version); the shapes are in the [Data-plane API](../../reference/data-plane-api.md).

## Verify it worked

Run step 2. Expected: one line with the model's greeting, then a `CompletionUsage(...)` whose `total_tokens` is a positive integer. Then run the 401 probe from "How errors surface": expected `401 invalid_api_key`. On gateway/v0.17.0 the error body is `text/plain` and carries no `code`, so expect the probe to print `401 None` (the `openai` package sets `.code` only from a JSON body; check your client version). To see the gateway's own header, repeat the request with curl and `-D -`: the response carries `X-Kelvran-Overhead-Duration-Ms: <integer>` ([curl](curl.md)).

## Not available today

- `POST /v1/completions`, the legacy Completions API. This is why `is_chat_model=True` is mandatory.
- `POST /v1/responses`, the OpenAI Responses API.
- The Anthropic Messages API (`/v1/messages`) and `x-api-key` authentication; a client speaking the Anthropic wire format cannot use the gateway as a base URL.
- A first-party Kelvran SDK or a Kelvran-specific LlamaIndex integration package. `OpenAILike` with `api_base` is the integration path by recorded decision ([Why no SDK](../../explanation/why-no-sdk.md)).
- OpenAI's array-of-parts `content` on inbound messages (400 `invalid_json`); multimodal input uses Kelvran's `parts` extension.
- OpenAI-shaped streaming tool-call deltas. The gateway streams flat `{index, id, name, arguments_json}` elements with no `function` nesting; buffered tool calls use OpenAI's nesting.
- `X-Kelvran-Overhead-Duration-Ms` on streaming responses.
- Embeddings and tool calling through LlamaIndex: not exercised in the recording. `POST /v1/embeddings` exists for `openai` and `bedrock` deployments ([Data-plane API](../../reference/data-plane-api.md)); whether LlamaIndex's OpenAI embedding class applies the same model-name check as Gotcha 1 is a client detail (check your client version). Confirm the route with [curl](curl.md) first.
- A CI compatibility test that runs LlamaIndex against the gateway. The facts on this page come from the single recording named in Prerequisites.

## Related

- Other clients: [openai-python](openai-python.md), [openai-node](openai-node.md), [curl](curl.md).
- [Streaming](../streaming.md) for the SSE contract and the per-provider usage frame; [Troubleshooting](../troubleshooting.md).
- [Compatibility](../../reference/compatibility.md), [Error codes](../../reference/error-codes.md), [Data-plane API](../../reference/data-plane-api.md).
- [Quickstart](../../tutorials/quickstart.md) and the [documentation index](../../README.md).
