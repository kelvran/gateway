# Telemetry & Observability

Operator companion to the OTel commitment already made in `gateway/ARCHITECTURE.md` and `evals/ARCHITECTURE.md`. For `gateway`, this is now real, per `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md` — every `gateway` request (buffered or streaming) emits a real span. `evals` tracing remains aspirational.

## What Is Emitted

| Signal | Component | OTel namespace | Status |
|---|---|---|---|
| Request/response spans | Gateway | Standard `gen_ai.*` semantic conventions (`operation.name`, `provider.name`, `request.model`, `response.model`/`id`/`finish_reasons`, `usage.{input,output}_tokens`) | **Real** |
| Agent-run cost attribution | Gateway | `kelvran.agent_run_id` via W3C Baggage (`baggage: agent_run_id=<value>` header), plus `kelvran.virtual_key.id`/`kelvran.cost.usd` | **Real** |
| Cache hit/miss | Gateway | `kelvran.cache.hit` (bool) — not a standardized `gen_ai.*` attribute anywhere upstream | **Real** (hit/miss only; no per-layer breakdown yet — L2/L3 don't exist) |
| Rollout/judge spans | Evals | Standard `gen_ai.*` plus Kelvran-custom `harness_config` fields (per `evals/ARCHITECTURE.md`'s Data Model) | Not built |

Standard `gen_ai.*` attributes are consumed by any generic OTel-aware backend; Kelvran-custom attributes require Kelvran-aware dashboards/queries to be meaningful — that distinction matters when picking a backend.

## Supported Exporters

`gateway` supports three exporters via its `telemetry:` config section (`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`): `stdout` (the default — spans printed locally, nothing shipped anywhere), `otlp` (any OTLP-compatible collector/backend, via `otlp_endpoint`), and `none` (tracing fully disabled). `evals` has no exporter wiring yet. Validated backends beyond "a real OTLP collector accepts the spans" will be listed here once actually tested against a running system with real production-shaped traffic.

## Key SLIs/SLOs

- **Gateway**: provider-call latency percentiles (p50/p95/p99) per provider, error rate per provider, rate-limit rejection rate.
- **Cache**: hit rate per layer (L1/L2/L3), eviction rate, semantic-hit false-positive rate (this last one requires the correctness-tracking discipline `PRD.md`'s Success Metrics section already commits to — hit rate alone is explicitly called out there as a vanity metric without it).
- **Evals**: cost per eval run, run duration, judge-score drift over time (a rising or falling trend independent of model/prompt changes is itself a signal worth alerting on).
- **Cross-cutting**: end-to-end latency budget from client request to response, broken down by pipeline stage (per `gateway/ARCHITECTURE.md`'s Request Lifecycle).

None of these have concrete target numbers yet — those get set once there's real production traffic to baseline against, not guessed at now.

## Dashboards & Example Queries

*(Shape only, not full JSON — no backend has been chosen yet.)* A cost dashboard is the one explicitly called out as needed from day one: total spend broken down by agent_run_id, team, and provider, since "why did this cost $X" is the exact gap `README.md`'s "Why Kelvran" section names as a competitive differentiator — if the dashboard can't answer that question, the feature isn't actually delivered yet, regardless of what the code does.

## Alerting Guidance

Starting-point thresholds only, explicitly **not** an SLA (see `SECURITY.md`'s acknowledgement/resolution targets, which are similarly marked aspirational pre-release): alert on error-rate spikes per provider, alert on cache correctness metrics degrading (not just hit rate dropping), alert on eval judge-score drift crossing a threshold without a corresponding model/prompt change logged in `DECISIONS.md`.

## Privacy & Redaction

Prompt/completion content in trace events is opt-in, not default-on — this is a deliberate privacy stance, not an oversight, and follows directly from `docs/operations/PROVIDERS.md`'s data-flow inventory and `THREAT_MODEL.md`'s Information Disclosure rows for both Gateway and Evals. This document doesn't restate the threat analysis — see `THREAT_MODEL.md` and `SECURITY.md` for that.

## Local Debugging

For local development, the console exporter (print spans to stdout instead of shipping to a backend) is the real default (`telemetry.exporter: "stdout"`, or simply omitting the `telemetry:` section entirely) — complements `docs/operations/DEPLOY.md`'s Compose section, where standing up a full observability backend for local dev would be overkill.
