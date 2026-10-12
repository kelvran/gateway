# Deprecations

The first deprecation landed with `gateway/v0.20.0` (the flat streamed tool-call delta members, below); before it no surface had been removed or superseded, only added to. Rows oldest first; what counts as a public surface, how long a deprecation lasts and how it is signalled at runtime: `docs/VERSIONING.md` § 2 and § 5.

| Deprecated Surface | Since Version | Planned Removal | Replacement |
|---|---|---|---|
| `choices[].delta.tool_calls[].name` and `choices[].delta.tool_calls[].arguments_json` on the `POST /v1/chat/completions` SSE stream — the flat tool-call delta members, emitted beside their replacement meanwhile (`gateway/openapi/kelvran-gateway.openapi.yaml` marks both `deprecated: true`) | `gateway/v0.20.0` | `gateway/v0.22.0`, and no earlier than 90 days after `gateway/v0.20.0` ships (§ 5.1: whichever is later); a `**BREAKING**` entry and an `UPGRADE.md` row at removal. No runtime signal: the gateway writes the members and cannot see a client read them (§ 5.3) | `type` and `function.{name,arguments}` on the same element — OpenAI's `ChatCompletionMessageToolCallChunk` shape, which the official SDKs' stream accumulators read ([data-plane API](docs/reference/data-plane-api.md), [streaming how-to](docs/how-to/streaming.md)) |
