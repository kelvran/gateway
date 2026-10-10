# Why Kelvran ships no SDK

This page explains why Kelvran has no first-party client library, what the project does instead, and what would change that decision. It is for developers deciding how to call the gateway and for maintainers asked "where is the SDK?". It is an explanation, not a guide: the per-client how-tos are [openai-python](../how-to/clients/openai-python.md), [openai-node](../how-to/clients/openai-node.md) and [curl](../how-to/clients/curl.md); the wire contract is in the [data-plane API reference](../reference/data-plane-api.md).

## The decision

There is no client or SDK directory anywhere in the repository. The top level holds `api/`, `gateway/`, `evals/`, `deploy/`, `docs/`, `scripts/`, `terraform/`, `.github/`, `docker-compose.yml` and the root documents. That is deliberate. The recorded decision, taken on 2026-09-13 and restated unchanged in the 2026-10-08 discoverability round, is that a first-party SDK is `not_yet`, and that pointing an existing OpenAI SDK's `base_url` at the gateway is the integration path. [README.md](../../README.md) lists it under "Not built, by recorded decision". The research behind it is `docs/upgrade-research/client-sdk-strategy-2026-09-13.md`.

The same decision classified three zero-code items as `build_now`: a drop-in guide for the OpenAI SDKs, a note that Kelvran's reasoning data rides as additive JSON, and a retry/backoff recipe for raw-HTTP callers. Those three are documentation, and this docs set is where they land.

## Why the wire format makes a client redundant

Kelvran's canonical request and response schema is OpenAI Chat-Completions-shaped. [gateway/ARCHITECTURE.md](../../gateway/ARCHITECTURE.md) describes it as the dialect that vLLM, TGI, Ollama, DeepSeek, Together and Groq already speak natively. The `gateway/internal/adapter/openai/` and `gateway/internal/adapter/openaicompat/` packages implement that format on the provider side; the public routes in `gateway/cmd/gateway/main.go` expose it on the client side as `POST /v1/chat/completions`, `POST /v1/embeddings` and `GET /v1/models`.

An SDK exists to hide a wire protocol behind typed calls. When the wire protocol is one that the official OpenAI libraries already speak, the typed calls already exist and are maintained by someone else. The whole integration collapses to one constructor argument:

```python
import os
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key=os.environ["KELVRAN_KEY"])  # the raw secret, never the hash
resp = client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
```

The address matches the `listen_addr: ":8080"` default in [gateway/config.example.yaml](../../gateway/config.example.yaml). Adding `stream=True` turns the same call into Server-Sent Events (see [streaming](../how-to/streaming.md)); the SDK's model listing reads `GET /v1/models`. Nothing in that snippet is Kelvran-specific except the URL and the key.

This also explains what a Kelvran SDK would have to be: a second implementation of the OpenAI client surface, kept in step with the gateway by hand. Every change to the wire would then need a matching change in each language. The project's compatibility promise instead lives on the wire itself, which is the surface [docs/VERSIONING.md](../VERSIONING.md) covers: routes and bodies, the error envelope's `type` and `code`, headers and SSE framing.

## What peer gateways do

The research compared production practice at LiteLLM Proxy and Vercel AI Gateway. Both document "change your `base_url`" as the primary, most-promoted integration path, not a bespoke client. Where a vendor also owns an SDK, the two paths coexist: Vercel recommends its AI SDK for new projects but documents the `base_url` override with equal prominence for existing code, and LiteLLM actively steers users away from calling its proxy with its own SDK. LiteLLM's own quick start attaches an explicit warning to the tab that calls the proxy with LiteLLM's native SDK: the proxy already uses that SDK internally, so there is duplicate logic that "might lead to unexpected errors". A gateway client that re-implements logic the gateway already owns is a known failure mode, not a hypothetical one.

The official OpenAI and Anthropic SDKs are built for exactly this redirection. The Anthropic Python SDK resolves its base URL from the constructor argument, then the `ANTHROPIC_BASE_URL` environment variable, then profile configuration, then the hardcoded default. openai-python exposes the same mechanism. Any caller can redirect all of its traffic with one argument or one environment variable, and Kelvran benefits from that engineering without writing a line.

## What an SDK would cost

The cost side is quantified by a first-person case study the research relies on. Cloudflare's engineering blog reports that its hand-maintained per-language SDKs needed a minimum of four pull requests for a single API change, and that building an in-house generator was estimated at six to nine months for a single high-quality language, with recurring cost for every language after that. Cloudflare bought a generation engine rather than continuing to hand-roll.

For a project with one gateway and one known external caller, that is an investment with no demand behind it. The decision does not say an SDK is wrong. It says an SDK is premature, and names the trigger that would make it right.

## The one real argument for a client, and how it is met

The research is candid about the strongest argument on the other side. Official SDKs retry automatically on 408, 409, 429 and 5xx with exponential backoff and jitter, honour a server-supplied `Retry-After`, and map status codes to a typed exception hierarchy. A hand-rolled HTTP caller gets none of that for free.

The project chose to meet this on the server rather than in a client library, so that the official SDKs' existing retry and error machinery works against Kelvran unmodified:

- Every client-facing error on the three `/v1/*` routes (`/v1/chat/completions`, `/v1/embeddings` and `/v1/models`) is an OpenAI-shaped JSON envelope, `{"error":{"message","type","param","code"}}`. The official SDKs therefore raise their usual exception classes with `.code` and `.type` populated. A budget rejection is `type: insufficient_quota` so it does not look like a transient `rate_limit_error`. This first shipped in `gateway/v0.18.0`; `gateway/v0.17.0`, which has no `/v1/models` route, returns `text/plain` bodies with the same status codes and message text. The codes are listed in the [error-codes reference](../reference/error-codes.md).
- A `429` carries `Retry-After` for the RPM/TPM and concurrency cases and not for budget, so a backoff loop can tell the two apart. A `502` with `code: upstream_error` carries `Retry-After` too. The OpenAI SDK retries 5xx twice and then raises `openai.InternalServerError`.
- OpenAI's `tool_choice` wire forms, the bare strings `"auto"`, `"required"` and `"none"` and the function object, are accepted since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier the string form was a `400` and the object a `502`. The bare strings and the function object are what the OpenAI SDKs send whenever a caller sets `tool_choice`, so this fix is what makes an explicit `tool_choice` work from an unmodified client; a request that omits `tool_choice` worked in `gateway/v0.17.0` too.
- Every chat completion carries the OpenAI envelope fields `id`, `object` and `created`, on buffered responses and on every streaming frame, with a gateway-issued `chatcmpl-` id when the provider (Bedrock Converse) has none. Since `gateway/v0.18.0`; in `gateway/v0.17.0` and earlier Bedrock completions reached clients with an empty `id`. Tooling that keys traces or deduplicates retries on `id` depends on this.
- `GET /v1/models` returns one superset document that the OpenAI SDK, the Anthropic SDK and Claude Code's provider discovery all read, filtered to the models the calling key may use. Since `gateway/v0.18.0`.

Two of these, the `tool_choice` forms (defect F4) and the completion envelope (defect F7), came out of the 2026-10-07/08 live run described below; the JSON error envelope and `GET /v1/models` came out of the 2026-10-08 discoverability research, which compared the gateway's plain-text error bodies and its route list against the OpenAI-shaped errors peer gateways advertise and the `/v1/models` discovery that Anthropic's Claude Code gateway-protocol page requires. Each is recorded in `gateway/changelog/0.18.0.md`. The pattern is consistent: when an unmodified SDK misbehaved or a peer contract was missing, the fix went into the gateway's wire behaviour, never into a client.

## What rides outside the OpenAI shape

Two things do not fit the OpenAI message shape, and the decision handles both without a client.

Reasoning content is the first. An assistant message may carry `reasoning_blocks`, an ordered slice of opaque extended-thinking blocks defined in `gateway/internal/adapter/types.go` and specified by the [reasoning-content RFC](../rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md). It is an additive field, so an OpenAI SDK that does not know it ignores it. Callers that continue a conversation must echo the slice back unmodified on the same assistant turn, or Anthropic's and Bedrock's Claude Messages API returns a hard `400`. That is a documented obligation on the caller, not a reason for a typed wrapper; the research notes that Anthropic's own `thinking_delta`/`signature_delta` events are the nearest prior art and are likewise handled by convention.

Multimodal input is the second. OpenAI content-array messages are not accepted; `content` is a string, and images go in Kelvran's own `parts` field. `parts` is a field on the message object: openai-python forwards the message dict as given, and openai-node needs a cast because its TypeScript types reject the unknown key (see the per-client how-tos). The [compatibility reference](../reference/compatibility.md) is the place to check what each client sends and what the gateway accepts.

## The decision has been exercised

The research document's own caveat, written on 2026-09-13, was that it had not pointed a live official SDK at a running gateway. That caveat is superseded. [DECISIONS.md](../../DECISIONS.md) records, under `[2026-10-08]`, a live end-to-end run with the real, unmodified Deep-Research project as the client against real Bedrock. Deep-Research reached the gateway through a throwaway header-injecting proxy that supplied the bearer token and a default `model`; its own code did not change. The run surfaced two of the defects fixed above, the `tool_choice` wire forms and the empty Bedrock completion `id`, plus the kind-unaware health probe (defect F3, also fixed in `gateway/v0.18.0`) and one structured-output whitelist bug fixed in-session. Deep-Research is a sibling project outside this repository, so its client code is not inspectable here; the record is the DECISIONS entry.

## What would change the decision

The research names two triggers. Either real external callers in several languages ask for typed clients, or Kelvran needs an extension that the OpenAI and Anthropic wire formats genuinely cannot express. Neither has happened. The 2026-10-08 round re-asked the question across five independent lanes and left "first-party SDK `not_yet`" standing.

If a trigger does arrive, the research's open question is whether to hand-roll or to start from a schema-driven generator, given the Cloudflare experience. A generator needs a machine-readable contract, which is where the OpenAPI decision below becomes load-bearing.

## Not available today

- A first-party typed SDK in any language.
- A `.parse()`-style structured-output client wrapper. Structured output is a wire parameter, `response_format` with a JSON schema; see [structured output](../how-to/structured-output.md). The research notes that openai-python's `.parse()` is client-side sugar over that same wire shape, so an OpenAI SDK user already has it.
- A provider-agnostic reasoning-effort enum as a Kelvran-native abstraction.
- An OpenAPI 3.1 document or Swagger UI. Kelvran has no OpenAPI document anywhere. This is a separate decision from the SDK one: `not_yet` on 2026-09-13 with "a documented external API consumer beyond the pilot" as the trigger, and proposed on 2026-10-08 to flip to `build_now` because discoverability is itself the consumer. That flip is a user decision and has not been taken.
- An Anthropic Messages API serving mode. `GET /v1/models` is readable by the Anthropic SDK and Claude Code, but Claude Code's discovery runs only in Anthropic-Messages mode, which Kelvran does not yet serve. Chat is OpenAI-shaped only.

## Related

- [Design decisions](design-decisions.md) explains how decisions are recorded in layers (ADRs, DECISIONS.md, RFCs and research reports) and lists the researched-and-declined items; it also records the shape an admin web UI would take if one is ever built. The full "not built, by recorded decision" list, including the admin web UI and the Helm chart (third-party charts only, per `docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md`), is in [README.md](../../README.md).
- [MCP and A2A status](mcp-a2a-status.md) covers the brokering surface that is also design-only.
- [STATUS.md](../../STATUS.md) and [UPGRADE.md](../../UPGRADE.md) track what is on `main` versus the latest release (`gateway/v0.18.0`).
