# Telemetry & Observability

Operator companion to the OTel commitment already made in `gateway/ARCHITECTURE.md` and `evals/ARCHITECTURE.md`. For `gateway`, this is now real, per `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md` — every `gateway` request (buffered or streaming) emits a real span. **Corrected 2026-09-05**: `evals` tracing is also now real, per `docs/rfcs/2026-09-04-evals-trace-span-model.md` — `evals/evals/tracing.py` wraps every `run_in_sandbox()` call in a self-contained `Span` (no gateway/cross-service dependency, no OTel Collector transport — a deliberate, self-instrumenting design, not the standardized wire-format `gen_ai.*` spans `gateway` emits), persisted via `rollout --traces` and readable via `report --traces`.

## What Is Emitted

| Signal | Component | OTel namespace | Status |
|---|---|---|---|
| Request/response spans | Gateway | Standard `gen_ai.*` semantic conventions (`operation.name`, `provider.name`, `request.model`, `response.model`/`id`/`finish_reasons`, `usage.{input,output}_tokens`) — `provider.name` is remapped to the real registry's well-known values where Kelvran's own internal provider identifier differs (`bedrock`→`aws.bedrock`, `gemini`→`gcp.gemini`); `openai`/`anthropic`/`openaicompat` pass through verbatim | **Real**, per `docs/rfcs/2026-09-05-gateway-gen-ai-provider-name-validation.md` |
| Agent-run cost attribution | Gateway | `kelvran.agent_run_id` via W3C Baggage (`baggage: agent_run_id=<value>` header), plus `kelvran.virtual_key.id`/`kelvran.cost.usd` | **Real** |
| Cache hit/miss + provenance | Gateway | `kelvran.cache.hit` (bool), `kelvran.cache.layer` ("L1"/"L2"/"L3", set only on a hit), `kelvran.cache.similarity`/`kelvran.cache.age_ms` (L3 hits only — real Jaccard estimate + age captured at write time) — none are standardized `gen_ai.*` attributes | **Real**, per `docs/rfcs/2026-09-05-gateway-cache-hit-provenance.md`. L1/L2 report their layer but not an age — neither currently captures a write-time timestamp, a named future extension |
| Rate-limiter fail-open (metric) | Gateway | `kelvran.ratelimit.fail_open` — an OTel **Metrics** counter (not a span attribute), attributed with `kelvran.virtual_key.id`, incremented every time `checkRateLimit`'s Redis backend errors and the request is allowed through fail-open | **Real**, per `docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md` |
| Cross-instance cache-check correlation (log) | Gateway | `cache_cross_instance_check` structured log line (NOT a metric — the key is an unbounded-cardinality SHA-256 hash), emitted at every real L1/L2/L3 cache check with `tenant_id`/`cache_key`/`cache_layer`/`instance_id`/`hit`/`ttl_ms` — the raw event stream `internal/telemetry/cachecorrelation.Analyze` needs to retroactively estimate cross-instance duplicate work, once a real multi-instance deployment exists | **Real (emission + a standalone, tested correlation function)**, per `docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md` and `docs/upgrade-research/cache-2026-09-06.md` Finding 5. Deliberately NOT wired to a live correlator — that analysis pass, and any dashboard built on it, is future work gated on real multi-instance traffic existing |
| Instance identity | Gateway | `service.instance.id` OTel Resource attribute (every span/metric) plus `kelvran.instance.id` on `kelvran.cache.l3.gate_outcome`'s data points — `telemetry.InstanceID`, computed once at process start as `hostname:pid` | **Real**, per `docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md` — the first instance identifier anywhere in this codebase |
| Cache savings (metric) | Gateway | `kelvran.cache.savings_usd` — an OTel **Metrics** Float64Counter (not a span attribute), dimensioned by `kelvran.cache.layer`, incremented by the notional cost of every cache-hit request (the same figure `kelvran.cost.usd` already reports per-request) | **Real**, per `docs/rfcs/2026-09-10-gateway-cache-savings-metric.md` |
| Sandbox-execution spans | Evals | Real OTel-SDK-generated `span_id`/`trace_id` (`evals.tracing`, no OTLP exporter wired — SDK used purely as a correct ID/timestamp/status generator, not a live export pipeline), joined to `Run.id`; `process.exit_code`/`container.id` conventions — deliberately **not** `gen_ai.*` (confirmed against the semantic-conventions registry to be LLM/model-inference-only, not applicable to a container-sandbox execution) | **Real**, per `docs/rfcs/2026-09-04-evals-trace-span-model.md`. No `Trace` wrapper yet — today's harness makes exactly one sandbox call per `Run`; a multi-step harness is the named trigger for introducing one |

Standard `gen_ai.*` attributes are consumed by any generic OTel-aware backend; Kelvran-custom attributes require Kelvran-aware dashboards/queries to be meaningful — that distinction matters when picking a backend.

## Supported Exporters

`gateway` supports three exporters via its `telemetry:` config section (`docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`): `stdout` (the default — spans printed locally, nothing shipped anywhere), `otlp` (any OTLP-compatible collector/backend, via `otlp_endpoint`), and `none` (tracing fully disabled). The same `Exporter`/`OTLPEndpoint` setting now also drives the separate OTel Metrics pipeline added per `docs/rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md` — one config knob, two signal types, sharing the same exporter-kind decision. `evals` has no exporter wiring yet. Validated backends beyond "a real OTLP collector accepts the spans" will be listed here once actually tested against a running system with real production-shaped traffic.

## Key SLIs/SLOs

- **Gateway**: provider-call latency percentiles (p50/p95/p99) per provider, error rate per provider, rate-limit rejection rate.
- **Cache**: hit rate per layer (L1/L2/L3), eviction rate, semantic-hit false-positive rate (this last one requires the correctness-tracking discipline `PRD.md`'s Success Metrics section already commits to — hit rate alone is explicitly called out there as a vanity metric without it).
- **Evals**: cost per eval run, run duration, judge-score drift over time (a rising or falling trend independent of model/prompt changes is itself a signal worth alerting on).
- **Cross-cutting**: end-to-end latency budget from client request to response, broken down by pipeline stage (per `gateway/ARCHITECTURE.md`'s Request Lifecycle).

None of these have concrete target numbers yet — those get set once there's real production traffic to baseline against, not guessed at now.

## Dashboards & Example Queries

*(Shape only, not full JSON — no backend has been chosen yet.)* A cost dashboard is the one explicitly called out as needed from day one: total spend broken down by agent_run_id, team, and provider, since "why did this cost $X" is the exact gap `README.md`'s "Why Kelvran" section names as a competitive differentiator — if the dashboard can't answer that question, the feature isn't actually delivered yet, regardless of what the code does. As of `docs/rfcs/2026-09-10-gateway-cache-savings-metric.md`, `kelvran.cache.savings_usd` (a Float64Counter, dimensioned by `kelvran.cache.layer`) is a real, already-aggregatable exported counter an operator's own Prometheus/Grafana can sum/graph by layer directly, without writing a raw-span aggregation query themselves.

## Alerting Guidance

Starting-point thresholds only, explicitly **not** an SLA (see `SECURITY.md`'s acknowledgement/resolution targets, which are similarly marked aspirational pre-release): alert on error-rate spikes per provider, alert on cache correctness metrics degrading (not just hit rate dropping), alert on eval judge-score drift crossing a threshold without a corresponding model/prompt change logged in `DECISIONS.md`.

## Privacy & Redaction

Prompt/completion content in trace events is opt-in, not default-on — this is a deliberate privacy stance, not an oversight, and follows directly from `docs/operations/PROVIDERS.md`'s data-flow inventory and `THREAT_MODEL.md`'s Information Disclosure rows for both Gateway and Evals. This document doesn't restate the threat analysis — see `THREAT_MODEL.md` and `SECURITY.md` for that. The cross-instance cache-check log line's `cache_key` field is always a SHA-256 hex digest (`internal/cache.Key`/`NormalizedKey`'s own output) — never raw prompt content — so it carries no additional exposure beyond what the existing cache-hit-provenance attributes already do.

## Local Debugging

For local development, the console exporter (print spans to stdout instead of shipping to a backend) is the real default (`telemetry.exporter: "stdout"`, or simply omitting the `telemetry:` section entirely) — complements `docs/operations/DEPLOY.md`'s Compose section, where standing up a full observability backend for local dev would be overkill.
