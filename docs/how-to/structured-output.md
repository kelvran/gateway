# Request structured JSON output with `response_format`

This page shows how to ask the Kelvran gateway for schema-constrained JSON through the OpenAI-shaped `response_format` field on `POST /v1/chat/completions`, which providers and models can enforce it, how the gateway routes around a deployment that cannot, and which errors you get when nothing can. It is for application developers who send the request and for operators who choose the deployments behind a model name.

**Use this when** your client must parse the model's answer as JSON that matches a schema you supply, instead of prose you post-process.

## Prerequisites

- A running gateway. The default `listen_addr` in [`gateway/config.example.yaml`](../../gateway/config.example.yaml) is `:8080`; this page uses `http://localhost:8080`.
- A virtual key, sent as `Authorization: Bearer <key>`. Keep the secret in an environment variable; this page uses `KELVRAN_KEY`. See [virtual keys and budgets](virtual-keys-and-budgets.md).
- A model name that at least one `deployments.<name>.model` entry serves, on a provider that can enforce the schema (next section). The examples use `gpt-4o`; replace it with your own. Config keys are in the [config reference](../reference/config.md).

## Which deployments can enforce a schema

The gateway decides enforceability per deployment from `provider` and `upstream_model`, never from the client-facing `model` name.

| `provider` | Enforces `response_format` | Wire shape sent upstream |
|---|---|---|
| `openai` | Always | `response_format` forwarded verbatim, including `json_schema.name` and `json_schema.strict` |
| `openaicompat` | Reported as always; real grammar enforcement depends on the backend (vLLM, Ollama, TGI, llama.cpp, LocalAI each differ) | Same as `openai` |
| `anthropic` | Always | `output_config.format` with `type` and `schema`; `name` and `strict` are dropped |
| `gemini` | Always | `generationConfig.responseMimeType: application/json` plus `responseSchema` |
| `bedrock` | Only when `upstream_model` names one of the whitelisted Claude families below | `additionalModelRequestFields.output_config.format` with `type` and `schema`; sent only for `type: json_schema` |

Bedrock whitelist, matched against `upstream_model` at a family boundary (the family must be followed by the end of the id, a `:` version separator, or `-` plus an 8-digit date or a `vN` revision; this boundary check is in place since gateway/v0.18.0; gateway/v0.17.0 and earlier match by bare substring): `claude-sonnet-5`, `claude-opus-4-6`, `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-opus-4-5`, `claude-haiku-4-5`. `claude-sonnet-5` does not admit `claude-sonnet-5-5`. `global.anthropic.claude-haiku-4-5-20251001-v1:0` matches; the example file's `anthropic.claude-3-5-sonnet-20241022-v2:0` does not. `claude-sonnet-5` is whitelisted since gateway/v0.18.0; in gateway/v0.17.0 and earlier a Sonnet 5 Bedrock deployment is treated as incapable.

The list is a Go slice in the gateway binary. There is no config key to extend it.

## Steps

### 1. Build the request

`response_format` is an optional top-level field. For `json_schema` it carries a `json_schema` object with `name`, an optional `strict` boolean and the raw `schema` document:

```json
{
  "type": "json_schema",
  "json_schema": {
    "name": "place",
    "strict": true,
    "schema": { "...": "..." }
  }
}
```

The gateway passes `schema` through as raw JSON. It does not validate it against any JSON Schema draft, and it does not validate `type`. On `openai`, `openaicompat` and `anthropic` an unknown `type` is forwarded to the provider as-is; on `bedrock` anything other than `json_schema` sends no format field at all, so the request proceeds unenforced; on `gemini` `type` is never read and any `response_format` selects JSON mode.

### 2. Write a schema every target can accept

If any deployment behind the model is on Bedrock, or might be in future, write to Bedrock's narrower dialect now:

- Do not use `$ref`, `minimum`, `maximum`, `multipleOf`, `minLength` or `maxLength`. When one of them sits at the top level or anywhere under `properties`, `items` or `$defs` (the only paths the gateway walks), the gateway rejects the request before it reaches AWS. If another eligible deployment serves the model on fallback (a mixed pool, or a `generic` fallback chain), the request falls back to it and returns 200 from that provider, with `fallbackHappened: true` in the `chat_completion` log line's `gatewayevents_v1` field. The same keyword under `anyOf`, `oneOf`, `allOf`, `definitions` or `patternProperties` is not caught by the gateway; the request is sent and AWS rejects it instead (502, `upstream provider returned status 400`).
- Set `"additionalProperties": false` on every object. If you leave it out, the gateway injects `false` into the top-level object and every object reached through `properties` or `items` that does not set it explicitly; object schemas under `anyOf`, `oneOf`, `allOf` or `$defs` are not visited, so set it there yourself. An explicit `true` is left alone for Bedrock to reject.

Two structural bounds apply on every provider: `schema` deeper than 32 nesting levels (each `{` or `[` counts) or longer than 10,000 JSON tokens is rejected, as is malformed schema JSON.

### 3. Send it

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [{"role":"user","content":"Give me a city and its country."}],
    "response_format": {
      "type": "json_schema",
      "json_schema": {
        "name": "place",
        "strict": true,
        "schema": {
          "type": "object",
          "properties": {"city": {"type":"string"}, "country": {"type":"string"}},
          "required": ["city","country"],
          "additionalProperties": false
        }
      }
    }
  }'
```

### 4. Parse the answer

The response is an ordinary chat completion. The JSON document is the string in `choices[0].message.content`; parse it with your JSON library. Enforcement is the provider's: the gateway does not check the content against the schema.

## How the gateway routes a schema-bearing request

1. The weighted-round-robin router picks a deployment for `model`.
2. If that deployment cannot enforce `response_format` (or sits outside the key's region allowlist), the gateway walks the rest of that model's deployment pool and uses the first deployment that can. Nothing is sent upstream until a capable deployment is found.
3. If no deployment in the pool is capable, the request fails before any upstream call (error table below).
4. On a failover hop, whether the legacy single hop or a `fallback_chains` target, an incapable deployment is skipped rather than served without enforcement. If the only candidate is incapable, the original upstream error propagates.

Operators: put at least one capable deployment behind every `model` name that clients will call with `response_format`. Mixing one capable and one incapable deployment under the same name works; the gateway steers schema-bearing requests to the capable one. See [routing and failover](routing-and-failover.md).

## Variants

### Streaming

`response_format` works with `"stream": true`. The same first-pick gate runs before the stream opens, so a pool with no capable deployment fails with the same error and no frame is sent. See [streaming](streaming.md).

### `type: json_object`

On `openai` and `openaicompat` the value is forwarded verbatim. On `bedrock` it is accepted and silently not enforced, even on a whitelisted model, because Converse has no equivalent; no `output_config.format` is sent. Behaviour on `anthropic` and `gemini` is not documented here.

### `strict`

`json_schema.strict` reaches `openai` and `openaicompat` only. On `anthropic` and `bedrock` it is a deliberate no-op; both enforce `json_schema` unconditionally.

### Cache

The `response_format` value is part of the L1 and L2 cache keys and is an L3 gate, so a request with a schema never receives a response cached for the same messages without one, or with a different schema. See [caching](caching.md).

## Errors

| Status | `type` / `code` | When | Message |
|---|---|---|---|
| 400 | `invalid_request_error` / `invalid_request` | `schema` deeper than 32 levels, over 10,000 tokens, or not valid JSON | Names `response_format.json_schema.schema` and the bound exceeded |
| 502 | `server_error` / `upstream_error` | No deployment in the pool can enforce `response_format`; no upstream call was made | `adapter: response_format is not supported by this model and no capable deployment was found: model <model>` |
| 502 | `server_error` / `upstream_error` | Bedrock schema-dialect rejection (`$ref`, `minimum`, ...) raised while building the upstream request, and no other eligible deployment served the model. If one does (a mixed pool, or a `generic` fallback chain), the request falls back to it and returns 200 from that provider, with `fallbackHappened: true` in the `chat_completion` log line's `gatewayevents_v1` field | `upstream call failed for model "<model>"`; the specific reason, for example `uses a numeric "minimum" constraint`, is only in the gateway's `chat_completion` log line, field `error` |

The 502 for "no capable deployment" has no dedicated code; it takes the default mapping. `type` and `code` are public surface under [docs/VERSIONING.md](../VERSIONING.md); message text is not. The JSON envelope itself ships since gateway/v0.18.0; in gateway/v0.17.0 and earlier the same statuses apply and the body is `text/plain` carrying the message. The `upstream call failed for model "<model>"` redaction in the third row also dates from gateway/v0.18.0: gateway/v0.17.0 and earlier return the same 502 with the full error text in the `text/plain` body, including `adapter "bedrock" ToProvider: bedrock: response_format.json_schema.schema uses a numeric "minimum" constraint, which Bedrock's structured-output schema dialect does not support ...`. The full vocabulary is in [error codes](../reference/error-codes.md).

## Verify it worked

Positive check. Run the step 3 command with `-s` and pipe through `jq`:

```bash
curl -s http://localhost:8080/v1/chat/completions ... | jq -r '.choices[0].message.content' | jq .
```

The first `jq` prints a JSON string; the second parses it. You get an object with exactly the keys `city` and `country`, both strings, and `jq` exits 0. A non-JSON answer makes the second `jq` fail with a parse error.

Negative check against the gateway's own gate. Send the same body with `"model": "claude-bedrock"` to a gateway running the unmodified example config, using a virtual key with no `allowed_models` restriction: in that file this is `team-beta`, whose public example secret `example-team-beta-secret-do-not-use` is in the file's header comment. `team-alpha` only allows `gpt-4o` and `claude-opus-4` and returns `403` `permission_error` / `model_not_allowed` before the capability gate runs. The config's only `claude-bedrock` deployment uses `anthropic.claude-3-5-sonnet-20241022-v2:0`:

```text
HTTP/1.1 502 Bad Gateway

{"error":{"message":"adapter: response_format is not supported by this model and no capable deployment was found: model claude-bedrock","type":"server_error","param":null,"code":"upstream_error"}}
```

No upstream call is made and the `chat_completion` log line carries the same text. Replacing `upstream_model` with a whitelisted family turns this into a 200.

Observability. A request that set `response_format` and was still served by an incapable deployment carries the span attribute `kelvran.response_format.requested_not_enforced=true`; it is never emitted otherwise. With the first pick and both fallback paths gated, this attribute is reachable only in edge cases. See [metrics and logs](../reference/metrics-and-logs.md) and [TELEMETRY](../operations/TELEMETRY.md).

## Not available today

- No gateway-side validation of the model's output against your schema. If a provider returns non-conforming JSON, the gateway passes it through.
- No validation of `response_format.type`; unknown values are forwarded as-is on `openai`, `openaicompat` and `anthropic` and fail, or not, there; `bedrock` sends no format field for them and `gemini` ignores `type` entirely.
- No dedicated error code for "no capable deployment"; it is the generic 502 `server_error` / `upstream_error`.
- No `name` or `strict` on the Anthropic wire, and no `strict` on Bedrock.
- No `json_schema.description`; the field is accepted and silently dropped, never forwarded to any provider.
- No schema-graph cycle detector on Bedrock, so even an internal-only `$ref` is rejected; `json_object` is never enforced on Bedrock.
- No per-model capability tracking for `openai`, `anthropic`, `gemini` or `openaicompat`; each is a flat per-provider yes.
- No config key to extend or override the Bedrock whitelist.

## Versions

Structured output shipped in gateway/v0.5.0 (2026-09-15). The first-pick gate that rejects a pool with no capable deployment was corrected 2026-09-21. The latest tagged release is gateway/v0.18.0 (2026-10-10); the Sonnet 5 whitelist entry and the JSON error envelope first shipped in it. See [versioning](../explanation/versioning.md) and [compatibility](../reference/compatibility.md).

## Related

- [Data-plane API reference](../reference/data-plane-api.md), [error codes](../reference/error-codes.md), [config reference](../reference/config.md), [metrics and logs](../reference/metrics-and-logs.md)
- [Routing and failover](routing-and-failover.md), [caching](caching.md), [streaming](streaming.md), [provider credentials](provider-credentials.md), [curl client](clients/curl.md)
- Design: [RFC 2026-09-12 structured-output normalization](../rfcs/2026-09-12-gateway-structured-output-normalization.md), [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md), feature table in [README.md](../../README.md)
