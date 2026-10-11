# OpenAPI document

The gateway's HTTP API surface is described by one OpenAPI 3.1 document, [`gateway/openapi/kelvran-gateway.openapi.yaml`](../../gateway/openapi/kelvran-gateway.openapi.yaml) (plan item 14, shipped on `main` 2026-10-11; first release `gateway/v0.20.0`). It covers both listeners: the eight data-plane routes (`POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models`, `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /healthz`, `GET /readyz`, `HEAD /api/hello`) and the 33 admin method-and-path patterns over 28 admin paths (the two spend-ledger routes of item 13d joined on 2026-10-11), as 36 path items and 41 operations, with 60 component schemas, 7 response headers, 4 request-header parameters and 6 security schemes. This page says what the document is for, what it guarantees, and what it cannot carry. The prose references remain [Data-plane API](data-plane-api.md), [Admin API](admin-api.md), [Error codes](error-codes.md) and [Configuration](config.md).

## What it is pinned to

The document is not generated at build time and not hand-maintained on trust: the Go test suite keeps it equal to the code in both directions, and a change to either side without the other goes red in CI.

| Claim in the document | Pinned by |
|---|---|
| Paths and methods are exactly the registered routes | `TestOpenAPIRouteSetEqualsRegisteredRoutes` against the data-plane route table (`gateway/cmd/gateway/mux.go`) and the admin route table (`gateway/internal/admin/routes.go`) |
| Every operation has an `operationId` and the right `security` (virtual key, its `x-api-key` alias on the three routes that read it, the four admin tiers; the probes `security: []`) | `TestOpenAPIEveryOperationHasIDAndSecurity` |
| Tags, the admin `servers` entry, `x-kelvran-conditional` and `x-kelvran-subtree` markers | `TestOpenAPIListenerTagsServersConditionals` |
| Every status and every `code` a route can answer, and the `type` and `code` vocabularies | `TestOpenAPIStatusesAndCodes` against the error writers' table and the handler-level codes; admin statuses by `TestOpenAPIAdminStatuses` |
| Where `Retry-After` is present (`x-kelvran-retry-after` lists the codes per response) | `TestOpenAPIRetryAfter` |
| The wrong-method answer of every data-plane route (`x-kelvran-method-not-allowed`: status, `Allow`, content type) | `TestOpenAPI405ClaimsMatchTheMux`, executed through the real mux |
| Every schema's properties are the Go wire type's JSON members; `required` follows the direction (a response requires every always-emitted member, a request only what the handler demands on every path) | `TestOpenAPISchemasMirrorTheGoWireTypes` by reflection, with marshalled samples for the custom-marshal types (the response direction mechanically; the request direction for the two Anthropic schemas by `TestOpenAPIPermissiveMessages`, for `ChatCompletionRequest` and the other request-side schemas by review — the test only checks that each `required` name is a Go member) |
| Request-side schemas are open (`additionalProperties: true`) because no decoder rejects unknown members | `TestOpenAPIRequestSchemasAreOpen` and `TestDecodersNeverRejectUnknownMembers` (an AST walk over every non-test file) |
| Schema Objects use only known keywords (no `nullable`, no singular `example`, no typos the 3.1 meta-schema would let through) | `TestOpenAPISchemaObjectsUseOnlyKnownKeywords` |
| Every example names its source in `x-kelvran-example-source`: a committed fixture (`path#/pointer`, compared byte for byte) or the literal `hand-authored` for the six the fixtures cannot supply (the stamped chat completion, the two probes' `200`s, a missing-header `401`, a lossy `400`, a `count_tokens` answer) | `TestOpenAPIExamplesMatchTheirSources` |
| The `x-kelvran-*` vocabulary is closed (the registry at the root) | `TestOpenAPIExtensionsAreClosed` |
| `POST /v1/messages` and `POST /v1/messages/count_tokens` are permissive by construction (RFC-1 Option C) | `TestOpenAPIPermissiveMessages` |
| The document carries no request-id header (the gateway adds none) | `TestOpenAPINoRequestIDHeader` |
| The ServeMux behaviours the vocabulary cannot carry (`x-kelvran-servemux`) | `TestOpenAPIServeMuxBlock` and `TestAdminServeMuxSemantics` |
| The Markdown route tables of the two reference pages name the same routes | `TestReferenceDocsRouteTablesMatchTheCode` |
| The document passes the OpenAPI 3.1 meta-schema; the committed fixtures and the live data-plane answers conform to their schemas | the conformance tests in `gateway/compat` (libopenapi; a separate Go module, so the validator never enters the gateway's dependency graph) |

## Reading it

- **Two HTTP API listeners.** The root `servers` entry is the data plane (`listen_addr`). Every admin path item carries a `servers` entry for the admin listener (OpenAPI applies it to each operation under it) (`admin.listen_addr`, default `127.0.0.1:8081`), which exists only when `admin.token_env` is set. Admin operations marked `x-kelvran-conditional` exist only with the named configuration key (`admin.audit_log_path`, `admin.enable_pprof`).
- **Error bodies.** Three shapes: the OpenAI envelope (`OpenAIError`) on `/v1/chat/completions`, `/v1/embeddings` and `/v1/models`; the Anthropic envelope (`KelvranAnthropicError`) on `/v1/messages` and `/v1/messages/count_tokens`, where an `anthropic` deployment's own `400`/`422` is relayed verbatim (`anyOf` with `AnthropicUpstreamError`); plain text on `/healthz` (`405`) and `/readyz` (`405`, `500`; its `503` is the readiness document itself, with `ready: false`) and on every admin error; `HEAD /api/hello` answers a wrong method in the OpenAI envelope. `x-kelvran-codes` on each response lists the `code` values it can carry; [Error codes](error-codes.md) explains each.
- **Permissive by construction.** The two Anthropic request schemas type the known members loosely and are open: members the canonical schema has no home for reach an `anthropic` deployment as sent (only `model` and `stream` are rewritten on the top level) and are refused with `400` `lossy_ingress_rejected` on a translate hop unless the deployment sets `accept_lossy_anthropic_ingress: true`.
- **`required` means demanded on every path.** `ChatCompletionRequest` requires `model` alone: `messages` or `prompt_id` supplies the conversation, never both. An Anthropic `messages[]` item requires `content` alone. `AnthropicCountTokensRequest` requires `model` alone; `messages` is demanded only where the body is parsed, on an `anthropic` deployment. A validator built from the document therefore never rejects a body the gateway accepts; the either/or rules are in the descriptions.
- **Streams.** `text/event-stream` responses declare a string body and list their events in `x-kelvran-sse-events`; the per-event payloads are the named components `ChatCompletionChunk`, `OpenAIStreamErrorFrame`, `AnthropicStreamEvent` and `AnthropicStreamErrorEvent`. OpenAPI 3.1 has no per-event schema (3.2's `itemSchema` would be the fit), so a generated client sees a string.
- **What the vocabulary cannot carry** is stated in `info.description` and pinned in `x-kelvran-servemux`: data-plane patterns carry no method (a wrong method is the route's own `405`, in the route's own envelope); admin patterns carry a method (a wrong method is net/http's plain-text `405` before any credential check, every admin `GET` also answers `HEAD`, `/admin/debug/pprof/` is a subtree and `/admin/debug/pprof` without the slash is a `307` under the pinned Go toolchain); an insufficient admin tier is `401`, never `403`; body caps (32 MiB on the data plane, none on the admin listener; `anthropic-*` request headers bounded at 16 KiB; a relayed `count_tokens` answer at 64 KiB).

## The `x-kelvran-*` extensions

The root `x-kelvran-extensions` is the closed registry; a new extension must be added there and to the allowlist in `TestOpenAPIExtensionsAreClosed` in the same commit.

| Extension | Where | Meaning |
|---|---|---|
| `x-kelvran-servemux` | root | The ServeMux facts above, as data |
| `x-kelvran-extensions` | root | This registry |
| `x-kelvran-conditional` | admin operations | The configuration key without which the route is not registered |
| `x-kelvran-subtree` | `GET /admin/debug/pprof/` | The pattern is a subtree |
| `x-kelvran-method-not-allowed` | data-plane responses | The executed wrong-method answer: status, `allow`, `content_type`, body shape |
| `x-kelvran-codes` | error responses | The `code` values the response can carry |
| `x-kelvran-retry-after` | responses | The codes on that response whose answer carries `Retry-After` (a list, possibly empty) |
| `x-kelvran-passthrough` | the Anthropic request and response schemas | Permissive by construction: requests reach an `anthropic` deployment as sent, responses are relayed as received |
| `x-kelvran-example-source` | examples | The committed fixture the example is copied from (`path#/pointer`; a `[]` suffix wraps one golden object into a list response's one-element array), or the literal `hand-authored` |
| `x-kelvran-max-body-bytes` | the four data-plane `POST` operations | The byte cap the handler enforces |
| `x-kelvran-sse-events` | `text/event-stream` content | The events a stream carries, in order |
| `x-kelvran-relayed-header-prefix` | the `x-should-retry` response header | The response-header family (`anthropic-ratelimit-unified-*`) relayed verbatim from an `anthropic` deployment alongside it on the two Anthropic routes |
| `x-kelvran-when-unconfigured` | `POST /admin/backup` | The status (`501`) and the configuration key (`admin.backup_dir`) behind it |
| `x-kelvran-schema` | the `AdminConfig` schema (`GET /admin/config`'s `200`) | `untyped-go-struct`: the operator config struct marshalled without JSON tags; only the top-level members are named, nested shapes are not pinned, `config.md` is the key reference |

## Using it

- **Validate or render it** with any OpenAPI 3.1 tool, for example `npx @redocly/cli lint gateway/openapi/kelvran-gateway.openapi.yaml`, or a static page with `npx @redocly/cli build-docs gateway/openapi/kelvran-gateway.openapi.yaml -o kelvran-openapi.html` (the page loads the Redoc bundle from Redocly's CDN; the recommended ruleset's style warnings are not document errors). The gateway does not serve the document or a rendered page (owner gate G7 is open); download it from the repository.
- **Generate a client** from it if you must, knowing what the prose pages add and the SDK pages recommend: the OpenAI and Anthropic SDKs already speak these routes ([Compatibility](compatibility.md), [why no SDK](../explanation/why-no-sdk.md)). A generated client sees the Anthropic request bodies as open objects and the streams as strings.
- **Validate requests and responses** against it with a validator that honours `additionalProperties`, `anyOf` and `oneOf` under JSON Schema 2020-12. The `ToolChoice` `oneOf` carries `not` guards so a body naming both `mode` and `type` is refused, as the gateway refuses it.

## What is public

The public surface is the document's row in [`VERSIONING.md`](../VERSIONING.md) §2, from `gateway/v0.20.0`: paths, operations, operationIds, security requirements, response statuses, header names, the `type`/`code` vocabularies and every `x-kelvran-*` value. Description prose, example payloads and `info.version` are not. The document frozen across two consecutive minors is a `gateway/v1.0.0` precondition under §4. Release preflight (`scripts/release-preflight.sh`) fails when the document is missing and warns when `info.version` does not match the version being cut.

## Not in the document

- A per-event schema for the streams (OpenAPI 3.1 cannot express one; the payload components are named in `x-kelvran-sse-events`).
- Error-body examples on `/v1/embeddings`, `/v1/models`, `/v1/messages/count_tokens`, the probes and the admin routes: the two error examples carried (`/v1/chat/completions` `401`, `/v1/messages` `400`) are hand-authored; no fixture exists for the others and none were authored.
- An `x-kelvran-since` per route (the first shipping release of each route): the changelog lookup that would fill it returns first mentions, not first shipments, so it is deferred (plan item 14, owner question 13). The route history is in the [changelog](../../gateway/changelog/).
- The optional Prometheus pull listener (`telemetry.prometheus`, since item 13c; `GET /metrics` on `127.0.0.1:9464` by default): Prometheus exposition, not an API, with its own one-route table in `gateway/cmd/gateway/metrics_server.go`.
- A hosted or rendered copy (owner gate G7).
