# Kelvran

**A self-hosted Go LLM gateway that speaks OpenAI's API in front of OpenAI, Anthropic, Gemini, Bedrock and self-hosted models, with an embedded three-layer response cache, virtual keys with budgets, and a separate agent-evaluation CLI.**

[![CI](https://github.com/kelvran/gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/kelvran/gateway/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/kelvran/gateway/badge)](https://scorecard.dev/viewer/?uri=github.com/kelvran/gateway)
[![Go Reference](https://pkg.go.dev/badge/github.com/kelvran/gateway/gateway.svg)](https://pkg.go.dev/github.com/kelvran/gateway/gateway)
[![gateway release](https://img.shields.io/github/v/release/kelvran/gateway?filter=gateway%2F*&label=gateway)](https://github.com/kelvran/gateway/releases?q=gateway%2F)
[![evals release](https://img.shields.io/github/v/release/kelvran/gateway?filter=evals%2F*&label=evals)](https://github.com/kelvran/gateway/releases?q=evals%2F)
[![Go version](https://img.shields.io/github/go-mod/go-version/kelvran/gateway?filename=gateway%2Fgo.mod)](gateway/go.mod)
[![License](https://img.shields.io/github/license/kelvran/gateway)](LICENSE)

## What Kelvran is

Kelvran is a self-hosted LLM gateway written in Go. It exposes OpenAI's Chat Completions and Embeddings wire format (`POST /v1/chat/completions`, `POST /v1/embeddings`) and routes each request to one of five provider adapters: OpenAI, Anthropic, Google Gemini, AWS Bedrock, or any OpenAI-compatible self-hosted server (vLLM, Ollama, TGI, llama.cpp, LocalAI). The same binary holds a three-layer response cache, per-tenant virtual keys with budgets and rate limits, regex guardrails for PII, secrets and prompt injection, and OpenTelemetry tracing that attributes cost to an `agent_run_id` whenever the caller propagates one in W3C `baggage`. A separate Python CLI, `evals`, scores agent outputs with Wilson confidence intervals, an LLM judge, and a Docker sandbox for rollouts. It is for teams that route LLM traffic from agents and want one place to meter, cap, cache and trace it.

What it does that most gateways do not, with the code that proves it:

- **Agent-run cost attribution.** Standard W3C `traceparent`/`baggage` headers carry `agent_run_id`; when present, the gateway puts it on the request's span (`kelvran.agent_run_id`, `kelvran.cost.usd`) and on its `GatewayDecisionEvent` log record, so `evals cost-report --agent-run-id` can sum one run. Per request only; roll-up is the consumer's job. Proof: [`gateway/internal/telemetry/telemetry.go`](gateway/internal/telemetry/telemetry.go), [`api/gatewayevents/v1/gatewayevents.proto`](api/gatewayevents/v1/gatewayevents.proto), [`docs/operations/TELEMETRY.md`](docs/operations/TELEMETRY.md).
- **Correctness-gated cache.** A near-duplicate hit must pass entity/number/date, negation, 24 h freshness, exact-model, `response_format` and guardrail-policy-version gates, and volatile queries (weather, price, today, ...) bypass it outright; no gate can be configured off. The match is lexical (MinHash/Jaccard at or above 0.9), not embedding-based, so it tolerates typos and single-word swaps, not paraphrases. Proof: [`gateway/internal/gateway/dataplane/entities.go`](gateway/internal/gateway/dataplane/entities.go), [`gateway/internal/cache/lexical.go`](gateway/internal/cache/lexical.go), [`THREAT_MODEL.md`](THREAT_MODEL.md).
- **Adversarial multi-judge evals.** `evals run --llm-judge-panel` sends the identical prompt to Claude Sonnet 5 and Claude Haiku 4.5 on Bedrock concurrently, reduces by strict majority, and fails closed on a 1-1 tie; every judge score records its bias mitigations and whether the quoted evidence is a verbatim span of the output. Proof: [`evals/evals/judge/llm_judge.py`](evals/evals/judge/llm_judge.py), [`docs/rfcs/2026-09-08-evals-judge-panel-reducer.md`](docs/rfcs/2026-09-08-evals-judge-panel-reducer.md).

## Quickstart (five minutes)

```bash
# (a) Go 1.26.8+ (or any Go 1.21+ with the default GOTOOLCHAIN=auto, which fetches 1.26.8); the binary lands at $(go env GOPATH)/bin/gateway
go install github.com/kelvran/gateway/gateway/cmd/gateway@latest
```

```bash
# (b) Published image: linux/amd64 only. arm64 hosts run it under amd64 emulation (QEMU/Rosetta binfmt, which Docker Desktop and colima set up;
#     a host without it fails with "exec format error" whether or not the flag is set); Docker prints a platform-mismatch warning, which
#     --platform linux/amd64 silences and makes explicit. CMD is already "-config /config.yaml"; the run command is under "Validate, then run" below, once config.yaml exists.
docker pull ghcr.io/kelvran/gateway:latest
```

```bash
# (c) From source; the result is a single self-contained binary (statically linked in the published Linux image)
git clone https://github.com/kelvran/gateway.git && cd gateway/gateway
go build -o /tmp/kelvran-gateway ./cmd/gateway   # out of tree: a gateway/gateway binary is not gitignored
```

### Minimal config

A virtual key is a bearer secret you generate; `config.yaml` stores only its SHA-256 hash and the gateway can never recover the secret. Generate the pair (`export` so the Python snippet below can read it; `printf '%s'` so no trailing newline is hashed; if `sha256sum` is missing (older macOS), use `shasum -a 256`):

```bash
export KELVRAN_KEY=$(openssl rand -hex 32)   # the raw secret: clients send it, config never stores it
printf '%s' "$KELVRAN_KEY" | sha256sum | cut -d' ' -f1   # prints only the hex digest, which goes into key_hash below
```

Save this as `config.yaml`. The `key_hash` shown is the real SHA-256 of the fake example secret `kelvran-example-secret-do-not-use`; replace it with your own digest. Secrets are never written here, only the names of environment variables that hold them.

```yaml
listen_addr: ":8080"
virtual_keys:
  team-alpha:
    # SHA-256 of the example secret "kelvran-example-secret-do-not-use" -- replace with your own digest.
    key_hash: "bc92cf1ba2ca07ba70b0681b7acf8cfac6761d24026e50f11ad922ffd49bb3f6"
    budget_usd: 25.0                # optional; omit or 0 for unlimited
deployments:
  gpt4o-primary:
    model: "gpt-4o"                 # the name clients send
    provider: "openai"              # openai | anthropic | gemini | bedrock | openaicompat
    upstream_model: "gpt-4o"
    base_url: "https://api.openai.com/v1/chat/completions"
    api_key_env: "OPENAI_API_KEY"   # env var NAME; the value is never written here
price_table:                        # optional; an unpriced model is billed as 0 USD
  gpt-4o:
    prompt_per_token: 0.0000025
    completion_per_token: 0.00001
```

If you kept the example `key_hash`, send `Authorization: Bearer kelvran-example-secret-do-not-use` for this first request. Otherwise substitute your own digest in one step (the same `sha256sum`/`shasum -a 256` swap as above applies):

```bash
sed -i.bak "s/bc92cf1b[0-9a-f]*/$(printf '%s' "$KELVRAN_KEY" | sha256sum | cut -d' ' -f1)/" config.yaml   # -i.bak works on both GNU and BSD sed
```

The config parser is a small YAML subset: `key: value` scalars and 2-space-indented mappings only, no lists, anchors or multi-line strings, and `#` starts a comment anywhere. For Bedrock set `provider: "bedrock"` with `access_key_id_env`, `secret_access_key_env`, `region` and a `base_url` ending in `/converse`; for a self-hosted runtime set `provider: "openaicompat"` (plus `allow_insecure_http: true` for plain `http://`). Every field, including rate limits, budgets, the admin API and Redis, is annotated in [`gateway/config.example.yaml`](gateway/config.example.yaml).

By default the gateway prints every OTel span and a metrics dump every 60 s as JSON on stdout, interleaved with its own JSON log lines (both go to stdout, so redirecting stderr does not separate them). For a quiet terminal add `telemetry:` / `  exporter: "none"` to `config.yaml`, or `exporter: "otlp"` with `otlp_endpoint: "localhost:4318"` to ship to a collector; see [`gateway/config.example.yaml`](gateway/config.example.yaml) and [`docs/operations/TELEMETRY.md`](docs/operations/TELEMETRY.md). Validate, then run:

```bash
# Validate: prints "config is valid" and exits 0; does not resolve env vars and does not check the Gemini/Bedrock base_url suffix. Pick the line for your install path.
/tmp/kelvran-gateway -validate -config config.yaml            # (c) source build
$(go env GOPATH)/bin/gateway -validate -config config.yaml    # (a) go install
docker run --rm -v "$PWD/config.yaml:/config.yaml:ro" ghcr.io/kelvran/gateway:latest -validate -config /config.yaml   # (b) image; no -p or -e needed
# Which build is this? Prints one line, e.g. "kelvran-gateway 0.17.0 (<commit>, built <date>, go1.27.1, linux/amd64)"; a source build says "dev".
/tmp/kelvran-gateway -version
export OPENAI_API_KEY="<your provider key>"
# Run. (b) only now that config.yaml exists: Docker bind-mounts a missing host file as an empty directory and the container exits with "read /config.yaml: is a directory".
/tmp/kelvran-gateway -config config.yaml                      # (c)
$(go env GOPATH)/bin/gateway -config config.yaml              # (a)
docker run --rm -p 8080:8080 -e OPENAI_API_KEY -v "$PWD/config.yaml:/config.yaml:ro" ghcr.io/kelvran/gateway:latest   # (b)
```

On macOS, Windows or any other non-Linux host the first two log lines are a harmless pair, `ERROR failed to set GOMEMLIMIT` and `WARN automemlimit: could not set GOMEMLIMIT from cgroup` (cgroups exist only on Linux, so the probe cannot run; the error text is "cgroups is not supported on this system"); the gateway is up once you see `gateway listening`. Set `AUTOMEMLIMIT=off` to skip the cgroup probe.

### First request

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $KELVRAN_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Say hello in five words."}]}'
```

```python
# Any OpenAI SDK works by changing base_url; add stream=True for Server-Sent Events.
import os
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key=os.environ["KELVRAN_KEY"])  # the raw secret, never the hash
resp = client.chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": "hi"}])
print(resp.choices[0].message.content)
```

A success is `HTTP 200` with an OpenAI-shaped JSON body (`id`, `model`, `choices[0].message.content`, `usage`). Every error is an OpenAI-shaped `{"error":{"message","type","param","code"}}` body, so the official SDKs raise their usual exception classes with `.code` populated. Otherwise:

- `401` means the `Authorization: Bearer` header is missing or its SHA-256 matches no configured virtual key.
- `429` means this virtual key hit its RPM/TPM limit, its concurrency cap, or its `budget_usd`; the JSON error body says which (`error.type` is `rate_limit_error` for the limit and concurrency cases, `insufficient_quota` for budget, with a matching `error.code`), and `Retry-After` is set for the limit and concurrency cases (not for budget).
- `502` with `error.message` `upstream provider returned status N` (`error.type` `server_error`, `error.code` `upstream_error`, and `Retry-After`) means your virtual key was accepted and the request reached the provider, which answered N. `N=401` is almost always a wrong or unset `OPENAI_API_KEY` (the gateway starts anyway, logging `deployment's upstream API key env var is not set`); the provider's full error text is only in the gateway's `chat_completion` log line, never in the response. The OpenAI SDK retries 5xx twice, then raises `openai.InternalServerError`.

**Evals (optional).** Needs Python 3.12+ and `uv`, no provider credentials; `--llm-judge` needs AWS Bedrock credentials by default (or an Anthropic/OpenAI key with `--llm-judge-provider`), `--llm-judge-panel` always needs Bedrock, and `evals rollout` needs Docker. The golden fixture deliberately contains one wrong answer (2 of 3 pass, Wilson 95 percent CI [0.2077, 0.9385]), so a `--fail-under` of 0.5 or higher is expected to fail.

```bash
cd evals && uv sync && uv run evals run --suite tests/fixtures/golden_example.json --scores /tmp/kelvran-scores.jsonl && uv run evals report --scores /tmp/kelvran-scores.jsonl --fail-under 0.2
```

## Features

| Area | Feature | What it does | Doc |
|---|---|---|---|
| Routing & reliability | OpenAI-compatible API | `POST /v1/chat/completions` (buffered and SSE), `POST /v1/embeddings` and, since 2026-10-08 on `main`, `GET /v1/models` (the canonical models the calling key may use, as one document the OpenAI SDK, the Anthropic SDK and Claude Code's discovery all read; `/healthz` and `/readyz` are the only other routes in [`gateway/cmd/gateway/main.go`](gateway/cmd/gateway/main.go)). | [USER_GUIDE.md](docs/users/USER_GUIDE.md) (chat how-to), [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md) (embeddings pipeline), [changelog 0.13.0](gateway/changelog/0.13.0.md) |
| Routing & reliability | Weighted routing, canary, aliases | Smooth weighted round-robin across deployments sharing one `model`, skipping deployments the synthetic health probes mark unhealthy (no traffic-derived circuit breaker exists), with probe-latency de-weighting and `cost_tier` bias toward the cheapest healthy tier; `sticky: true` hash-buckets a virtual key onto a canary; deployments sharing a `model` may use different providers, billed at one `price_table` rate. | [RFC](docs/rfcs/2026-09-04-weighted-routing.md), [probes RFC](docs/rfcs/2026-09-07-gateway-active-health-probing.md) |
| Routing & reliability | Failover and fallback chains | One same-pool retry by default, or error-classified multi-hop `fallback_chains` (`content_policy`, `context_window_exceeded`, `generic`) across models and providers, with a 3-failure per-request breaker and jittered backoff; classification is a keyword heuristic over the upstream body. | [RFC](docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md) |
| Routing & reliability | Deployment capacity gates | Per-deployment RPM, TPM (reserve-then-reconcile, provider-style `tpm_accounting`) and concurrency ceilings; a rejection is `503` with `Retry-After`. | [changelog 0.17.0](gateway/changelog/0.17.0.md) |
| Routing & reliability | Retry-After | Rate-limit and concurrency `429`, `502` and `503` carry an escalating per-key backoff, raised to the upstream's own value and capped at 60 s; a budget `429` does not. | [RFC](docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md) |
| Routing & reliability | Idempotency-Key | Replays within 10 minutes return the stored response; scoped per virtual key; in-process, so single instance only (Proof: [`gateway/internal/gateway/dataplane/dataplane.go`](gateway/internal/gateway/dataplane/dataplane.go), [`gateway/internal/idempotency/inprocess/inprocess.go`](gateway/internal/idempotency/inprocess/inprocess.go)). | [changelog 0.11.0](gateway/changelog/0.11.0.md), [0.12.0](gateway/changelog/0.12.0.md) |
| Routing & reliability | Structured output | `response_format` json_schema forwarded natively to every provider; on Bedrock only whitelisted Claude families qualify (matched at a family boundary, so `claude-sonnet-5` does not cover `claude-sonnet-5-5`), and `json_object` is accepted but not enforced there. | [RFC](docs/rfcs/2026-09-12-gateway-structured-output-normalization.md) |
| Routing & reliability | Tool calling | OpenAI `tools`, `tool_calls` and `tool_choice` work on all five providers: `tool_choice` accepts OpenAI's bare strings (`"auto"`, `"required"`, `"none"`), OpenAI's function object and Kelvran's canonical `{"mode","tool_name"}` object (on `main` since 2026-10-08; `v0.17.0` rejects the OpenAI forms). | [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md) |
| Routing & reliability | Reasoning blocks | Extension fields `reasoning_blocks[]` and `usage.reasoning_tokens` round-trip extended thinking for Anthropic, Bedrock, Gemini and openaicompat. | [USER_GUIDE.md](docs/users/USER_GUIDE.md) |
| Routing & reliability | Prompt store | `prompt_id`, `prompt_label` and `prompt_variables` resolve server-side templates managed over `/admin/prompts`. | [RFC](docs/rfcs/2026-09-13-gateway-prompt-management.md) |
| Routing & reliability | MCP/A2A tool brokering | Not built; two design-only RFCs exist and `gateway/internal/` has no `mcp` package. | [RFC](docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md) |
| Access control & cost | Virtual keys | Bearer secret, SHA-256 hash in config; per-key `allowed_models`, `allowed_regions` and `allowed_source_cidrs` (TCP peer only, `X-Forwarded-For` is not trusted). | [RFC](docs/rfcs/2026-09-02-virtual-keys-budgets.md) |
| Access control & cost | Budgets | `budget_usd` with rolling `budget_reset_interval_seconds`, a 50/75/90/100 percent ladder, `budget_warn_percent` and Standard-Webhooks alerts; over budget returns `429`. | [RFC](docs/rfcs/2026-09-05-gateway-budget-warn-threshold.md) |
| Access control & cost | Rate limits | Per-key RPM (default 20 burst, 10 per second), TPM, `max_concurrent_requests` and per-model overrides. | [RFC](docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md) |
| Access control & cost | Cost accounting | Decimal USD from `price_table`; cache hits and coalesced duplicates are never billed; a post-call guardrail block still is. | [config.example.yaml](gateway/config.example.yaml) |
| Access control & cost | Multi-replica state | Redis backs budgets, virtual keys, RPM/TPM and HMAC-signed config propagation; per-key concurrency, deployment ceilings, the response cache, prompts and Idempotency-Key stay per replica. | [DEPLOY.md](docs/operations/DEPLOY.md) |
| Cache | Three layers | L1 exact, L2 normalized, L3-lite lexical near-duplicate (MinHash/Jaccard, threshold 0.9); embedding-based semantic matching is designed, not built. | [RFC](docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md) |
| Cache | Hard gates | An L3 hit must match entity/number/date and negation fingerprints, be under 24 h old, share model, `response_format` and guardrail policy version, and not be a volatile query; no knob disables this. | [THREAT_MODEL.md](THREAT_MODEL.md) |
| Cache | Tenant partitioning | Every layer is partitioned per virtual key, each with its own LRU cap (default 10,000 entries per key per layer, overridable via `cache.max_entries`, `cache.l2.max_entries`, `cache.l3.max_entries`); `cache_scope_to_end_user` adds a per-end-user split via `X-Kelvran-End-User-Id`. | [changelog 0.14.0](gateway/changelog/0.14.0.md) |
| Cache | Stampede protection | Concurrent identical misses are coalesced with singleflight into one upstream call; the cache is always on, in-process, and empty after a restart. | [RFC](docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md) |
| Cache | Never cached | `finish_reason: length`, non-2xx upstream responses and post-call guardrail blocks are never written; streaming requests are served from and written to the cache too. | [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md) |
| Cache | Provider-side prompt caching | One `cache_control` marker becomes Anthropic `cache_control`, Bedrock `cachePoint` or OpenAI `prompt_cache_key`; Anthropic and Bedrock system prompts are auto-marked unless `disable_cache_control_auto_populate` is set; `usage` reports `cache_read_tokens` and `cache_creation_tokens`. | [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md) |
| Cache | Erasure | `POST /admin/cache/erase` removes one request's L1/L2 entries; L3 has no erasure path. | [DATA-SUBJECT-REQUESTS.md](docs/operations/DATA-SUBJECT-REQUESTS.md) |
| Safety | Built-in detectors | Email, phone, US SSN, IBAN, credit card, IP, secret keys and prompt injection run pre-call, post-call and on embeddings; regex and checksums, no ML by default; zero-width, bidi and Tags-block characters are flagged and stripped before every detector runs. | [RFC](docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md) |
| Safety | Block and warn tiers | `credential`, `financial_id`, `government_id` block; `contact_info`, `network_id`, `prompt_injection` warn; `category_overrides` flips either way; a warn-tier detector error fails open and increments `kelvran.guardrail.fail_open`. | [changelog 0.17.0](gateway/changelog/0.17.0.md) |
| Safety | Optional AWS detectors | Bedrock Guardrails `PROMPT_ATTACK` and Titan-embedding similarity against a known-injection corpus, both with file-based credential hot-reload. | [config.example.yaml](gateway/config.example.yaml) |
| Safety | Enforcement limits and request caps | A block is `400`; findings are logged, never redacted; on streams the post-call check is audit-only because chunks are already sent. Compiled-in caps: 32 MiB body (`413`), 2,000 messages, 256 tools, 8 MiB per content field, schemas 32 levels / 10,000 tokens; upstream error bodies are redacted to `upstream provider returned status N`. | [THREAT_MODEL.md](THREAT_MODEL.md) |
| Observability | OTel GenAI spans and metrics | One `chat <model>` GenAI span per chat request, nested as a child of the otelhttp `POST /v1/chat/completions` HTTP server span that wraps every route (chat, embeddings, `/healthz`, `/readyz`); `gen_ai.client.operation.duration` and five `gen_ai.client.inference.usage.*` token counters (input, output, cache read, cache write, reasoning); OTLP over HTTP or stdout. Embeddings emit no GenAI span, only the HTTP server span. | [TELEMETRY.md](docs/operations/TELEMETRY.md) |
| Observability | Kelvran counters | `kelvran.llm.spend_usd`, `kelvran.budget.threshold_crossed`, `kelvran.cache.lookup`, `kelvran.cache.savings_usd`, `kelvran.cache.l3.gate_outcome`, and `kelvran.{ratelimit,budget,guardrail}.fail_open`. | [TELEMETRY.md](docs/operations/TELEMETRY.md) |
| Observability | Agent-run attribution | The W3C `baggage` member `agent_run_id` lands on spans and cost events; the gateway does no per-run roll-up. | [RFC](docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md) |
| Observability | GatewayDecisionEvent | A protobuf-defined JSON log field per chat request, shipped by Vector to S3 or GCS for `evals ingest`. | [api/README.md](api/README.md) |
| Observability | Dashboards and SLOs | A provisioned Grafana overview, Prometheus multi-window burn-rate rules against a provisional 99 percent target, and an Alertmanager config with a placeholder receiver. | [grafana/](docs/operations/grafana/) |
| Operations | Admin API | Off by default on its own loopback listener `127.0.0.1:8081`; Admin, Viewer, CostViewer and Operator bearer tiers over 18 routes (keys, spend, prompts, weights, cache erase, backup, audit) plus pprof behind `enable_pprof`; every call is audited; a valid credential of an insufficient tier gets `401`, not `403`; optional mTLS; no OpenAPI spec. | [RFC](docs/rfcs/2026-09-05-gateway-admin-api.md) |
| Operations | Live key management | Create, rotate with a grace period, and delete (purging spend) virtual keys without restart, persisted via `admin.persist_path` (bbolt) or `admin.redis_addr`; deployment weights change live but are in-memory only and revert to `config.yaml` on restart. | [RFC](docs/rfcs/2026-09-20-gateway-admin-rbac-risk-tiering.md) |
| Operations | Credential hot-reload | `*_file` credential fields are re-read every `credential_reload.interval_seconds` (default 60 s) so rotated Secrets reach a running process; `*_env` values stay fixed until restart. | [config.example.yaml](gateway/config.example.yaml) |
| Operations | Backup, restore, validation | `POST /admin/backup` copies bbolt stores live; `-restore-store` restores offline; `-validate` checks config shape and references but never env vars. | [DEPLOY.md](docs/operations/DEPLOY.md) |
| Operations | Runtime and health | Static binary, `FROM scratch`, non-root UID 65532, `GOMEMLIMIT` set from the cgroup limit (automemlimit), graceful shutdown of both listeners; `/healthz` is liveness, `/readyz` reports per-model readiness and returns `503` when any model has no healthy deployment. | [DEPLOY.md](docs/operations/DEPLOY.md) |
| Evals | Deterministic scoring | Exact or regex match per case; every pass rate prints a Wilson 95 percent confidence interval. | [evals/ARCHITECTURE.md](evals/ARCHITECTURE.md) |
| Evals | CI gate | `evals report --fail-under` fails on the Wilson lower bound, per tier and per tag; a 113-case regression corpus gates this repo's own CI. | [evals/ARCHITECTURE.md](evals/ARCHITECTURE.md) |
| Evals | LLM judge and panel | `--llm-judge` defaults to Claude Haiku 4.5 on Bedrock (`--llm-judge-provider bedrock\|anthropic\|openai`); `--llm-judge-panel` runs two Bedrock Claude judges and fails closed on a tie; Bedrock judge cost is reported as unknown. | [RFC](docs/rfcs/2026-09-08-evals-judge-panel-reducer.md) |
| Evals | Sandboxed rollouts | `evals rollout` runs each case in Docker with no network, a read-only filesystem and CPU, memory and pid limits, one case at a time. | [evals/ARCHITECTURE.md](evals/ARCHITECTURE.md) |
| Evals | Production ingestion | `evals ingest` samples GatewayDecisionEvents from S3 or GCS into promotable `drift_sample` cases; no online judging of production traffic exists. | [Vector config](docs/operations/vector-gatewayevents-s3.yaml) |

## Providers

Only these five adapters exist; a deployment naming any other `provider` fails at startup. Every `base_url` must be `https` unless the deployment sets `allow_insecure_http: true`.

| Provider | `provider:` | Credentials in config | Status |
|---|---|---|---|
| OpenAI | `openai` | `api_key_env` or `api_key_file` | Chat, SSE, `/v1/embeddings`, structured output, tools; `cached_tokens` and `reasoning_tokens` read from usage; a sha256-of-body `Idempotency-Key` is sent upstream. |
| Anthropic | `anthropic` | `api_key_env` or `api_key_file` | Chat and SSE with `anthropic-version 2023-06-01`; structured output via `output_config.format`; auto-marked prompt caching; preserved-thinking binding for Claude Fable 5.1 and Opus 5.5; `disable_parallel_tool_use` honored here only; `max_tokens` defaults to 4096; no embeddings. |
| Google Gemini | `gemini` | `api_key_env` or `api_key_file` (Generative Language API only; no Vertex AI or service accounts) | Chat and SSE; `base_url` must end in `:generateContent`; `responseSchema` forwarded unvalidated; safety blocks classified as `content_policy` for fallback; no embeddings; reasoning tokens always 0. |
| AWS Bedrock | `bedrock` | `access_key_id_env`, `secret_access_key_env`, optional `session_token_env` (or `*_file`), plus `region`; requests are SigV4-signed; `base_url` must end in `/converse` | Live pilot traffic. Converse and ConverseStream, Titan embeddings (one input per call); structured output only for a whitelisted set of Claude families (`claude-sonnet-5`, `claude-opus-4-6`, `claude-sonnet-4-6`, `claude-sonnet-4-5`, `claude-opus-4-5`, `claude-haiku-4-5`; `claude-sonnet-5` is on `main`, not in `v0.17.0`). Claude Sonnet 5.5 (`claude-sonnet-5-5`) is deliberately not whitelisted because Bedrock still rejected `output_config.format` for it when live-tested on 2026-10-08, so `response_format` to a Sonnet 5.5 deployment fails with `502` unless another capable deployment serves the same `model`; `json_object` is accepted but not enforced; inline base64 media only, URL image parts rejected; empty response `id` (see Status and known limitations). |
| OpenAI-compatible | `openaicompat` | `api_key_env` or `api_key_file`; `allow_insecure_http: true` for plaintext `http://` | vLLM, Ollama, TGI, llama.cpp, LocalAI; the wire shape is forwarded verbatim, so schema enforcement is the backend's responsibility; no embeddings. |

The Gemini `:generateContent` and Bedrock `/converse` suffixes are not checked at load or by `-validate`: a wrong suffix fails on the first `stream: true` request (the dataplane derives `:streamGenerateContent?alt=sse` / `/converse-stream` from it), while buffered requests are sent to the configured URL as-is.

Tool calling: OpenAI's `tools[]`, `tool_calls` and `tool_choice` work on all five providers. `tool_choice` accepts OpenAI's bare strings (`"auto"`, `"required"`, `"none"`), OpenAI's `{"type":"function","function":{"name":...}}` object and Kelvran's canonical `{"mode","tool_name","disable_parallel_tool_use"}` object; any other shape is a `400` whose envelope names `tool_choice` (on `main` since 2026-10-08, not in `v0.17.0`). Unknown request fields (`n`, `stop`, `top_p`, `seed`, ...) are dropped silently, neither forwarded nor rejected.

## How it compares

| | Kelvran | LiteLLM | Portkey | TensorZero | Bifrost | Kong AI Gateway | Langfuse | Braintrust |
|---|---|---|---|---|---|---|---|---|
| Language | Go + Python | Python→Rust (migrating) | TypeScript | Rust | Go | Lua/OpenResty + Go | Python/TS | Python/TS/Go/... |
| Agent-run-level cost attribution | **Yes, foundational** | No (call-level only) | No | No | No | No | Partial (tracing only) | Partial |
| Cache reuse gated on correctness, not just similarity | **Yes** | No | No (threshold only) | N/A | No (threshold only) | No (threshold only) | N/A | N/A |
| Adversarial multi-judge eval verification | **Yes, opt-in (`--llm-judge-panel`)** | N/A | N/A | Single judge | N/A | N/A | Single judge | Single judge |
| Self-hostable | Yes | Yes | Yes (core) | Yes | Yes | Core only | Yes | No (SaaS-primary) |
| Open source | Yes (Apache-2.0) | Yes + paid Enterprise | OSS core + paid | Yes, no paid tier | Yes | OSS core, AI plugins gated | Yes | No |

- Provenance: the competitor cells summarize a September 2026 desk survey of each project's public documentation; nothing in this repository verifies them, so check each project's current docs before relying on a cell.
- Cache row: Kelvran's near-duplicate layer is lexical (MinHash/Jaccard at or above 0.9), so it tolerates typos and single-word swaps rather than paraphrases, and every hit must also pass the entity, negation, freshness, model and policy-version gates.
- Multi-judge row: the panel is opt-in via `--llm-judge-panel`; the default scorer is deterministic exact/regex matching and `--llm-judge` is a single Bedrock Claude Haiku 4.5 judge (`evals/evals/cli.py`). The shipped panel is two same-vendor judges (Claude Sonnet 5 and Haiku 4.5, both via Bedrock), an accepted trade-off rather than the cross-vendor composition that best defends against correlated judge bias.

## Architecture at a glance

```
client / agent (OpenAI SDK; W3C traceparent + baggage agent_run_id) ──► POST /v1/chat/completions
  auth (virtual key, source-IP and model allowlists) ─► Idempotency-Key claim ─► prompt resolution
  ─► rate limit + budget reserve ─► cache lookup L1 ─► L2 ─► L3-lite      (hit ─► response, not billed, no upstream call)
  ─► guardrail pre-call ─► route (weighted / sticky pick, capability and region reroute)
  ─► per-hop deployment capacity gate ─► provider adapter ─► upstream call (+ error-classified fallback)
  ─► guardrail post-call ─► cache write-back ─► cost / OTel finalize (always runs) ──► response
POST /v1/embeddings: auth ─► rate limit ─► budget ─► guardrail ─► route ─► provider   (no cache, idempotency, fallback or GenAI span)
operator ──► Admin API (127.0.0.1:8081, bearer tiers) ──► live keys / weights ──► Redis pub/sub (when config_propagation.redis_addr is set) ──► other replicas
                                                      └──► prompts: this replica only (optional bbolt persistence via prompt.persist_path)
OTel spans + metrics ──► OTLP collector / Grafana        GatewayDecisionEvent log line ──► Vector ──► S3 / GCS ──► evals ingest
```

Full detail: [ARCHITECTURE.md](ARCHITECTURE.md), [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md) (Request Lifecycle), [evals/ARCHITECTURE.md](evals/ARCHITECTURE.md).

## Deploy

- **Docker**: `ghcr.io/kelvran/gateway` is `FROM scratch`, runs as UID 65532, listens on 8080 and has no shell; mount your `config.yaml` at `/config.yaml` and pass provider credentials as environment variables. Tags: `latest`, `sha-<commit>`, `vX.Y.Z`. Single-arch linux/amd64.
- **Local dev stack**: `cp gateway/config.example.yaml gateway/config.yaml`, `cp .env.example .env`, then `docker compose up gateway`. Opt-in profiles: `redis` (redis:7 with AOF), `multi-instance` (a second gateway), `vector-s3` (Vector log shipper), `observability` (grafana/otel-lgtm plus Alertmanager), `chaos` (Toxiproxy). All host ports bind to 127.0.0.1. Use `make config-safe` to inspect the resolved compose config without printing secrets.
- **Kubernetes**: a plain Kustomize base at `deploy/k8s/base` (namespace `kelvran`, 2 replicas pinned to the `v0.17.0` digest, ClusterIP Service, NetworkPolicy, PodDisruptionBudget, `/healthz` for both probes). Put a `config.yaml` next to `kustomization.yaml`, then `kubectl apply -k deploy/k8s/base/`; `overlays/eks-irsa` adds External Secrets Operator. Never applied to a live cluster yet; no Helm chart. See [deploy/k8s/README.md](deploy/k8s/README.md).
- **ECS/Fargate**: `deploy/ecs` is a Terraform module that registers one Fargate task definition and a CloudWatch log group; cluster, service, networking and load balancer are yours to provide, with an ALB health check on `/healthz`. See [deploy/ecs/README.md](deploy/ecs/README.md).
- **Redis for more than one replica**: set `budget.redis_addr`, `admin.redis_addr`, `rate_limit.redis_addr` and `config_propagation.redis_addr` (with its required `signing_secret_env`); each section accepts `redis_password_env`, `redis_username` and `redis_tls`. Per-key concurrency, deployment ceilings, the response cache, prompts and idempotency stay per replica. Procedures and the backup/restore runbook: [docs/operations/DEPLOY.md](docs/operations/DEPLOY.md).

## Security and supply chain

Every published image digest is cosign keyless-signed through GitHub Actions OIDC and carries a CycloneDX SBOM and a SLSA Build Level 2 provenance attestation, attached to the digest rather than a tag. The image is `FROM scratch` and non-root. Since 2026-10-08 on `main`, every `gateway/v*` GitHub Release also carries archives and deb/rpm/apk packages with a cosign-signed `checksums.txt`, per-archive SBOMs and a provenance attestation, built by `.github/workflows/release.yml` ([RELEASE.md](RELEASE.md) shows how to verify them); releases up to `gateway/v0.17.0` predate that and have no assets. Verify it yourself, exactly as [RELEASE.md](RELEASE.md) documents:

```bash
docker pull ghcr.io/kelvran/gateway:latest
DIGEST=$(docker inspect ghcr.io/kelvran/gateway:latest --format '{{index .RepoDigests 0}}')
cosign verify "$DIGEST" \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
cosign verify-attestation "$DIGEST" \
  --type cyclonedx \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
cosign verify-attestation "$DIGEST" \
  --type slsaprovenance1 \
  --certificate-identity-regexp "^https://github.com/kelvran/gateway/" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

- CI runs golangci-lint with gosec, govulncheck, `go test -race`, ruff, mypy, pip-audit and same-runner coverage gates on every push and pull request; Trivy scans each pushed image digest (report-only) and CodeQL analyzes Go and Python weekly and on every push.
- OpenSSF Scorecard runs weekly and publishes its results (badge above). All five workflows start from `permissions: {}` and every action is pinned to a full commit SHA; Dependabot tracks Go, pip, Actions and Docker weekly.
- Infrastructure code is checked with `terraform validate`, Checkov (soft-fail) and `kubeconform -strict`; the `api/` protobuf contract is guarded by `buf lint` and `buf breaking`.
- Threat model: [THREAT_MODEL.md](THREAT_MODEL.md) (STRIDE per component, OWASP LLM Top 10 and NIST AI 600-1 crosswalks); machine-readable posture: [SECURITY-INSIGHTS.yml](SECURITY-INSIGHTS.yml). Disclosure: report privately via [GitHub Security Advisories](https://github.com/kelvran/gateway/security/advisories/new) per [SECURITY.md](SECURITY.md); acknowledgement within 3 business days, a fix or mitigation plan within 14 days for Critical/High. Only the latest minor release of each deployable is supported; there is no bug bounty.

## Status and known limitations

- **Versions**: `gateway/v0.17.0` (2026-10-07) and `evals/v0.10.1` (2026-09-22); 20 gateway and 11 evals releases since 2026-09-03, versioned independently. Per-release history lives in [gateway/changelog/](gateway/changelog/) and [evals/changelog/](evals/changelog/); `gateway/changelog/unreleased.md` lists what has landed on `main` since that cut and is not yet released (the `-version` build identity, the JSON error envelope, and four fixes: OpenAI `tool_choice` forms, kind-aware health probes for embedding deployments, Bedrock structured output on Claude Sonnet 5, and a concurrent-admin-mutation ordering fix); `evals/changelog/unreleased.md` is empty even though a judge-prompt change landed after the `v0.10.1` cut (commit f90d9d28, 2026-09-25: structural-marker injection neutralization, `JUDGE_PROMPT_VERSION` v1 -> v2), so scores from a source checkout are stamped `v2` and are not directly comparable to `v0.10.1`'s `v1` scores.
- **Not built, by recorded decision**: MCP/A2A brokering ([PRD.md](PRD.md); design-only [outbound](docs/rfcs/2026-09-11-gateway-mcp-outbound-credential-design.md) and [inbound](docs/rfcs/2026-09-20-gateway-mcp-inbound-design.md) RFCs); an admin web UI ([decision](docs/upgrade-research/admin-dashboard-ui-2026-09-14.md)); a first-party SDK, the OpenAI SDK `base_url` drop-in is the integration path ([decision](docs/upgrade-research/client-sdk-strategy-2026-09-13.md)); an OpenAPI spec ([decision](docs/upgrade-research/developer-experience-repo-tooling-2026-09-13.md)); a Helm chart ([decision](docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md)); PyPI publication of `kelvran-evals` ([RELEASE.md](RELEASE.md), pending trademark clearance); embedding-based semantic L3 and a Redis-backed shared cache ([design RFC](docs/rfcs/2026-09-11-gateway-redis-backed-cache-design.md)).
- **Not yet built (no decision recorded against it)**: a multi-arch image; the `publish-image` job in `.github/workflows/ci.yml` sets no `platforms`, so the image is linux/amd64 only (arm64 hosts run it only under Rosetta/QEMU emulation; `--platform linux/amd64` just makes the choice explicit and silences Docker's platform-mismatch warning); the [2026-10-08 discoverability research](docs/upgrade-research/kelvran-deep-research-round4-discoverability-2026-10-08.md) recommends adding it.
- **Open defects from the 2026-10-07/08 live end-to-end verification**, all present in `v0.17.0`:
  - OpenAI's `tool_choice` forms (`"auto"`, `"none"`, `"required"`, `{"type":"function",...}`) are rejected with `400` or `502` in `v0.17.0`; fixed on `main` 2026-10-08 (all three forms accepted), ships in the next release.
  - With `health_probe` configured, `v0.17.0` sends the chat-shaped probe to `kind: embedding` deployments too, which reject it and are marked unhealthy, so `/readyz` returns `503` whenever the config holds an embedding deployment; fixed on `main` 2026-10-08 (an embedding deployment is probed with a one-string embedding through the same adapter real traffic uses), ships in the next release. Without `health_probe` (the default, commented out in `config.example.yaml`) `/readyz` was never affected, and the shipped Kubernetes manifests probe `/healthz` for both liveness and readiness.
  - With `rate_limit.redis_addr` set, a virtual key that has `tpm_capacity` admits one in-flight request at a time regardless of capacity; the in-memory limiter is unaffected.
  - In `v0.17.0`, Bedrock responses carry `"id": ""`, streaming chunks have empty `id` and `model`, and no response carries `object` or `created`; fixed on `main` 2026-10-08 (gateway-issued `chatcmpl-` ids, `object`/`created` on every response and frame, canonical `model` filled on chunks), ships in the next release.
  - `temperature` is forwarded verbatim, and Bedrock Claude Sonnet 5 rejects it; without a fallback chain the call fails with `502`.
- **Also worth knowing**: `-validate` never resolves environment variables, so a missing provider key surfaces only at startup as a warning and then as request failures; `docs/users/USER_GUIDE.md` predates multi-hop `fallback_chains` and persisted keys, so prefer `gateway/ARCHITECTURE.md` on those two topics.

## Documentation

- **Start here**: [docs/users/USER_GUIDE.md](docs/users/USER_GUIDE.md) (operator how-to), [gateway/config.example.yaml](gateway/config.example.yaml) (every config key, commented), [REPO_LAYOUT.md](REPO_LAYOUT.md).
- **Architecture and design**: [ARCHITECTURE.md](ARCHITECTURE.md), [gateway/ARCHITECTURE.md](gateway/ARCHITECTURE.md), [evals/ARCHITECTURE.md](evals/ARCHITECTURE.md), [api/README.md](api/README.md), [PRD.md](PRD.md), [DESIGN.md](DESIGN.md).
- **Decisions**: [DECISIONS.md](DECISIONS.md) (terse log), [docs/decisions/](docs/decisions/) (ADRs), [docs/rfcs/](docs/rfcs/) / [docs/plans/](docs/plans/) / [docs/research/RESEARCH.md](docs/research/RESEARCH.md) (proposal to plan to open questions).
- **Operations**: [DEPLOY.md](docs/operations/DEPLOY.md) (including backup and restore), [TELEMETRY.md](docs/operations/TELEMETRY.md), [DATA-SUBJECT-REQUESTS.md](docs/operations/DATA-SUBJECT-REQUESTS.md), [PROCUREMENT.md](docs/operations/PROCUREMENT.md), [PROVIDERS.md](docs/operations/PROVIDERS.md) (data-flow and residency inventory, not a configuration guide), [grafana/](docs/operations/grafana/) (dashboard, SLO rules, Alertmanager), Vector shipper configs for [S3](docs/operations/vector-gatewayevents-s3.yaml), [GCS](docs/operations/vector-gatewayevents-gcs.yaml) and [Compose](docs/operations/vector-gatewayevents-s3-compose.yaml), [deploy/k8s/README.md](deploy/k8s/README.md), [deploy/ecs/README.md](deploy/ecs/README.md).
- **Security**: [THREAT_MODEL.md](THREAT_MODEL.md), [SECURITY.md](SECURITY.md), [SECURITY-INSIGHTS.yml](SECURITY-INSIGHTS.yml).
- **Releases**: [gateway/changelog/](gateway/changelog/), [evals/changelog/](evals/changelog/), [RELEASE.md](RELEASE.md) (mechanics and image verification), [RELEASE_NOTES.md](RELEASE_NOTES.md) and [UPGRADE.md](UPGRADE.md) (cover v0.1.0 only), [DEPRECATED.md](DEPRECATED.md), [STATUS.md](STATUS.md) (project-status snapshot; its Status and Current Version sections are current, the narrative from its Current Phase section onward stops at gateway/v0.9.0 + evals/v0.8.0, 2026-09-11 — for everything since read gateway/changelog/, evals/changelog/ and the tail of docs/agents/LOGS.md).
- **Testing and development**: [docs/testing/TESTING.md](docs/testing/TESTING.md), [docs/development/BRANCHES.md](docs/development/BRANCHES.md), [scripts/README.md](scripts/README.md).
- **For AI coding agents**: [AGENTS.md](AGENTS.md), [CLAUDE.md](CLAUDE.md), [docs/agents/ETHOS.md](docs/agents/ETHOS.md), [docs/agents/AGENTS_LEARNING.md](docs/agents/AGENTS_LEARNING.md), [docs/agents/LOGS.md](docs/agents/LOGS.md).

## Contributing, support, license

- [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md): branch from `main` as `feat/...` or `fix/...`; `cd gateway && go build ./... && go test ./...` and `cd evals && uv sync && uv run pytest` before opening a pull request.
- [SUPPORT.md](SUPPORT.md): there is no chat channel yet; GitHub issues are the support channel, best-effort with no SLA. Security reports go through [SECURITY.md](SECURITY.md), never a public issue.
- License: [Apache-2.0](LICENSE); third-party attributions in [NOTICE](NOTICE).
