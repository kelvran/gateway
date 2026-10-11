# Use Claude Code with Kelvran

This page shows a developer who runs Claude Code how to point its native Anthropic Messages mode at a Kelvran gateway, so every turn goes through the gateway's virtual keys, budgets, rate limits, guardrails, cache and routing instead of straight to a provider. Claude Code speaks the Anthropic Messages API; Kelvran serves it on `POST /v1/messages` since `gateway/v0.19.0` (item 11 of the round-4 plan, [RFC-1](../../rfcs/2026-10-09-gateway-anthropic-messages-ingress.md)) and translates each turn to whichever provider the model's deployment names — a translate hop for Bedrock, OpenAI, Gemini and OpenAI-compatible deployments; an `anthropic` deployment receives the request body as received and answers with Anthropic's own bytes, buffered or streamed (every Claude Code turn streams).

Use this when you have a running gateway and a virtual key and you want Claude Code's requests to go through Kelvran. For an application that uses the Anthropic SDK directly, see [Use the Anthropic Python SDK with Kelvran](anthropic-python.md).

## Prerequisites

- A gateway that answers `GET /healthz` with `{"status":"ok"}` and serves at least one chat deployment whose canonical model id contains `claude` or `anthropic` — Claude Code's model picker keeps only such ids from discovery (gateway compatibility page, 2026-10-10), and the model names it uses by default are Anthropic ids. A deployment can map such a canonical name to a Bedrock id (`upstream_model`); see [config.md](../../reference/config.md).
- A virtual key whose `allowed_models` (if set) includes that model. `kelvran connect claude --check` reads a `403` `model_not_allowed` on its placeholder model as a proven URL and credential, but Claude Code itself needs the real model allowed.
- Claude Code installed (`claude --version`). The behaviour described below as Claude Code's own — the credential variables, the discovery request, the hint headers, the recovery strings, the streaming watchdogs — comes from Anthropic's gateway compatibility and network-config pages as read on 2026-10-10; nothing in this repository pins a Claude Code version.
- Optional: the `kelvran` CLI, which writes the settings below for you (`kelvran connect claude --write`) and probes the gateway (`--check`); see [kelvran-cli.md](../../reference/kelvran-cli.md#kelvran-connect).

## Steps

### 1. Set the base URL and exactly one credential variable

Claude Code reads the gateway address from `ANTHROPIC_BASE_URL` (no `/v1` suffix — Claude Code appends `/v1/messages` itself) and sends the credential from one of two variables, each in a different header:

| Variable | Header Claude Code sends | Kelvran reads it on |
|---|---|---|
| `ANTHROPIC_AUTH_TOKEN` | `Authorization: Bearer <secret>` | every authenticated route |
| `ANTHROPIC_API_KEY` | `x-api-key: <secret>` | `POST /v1/messages`, `POST /v1/messages/count_tokens`, `GET /v1/models` |

Set exactly one. When both arrive with different values, the gateway verifies the `Authorization` header alone — a wrong `ANTHROPIC_AUTH_TOKEN` beside a valid `ANTHROPIC_API_KEY` is a `401` with no second attempt (owner decision Q2). `ANTHROPIC_API_KEY` also makes Claude Code prompt once, in interactive mode, to approve the key over a saved claude.ai login; `ANTHROPIC_AUTH_TOKEN` takes precedence immediately.

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080        # listen_addr from config.yaml, no /v1
read -rs ANTHROPIC_AUTH_TOKEN && export ANTHROPIC_AUTH_TOKEN   # the raw virtual-key secret, typed, never pasted into history
```

`kelvran connect claude --write` puts the same pair into Claude Code's settings file (`~/.claude/settings.json` by default) and refuses to write a file git tracks; `kelvran connect claude` alone prints the block.

A claude.ai login (OAuth) is not a virtual key: with `ANTHROPIC_BASE_URL` set and no credential variable, Claude Code sends its OAuth bearer, which the gateway rejects with `401` `invalid_api_key`. A gateway credential is required.

### 2. Turn on model discovery and the hint headers

```bash
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1   # GET /v1/models?limit=1000 at startup fills the /model picker
export CLAUDE_CODE_GATEWAY_HINT_HEADERS=1             # x-claude-code-request-class and friends, used for attribution
```

Discovery runs only in Anthropic-Messages mode (`ANTHROPIC_BASE_URL`, no `CLAUDE_CODE_USE_*` variable), with a 3 s timeout, and treats any redirect as failure — the gateway registers `/v1/models` on the exact path and never redirects. The picker shows each entry's `display_name` and `description` from the gateway's `models:` section ([config.md](../../reference/config.md)), keeps only ids containing `claude` or `anthropic`, and labels an entry without a description "From gateway". The hint headers are the bounded attribution values the gateway records on spans and metrics (`kelvran.claude_code.request_class`, `kelvran.client.tool`, ...); they are off by default behind a custom base URL, so set the variable to get them.

### 3. On a Bedrock, OpenAI or Gemini pool, disable the pre-release betas

Claude Code sends an `ANTHROPIC_BASE_URL` gateway its full capability set: `anthropic-beta` headers paired with body members such as `context_management`, `safeguards`, beta tool-schema fields and MCP tool-search `tool_reference` blocks. The gateway's canonical schema holds `thinking`, `output_config.effort`, `output_config.format`, `cache_control`, tools, `metadata.user_id` and the content blocks; a member it cannot hold is recorded by JSON pointer, and a request carrying one is served only by an `anthropic` deployment or one with `accept_lossy_anthropic_ingress: true`. On any other pool it is refused before any upstream call:

```json
{"type":"error","error":{"type":"invalid_request_error","code":"lossy_ingress_rejected","param":"/context_management,/tools/0/strict",
 "message":"dataplane: request carries 1 member this gateway cannot translate for the deployment that would serve it (context_management) and 1 nested member (listed in param); set accept_lossy_anthropic_ingress: true on that deployment to drop them, or start Claude Code with CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"}}
```

Two remedies, pick one:

```bash
export CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1   # Claude Code stops sending the pre-release pairs (keeps thinking, effort, cache_control)
```

or, on the deployment, `accept_lossy_anthropic_ingress: true`, which drops the members quietly and records them as `kelvran.ingress.dropped_fields` on the span and `dropped_fields` on the `chat_completion` log line. `kelvran doctor` warns about every non-`anthropic` chat deployment that leaves the flag off. Residue the variable keeps — `thinking: {"type":"adaptive"}`, `output_config.effort`, the extended-context and interleaved-thinking betas — is canonical and translates (Bedrock drops a field it cannot carry and logs `request_field_dropped`), and the `anthropic-beta` header is stripped on a Bedrock deployment unless its `anthropic_beta_policy` is `forward_known` (applied when the upstream leg lands).

The attribution block Claude Code puts first in `system` (`x-anthropic-billing-header: …`, controlled by `CLAUDE_CODE_ATTRIBUTION_HEADER`) travels as its own system block, so the protocol page's remedy for gateways that merge the system array, `CLAUDE_CODE_ATTRIBUTION_HEADER=0`, is not needed here. On 2026-10-11 Bedrock's Anthropic backend accepted Claude Code's block and counted none of it as input tokens; an invented block with the same prefix was refused by the backend as a reserved keyword.

A `role: system` entry inside `messages` — Claude Code's mid-conversation system message, sent with the betas on under the `mid-conversation-system-2026-04-07` beta value seen in the 2026-10-11 run — is parsed like the top-level `system` array (one system block per text block, `cache_control` kept). On a Bedrock, Gemini or (translate-hop) Anthropic deployment it is hoisted into the provider's system prompt after the other system blocks, since none of them has a mid-conversation slot; on an OpenAI or OpenAI-compatible deployment it stays in place as a `role: system` message. The protocol page asks gateways to keep block-form system content rather than flattening it to a string, and that is what happens here (2026-10-11).

The message never names a deployment and never contains the strings Claude Code matches on for its own recovery (`thinking`, `cache_control`, `system`, `output_config.effort`, ...), so Claude Code surfaces it to you rather than silently disabling a capability.

### 4. Verify

```bash
kelvran connect claude --check --url "$ANTHROPIC_BASE_URL"   # POST /v1/messages with max_tokens: 1 and a placeholder model
claude -p "say ok"
```

`--check` exits `0` on a `200`, a `400` `model_not_found` or a `403` `model_not_allowed` (the gateway authenticated the request before it routed or applied the allowlist); a `404` means a gateway older than `gateway/v0.19.0`; a `401` means the key is not a virtual key of this gateway. In Claude Code, `/status` shows the "Auth token or API key" line naming your variable. On the gateway side a turn is one `chat_completion` log line with `ingress_format: anthropic-messages`, and the request span carries `kelvran.ingress.format`. In a headless `claude -p` run on 2026-10-11 (Claude Code 2.1.296) with `CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1` the gateway saw exactly that one line and no discovery or `count_tokens` request; with the betas left on it saw one `GET /v1/models`, one `count_tokens` (answered `404`, after which the turn went on) and the turn, which answered `ok` once the same night's fix for mid-conversation system messages was in (see the change log).

## Variants

### Streaming, pings and cut-off responses

Claude Code reads every turn as a stream. The gateway emits the Anthropic event sequence as the provider's chunks arrive and an `event: ping` after 15 s or more of upstream silence — Claude Code's byte-level watchdog allows 180 s through a custom base URL (300 s before it has fetched feature flags) and its event-level watchdog 300 s, both reset by pings (network-config page, 2026-10-10). A response the gateway cuts off (the runaway ceiling, a mid-stream budget or token top-up) ends with `stop_reason: max_tokens`; a failure after the first event ends with one `event: error` (type and redacted message) and no `message_stop`, which Claude Code treats as a failed turn rather than a complete one.

### Exact token counts

Claude Code calls `POST /v1/messages/count_tokens` for `/context`. On an `anthropic` deployment the gateway answers with Anthropic's own count (slice S11c: the body is scanned by the pre-call guardrail, then forwarded to the deployment's `count_tokens` as received, only `model` rewritten to the deployment's `upstream_model`, and the answer comes back unchanged). There `/context` is exact; on every other deployment it answers `404` `not_found_error` (`code` `count_tokens_unavailable`) and Claude Code falls back to its character-based estimate, as the protocol page describes for an absent endpoint. Either way the call authenticates, applies the key's allowlists and consumes one RPM token; it touches no budget and no cache (Anthropic bills nothing for a count). In the 2026-10-11 run with the betas on, against the Bedrock pilot, Claude Code called it once, received the `404` and continued the turn; the `anthropic` branch is mock-proven only, no key being available.

### Auto mode

In auto mode Claude Code asks the server to run its permission classifier by adding a `safeguards` body member and a beta value to its turns. A translate hop cannot forward them — they are dropped under `accept_lossy_anthropic_ingress: true` and refused without the flag; an `anthropic` deployment forwards them with the rest of the body and relays the response — buffered or streamed — as Anthropic sent it, `safeguard_results` included (slices S11b/S11b2: a consequence of byte identity). On a translate hop Claude Code prints the notice that the session "isn't eligible" for the no-charge classifier requests, names the gateway, holds the first checked action until you press Enter, and runs its own classifier requests, billed as token usage as before (auto-mode classifier page, 2026-10-11). Set `CLAUDE_CODE_AUTO_MODE_SERVER=0` to stop it asking on a translate hop. On an `anthropic` deployment Anthropic's `safeguard_results` reach Claude Code unchanged, so the notice should not appear; not yet observed live.

### Over-long prompts

When the provider rejects a prompt as too long, the gateway's error message starts with `capability_rejected: prompt_too_long` — the marker Claude Code's recovery matches on — before the redacted text; the status stays what the gateway maps the upstream failure to (`502` `api_error`) — except on an `anthropic` deployment, whose own `400` reaches Claude Code unchanged, wording and status included (slice S11b). Whether Claude Code's reactive compaction fires on it is a live check recorded in the slice's log entry, not a promise of this page.

### GitHub Actions and headless runs

`claude -p` and the GitHub Action read the same variables; both need `ANTHROPIC_BASE_URL` and one credential variable in the job's environment. A headless run that hits an `allowed_models` key prints the `403` envelope; give the key the model or the model the key.

## How errors surface in Claude Code

Every error is Anthropic's envelope with Kelvran's `code` kept — except an `anthropic` deployment's own `400`/`422`, relayed exactly as Anthropic sent it, `x-should-retry` and `anthropic-ratelimit-unified-*` headers included ([error-codes.md](../../reference/error-codes.md)):

| Status | `type` | `code` | What to do |
|---|---|---|---|
| 400 | `invalid_request_error` | `model_not_found` | The model id is not a canonical model of this gateway; pick one from `/model` or `GET /v1/models`. |
| 400 | `invalid_request_error` | `lossy_ingress_rejected` | Step 3: disable the pre-release betas, or set `accept_lossy_anthropic_ingress` on the deployment. `param` lists the members. |
| 400 / 422 | Anthropic's own | none | An `anthropic` deployment's own rejection, relayed as Anthropic sent it; read `message`. No `Retry-After`. |
| 401 | `authentication_error` | `invalid_api_key` / `key_expired` / none | The credential is not a virtual key, has expired, or two variables carry different values; set exactly one current key. |
| 403 | `permission_error` | `model_not_allowed` | The key's `allowed_models` excludes the model. |
| 404 | `not_found_error` | `count_tokens_unavailable` | `/context` on a deployment other than `anthropic`; Claude Code estimates locally. |
| 429 | `rate_limit_error` | `rate_limit_exceeded` / `concurrency_limit_exceeded` | Retry after `Retry-After` (integer seconds, at most 60). |
| 429 | `rate_limit_error` | `insufficient_quota` | The key's budget is spent; no `Retry-After`, Claude Code retries on its own schedule until an operator raises the budget or the window resets. |
| 502 / 503 | `api_error` | `upstream_error` / `deployment_capacity_exceeded` | A provider or capacity failure after fallback; the message is redacted. |

## What Kelvran adds that Claude Code ignores

`code` and `param` in the error envelope (Anthropic's own has neither), the `X-Kelvran-Overhead-Duration-Ms` header on buffered responses, `kelvran.ingress.*` span attributes, and the `request_field_dropped` and `dropped_fields` log fields. Claude Code reads none of them; `kelvran connect claude --check` reads `code`.

## Not available today

- Fidelity on a translate hop: every deployment other than `anthropic` is served by shadow-encoding the canonical request, so unknown members are dropped there (reported in `dropped_fields`) and `kelvran.ingress.passthrough` is `false`. An `anthropic` deployment relays request and response as sent, buffered and streamed (slices S11a/S11b/S11b2, `kelvran.ingress.passthrough: true`); on a streamed turn the gateway's own `event: ping` may be interleaved after 15 s or more of upstream silence, and an upstream `error` frame that follows a relayed frame reaches Claude Code as Anthropic sent it (one that is the very first event, like a first frame the gateway cannot decode, is redacted and the turn may fall back).
- Exact token counts on a translate hop (`count_tokens` answers `404` there; an `anthropic` deployment counts exactly, see above).
- The Amazon Bedrock and Google Vertex AI request formats (`CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`): Kelvran speaks the Anthropic Messages format only; point Claude Code at it with `ANTHROPIC_BASE_URL`.
- Forwarding `anthropic-beta` values to Bedrock: `anthropic_beta_policy: forward_known` loads and validates today and is applied when the upstream leg lands; a Bedrock hop strips the header until then; an `anthropic` deployment forwards it as sent.
- `anthropic-ratelimit-unified-*` and `x-should-retry` response headers on a translate hop: the gateway forwards them from an `anthropic` deployment and synthesises none for any other (they express Anthropic plan limits).

## Related pages

- [Data-plane API: `POST /v1/messages`](../../reference/data-plane-api.md#post-v1messages) and [`POST /v1/messages/count_tokens`](../../reference/data-plane-api.md#post-v1messagescount_tokens)
- [`kelvran connect`](../../reference/kelvran-cli.md#kelvran-connect)
- [Compatibility](../../reference/compatibility.md) — the `anthropic` SDK and Claude Code rows
- [Error codes](../../reference/error-codes.md) — the Anthropic envelope
- [Use the Anthropic Python SDK with Kelvran](anthropic-python.md)
