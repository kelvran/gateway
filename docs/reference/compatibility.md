# Compatibility reference

This page states, field by field, how an OpenAI-shaped client behaves against the Kelvran gateway: which request fields the gateway reads, ignores or rejects; the accepted `tool_choice` forms; `response_format` behaviour per provider; the Kelvran extension fields; the streaming wire shape; `GET /v1/models` semantics; the error envelope; and which client SDKs can and cannot use the gateway today. It is for developers pointing an existing OpenAI SDK, framework or coding agent at Kelvran, and for operators deciding whether a given client will work. Every statement here is taken from the gateway source; where behaviour differs from OpenAI's own API, the difference is named.

Versions: the latest tagged release is `gateway/v0.20.0` (2026-10-15). Items that first shipped in `gateway/v0.20.0` (absent from `gateway/v0.19.0` and earlier), in `gateway/v0.19.0` (absent from `gateway/v0.18.0` and earlier) or in `gateway/v0.18.0` (absent from `gateway/v0.17.0` and earlier) are marked with the release they shipped in; the table below lists them by release. The release-to-release promise for these surfaces is in [docs/VERSIONING.md](../VERSIONING.md).

## Items added since gateway/v0.17.0, by release

| Item | First release; source | Behaviour in the release before |
|---|---|---|
| `GET /v1/models` | `gateway/v0.18.0`; `gateway/cmd/gateway/models_handler.go` | Route absent |
| `POST /v1/messages` (the Anthropic Messages ingress, buffered and SSE, Anthropic error envelope, `x-api-key` alias) | `gateway/v0.19.0`; `gateway/cmd/gateway/messages_handler.go` | Route absent (404) |
| `POST /v1/messages/count_tokens` (authenticated, allowlisted, one RPM token; an `anthropic` deployment's own count as received, `404` `count_tokens_unavailable` on every other) and `connect claude --check` reading `403` `model_not_allowed` as a proven credential | `gateway/v0.19.0`; `gateway/cmd/gateway/count_tokens_handler.go`, `gateway/internal/cli/connect.go` | Route absent (plain 404); `--check` exits 1 on every key, allowlisted or not, because `/v1/messages` is absent too |
| OpenAI `tool_choice` string and function-object forms | `gateway/v0.18.0`; `gateway/internal/adapter/tool_choice_wire.go` | String form: `400 invalid request body`; object form: `502 unknown tool_choice mode ""` |
| OpenAI-shaped JSON error envelope | `gateway/v0.18.0`; `gateway/cmd/gateway/error_envelope.go` | `text/plain` body, same status codes |
| `id`, `object`, `created` on every completion and chunk | `gateway/v0.18.0`; `gateway/internal/gateway/dataplane/completion_envelope.go` | Bedrock responses carry `"id": ""`; no response carries `object` or `created` |
| In-band `data: {"error":…}` frame for mid-stream failures | `gateway/v0.18.0`; `gateway/cmd/gateway/error_envelope.go` | Stream ends without an error frame |
| `claude-sonnet-5` on the Bedrock structured-output whitelist | `gateway/v0.18.0`; `gateway/internal/adapter/capabilities.go` | `response_format` to a Sonnet 5 Bedrock deployment fails with `502` `upstream_error` (the status is `400` `response_format_unsupported` since gateway/v0.19.0) unless another capable deployment serves the same `model` |
| `tools` and `tool_choice` folded into cache keys | `gateway/v0.18.0`; `gateway/changelog/0.18.0.md` | Identical messages with different `tool_choice` can share a cached response |
| Thinking configuration (`adapter.ChatRequest.Thinking`, a `/v1/messages` field) folded into cache keys | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | Field absent; keys do not fold it. No `/v1/chat/completions` behaviour differs — every key changes once on upgrade |
| `stop` (string or array) and `top_p` forwarded to every provider | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | Both accepted and dropped silently; the provider never saw them |
| `top_k` and `effort` (`/v1/messages` fields) canonical, by provider capability; the sampling fields folded into cache keys and the `Idempotency-Key` fingerprint | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | Fields absent; keys do not fold them. A `/v1/chat/completions` body carrying `top_k` or `effort` is ignored as before |
| Tool-result `parts` (image, document) on a `role: tool` message carried to `anthropic` (a `tool_result` block array) and `bedrock` (`toolResult.content` blocks); `openai`/`openaicompat` text parts only, `gemini` none — such a request is routed to a capable deployment in the pool, else `400` `tool_result_parts_unsupported` | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | `anthropic`, `bedrock` and `gemini` rejected the message before any upstream call (`502` `upstream_error`); `openai`/`openaicompat` forwarded an `image_url` part the provider rejects (a document part was already the adapter's own `502`) |
| A response carrying a provider block the canonical schema cannot represent (an Anthropic block or delta type newer than the adapter) is never cached | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | Cached and replayed without the block |
| The Anthropic ingress's passthrough fingerprint (the members of a `/v1/messages` body the canonical schema cannot hold, and its `is_error` tool results) folded into cache keys — empty for every `/v1/chat/completions` request; `parts[].text` bounded at 8 MiB like `content` | `gateway/v0.19.0`; `gateway/changelog/0.19.0.md` | Keys did not fold it (no `/v1/messages` route); a text part was bounded only by the 32 MiB body cap |
| Claude 5.x request gates: fixed sampling dropped and logged, prefill `400` `assistant_prefill_unsupported` (a routing property), `thinking: disabled` rewritten or dropped per family, Sonnet 5.5 a forced-`tool_choice` rejecter, Bedrock `required` refused on the rejecters | `gateway/v0.20.0`; `gateway/internal/adapter/capabilities.go` | A `temperature`, `top_p` or `top_k` the Claude 5.x model rejects, a prefill, `thinking: disabled` on Sonnet 5.5/Fable 5.1/Opus 5.5 and `tool_choice: required` on Bedrock Sonnet 5.5 all reach the model and come back as `502` `upstream_error` (the 2026-10-08 live verification's F5) |
| Compaction usage metered from `usage.iterations` (both compaction betas, through the `/v1/messages` passthrough to an `anthropic` deployment): the summed iterations replace the top-level counts for cost, budget, both TPM limiters, telemetry and the ledger; such a response is never cached; `kelvran.usage.compaction_iterations` on the chat span | `gateway/v0.20.0`; `gateway/internal/adapter/anthropic/anthropic.go` (`sumIterations`) | An on-demand compaction bills the key $0 and a threshold compaction omits the summarization call on a buffered body (on a stream, the compaction's input and the reply's output only) (the 2026-10-14 round-5 research's item 3) |
| OpenAI `service_tier` forwarded on `openai` deployments and the served tier priced (`price_table.<model>.service_tier_multipliers`; `deployments.<name>.service_tier_policy: forward` default, `strip` to close); `kelvran.usage.service_tier` on the span for OpenAI too | `gateway/v0.20.0`; `gateway/internal/costaccounting/costaccounting.go` (`billedTierMultiplier`) | `service_tier` accepted and dropped silently (the provider never saw it); a tier OpenAI served anyway (a project-level Fast default) billed at Standard |
| Long-context price tiers (`price_table.<model>.long_context`: a request whose cache-inclusive prompt exceeds `above_tokens` billed at the row's two multipliers for the whole request, before the US and service-tier multipliers; `kelvran.pricing.long_context_tier` on the span; `kelvran init` writes Haiku 5.5's `100000 / 5 / 5` and the GPT-6/5.6 rows' `272000 / 2 / 1.5`) | `gateway/v0.20.0`; `gateway/internal/costaccounting/costaccounting.go` (`LongContextTier`) | Every prompt length billed at Standard: Haiku 5.5 over 100,000 tokens under-billed 5×, the GPT-6/5.6 lines over 272K 2× input / 1.5× output (item (i)'s documented residual; the 2026-10-14 round-5 research's item 5) |
| Thinking-binding gate widened to Claude Sonnet 5.5 and Haiku 5.5: requests to them on `/v1/chat/completions` served by an `anthropic` deployment carry `thinking: {type: adaptive, block_binding: {prefix_mismatch_behavior: drop_block}}` (the `/v1/messages` passthrough relays the caller's body unchanged) (`error` under `thinking_binding_mode: strict`) and the `thinking-binding-controls-2026-08-01` beta, as Fable 5.1 and Opus 5.5 already did; `input_transformations[].reason` documents `organization_binding_mismatch` | `gateway/v0.20.0`; `gateway/internal/adapter/anthropic/anthropic.go` (`thinkingBlockBindingModelSubstrings`) | Those two models ran Anthropic's own default — strict `error` on accounts created on or after 2026-08-31 — so an admin prompt edit could `400` an in-flight reasoning conversation, the failure Kelvran's non-strict default exists to prevent; on accounts created before 2026-08-31 no check at all — Kelvran's always-set `prefix_mismatch_behavior` opts such an account into the check at `drop_block`, so a stale replayed block that was previously accepted is now dropped and reported in `input_transformations[]` (the 2026-10-14 round-5 research's item 6) |
| Pre-decode body check on the OpenAI routes: `POST /v1/chat/completions` and `POST /v1/embeddings` run the Anthropic ingress's `CheckBody` before `encoding/json`, so a member that appears twice in one object, nesting beyond 64 levels or invalid UTF-8 is `400` `invalid_json` (`invalid request body: the body has a duplicate member "model"`, `… /messages/0 has a duplicate member "content"`, `… nests deeper than 64 levels`, `… not valid UTF-8`) before any pipeline work | on `main` since 2026-10-15, not in `gateway/v0.20.0`; `gateway/cmd/gateway/main.go`, `gateway/internal/ingress/anthropicmsgs/bodycheck.go` | Accepted: `encoding/json` kept the last duplicate, put U+FFFD where the UTF-8 was invalid and nested up to its own 10,000-level limit (`exceeded max depth`); `/v1/messages` has refused the same shapes since `gateway/v0.19.0` |

## Routes

| Method | Path | Auth | Status |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | `Authorization: Bearer <virtual key secret>` | In `gateway/v0.17.0` |
| `POST` | `/v1/embeddings` | `Authorization: Bearer <virtual key secret>` | In `gateway/v0.17.0` |
| `GET` | `/v1/models` | `Authorization: Bearer <virtual key secret>`; `401` envelope without it | Since `gateway/v0.18.0` |
| `POST` | `/v1/messages` | `Authorization: Bearer <virtual key secret>` or `x-api-key: <virtual key secret>` | Since `gateway/v0.19.0` (item 11 slice S10a): the Anthropic Messages shape, buffered and streaming, with the Anthropic error envelope |
| `POST` | `/v1/messages/count_tokens` | `Authorization: Bearer <virtual key secret>` or `x-api-key: <virtual key secret>` | Since `gateway/v0.19.0` (slices S10b/S11c): an `anthropic` deployment's own count as received; `404` `count_tokens_unavailable` on every other deployment |
| `GET` | `/healthz`, `/readyz` | Not covered here | Operational routes, not OpenAI-shaped |

`/v1/models` is registered on the exact path. `/v1/models/` (trailing slash) is a `404`, never a `301`.

The OpenAPI 3.1 document for the data-plane and admin listeners is `gateway/openapi/kelvran-gateway.openapi.yaml` (since `gateway/v0.20.0`; [what it covers and guarantees](openapi.md)).

Not available today:

- Exact token counts on a translate hop: an `anthropic` deployment receives the request as sent and answers with its own bytes — turns buffered and streamed, and `count_tokens` (since `gateway/v0.19.0`); every other deployment is a translate hop, where `POST /v1/messages/count_tokens` answers `404`: the gateway translates no provider's token-counting call and relays only an `anthropic` deployment's.
- OpenAI Responses API, legacy Completions API, images, audio and files routes. `gateway/cmd/gateway/main.go` registers no such routes.

## Authentication

| Item | Behaviour |
|---|---|
| Header | `Authorization: Bearer <secret>` (`gateway/internal/identity/identity.go`); on `GET /v1/models`, `POST /v1/messages` and `POST /v1/messages/count_tokens`, `x-api-key: <secret>` is the bearer's alias and a non-empty `Authorization` wins |
| Lookup | SHA-256 of the presented secret, matched against configured `key_hash` values |
| Missing or malformed header | `401`, `type: authentication_error`, `code: null` |
| Unknown key | `401`, `type: authentication_error`, `code: invalid_api_key` |
| Expired key | `401`, `type: authentication_error`, `code: key_expired` — the key's `expires_at` has passed (since `gateway/v0.18.0`) |
| `x-api-key` header | Read on `GET /v1/models`, `POST /v1/messages` and `POST /v1/messages/count_tokens` as the bearer's alias (`bearerFromRequest`, `gateway/cmd/gateway/mux.go`; a non-empty `Authorization` wins), not on the chat or embeddings routes. It also appears on the outgoing upstream request the gateway builds for `anthropic` deployments (`gateway/internal/gateway/dataplane/dataplane.go`, `setUpstreamAuthHeaders`) |

The OpenAI SDKs send the `api_key` constructor argument as `Authorization: Bearer`, so `OpenAI(base_url=…, api_key=os.environ["KELVRAN_KEY"])` authenticates with the raw virtual-key secret. The Anthropic SDK's default `x-api-key` path authenticates on `GET /v1/models`, `POST /v1/messages` and `POST /v1/messages/count_tokens` (all three since `gateway/v0.19.0`); on the chat and embeddings routes it is a `401`. Example secrets in [gateway/config.example.yaml](../../gateway/config.example.yaml) are `example-team-alpha-secret-do-not-use` and `example-team-beta-secret-do-not-use`.

## Request headers the gateway reads

| Header | Routes | Meaning |
|---|---|---|
| `Authorization` | all `/v1/*` | Bearer virtual key, above |
| `Idempotency-Key` | `/v1/chat/completions` (buffered and streaming) | Replay protection; see [Idempotency-Key](#idempotency-key) |
| `X-Kelvran-End-User-Id` | `/v1/chat/completions` | Caller-supplied, unauthenticated end-user identity. Consulted only when the virtual key has cache scoping to end users enabled; otherwise a no-op (`gateway/internal/gateway/dataplane/dataplane.go`, `EndUserIDHeader`) |
| `traceparent`, `baggage` | all `/v1/*` | W3C trace context. A client-sent `traceparent` is honoured; an `agent_run_id` baggage entry attributes cost ([README.md](../../README.md)) |

## Response headers the gateway sets

| Header | When |
|---|---|
| `Content-Type: application/json` | Buffered success on `/v1/chat/completions`, `/v1/embeddings`, `/v1/models` |
| `Content-Type: application/json; charset=utf-8` | Every error envelope |
| `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive` | Streaming `/v1/chat/completions` |
| `X-Kelvran-Overhead-Duration-Ms` | Buffered `/v1/chat/completions` success only: wall-clock time minus upstream time, in milliseconds. Never set on a stream |
| `Retry-After` | `/v1/chat/completions` and `/v1/messages` (buffered and streaming): `429` for rate-limit and concurrency rejections (not for budget), `502`, and `503 deployment_capacity_exceeded`; `/v1/messages/count_tokens` on its `429`; not on an `anthropic` deployment's relayed `400`/`422`. Never set on `/v1/embeddings` or `/v1/models` |
| `Allow` | `405` responses |
| `X-Content-Type-Options: nosniff` | Error envelopes and `/v1/models` |

## Chat completion request fields

The handler decodes the body with plain `json.Unmarshal` into `adapter.ChatRequest` (`gateway/internal/adapter/types.go`). Unknown fields are ignored without error or warning.

### OpenAI fields the gateway reads

| Field | Type | Behaviour |
|---|---|---|
| `model` | string | Canonical model name from the operator's `model:` config, resolved to a deployment. An unknown model is `400`, `code: model_not_found` (OpenAI returns `404`) |
| `messages` | array | At most 2000 entries, else `400 invalid_request`. Zero messages after prompt resolution is `400`, `code: empty_messages`, `param: messages` |
| `temperature` | number | Forwarded; on an `anthropic`/`bedrock` deployment serving a fixed-sampling Claude family any value but `1` is dropped and logged — see [Claude 5.x request gates](#claude-5x-request-gates) |
| `max_tokens` | integer | Forwarded |
| `top_p` | number | Forwarded (since gateway/v0.19.0; dropped silently before); on a fixed-sampling Claude family any value but `0.99`, and `0.99` beside `temperature: 1`, is dropped and logged — see [Claude 5.x request gates](#claude-5x-request-gates) |
| `stop` | string or array of strings | Forwarded as an array to every provider (since gateway/v0.19.0; dropped silently before); provider limits are the provider's own `400` |
| `service_tier` | string | Forwarded verbatim on `openai` deployments (since `gateway/v0.20.0`; dropped silently before) unless the deployment's `service_tier_policy` is `strip`; dropped and logged on every other provider; never validated. The tier the response reports is priced — see [Service tier pricing](#service-tier-pricing-service_tier) |
| `tools` | array | At most 256 entries, else `400 invalid_request`; see [tools](#tools) |
| `stream` | boolean | Selects the SSE path |
| `response_format` | object | See [response_format](#response_format) |
| `tool_choice` | string or object | See [tool_choice](#tool_choice) |

### OpenAI fields the gateway drops silently

`ChatRequest` has no field for any of these. A request that sets them is accepted and the value is never forwarded: `n`, `seed`, `user`, `logprobs`, `top_logprobs`, `frequency_penalty`, `presence_penalty`, `logit_bias`, `stream_options`, `store`, `metadata`, `parallel_tool_calls`, `max_completion_tokens`, `reasoning_effort`, `modalities`, `audio`, `prediction`, `web_search_options`.

Consequences: `max_completion_tokens` does not cap output (use `max_tokens`); `n` never yields more than one choice; `stream_options.include_usage` from the client is ignored (the gateway sets it itself, see [Streaming](#streaming)).

### Kelvran extension fields (not in OpenAI)

| Field | Type | Meaning |
|---|---|---|
| `prompt_id` | string | Names a server-side prompt template to resolve into `messages` before routing. Any virtual key may reference any prompt. See [prompt management](../how-to/prompt-management.md) |
| `prompt_version` | integer | Pins `prompt_id` to a stored version; `<= 0` means latest. Mutually exclusive with `prompt_label` (`400`, `code: invalid_prompt_reference`) |
| `prompt_label` | string | Resolves `prompt_id` through an operator-mutable label such as `production` |
| `prompt_variables` | object of string | `{{name}}` substitutions for the resolved prompt |
| `thinking_binding_mode` | `""`, `non_strict`, `strict` | Anthropic preserved-thinking prefix check. `""` and `non_strict` request Anthropic's non-strict `drop_block`; `strict` requests `error`. Applies only to Anthropic models matching `claude-fable-5-1`, `claude-opus-5-5`, `claude-sonnet-5-5` or `claude-haiku-5-5` (the four the preserved-thinking page names, re-read 2026-10-14; Mythos 5.1 and models before Fable 5.1 do not run the check); a no-op everywhere else |
| `messages[].parts` | array | Multimodal content; see [messages](#messages) |
| `messages[].cache_control` | object `{ttl, key}` | Provider-side prompt caching marker. `ttl` (`""` = provider default, `"1h"`) is read by the direct Anthropic adapter and, on `bedrock`, forwarded as `cachePoint.ttl` only for model ids containing `claude-opus-5`, `claude-opus-4-8`, `claude-opus-4-7`, `claude-opus-4-6`, `claude-opus-4-5`, `claude-sonnet-5`, `claude-sonnet-4-6`, `claude-sonnet-4-5` or `claude-haiku-4-5` (`bedrockCacheTTLModelSubstrings` in `gateway/internal/adapter/bedrock/bedrock.go`); on every other Bedrock model the marker is sent without `ttl`, so Bedrock's 5-minute default applies. `key` is read only by the OpenAI adapter as `prompt_cache_key` |
| `messages[].reasoning_blocks` | array | Opaque reasoning blocks to echo back; see [messages](#messages) |
| `tools[].cache_control` | object | Per-tool caching marker, sibling of `type`/`function` |
| `tools[].strict` | boolean | Strict schema for this tool, sibling of `type`/`function`. Forwarded by Anthropic, OpenAI, openaicompat and Bedrock; Gemini ignores it |
| `tool_choice.disable_parallel_tool_use` | boolean | Honoured only by the direct Anthropic adapter; every other adapter ignores it |

Setting both `prompt_id` and `messages` is `400`, `code: invalid_prompt_reference`.

### Claude 5.x request gates

Since `gateway/v0.20.0` (round-5 research 2026-10-14, Tier A item 2) the gateway knows four request shapes the Claude 5.x generation answers `400` to, and handles each before the model sees it. The rules are provider-agnostic (`anthropic` and `bedrock` deployments alike; the direct API and Converse run the same per-model validation) with one scope limit: on an `anthropic` deployment only `/v1/chat/completions` traffic is gated, because a `/v1/messages` body is relayed to it as received and Anthropic's own `400` is relayed under its envelope — so the `thinking` rewrite below is reachable only on `bedrock`. Matched at a family boundary (`claude-sonnet-5` never admits `claude-sonnet-5-5`), and listed in `gateway/internal/adapter/capabilities.go` with a proven-or-documented label per family. Every row below marked proven was probed on Bedrock on 2026-10-14 (`max_tokens: 1`); Mythos 5.1 is placed as Fable 5.1's twin by Anthropic's release note, Opus 5.5's thinking rule by its own release note.

| Shape | Families | What the gateway does | Client sees |
|---|---|---|---|
| `temperature` other than `1`, `top_p` other than `0.99`, `top_p` beside `temperature: 1`, any `top_k` (fixed sampling) | Sonnet 5, Sonnet 5.5, Haiku 5.5, Fable 5.1 (proven), Mythos 5.1 (documented). Opus 5.5 is deliberately not gated: unprobeable on the verifying account and undocumented | The value is dropped before the call (the pair keeps `temperature`) and logged on the `request_field_dropped` line `top_k`/`effort`/`thinking` drops already use, with field, value and reason | `200`, the model's only sampling; before 2026-10-14 a `502 upstream_error`. The response carries no marker of the drop (operator log only), so `temperature: 0` determinism is not available on these families; cache keys and the `Idempotency-Key` fingerprint still fold the caller's values, so two requests differing only in a dropped value are two cache entries |
| A conversation ending on an `assistant` message (prefill) | Sonnet 4.6, Sonnet 5, Sonnet 5.5, Haiku 5.5, Fable 5.1 (proven), Mythos 5.1 (documented) | Prefill is a routing property: a pool with a deployment that accepts it serves the request there; otherwise refused before any upstream call. A `/v1/messages` body relayed as received to an `anthropic` deployment is never checked (a `bedrock` translate hop is) | `400`, `code: assistant_prefill_unsupported`, `param: messages`; before, the model's `400` relayed as a `502` |
| `thinking: {"type": "disabled"}` (a `/v1/messages` field) | Sonnet 5.5 (proven): rewritten to `between_tools`, its documented way to turn up-front thinking off. Fable 5.1 (proven), Mythos 5.1 and Opus 5.5 (documented): dropped, the request runs as one without a `thinking` field. Haiku 5.5, Sonnet 5 and the 4.x families accept `disabled` and get it verbatim | Logged on the same `request_field_dropped` line, the reason naming the rewrite or the drop | `200` |
| Forced `tool_choice` (`required`, `tool`) | Fable 5.1, Mythos 5.1, Opus 5.5, and since `gateway/v0.20.0` Sonnet 5.5 (proven); Haiku 5.5 accepts a forced choice (proven) | Adapter error before any upstream call, on `bedrock` now for `required` (Converse's `any`) as well as `tool`; see [Forced tool choice by provider and model](#forced-tool-choice-by-provider-and-model) | `502`, `code: upstream_error` (the pre-existing convention for adapter errors) |

### Compaction usage (`usage.iterations`)

Since `gateway/v0.20.0` (round-5 research 2026-10-14, Tier A item 3) the `anthropic` adapter meters Anthropic's two compaction betas from `usage.iterations`. Both reach Anthropic only through the `/v1/messages` passthrough to an `anthropic` deployment: on-demand compaction (`anthropic-beta: compact-2026-09-04`) is a top-level `compaction` request member and threshold compaction (`compact-2026-01-12`) a `context_management` edit, neither of which the canonical schema holds (a `bedrock` translate hop records them as lossy), and AWS supports compaction only on InvokeModel, never on the Converse API the `bedrock` adapter speaks. The client receives Anthropic's bytes unchanged; what changes is what the gateway bills, debits and records.

| Response | Anthropic's top-level `usage` | Kelvran meters |
|---|---|---|
| On-demand compaction (`stop_reason: compaction`, one `compaction` content block) | `input_tokens` and `output_tokens` are `0`; the summarization call appears only in `iterations[{type: compaction}]` | The iteration's counts (live 2026-10-14: 172 / 241; before, $0) |
| Threshold compaction (an ordinary `stop_reason`, a `compaction` block before the reply) | The reply alone; the compaction iteration appears only in `iterations` | The sum of every iteration (live: 57,411 + 165 / 64 + 56). Before: the reply alone on a buffered body; on a stream the compaction's input from `message_start` and the reply's output from `message_delta` (57,411 / 16 live), missing the reply's input and the compaction's output |
| Beta on, no compaction | Equal to the single `message` iteration | Unchanged |
| Failed summarization (a `200` with empty `content` and an ordinary `stop_reason`) | Zeros; the attempt still appears in `iterations` | The iteration's counts |

Rules. When `iterations` is present and non-empty it replaces the top-level counts, never adds to them (the `message` entries already equal the top-level numbers), floored per count at the top-level value so a partially populated array can never meter below what the top-level counts say. Each entry's `cache_read_input_tokens`, `cache_creation_input_tokens` and `cache_creation` breakdown feed the cache tiers and the 1-hour rate. On a stream the array rides the final `message_delta`; `message_start` is honoured as a fallback. `reasoning_tokens`, the server-tool counts, `service_tier` and `inference_geo` stay per response. An entry naming another model (`advisor_message`, `fallback_message`) is summed and priced at the serving model's rates, with one `usage_iterations_foreign_model` warning line. A response that ran a compaction is never written to the cache — its output is bound to that moment's conversation, and a failed summarization would otherwise be replayed for the TTL. The Idempotency-Key store replays only a representable compaction response (a failed summarization): a response carrying a `compaction` block is unrepresentable, its claim is released (`idempotency_complete_skipped_unrepresentable`) and a retried key re-runs and re-bills the call, RFC-1 §8's rule unchanged. A compaction's tokens are debited from the key's TPM bucket in full at settle and raise that key's reservation size for later requests; size TPM for keys that enable the compaction betas accordingly. The chat span carries `kelvran.usage.compaction_iterations` when at least one ran; `stop_reason: compaction` maps to `finish_reason: compaction` verbatim.

Evidence (real Bedrock InvokeModel responses, Anthropic's native shape, 2026-10-14; the bodies are the fixtures under `gateway/internal/adapter/anthropic/testdata/`): on-demand compaction proven on Haiku 5.5 (buffered and streamed) and Sonnet 5.5 (buffered); threshold compaction proven on Sonnet 4.6 above the trigger (buffered and streamed) and below it; beta on with no compaction proven on Haiku 5.5. The failed-summarization row follows Anthropic's compaction-on-demand page and is not yet observed live; the stream fixture with `iterations` on `message_start` is synthetic (the fallback placement). Two documentation claims fell: AWS's "the `iterations` array is only present when a new compaction is triggered" (it is present on every response once the beta is on, on Bedrock too), and Sonnet 5.5's Bedrock model card omitting on-demand compaction (it accepts it).

### Service tier pricing (`service_tier`)

Since `gateway/v0.20.0` (round-5 research 2026-10-14, Tier A item 4) OpenAI's `service_tier` is forwarded and priced. The request member (`auto`, `default`, `flex`, `scale`, `priority`, `fast`) reaches an `openai` deployment verbatim unless the deployment sets `service_tier_policy: strip`, in which case it is dropped and logged (`request_field_dropped`, reason naming the policy) and the provider runs its default tier. Every other provider has no such member — Anthropic's own `service_tier` takes `auto | standard_only` on `/v1/messages`, a different vocabulary, and the passthrough relays it untouched — so the dataplane drops it there with the same line. The gateway validates nothing: the enum, and a tier a model lacks, are the provider's own `400`.

Pricing follows the tier the response says was served, never the one requested: a `fast` request that OpenAI ramp-downgrades comes back `default` and is billed Standard; `auto` is a routing preference, never a price. `price_table.<model>.service_tier_multipliers` holds the multiple of Standard per tier, applied to the whole token cost (input, cached input, cache write and output scale alike on the pricing page) beside the US multiplier and before the unscaled search fee; `fast` and `priority` are one price column (OpenAI accepts both and bills them alike; which literal it echoes depends on the model generation), so either key serves both. When the response is silent (an older model, a backend that never echoes the member) a requested premium tier is billed at its multiplier — never undercount a lever the client pulled — and a requested discount is never applied unconfirmed; a SERVED non-base tier that a priced model's map lacks (`scale`, a future tier, or a price row written without the map) bills Standard and logs `service_tier_unpriced` once (openai deployments only; a silent response with a requested tier the map lacks bills Standard with no line; a model absent from `price_table` bills $0 and never warns — `kelvran doctor` names it). Anthropic's `usage.service_tier` is never priced, whatever a Claude row carries. The served tier reaches the chat span as `kelvran.usage.service_tier`. The member is in the `Idempotency-Key` fingerprint like every wire field but deliberately not in cache keys: it changes price and latency, not what the model produces, and a cache hit bills $0 either way.

| Model (pricing page, 2026-10-14) | `flex` | `priority` / `fast` | `ultrafast` (Responses API only, see below) |
|---|---|---|---|
| gpt-6-astra, gpt-6.1-sol | 0.5 | 2 | 6 |
| gpt-6-sol, gpt-6-luna, gpt-5.6-sol, gpt-5.6-luna, gpt-5.6-terra | 0.5 | 2 | — |
| gpt-5.5 (not in `kelvran init`'s table) | 0.5 | 2.5 | — |
| gpt-4o | — (no Flex) | 1.7 | — |

`kelvran init` writes the GPT-6, GPT-5.6 and gpt-4o rows; gpt-5.5 is not in its table, so add `service_tier_multipliers: {flex: 0.5, priority: 2.5}` by hand (a `--price` row for a model outside the table carries no multipliers; on an embedded row `--price` keeps them while replacing the rates). `ultrafast` is a tier of OpenAI's Responses API, which the gateway has no route for: Chat Completions does not offer it, so through this gateway the lever is 0.5× to 2.5× of Standard. The gateway forwards the member unchanged and the `ultrafast: 6` rows are kept, so should Chat Completions ever accept it (or a Responses route land) it is priced from day one. A tier the loader does not know is accepted at 1 or above (a new OpenAI tier can be priced without a release; a typo can only over-bill) and refused below 1. The lever this opens — a client choosing a premium tier on the operator's key — is bounded like `max_tokens` by budgets and TPM (the budget reservation is not request-aware, so the overshoot past `budget_usd` is one request's cost at the tier multiple), always billed, and closable per deployment with `service_tier_policy: strip`; a per-key allowed-tiers list is a follow-up. A requested premium tier also sizes the mid-stream budget top-up, so a `fast` stream OpenAI downgrades to `default` may be stopped earlier than a Standard one on a nearly exhausted key.

## Messages

| Field | Type | Behaviour |
|---|---|---|
| `role` | string | `system`, `user`, `assistant`, `tool` |
| `content` | string | Must be a JSON string. OpenAI's content-array form (`[{"type":"text",…},{"type":"image_url",…}]`) fails to decode: `400`, `code: invalid_json` |
| `parts` | array of `ContentPart` | Kelvran's multimodal content, alongside or instead of `content` |
| `tool_calls` | array | OpenAI nesting `{id, type: "function", function: {name, arguments}}` on request and response |
| `tool_call_id` | string | On `role: tool` messages |
| `cache_control` | object | Extension, above |
| `reasoning_blocks` | array | Extension; see below |
| `refusal` | string | Response only. Populated by `openai` and `openaicompat`; permanently empty from Anthropic, Gemini and Bedrock, which signal the condition through `finish_reason` instead |

### `parts[]` (ContentPart)

| Field | Type | Rule |
|---|---|---|
| `type` | `text`, `image`, `document` | Required |
| `text` | string | When `type` is `text` |
| `media_type` | string | MIME type, when `type` is not `text` |
| `data` | string | Base64 inline content. Exactly one of `data` or `url` when `type` is not `text` |
| `url` | string | Passed to the provider verbatim; the gateway never fetches it. Bedrock rejects URL parts (inline base64 only) |
| `cache_control` | object | Per-part caching marker |

Not available today: inbound translation of OpenAI's `content` array into `parts`. An OpenAI SDK multimodal request (`content=[{"type":"image_url",…}]`) is rejected. Shape per [docs/rfcs/2026-09-06-gateway-multimodal-content.md](../rfcs/2026-09-06-gateway-multimodal-content.md).

### `reasoning_blocks[]` (ReasoningBlock)

| Field | Type | Meaning |
|---|---|---|
| `sequence` | integer | Position relative to `tool_calls`: `N` means immediately before `tool_calls[N]` |
| `redacted` | boolean | Provider-encrypted block; the gateway never inspects `data` |
| `text` | string | Plaintext reasoning when not redacted |
| `data` | string | Opaque ciphertext when redacted |
| `signature` | string | Provider signature over `text`; replay verbatim |

A client that continues a conversation with Anthropic or Bedrock must echo the exact `reasoning_blocks` slice, unmodified and in order, on the assistant message it belongs to; otherwise the provider returns `400`. OpenAI SDK message types have no such field, so a client that rebuilds the assistant turn from `message.content` and `message.tool_calls` alone drops them. Shape per [docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md](../rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md).

## Tools

| Item | Rule |
|---|---|
| Wire shape | OpenAI's `{type: "function", function: {name, description, parameters}}` |
| Extension siblings | `cache_control`, `strict` (siblings of `type` and `function`, never inside `function`) |
| Count | At most 256, else `400 invalid_request` |
| `parameters` complexity | Nesting depth at most 32 and at most 10000 JSON tokens, else `400 invalid_request` |
| `parameters` content | Not validated as JSON Schema; forwarded as given |
| Response `tool_calls[]` | OpenAI nesting `{id, type: "function", function: {name, arguments}}`; `arguments` is always a JSON-encoded string |

## tool_choice

Parsed by `ToolChoice.UnmarshalJSON` in `gateway/internal/adapter/tool_choice_wire.go`. OpenAI string and object forms are accepted since `gateway/v0.18.0`.

### Accepted forms

| Form | Example | Canonical result |
|---|---|---|
| OpenAI bare string | `"auto"`, `"required"`, `"none"` | `{"mode": "<string>"}` |
| OpenAI function object | `{"type":"function","function":{"name":"get_weather"}}` | `{"mode":"tool","tool_name":"get_weather"}` |
| Kelvran canonical object | `{"mode":"tool","tool_name":"get_weather","disable_parallel_tool_use":true}` | As given |

`mode` is one of `auto`, `required`, `none`, `tool`. `tool_name` is read only when `mode` is `tool`.

### Rejected forms

Each is `400`, `type: invalid_request_error`, `code: invalid_tool_choice`, `param: tool_choice`.

| Input | Reason in the message |
|---|---|
| Any other string | unknown string |
| `{"type":"allowed_tools",…}`, `{"type":"custom",…}` | type not supported by this gateway yet |
| Any other `type` value, including Anthropic's `{"type":"auto"|"any"|"tool"}` | unsupported `type` |
| `{"type":"function"}` without `function.name` | requires a non-empty `function.name` |
| Both `mode` and `type` set | send one shape |
| `{"mode":"tool"}` without `tool_name` | requires a non-empty `tool_name` |
| Unknown `mode` | unknown mode |
| `mode: tool` naming a tool absent from `tools[]` | `tools[]` does not define it (`ValidateToolChoice`) |

Anthropic's object dialect is deliberately not accepted on this route. Re-encoding a `tool_choice` always produces the canonical object; `MarshalJSON` is not overridden, so the idempotency fingerprint is stable across the three accepted input forms of the same choice.

`"auto"` and `"required"` without `tools` are passed to `openai`, `openaicompat`, `anthropic` and `gemini`, which apply their own rule; on `bedrock` any `tool_choice` without `tools` is rejected by the adapter before any upstream call (`bedrock: tool_choice is set but no tools were provided`). `"none"` is passed to `openai`, `openaicompat`, `anthropic` and `gemini`; on `bedrock` it is rejected by the adapter before any upstream call, with or without `tools` (Converse has no "forbid tool use" member; `gateway/internal/adapter/bedrock/bedrock.go`, `toolChoiceToProvider`). Both Bedrock rejections reach the client as `502`, `code: upstream_error`, message `upstream call failed for model "<model>"`; omit `tools` instead of sending `"none"`.

### Forced tool choice by provider and model

| Provider | Rule | Failure |
|---|---|---|
| `bedrock` | `mode: tool` only for model ids containing `claude-3` or `nova` (AWS's Converse `SpecificToolChoice` restriction) | Adapter error before any upstream call; `502`, `code: upstream_error`, message `upstream call failed for model "<model>"` (the reason appears only in the gateway's `chat_completion` log line) |
| `bedrock`, Anthropic-family models | Additionally, model ids containing `claude-fable-5-1`, `claude-mythos-5-1`, `claude-opus-5-5` or `claude-sonnet-5-5` (the last since `gateway/v0.20.0`) reject `mode: tool` and, since `gateway/v0.20.0`, `mode: required` (Converse's `any`; before, forwarded and the model's `400` relayed) | Same `502`, same message |
| `bedrock` | `mode: none` has no Converse equivalent | Adapter error before any upstream call; `502`, `code: upstream_error`, message `upstream call failed for model "<model>"` |
| `anthropic` | Model ids containing `claude-fable-5-1`, `claude-mythos-5-1`, `claude-opus-5-5` or `claude-sonnet-5-5` (the last since `gateway/v0.20.0`) reject forced modes (`required` and `tool`, Anthropic's `any`/`tool`) | Same `502`, same message |
| `openai`, `openaicompat`, `gemini` | No model restriction in the gateway | Provider's own answer |

Lists are in `gateway/internal/adapter/capabilities.go`. Design: [docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md](../rfcs/2026-09-14-gateway-tool-choice-normalization.md).

## response_format

### Shape

| Field | Type | Rule |
|---|---|---|
| `type` | string | `json_schema` is the canonical, enforced type. Other strings are accepted on the wire and handled per provider below |
| `json_schema.name` | string | Not checked by the gateway. Forwarded only to `openai` and `openaicompat`, whose wire shapes require it. Dropped on `anthropic`, `bedrock` and `gemini`, whose native formats carry no name field |
| `json_schema.strict` | boolean | Forwarded to `openai` and `openaicompat`; a no-op on `anthropic`, `bedrock` and `gemini`, whose native enforcement has no strict toggle |
| `json_schema.schema` | object | Raw JSON Schema. Nesting depth at most 32 and at most 10000 JSON tokens, else `400 invalid_request`. Not validated as JSON Schema by the gateway; `bedrock` additionally rejects six keywords and injects `additionalProperties: false` (see [Support by provider](#support-by-provider)) |

Not available today: a client-side validation fallback. A provider with no native enforcement simply gets no support (`adapter.SupportsStructuredOutput`).

### Support by provider

| Provider | `type: json_schema` | `type: json_object` |
|---|---|---|
| `openai` | Forwarded as `response_format` with `name`, `strict`, `schema` | `type` string forwarded natively |
| `openaicompat` | Same wire shape as `openai`; enforcement quality is the backend's (vLLM, Ollama, TGI, llama.cpp, LocalAI) | `type` string forwarded natively |
| `anthropic` | `output_config.format.{type, schema}`; `name` dropped | `type` copied verbatim into `output_config.format.type`. Anthropic's shape documents only `json_schema`; the upstream answer for `json_object` is not documented here |
| `gemini` | `generationConfig.responseSchema` from `json_schema.schema`, `responseMimeType: application/json` | `responseMimeType: application/json` with no `responseSchema` |
| `bedrock` | `additionalModelRequestFields.output_config.format.{type, schema}`, only for whitelisted Claude families (below). The gateway rejects a schema using `$ref`, `minimum`, `maximum`, `multipleOf`, `minLength` or `maxLength` at the top level or inside any nested `properties`, `items` or `$defs` schema (`502`, `code: upstream_error`, message `upstream call failed for model "<model>"`) and sets `additionalProperties: false` on every `type: object` node that leaves it unset | Whitelisted Claude families (below) only: accepted and forwarded with no format field, not enforced. Any other `bedrock` model: the same enforcement gate as `json_schema` (`400` `response_format_unsupported`; `502` before gateway/v0.19.0); the gate (`checkResponseFormatEnforceable`) does not look at `type` |

Bedrock whitelist (family-boundary match in `gateway/internal/adapter/capabilities.go`): `claude-sonnet-5` (since `gateway/v0.18.0`), `claude-opus-4-6`, `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-opus-4-5`, `claude-haiku-4-5`. Matching is by family boundary, so `claude-sonnet-5` does not admit `claude-sonnet-5-5`; the code comment records Bedrock rejecting `output_config.format` for Sonnet 5.5 on 2026-10-08. All other `bedrock` models are unsupported.

### Enforcement gate

| Condition | Result |
|---|---|
| `response_format` set and no deployment in the model's pool can enforce it | `400`, `code: response_format_unsupported`, `param` `response_format` (since gateway/v0.19.0; `502` `upstream_error` before), message `adapter: response_format is not supported by this model and no capable deployment was found: model <canonical model>` (`adapter.ErrStructuredOutputUnsupported`, wrapped by `checkResponseFormatEnforceable` in `gateway/internal/gateway/dataplane/dataplane.go`); no `Retry-After` (the `502` before carried one) |
| `response_format` set and the serving deployment could not enforce it | Span attribute `kelvran.response_format.requested_not_enforced=true` (`gateway/internal/telemetry/result.go`); see [metrics and logs](metrics-and-logs.md) (the attribute has no row in `docs/operations/TELEMETRY.md` as of 2026-10-08) |
| Fallback hop to a deployment that cannot enforce it | Skipped by the fallback chain's capability gate |

How-to: [structured output](../how-to/structured-output.md). Design: [docs/rfcs/2026-09-12-gateway-structured-output-normalization.md](../rfcs/2026-09-12-gateway-structured-output-normalization.md).

## Streaming

Set `stream: true`. Frames are `data: <json>\n\n`; the stream ends with `data: [DONE]` on success.

### Chunk shape

| Field | Value |
|---|---|
| `id` | One id for the whole stream, stamped once (provider id when sent; otherwise `chatcmpl-` + 32 lowercase hex). Since `gateway/v0.18.0` |
| `object` | `chat.completion.chunk`. Same release note |
| `created` | Unix seconds, identical on every chunk of the stream. Same release note |
| `model` | Upstream model name when the provider's chunks carry one (`openai`, `openaicompat`, `anthropic`); the canonical model when they do not (`gemini`, `bedrock`) |
| `choices[].index` | Candidate index |
| `choices[].delta.role` | First chunk of a message only |
| `choices[].delta.content` | Text fragment |
| `choices[].delta.tool_calls[]` | `{index, id, type, name, arguments_json, function: {name, arguments}}` since gateway/v0.20.0: OpenAI's members beside the deprecated flat `name`/`arguments_json` ([`DEPRECATED.md`](../../DEPRECATED.md)); `index` is the call's position, `0` first. Before: flat only; see below |
| `choices[].delta.reasoning_blocks[]` | Extension: `{index, text, signature, redacted, data}`, keyed by `index` like `tool_calls` |
| `choices[].delta.refusal` | Extension sibling, `openai`/`openaicompat` only |
| `choices[].finish_reason` | `null` until the final delta |
| `choices[].stop_reason`, `choices[].stop_sequence` | Extension, on the chunk that sets `finish_reason`, `anthropic`/`bedrock` only; `stop_sequence` Anthropic only when a stop sequence matched. Since gateway/v0.19.0 |
| `usage` | Present only on the chunk (typically the last) where the provider supplied usage; otherwise absent on every chunk |

### Divergences from OpenAI streams

| Topic | OpenAI | Kelvran |
|---|---|---|
| Tool-call delta | `delta.tool_calls[].{index, id, type, function: {name, arguments}}` | The same members since gateway/v0.20.0, with `index` the call's position (`0` first), plus the deprecated flat `name` and `arguments_json` beside them until their removal ([`DEPRECATED.md`](../../DEPRECATED.md)); the official SDKs' stream accumulators reassemble the call (pinned by the compatibility matrix). Before gateway/v0.20.0: flat only, no `type`, no `function`, and on `anthropic`/`bedrock` deployments `index` was the provider's content-block index (`1` after a thinking block), so an accumulator that read `function.arguments` found nothing and one that trusted `index` padded a phantom call |
| `stream_options.include_usage` | Client opt-in | Client value ignored. The gateway always sends `stream_options.include_usage: true` to `openai` and `openaicompat` upstreams |
| `model` on chunks vs buffered response | Same | For `openai`, `openaicompat` and `anthropic`, chunks keep the upstream model name while the buffered response reports the canonical name, so the two can differ (recorded follow-up) |
| Mid-stream failure | `data: {"error":…}` frame | Same convention: one `data: {"error":{"message","type","param":null,"code"}}` frame with the buffered envelope's type and code (`param` is always `null` in a stream frame), then the stream ends without `[DONE]`. Since `gateway/v0.18.0` |
| `X-Kelvran-Overhead-Duration-Ms` | n/a | Not set on streams |

Pre-stream failures (auth, rate limit, routing) return a normal status and JSON envelope before any frame. Buffered tool calls (non-streaming `message.tool_calls[]`) use OpenAI's nesting; only the streaming delta diverges. How-to: [streaming](../how-to/streaming.md). Design: [docs/rfcs/2026-09-02-streaming-support.md](../rfcs/2026-09-02-streaming-support.md).

## Buffered response envelope

| Field | Value |
|---|---|
| `id` | Provider id when the provider sent one (OpenAI, Anthropic, Gemini); otherwise gateway-issued `chatcmpl-` + 32 lowercase hex (Bedrock). A cache or `Idempotency-Key` replay returns the stored id. Since `gateway/v0.18.0` |
| `object` | `chat.completion`. Same release note |
| `created` | Unix seconds at completion time; a replay returns the stored value. Same release note |
| `model` | Canonical model name |
| `choices[].index`, `choices[].message`, `choices[].finish_reason` | OpenAI shape; `message` fields as in [Messages](#messages) |
| `usage.prompt_tokens`, `usage.completion_tokens`, `usage.total_tokens` | OpenAI shape. `prompt_tokens` already includes the cache subsets below |
| `usage.cache_read_tokens` | Extension, omitted when zero: prompt tokens served from the provider's prompt cache |
| `usage.cache_creation_tokens` | Extension, omitted when zero: prompt tokens spent writing a cache entry |
| `usage.reasoning_tokens` | Extension, omitted when zero: completion tokens spent on reasoning; already inside `completion_tokens` |
| `input_transformations[]` | Extension, Anthropic only: `{type, path, reason}` entries for thinking blocks dropped or let through by the prefix-integrity check, or dropped as another account's (`organization_binding_mismatch`, Sonnet 5.5 / Haiku 5.5; since `gateway/v0.20.0`). Absent otherwise |
| `stop_reason`, `stop_sequence` | Extension, `anthropic` and `bedrock` only: the provider's native stop reason and, for Anthropic, the matched stop sequence when `stop_reason` is `stop_sequence`; `finish_reason` unchanged. Absent otherwise. Since gateway/v0.19.0 |

OpenAI's `usage.prompt_tokens_details` and `usage.completion_tokens_details` objects are not emitted; the flat extension fields above carry the equivalent counts.

## Embeddings

`POST /v1/embeddings` decodes into `adapter.EmbeddingRequest`.

| Request field | Type | Rule |
|---|---|---|
| `model` | string | Required; `400`, `code: missing_required_parameter`, `param: model` when absent. Must resolve to a `kind: embedding` deployment, else `400`, `code: not_an_embedding_model` |
| `input` | string or array of strings | Required and non-empty; `400`, `code: missing_required_parameter`, `param: input` when absent. Any other JSON type is `400 invalid_json`. An array with more than one entry is accepted by `openai` only; on `bedrock` (Titan) it is `502`, `code: upstream_error`, with a message containing `bedrock: InvokeModel embeddings accepts exactly one input per call, batch requests are not supported` |
| `dimensions` | integer | Optional. `0`/absent means the provider default (OpenAI: the model's native size; Bedrock Titan V2: 1024). Titan V2 accepts 256, 512 or 1024 |
| `encoding_format`, `user`, any other field | — | Dropped silently |

| Response field | Value |
|---|---|
| `model` | Canonical model name |
| `data[].index`, `data[].embedding` | One vector per input, in input order (`bedrock`: always exactly one) |
| `usage` | `prompt_tokens`, `completion_tokens` (always 0), `total_tokens` |
| `object`, `data[].object` | Not emitted. OpenAI returns `object: list` and `data[].object: embedding`; the gateway's `EmbeddingResponse` has neither field |

Providers with an embedding adapter: `openai` and `bedrock` only. A `kind: embedding` deployment on any other provider fails config load. A request routed to a provider without an adapter is `501`, `code: embeddings_not_configured`.

## GET /v1/models

First shipped in `gateway/v0.18.0`. Lists the canonical models the calling virtual key may use (one entry per `model:` name, filtered by the key's `allowed_models`), after the Bearer and source-IP checks (a request from a source IP outside the key's allowlist is `403`, `code: source_ip_not_allowed`, not a shorter list), with no upstream call. One document serves three readers.

| Field | Dialect | Value |
|---|---|---|
| `object` | OpenAI | `list` |
| `data[].id` | all | Canonical model name |
| `data[].object` | OpenAI | `model` |
| `data[].created` | OpenAI | Unix seconds when this instance loaded its catalog |
| `data[].owned_by` | OpenAI | Serving providers, comma-joined |
| `data[].type` | Anthropic | `model` |
| `data[].created_at` | Anthropic | RFC 3339 catalog load time |
| `data[].display_name` | Anthropic, Claude Code | From the `models:` config section; falls back to `id` |
| `data[].description` | Claude Code | From the `models:` config section; `""` when unset |
| `data[].kind` | Kelvran | `chat` or `embedding` |
| `first_id`, `last_id` | Anthropic | First and last `id` on the page; `null` when empty |
| `has_more` | Anthropic | Whether another page follows |

| Query parameter | Rule |
|---|---|
| `limit` | Default: every model. Clamped to 1000. Non-integer or non-positive: `400`, `param: limit` |
| `after_id` | Page after this id (id-sorted). Unknown id: `400`, `param: after_id` |
| `before_id` | Page before this id. Unknown id: `400`, `param: before_id`. Combining with `after_id`: `400`, `param: after_id` |

`created`/`created_at` differ between replicas restarted at different times. Claude Code's picker keeps only ids containing `claude` or `anthropic`, and it runs model discovery only in Anthropic-Messages mode, which Kelvran serves since `gateway/v0.19.0` (`POST /v1/messages`).

## Error envelope

Since `gateway/v0.18.0`; `gateway/v0.17.0` and earlier returned the same message text as `text/plain` with the same status codes.

```json
{"error":{"message":"...","type":"...","param":null,"code":"..."}}
```

All four keys are always present (`null` when unknown). The OpenAI SDKs choose the exception class from the HTTP status and read `message` and `code`; LangChain and LiteLLM stop retrying on `type: insufficient_quota`.

| Status | `type` | `code` values | Notes |
|---|---|---|---|
| `400` | `invalid_request_error` | `invalid_body`, `invalid_json`, `invalid_request`, `invalid_tool_choice`, `missing_required_parameter`, `model_not_found`, `content_policy_violation`, `streaming_not_supported`, `not_an_embedding_model`, `empty_messages`, `invalid_prompt_reference`, `response_format_unsupported` (since gateway/v0.19.0) | `model_not_found` is `400`, not OpenAI's `404` (recorded decision) |
| `401` | `authentication_error` | `invalid_api_key`, `key_expired`, or `null` for a missing header | |
| `403` | `permission_error` | `model_not_allowed`, `source_ip_not_allowed` | |
| `405` | `invalid_request_error` | `method_not_allowed` | `Allow` header set |
| `413` | `invalid_request_error` | `request_too_large` | |
| `422` | `invalid_request_error` | `idempotency_key_reused` | `Idempotency-Key` reused with a different body (`param` `Idempotency-Key`); since gateway/v0.19.0 |
| `429` | `rate_limit_error` | `rate_limit_exceeded`, `concurrency_limit_exceeded` | `Retry-After` set on `/v1/chat/completions` and `/v1/messages` (not on an `anthropic` deployment's relayed `400`/`422`) |
| `429` | `insufficient_quota` | `insufficient_quota` | Budget exhausted; no `Retry-After` |
| `501` | `server_error` | `streaming_not_configured`, `embeddings_not_configured` | |
| `502` | `server_error` | `upstream_error` | Default for every unclassified failure, including adapter rejections; `Retry-After` set on `/v1/chat/completions` and `/v1/messages` (not on an `anthropic` deployment's relayed `400`/`422`). Upstream bodies are redacted to `upstream provider returned status N` (on `/v1/messages` an `anthropic` deployment's own `400`/`422` is relayed as sent) |
| `503` | `server_error` | `deployment_capacity_exceeded` | `Retry-After` set on `/v1/chat/completions` and `/v1/messages` |

The full catalogue is in [error codes](error-codes.md).

## Idempotency-Key

| Item | Kelvran | OpenAI |
|---|---|---|
| Header | `Idempotency-Key` on `/v1/chat/completions`, buffered and streaming | Same header |
| Scope | Per virtual key (the key's server-assigned id plus the header value) | Per API key |
| Window | 10 minutes from the claim (`idempotencyKeyTTL`) | Not documented here |
| Fingerprint | SHA-256 of the canonical JSON of the decoded request (`json.Marshal(req)`), so the three accepted `tool_choice` input forms of one choice fingerprint identically; since gateway/v0.19.0 the fields the OpenAI wire cannot carry (the thinking configuration, `top_k`, `effort` — `/v1/messages` fields, never on this route) are folded in when set, and a request without any of them, and without `stop` or `top_p`, fingerprints exactly as before; `stop` and `top_p` are in the marshaled body like every other wire field (the previous build dropped them, so a body carrying either fingerprints differently than it did there; `service_tier` likewise since `gateway/v0.20.0`) | Not documented here |
| Replay | The stored canonical response is returned, including its `id` and `created`, with no upstream call (on `/v1/messages` it is re-encoded into the Anthropic shape, never the relayed bytes) | Same |
| Same key, different body | `422`, `type: invalid_request_error`, `code: idempotency_key_reused`, `param: Idempotency-Key`, no `Retry-After` (since gateway/v0.19.0; `502` plus `Retry-After` before) | `400` |
| Same key while the first request is in flight | The second waits for the first to finish, then replays | Not documented here |
| Storage | In-process only; a restart or a crash lets a retry re-execute | n/a |

Operational detail is row I2 of [docs/operations/FAILURE-MODES.md](../operations/FAILURE-MODES.md).

## Providers

| Provider name | Upstream | Chat | Embeddings | Compatibility notes |
|---|---|---|---|---|
| `openai` | OpenAI Chat Completions | Yes | Yes | Native field mapping; `stream_options.include_usage` forced on; `prompt_cache_key` from `cache_control.key`; tool-result `parts`: text only (image/document → rerouted or `400 tool_result_parts_unsupported`) |
| `anthropic` | Anthropic Messages | Yes | No | `output_config.format` for `response_format`; `disable_parallel_tool_use` honoured here only; forced tool choice rejected for three model families (above); tool-result `parts` (text, image, document) as a `tool_result` block array |
| `gemini` | Generative Language API | Yes | No | `responseSchema` only from `json_schema`; `tools[].strict` ignored; tool-result `parts` not carried (rerouted or `400 tool_result_parts_unsupported`) |
| `bedrock` | Converse / ConverseStream, Titan embeddings | Yes | Yes | Whitelisted Claude families for `response_format`; `claude-3`/`nova` only for forced tool choice; `tool_choice: "none"` rejected; single-input embeddings only; URL image parts rejected; gateway-issued `id`; tool-result `parts` (text, image, document) as `toolResult.content` blocks |
| `openaicompat` | vLLM, Ollama, TGI, llama.cpp, LocalAI | Yes | No | Wire shape forwarded verbatim; schema and tool-choice enforcement are the backend's responsibility |

The adapter registry is `newAdapterRegistry` in `gateway/cmd/gateway/main.go`. Operator-facing provider setup is in [docs/operations/PROVIDERS.md](../operations/PROVIDERS.md).

## Client SDK status

| Client | Works today | Limits | Evidence in the repository |
|---|---|---|---|
| `openai-python`, `openai-node` (`base_url` override) | Text chat (`content` as a string), tools with any accepted `tool_choice` form, `json_schema` and `json_object` output, buffered and streaming text, embeddings, `GET /v1/models` (since `gateway/v0.18.0`) | Content-array multimodal messages fail with `400 invalid_json`; streaming tool-call accumulators do not find `function.arguments`; `Idempotency-Key` body mismatch is `422` `idempotency_key_reused` since gateway/v0.19.0 (`502` before), not `400`; `max_completion_tokens` and the other dropped fields have no effect (`service_tier` is forwarded and priced since `gateway/v0.20.0`) | `gateway/compat/` (its own Go module; CI job `compat`, since `gateway/v0.19.0`) drives the official `openai-go` and `openai` (Python) packages against a gateway built from the tree with a replaying mock upstream: buffered and streamed chat, a tool loop (Go leg), `models.list`, the envelope's `code` (`model_not_found`) and a wrong key's 401. No `openai-node` leg: the nightly TypeScript leg (`.github/workflows/compat-ts-nightly.yml`, since 2026-10-11) covers `@anthropic-ai/sdk` and Claude Code, not this package. Before that, OpenAI-shaped clients were exercised live during the 2026-10-07/08 verification that found defects F4 (`tool_choice` forms) and F7 (empty Bedrock `id`), and an unmodified OpenAI-compatible client (the Deep-Research planner, `response_format: json_object`) was routed through the gateway; see `docs/upgrade-research/kelvran-deep-research-round4-discoverability-2026-10-08.md` |
| LangChain, LiteLLM and other OpenAI-compatible frameworks | Same as the OpenAI SDKs | Same | `type: insufficient_quota` is emitted so their retry logic treats a budget rejection as permanent |
| `anthropic` SDK | Yes, since `gateway/v0.19.0` | `POST /v1/messages` buffered and streaming (item 11 slice S10a) — an `anthropic` deployment receives the request body as sent and answers with Anthropic's own response bytes, buffered and streamed, and its 400/422 verbatim (slices S11a/S11b/S11b2), every other provider through a translate hop; `count_tokens` is the deployment's own count on an `anthropic` deployment and `404` on every other (the SDK raises `NotFoundError`). `GET /v1/models` answers in the SDK's list shape; all three routes read its `x-api-key`. How-to: [anthropic-python.md](../how-to/clients/anthropic-python.md) | `gateway/compat/` (CI job `compat`, since `gateway/v0.19.0`) drives the official `anthropic-sdk-go` and `anthropic` (Python) packages against a gateway built from the tree: buffered and streamed messages relayed from an `anthropic` deployment (Anthropic's own id, model and usage; the streamed wire byte for byte), a tool loop replaying thinking blocks with their signatures byte for byte, `count_tokens`, `models.list`, Anthropic's own 400 relayed verbatim with its headers, Kelvran's envelope with a `code`, `x-api-key` and bearer, a cache hit's re-encoded shape, and RFC-1's Bedrock thinking-400 case (redacted, never Bedrock's wording); an anthropic 500, 429, 401 or non-envelope 400 stays redacted. The Go leg covers every case; the Python leg buffered, streamed, `count_tokens`, `models.list`, the envelope's `code`, bearer and a wrong key. Nightly and non-blocking (`.github/workflows/compat-ts-nightly.yml`, since 2026-10-11): the official `@anthropic-ai/sdk` on the same seven cases, and Claude Code itself — `claude -p` for two headless turns (the pre-release betas off, then its defaults) with `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY` against the built gateway, its `anthropic-beta` values reaching the deployment verbatim. The evals LLM judge (`evals/evals/judge/providers.py`) still calls the provider directly |
| Claude Code | Yes, since `gateway/v0.19.0` | `POST /v1/messages` (buffered and streaming; an `anthropic` deployment receives the request as sent and answers with Anthropic's own bytes, buffered and streamed, every other provider through a translate hop) plus `GET /v1/models` discovery; `count_tokens` is the deployment's own count on an `anthropic` deployment, so `/context` is exact there, and `404` on every other, where `/context` shows an estimate; a body carrying members the schema cannot hold needs `accept_lossy_anthropic_ingress` on the deployment or `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` on the client | Same. How-to: [claude-code.md](../how-to/clients/claude-code.md) |
| First-party Kelvran SDK | None | The OpenAI SDK `base_url` drop-in is the integration path | Recorded decision; see [why there is no SDK](../explanation/why-no-sdk.md) |

Per-client how-tos: [OpenAI Python](../how-to/clients/openai-python.md), [OpenAI Node](../how-to/clients/openai-node.md), [curl](../how-to/clients/curl.md), [Anthropic Python](../how-to/clients/anthropic-python.md), [Claude Code](../how-to/clients/claude-code.md).

## Not available today

- OpenAI `content` arrays on inbound messages (`[{type: text | image_url …}]`); multimodal content uses `parts`.
- OpenAI request fields `n`, `seed`, `user`, `logprobs`, `top_logprobs`, `frequency_penalty`, `presence_penalty`, `logit_bias`, `max_completion_tokens`, `parallel_tool_calls`, client-side `stream_options`, `store`, `metadata`, `reasoning_effort`, `modalities`, `audio`, `prediction`, `web_search_options`; all dropped silently.
- `tool_choice` types `allowed_tools` and `custom`; explicitly rejected with `400 invalid_tool_choice`.
- Exact token counts on a translate hop: an `anthropic` deployment receives the `POST /v1/messages` request as sent and answers with Anthropic's own bytes, buffered and streamed, its 400/422 verbatim and its own `count_tokens` (since `gateway/v0.19.0`); every other deployment is a translate hop, where `POST /v1/messages/count_tokens` answers `404` `count_tokens_unavailable` ([RFC-1](../rfcs/2026-10-09-gateway-anthropic-messages-ingress.md)).
- OpenAI Responses API, Completions API, images, audio and files routes.
- OpenAI-shaped streaming tool-call deltas (`delta.tool_calls[].function.arguments`).
- `object` fields on the embeddings response.
- A first-party SDK (recorded decision).
- A client-side JSON Schema validation fallback for `response_format`.
- A CI compatibility leg for `openai-node` or a framework (LangChain, LlamaIndex): the `compat` merge gate covers `anthropic-sdk-go`, `openai-go`, `anthropic` and `openai`; the nightly, non-blocking TypeScript leg (`.github/workflows/compat-ts-nightly.yml`, since 2026-10-11 under owner decision G15 and the `docs/decisions/0003-go-python-split.md` note) covers `@anthropic-ai/sdk` and Claude Code itself (`claude -p`), not `openai-node`.
- MCP or A2A brokering; see [MCP and A2A status](../explanation/mcp-a2a-status.md).

## Related pages

- [Data-plane API reference](data-plane-api.md), the full request and response field reference.
- [Error codes](error-codes.md), every `type`/`code` pair with its status.
- [Configuration reference](config.md), including the `models:` section that feeds `display_name` and `description`.
- [Glossary](glossary.md).
- [Caching how-to](../how-to/caching.md), for what the cache key includes.
- [docs/VERSIONING.md](../VERSIONING.md), the compatibility promise between releases.
- [README.md](../../README.md), status and known limitations.
