# Metrics and logs reference

This page lists every OpenTelemetry metric instrument, span, span attribute and span event the gateway emits, and every structured log event it writes, with the fields on each. It is for operators building dashboards, alerts and log pipelines, and for anyone who needs to know exactly what a signal means. It is a lookup table, not a walkthrough; the how-to pages linked at the end cover shipping telemetry to a collector.

## Scope and stability

- Metric names, metric attributes and span attributes are public surface under [`docs/VERSIONING.md`](../VERSIONING.md). Log message text and log field names are not: they are best effort and may change in any release. The log sections of this page are informational.
- Everything on this page is in gateway/v0.17.0 unless a row says "on main since 2026-10-08, not in gateway/v0.17.0".
- The code is the source of truth: `gateway/internal/telemetry/telemetry.go` (instruments and the export pipeline), `gateway/internal/telemetry/result.go` (span attributes), `gateway/internal/gateway/dataplane/dataplane.go` (request log lines), `api/gatewayevents/v1/gatewayevents.proto` (the `gatewayevents_v1` log field).
- This page covers the gateway only. The evals deployable's signals are not documented here.

## Export pipeline

### Configuration

| Key | Type | Default | Meaning |
|---|---|---|---|
| `telemetry.exporter` | string | `"stdout"` (applied when the key is absent or empty) | One of `stdout`, `otlp`, `none`. Any other value makes telemetry initialisation return an error and the gateway does not start. |
| `telemetry.otlp_endpoint` | string | none | `host:port` of an OTLP/HTTP collector. Read only when `exporter` is `otlp`. The scheme defaults to HTTPS; set `OTEL_EXPORTER_OTLP_INSECURE=true` in the environment for a plain-HTTP collector. |

```yaml
telemetry:
  exporter: "otlp"
  otlp_endpoint: "otel-collector:4318"   # host:port; HTTPS unless OTEL_EXPORTER_OTLP_INSECURE=true
```

The annotated example is [`gateway/config.example.yaml`](../../gateway/config.example.yaml); the key-by-key reference is [`config.md`](config.md).

### Exporters

| `exporter` | Traces | Metrics |
|---|---|---|
| `stdout` | OTel stdout trace exporter, written to standard output | OTel stdout metric exporter, written to standard output |
| `otlp` | OTLP/HTTP to `otlp_endpoint`, through a batch span processor | OTLP/HTTP to `otlp_endpoint`, through a periodic reader that exports every 60 s (the SDK default; the gateway sets no interval) |
| `none` | No tracer provider is installed; spans are discarded | No meter provider is installed; measurements are discarded |

Collector outages never affect a request. The T1 row of [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md) describes what the SDK does and logs when the collector is unreachable.

### Resource attributes

Every exported span and metric data point carries these resource attributes.

| Attribute | Value |
|---|---|
| `service.name` | `kelvran-gateway` |
| `service.instance.id` | `<hostname>:<pid>`, computed once at process start (`unknown-host:<pid>` when the hostname lookup fails). The same value is `kelvran.instance.id` on the metric data points listed below, and `instance_id` in the `gateway_starting` and `cache_cross_instance_check` log lines. |

The dataplane's tracer and meter share one instrumentation-scope name: `github.com/kelvran/gateway/gateway/internal/gateway/dataplane`.

### Context propagation

- A composite W3C TraceContext + Baggage propagator is installed unconditionally, including under `exporter: none`.
- An incoming `traceparent` header makes the caller's trace the parent of the gateway's spans.
- The Baggage member `agent_run_id` becomes the `kelvran.agent_run_id` span attribute and the `agent_run_id` field of the `GatewayDecisionEvent`. It is empty when absent; it is never fabricated.

### Shutdown

On exit the gateway flushes both providers. A failed final metrics flush logs `telemetry_metrics_shutdown_flush_failed` (Warn) and is non-fatal. On main since 2026-10-08, not in gateway/v0.17.0: the flush is bounded to 5 s, and a failed trace-provider shutdown logs `telemetry_shutdown_failed` (Warn). In gateway/v0.17.0 the flush has no deadline and a trace-provider shutdown error is discarded silently.

## Metrics

All instruments are registered once at package init. Every attribute below is a closed set, except `kelvran.virtual_key.id` (one series per virtual key, bounded by the operator's own key count), `kelvran.instance.id` (one series per running process), and `gen_ai.request.model` / `gen_ai.response.model` (one series per configured model name and per provider-reported model name; `gen_ai.request.model` is sentinelled to `unresolved` when no deployment was resolved) — and, on `kelvran.fallback.rescued` only, `kelvran.fallback.from` / `kelvran.deployment.name` (one series per configured deployment name).

### Kelvran instruments

| Name | Instrument, unit | Attributes | Recorded when |
|---|---|---|---|
| `kelvran.ratelimit.fail_open` | Int64 counter, `{request}` | `kelvran.virtual_key.id` | A request is allowed through after the rate-limiter backend errored: the per-key check (`ratelimit_backend_unavailable`, `ratelimit_tpm_backend_unavailable`), the embeddings check (`embeddings_ratelimit_backend_unavailable`), and, on main since 2026-10-08 and not in gateway/v0.17.0, once per stream for the mid-stream TPM top-up (`ratelimit_tpm_backend_unavailable` with `op=mid_stream_topup`). The fallback-hop admission check (`ratelimit_backend_unavailable_fallback_hop`) logs without counting. |
| `kelvran.budget.fail_open` | Int64 counter, `{request}` | `kelvran.virtual_key.id` | A request is allowed through after the Redis-mode budget tracker errored. Paired with the `budget_backend_unavailable` log line. On main since 2026-10-08, not in gateway/v0.17.0, the mid-stream budget top-up also counts once per stream (`budget_backend_unavailable` with `op=mid_stream_topup`). |
| `kelvran.guardrail.fail_open` | Int64 counter, `{request}` | `kelvran.virtual_key.id`, `kelvran.guardrail.stage` | A guardrail detector errored and the request kept flowing with that detector's coverage missing. At most once per request per stage. Paired with `guardrail_fail_open`. |
| `kelvran.attribution.dropped` | Int64 counter, `{header}` | `kelvran.attribution.header` | The attribution middleware refused a request header: an identifier (`x-claude-code-session-id`, `-agent-id`, `-parent-agent-id`, `-prompt-id`) longer than 128 bytes or outside `[A-Za-z0-9._:-]`, an `x-claude-code-agent-type` over 64 bytes or not printable ASCII, or an `x-claude-code-prev-tool-durations` value from which no entry could be kept (well-formed: printable ASCII, a percent-decodable name, a duration of at most 86,400,000 ms; nothing is kept either when the first well-formed entry alone exceeds 4 KB). The attribute is the header name, never the value. On `main` since 2026-10-09. |
| `kelvran.fallback.rescued` | Int64 counter, `{request}` | `kelvran.virtual_key.id`, `kelvran.fallback.from`, `kelvran.deployment.name`, `kelvran.fallback.hop.error_class` | A fallback happened and the request produced a billed response (`err == nil || billable` in `finalize`): the first deployment failed and a fallback produced the response — including a streamed rescue that failed after its first byte, a rescue the post-call guardrail then blocked, and a coalesced singleflight follower of a rescued leader (it shares the leader's fallback record, as its decision event always has). `kelvran.fallback.from` is the deployment first tried, `kelvran.deployment.name` the one that served, and the class is the first failure's (see the attribute table). On `main` since 2026-10-09, not in gateway/v0.17.0. |
| `kelvran.streaming.cost_estimated` | Int64 counter, `{response}` | `kelvran.virtual_key.id` | A streamed response was billed from an estimated token count because the provider sent no terminal usage frame. |
| `kelvran.streaming.near_duplicate_collision` | Int64 counter, `{collision}` | `kelvran.virtual_key.id` | A streaming request found another streaming request for the identical exact-match cache key already in flight. Observation only; nothing is coalesced or blocked. Paired with `streaming_near_duplicate_collision`. |
| `kelvran.cache.l3.gate_outcome` | Int64 counter, `{check}` | `kelvran.cache.l3.gate`, `kelvran.cache.l3.outcome`, `kelvran.instance.id` | Each decision of the four named gates in the L3-lite (lexical) cache check: `volatile_bypass` once per check, the other three once per candidate. The L3 equality gates on guardrail policy version, response format, prompt, reasoning-blocks, thinking-binding-mode and tools fingerprints are not counted. |
| `kelvran.cache.savings_usd` | Float64 counter, `{USD}` | `kelvran.cache.layer` | Every cache hit, incremented by the request's notional cost (what the call would have cost upstream). Never recorded on a miss. |
| `kelvran.cache.lookup` | Int64 counter, `{lookup}` | `kelvran.cache.lookup_outcome`, `kelvran.instance.id`, `kelvran.cache.layer` (hit only) | Every finalized chat request that reached the cache check. A request rejected before the cache check (auth failure, model not allowed, rate limit, concurrency cap, budget exceeded, prompt-resolve failure) is not counted as a miss. |
| `kelvran.llm.spend_usd` | Float64 counter, `{USD}` | `kelvran.virtual_key.id`, `kelvran.client.tool`, `kelvran.claude_code.request_class` (on `main` since 2026-10-09; before, no attributes) | Real USD cost of upstream calls this process paid for: every billable chat request (not a cache hit, not a singleflight follower) and every successful embeddings request. |
| `kelvran.budget.threshold_crossed` | Int64 counter, `{crossing}` | `kelvran.virtual_key.id`, `kelvran.budget.percent_bucket` | A virtual key newly crosses a bucket of the fixed 50/75/90/100 % budget ladder; once per key per rolling-window epoch per bucket. Paired with `budget_threshold_crossed`. The per-key configurable warn threshold (`budget_warn_threshold_crossed`) is a log line only. |
| `kelvran.persistence.failed` | Int64 counter, `{write}` | `kelvran.persistence.store_kind`, `kelvran.virtual_key.id` | A durable-store write failed: a bbolt budget or identity write (paired with `budget_persist_failed`, `identity_persist_failed`) and, on main since 2026-10-08 and not in gateway/v0.17.0, the Redis-mode budget reconcile (paired with `budget_redis_backend_unavailable`, `op=reconcile`). |
| `kelvran.configpropagation.subscribe_stopped` | Int64 counter, `{event}` | `kelvran.instance.id` | The config-propagation Redis subscribe loop returned an error other than context cancellation. Paired with `configpropagation_subscribe_stopped`. |
| `kelvran.configpropagation.publish_failed` | Int64 counter, `{event}` | `kelvran.instance.id`, `kelvran.configpropagation.event_type` | A virtual-key upsert, virtual-key delete or deployment-weight change applied on this replica but failed to publish to the others. Paired with `configpropagation_publish_failed`. On main since 2026-10-08, not in gateway/v0.17.0. |

### OpenTelemetry GenAI instruments

These are recorded once per chat completion (buffered and streaming) at the end of the request. They are not recorded for embeddings.

| Name | Instrument, unit | Attributes | Recorded when |
|---|---|---|---|
| `gen_ai.client.operation.duration` | Float64 histogram, `s` | `gen_ai.operation.name` = `chat`, `gen_ai.request.model`, `gen_ai.provider.name` (when a deployment was resolved), `gen_ai.response.model` (when a response was produced), `error.type` (failures only), `kelvran.fallback.outcome` (`none` / `rescued` / `exhausted`, on every data point; on `main` since 2026-10-09), `kelvran.client.tool` and `kelvran.claude_code.request_class` (on every data point; on `main` since 2026-10-09) | Every finished chat request that reached the pipeline, success or rejection, measured from the pipeline's `HandleChatCompletion`/`HandleChatCompletionStream` entry (after the HTTP handler has read, decoded and validated the body; a 400 for a malformed body is not recorded) to the end of the request. |
| `gen_ai.client.inference.usage.input_tokens` | Int64 counter, `{token}` | the duration attributes plus `gen_ai.token.modality` | Billable request with input tokens > 0. |
| `gen_ai.client.inference.usage.output_tokens` | Int64 counter, `{token}` | same | Billable request with output tokens > 0. |
| `gen_ai.client.inference.usage.cache_read.input_tokens` | Int64 counter, `{token}` | same | Billable request with provider cache-read tokens > 0. |
| `gen_ai.client.inference.usage.cache_write.input_tokens` | Int64 counter, `{token}` | same | Billable request with provider cache-write tokens > 0. |
| `gen_ai.client.inference.usage.reasoning.output_tokens` | Int64 counter, `{token}` | same | Billable request with reasoning tokens > 0. A subset of output tokens. A provider that reports no breakdown produces no series. |
| `gen_ai.client.inference.operation.input_tokens` | Float64 histogram, `{token}`, explicit buckets | the duration attributes (no modality) | Billable request with input tokens > 0. |
| `gen_ai.client.inference.operation.output_tokens` | Float64 histogram, `{token}`, explicit buckets | the duration attributes (no modality) | Billable request with output tokens > 0. |

Billable gate: a cache hit (any layer) and a coalesced singleflight follower are not billable, so none of the seven token instruments count them. `gen_ai.client.operation.duration` is the only GenAI instrument that records every request.

Explicit bucket boundaries for the two token histograms: 1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864.

### Attribute values

| Attribute | Values | Notes |
|---|---|---|
| `error.type` | `auth_failed`, `model_not_allowed`, `rate_limited`, `budget_exceeded`, `no_deployment`, `upstream_error`, `guardrail_blocked`, `deployment_capacity`, `invalid_request` | The lowercase `GatewayDecisionEvent.Outcome` name without its `OUTCOME_` prefix. Absent on success. Some local rejections are classified `upstream_error`; see the closing notes of [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md). |
| `gen_ai.provider.name` | `openai`, `anthropic`, `aws.bedrock`, `gcp.gemini`, `openaicompat` | The deployment's `provider` remapped to the OTel well-known value: `bedrock` becomes `aws.bedrock`, `gemini` becomes `gcp.gemini`; the others pass through verbatim. |
| `gen_ai.request.model` | the requested model name, or `unresolved` | `unresolved` whenever no deployment was resolved for the request: every rejection before routing (so an unauthenticated caller cannot mint series) and every cache hit, which never touches a deployment. `gen_ai.provider.name` is absent on those same data points; `gen_ai.response.model` is still set on a hit. Metric attribute only; the span carries the model in its name. |
| `gen_ai.response.model` | the model name the provider reported | Absent when no response was produced. |
| `gen_ai.token.modality` | `text`, `unknown` | `unknown` when any message in the request carries multimodal content parts. |
| `kelvran.guardrail.stage` | `precall`, `postcall`, `embeddings` | |
| `kelvran.cache.l3.gate` | `volatile_bypass`, `entity_mismatch`, `freshness_risk_model`, `negation_mismatch` | |
| `kelvran.cache.l3.outcome` | `pass`, `reject` | |
| `kelvran.cache.layer` | `L1`, `L2`, `L3` | |
| `kelvran.cache.lookup_outcome` | `hit`, `miss` | |
| `kelvran.budget.percent_bucket` | `0.5`, `0.75`, `0.9`, `1.0` | float64 |
| `kelvran.persistence.store_kind` | `budget`, `identity` | |
| `kelvran.configpropagation.event_type` | `virtual_key_upsert`, `virtual_key_delete`, `deployment_weight` | On main since 2026-10-08, not in gateway/v0.17.0 (only on `kelvran.configpropagation.publish_failed`). |
| `kelvran.fallback.hop.error_class` | `content_policy`, `context_window_exceeded`, `generic` | On the `fallback_hop` span event: the class of a FAILED hop. On `kelvran.fallback.rescued` (main since 2026-10-09): the class of the FIRST deployment's failure, which is never a hop. One key, two documented meanings. |
| `kelvran.fallback.outcome` | `none`, `rescued`, `exhausted` | On `gen_ai.client.operation.duration` only, every chat data point. `rescued` = a fallback happened and the request is billed; `exhausted` = a fallback happened and nothing was billed. On `main` since 2026-10-09. |
| `kelvran.fallback.from` | a deployment name | On `kelvran.fallback.rescued` only: the deployment first tried and abandoned. |
| `kelvran.deployment.name` | a deployment name | On `kelvran.fallback.rescued` (the deployment that served) — its only use on a metric; also a request-span attribute and, on the `fallback_hop` span event, the hop's target. |
| `kelvran.client.tool` | `claude_code`, `openai_python`, `openai_node`, `openai_go`, `anthropic_python`, `anthropic_node`, `anthropic_go`, `codex`, `aider`, `continue`, `litellm`, `curl`, `other` | Normalised from the `User-Agent` product (a leading `Async`, then `Azure`, stripped) and, for the Stainless SDKs, the trailer language; `other` when absent or unknown. Verified wire values: `OpenAI/Python 3.14.1` and the Async/Azure/Bedrock/Vertex/AWS/GoogleCloud/BedrockMantle client classes of openai 3.14.1 / anthropic 1.6.0; `claude-cli/2.1.295 (external, sdk-cli)` and `claude-code/2.1.295` from Claude Code 2.1.295. On `main` since 2026-10-09. |
| `kelvran.claude_code.request_class` | `main`, `subagent`, `workflow`, `compaction`, `auxiliary`, `other`, `none` | The five page values pass through; `other` for an unknown value; `none` when the header was absent (the default behind a custom base URL without `CLAUDE_CODE_GATEWAY_HINT_HEADERS=1`). On `main` since 2026-10-09. |
| `kelvran.attribution.header` | `x-claude-code-session-id`, `x-claude-code-agent-id`, `x-claude-code-parent-agent-id`, `x-claude-code-prompt-id`, `x-claude-code-agent-type`, `x-claude-code-prev-tool-durations` | On `kelvran.attribution.dropped` only. |
| `kelvran.instance.id` | `<hostname>:<pid>` | Same value as the `service.instance.id` resource attribute. |
| `kelvran.virtual_key.id` | a virtual key id | |

### Prometheus names

A collector exporting to Prometheus translates dots to underscores, appends `_total` to counters and the unit to histograms: `kelvran.cache.lookup` is `kelvran_cache_lookup_total`, `kelvran.llm.spend_usd` is `kelvran_llm_spend_usd_total`, `gen_ai.client.operation.duration` has `gen_ai_client_operation_duration_seconds_count`. Attribute names translate the same way. Cache hit rate by layer:

```promql
sum by (kelvran_cache_layer) (rate(kelvran_cache_lookup_total{kelvran_cache_lookup_outcome="hit"}[5m]))
  / ignoring(kelvran_cache_layer) group_left sum(rate(kelvran_cache_lookup_total[5m]))
```

The provisioned Grafana dashboard `docs/operations/grafana/dashboards/kelvran-overview.json` and the Prometheus SLO rules `docs/operations/grafana/prometheus/kelvran-slo-rules.yml` query `gen_ai.client.operation.duration` by name; [`docs/operations/TELEMETRY.md`](../operations/TELEMETRY.md) describes the dashboard, and [`../how-to/deploy/docker-compose.md`](../how-to/deploy/docker-compose.md) shows how both files are mounted into the `observability` profile.

## Spans

### Span inventory

| Span name | Source | Covers |
|---|---|---|
| `{METHOD} {route}` (for example `POST /v1/chat/completions`; a request matching no registered route keeps the method-only name, such as `HEAD` for Claude Code's `/api/hello` probe; `gateway.http` is the operation name Kelvran passes to `otelhttp.NewHandler`, which otelhttp's default formatter does not use as the span name) | otelhttp server middleware on the data-plane listener | Every request to the data-plane listener: `/v1/chat/completions`, `/v1/embeddings`, `/healthz`, `/readyz`, and `/v1/models` (on main since 2026-10-08, not in gateway/v0.17.0). The admin listener is not wrapped. |
| `chat <model>` | the dataplane tracer | One chat completion, buffered or streaming. `<model>` is the requested model name truncated to 256 bytes. |

The embeddings route starts no dataplane span of its own; its only span is the otelhttp server span (operation `gateway.http`).

### Attributes on `chat <model>`

Attributes are set at the end of the request. A value that is not known is omitted, never written as an empty or zero placeholder, except for the three rows marked "always".

| Attribute | Type | Set when |
|---|---|---|
| `kelvran.cache.hit` | bool | Always. |
| `kelvran.cost.usd` | string (decimal) | Always. `"0"` on a failure. A string, not a float: TraceQL metrics functions cannot aggregate it directly; convert client-side. |
| `gen_ai.request.stream` | bool | Always. `true` for the streaming handler. |
| `kelvran.virtual_key.id` | string | A virtual key was resolved (absent on an auth failure). |
| `gen_ai.provider.name` | string | A deployment is known; remapped as in the attribute table above. |
| `kelvran.deployment.name` | string | A deployment is known: on success the deployment that served the request; on a failure the last deployment attempted (a fallback target that itself failed included). |
| `gen_ai.response.model` | string | A response was produced. |
| `gen_ai.response.id` | string | The response carries an id. |
| `gen_ai.response.finish_reasons` | string array | At least one choice has a non-empty finish reason. |
| `gen_ai.usage.input_tokens` | int | > 0. Reported on cache hits too; informational, not billing. |
| `gen_ai.usage.output_tokens` | int | > 0. |
| `gen_ai.usage.cache_read.input_tokens` | int | > 0. |
| `gen_ai.usage.cache_write.input_tokens` | int | > 0. |
| `gen_ai.usage.reasoning.output_tokens` | int | > 0. |
| `kelvran.agent_run_id` | string | The `agent_run_id` Baggage member was present on the request. |
| `kelvran.client.tool` | string | The request passed the attribution middleware (every request through the data server); the normalised `User-Agent` product, `other` when unknown. On `main` since 2026-10-09. |
| `kelvran.claude_code.session_id`, `kelvran.claude_code.agent_id`, `kelvran.claude_code.parent_agent_id`, `kelvran.claude_code.prompt_id` | string | The corresponding `x-claude-code-*` header was present and passed the identifier grammar, the request authenticated (an unauthenticated request never carries identifiers), and identifier capture is on for the gateway and the key. Span attributes only — never a metric label or log field. |
| `kelvran.claude_code.request_class`, `kelvran.claude_code.compaction`, `kelvran.claude_code.context_compacted` | string | The corresponding hint header was present (bounded vocabularies; `other` for an unknown value). |
| `kelvran.claude_code.agent_type` | string | `x-claude-code-agent-type` was present, ≤ 64 printable-ASCII bytes, the request authenticated, and identifier capture is on (an open set, so treated as an identifier). |
| `kelvran.claude_code.prev_tool_durations` | string | `x-claude-code-prev-tool-durations` had at least one well-formed entry (printable ASCII, a percent-decodable name, a duration ≤ 86,400,000 ms); the wire form of the kept entries only (≤ 32 entries, ≤ 4 KB), so the attribute is always valid UTF-8. |
| `kelvran.claude_code.prev_tool_count`, `kelvran.claude_code.prev_tool_total_ms` | int | Set together with `prev_tool_durations`. |
| `kelvran.prompt.id` | string | The request used server-side prompt management (`prompt_id`). |
| `kelvran.prompt.version` | int | Set together with `kelvran.prompt.id`. |
| `kelvran.response_format.requested_not_enforced` | bool | Only ever `true`: the request asked for structured output and the serving deployment could not enforce it. Never `false`. |
| `kelvran.cost.estimated` | bool | Only ever `true`: a streamed response was billed from an estimate. Never `false`. |
| `kelvran.fallback.hops` | int | Fallback hops admitted to the deployment call (a hop a deployment gate rejected before any upstream request still counts, like the `fallback_hop` event); set only when a fallback happened (on `main` since 2026-10-09). |
| `kelvran.savings.usd` | string (decimal) | Cache hit only: the notional cost the hit avoided. |
| `kelvran.cache.layer` | string | Cache hit only: `L1`, `L2` or `L3`. |
| `kelvran.cache.age_ms` | float64 | Cache hit only, every layer. |
| `kelvran.cache.similarity` | float64 | L3 hit only. |

On a failure the span records the error (`RecordError`) and its status is set to `Error` with the error message. `gen_ai.operation.name` and `gen_ai.request.model` are metric attributes only; the span name carries the requested model.

### Span events

| Event | On span | Attributes | Added when |
|---|---|---|---|
| `fallback_hop` | `chat <model>` | `kelvran.deployment.name` (the hop's target deployment), `kelvran.fallback.hop.error_class` (string; one of `content_policy`, `context_window_exceeded`, `generic`), `kelvran.fallback.hop.duration_ms` (float64) | Each fallback-chain hop that failed. A hop that succeeds adds no event; the final span attributes carry it. |

The `GatewayDecisionEvent` fallback fields record only the first abandoned deployment, never an intermediate hop; the span events are the only record of those.

## Logs

### Format

- Go `log/slog` with a JSON handler on standard output. `msg` is the event name; the other keys are the event's fields. The event names and field names are not a public contract.
- Request-path log lines written by the dataplane package (`chat_completion`, `embeddings`, `cache_cross_instance_check`, `lexical_cache_search_failed`, every `*_backend_unavailable` line except `budget_redis_backend_unavailable`, `guardrail_blocked_*`, `guardrail_fail_open`, `budget_threshold_crossed`, `budget_warn_threshold_crossed`, `anomaly_detected_*` and the `stream_*`/`streaming_*` lines) carry top-level `trace_id` and `span_id` when a valid span is in scope. When no valid span is in scope the two keys are absent, never a pair of zero ids. Lines written by leaf packages during a request (`guardrail_detector_error`, `guardrail_verdict_blocked`, `guardrail_verdict_warn`, `budget_persist_failed`, `budget_redis_backend_unavailable`) and the dataplane's `idempotency_complete_failed`, `idempotency_complete_marshal_failed`, `idempotency_fail_failed` and `gatewayevents_marshal_failed` lines never carry them; correlate those through the paired `guardrail_fail_open` or `chat_completion` line. Health-probe lines never carry them.
- go-redis dial failures are printed as plain text on standard error, not as JSON. OTLP exporter failures are printed by the SDK as JSON lines whose `msg` is the raw error, with no event name; see the T1 row of [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md).
- The `model` field on request lines is the requested model name truncated to 256 bytes.

### `chat_completion`

One line per chat completion. Level `INFO` on success, `ERROR` on failure.

| Field | Type | Present |
|---|---|---|
| `trace_id`, `span_id` | string | When the request span is valid. |
| `model` | string | Always. |
| `cache_hit` | bool | Always. |
| `cache_layer` | string | Cache hit: `L1`, `L2`, `L3`. |
| `cache_age_ms` | number | Cache hit. |
| `cache_similarity` | number | L3 hit. |
| `virtual_key_id` | string | A virtual key was resolved. |
| `client_tool`, `request_class` | string | Always (on `main` since 2026-10-09): the two bounded attribution values, `other` / `none` when unknown or absent. Identifiers (session, agent, prompt ids) are never log fields. |
| `gatewayevents_v1` | string (JSON) | Always, success and failure, unless marshalling failed (then `gatewayevents_marshal_failed` is logged at `WARN` and the field is omitted). See the next table. |
| `error` | string | Failure. |
| `upstream_status` | int | Failure caused by an upstream HTTP error: the provider's HTTP status. |
| `upstream_error_type` | string | Failure caused by an upstream HTTP error that named an exception type; sanitised and bounded to 128 runes. |
| `upstream_retry_after_ms` | int | Failure caused by an upstream HTTP error that sent `Retry-After`; the parsed value in milliseconds. |
| `upstream_provider` | string | Failure caused by a mid-stream upstream error frame: the provider. |
| `upstream_stream_error` | string | Failure caused by a mid-stream upstream error frame with a typed cause. |
| `prompt_tokens`, `completion_tokens`, `total_tokens` | int | Success. |
| `cost_usd` | string (decimal) | Success. A JSON string, not a number. |

### `gatewayevents_v1`: the `GatewayDecisionEvent`

The `gatewayevents_v1` value is a `GatewayDecisionEvent` message from `api/gatewayevents/v1/gatewayevents.proto`, serialised with protojson's default options: keys are lowerCamelCase (`traceId`, `virtualKeyId`), every field at its proto3 zero value (`""`, `false`, `0`) is omitted from the JSON rather than written, `outcome` is the enum name string (`OUTCOME_OK`) and `occurredAt` is an RFC 3339 string. A `""` or `false` in the table below therefore means the key is absent from the JSON, not present with that value: a non-hit row has no `savingsUsd` key, an auth failure has no `virtualKeyId` key, and `fallbackHappened`, `rateLimitFailOpen` and `costIsEstimated` appear only when `true`. Decode it with a protojson decoder rather than matching hand-written keys; the decoder yields the proto default for an absent key. It is the one record built for durable, offline analysis; the span is not.

| Proto field | JSON key | Type | Meaning |
|---|---|---|---|
| `trace_id` | `traceId` | string | Same span as the log line's top-level `trace_id`. |
| `span_id` | `spanId` | string | |
| `occurred_at` | `occurredAt` | `google.protobuf.Timestamp` | |
| `virtual_key_id` | `virtualKeyId` | string | `""` when auth failed and no key was resolved. |
| `requested_model` | `requestedModel` | string | |
| `outcome` | `outcome` | `Outcome` enum | See the next table. |
| `rate_limit_fail_open` | `rateLimitFailOpen` | bool | `true` only when the rate limiter errored and the request was allowed through. `false` also covers "the rate limiter never ran". |
| `fallback_happened` | `fallbackHappened` | bool | |
| `fallback_from_deployment` | `fallbackFromDeployment` | string | The first deployment tried and abandoned. `""` when no fallback happened. |
| `fallback_reason` | `fallbackReason` | string | The first attempt's error text. `""` when no fallback happened. |
| `budget_spent_usd` | `budgetSpentUsd` | string (decimal) | Cumulative spend for the key at the moment of the budget check, before this request's own cost. Always present as a decimal string: `"0"` both for a key that has spent nothing and for a request where the budget check never ran (`OUTCOME_AUTH_FAILED`, `OUTCOME_MODEL_NOT_ALLOWED` and `OUTCOME_RATE_LIMITED` precede it). Cross-reference `outcome` to tell the two apart. |
| `agent_run_id` | `agentRunId` | string | The propagated Baggage member; `""` when none. |
| `cost_usd` | `costUsd` | string (decimal) | This request's cost. `"0"` is a real value. |
| `savings_usd` | `savingsUsd` | string (decimal) | Notional cost avoided by a cache hit. Key absent on every non-hit (the proto default `""`, never `"0"`). |
| `cost_is_estimated` | `costIsEstimated` | bool | `true` only for a streamed response billed from an estimate. |
| `billing_subject_id` | `billingSubjectId` | string | The key's operator-supplied billing subject; `""` when none. |
| `finish_reason` | `finishReason` | string | The primary choice's finish reason; `""` when the response has no choices. |

`Outcome` values:

| Value | Meaning |
|---|---|
| `OUTCOME_UNSPECIFIED` | Never set by the gateway. |
| `OUTCOME_OK` | Served. |
| `OUTCOME_AUTH_FAILED` | No virtual key resolved. |
| `OUTCOME_MODEL_NOT_ALLOWED` | The key may not use the requested model. |
| `OUTCOME_RATE_LIMITED` | The caller's own per-key rate limit or concurrency cap. |
| `OUTCOME_BUDGET_EXCEEDED` | The key's budget cap. |
| `OUTCOME_NO_DEPLOYMENT` | No deployment serves the model. |
| `OUTCOME_UPSTREAM_ERROR` | The upstream call failed. |
| `OUTCOME_GUARDRAIL_BLOCKED` | A pre-call or post-call guardrail Block verdict. |
| `OUTCOME_DEPLOYMENT_CAPACITY` | A deployment-scoped rate-limit or concurrency ceiling. |
| `OUTCOME_INVALID_REQUEST` | A malformed request no upstream call could have resolved, for example zero messages after prompt expansion. |

The `GatewayDecisionEvent` has no `budget_fail_open` field.

### `embeddings`

One line per embeddings request. Level `INFO` on success, `ERROR` on failure. There is no `gatewayevents_v1` field on this line.

| Field | Type | Present |
|---|---|---|
| `trace_id`, `span_id` | string | When a valid span is in scope. |
| `model` | string | Always. |
| `input_count` | int | Always: the number of inputs in the request. |
| `duration_ms` | int | Always. |
| `virtual_key_id` | string | A virtual key was resolved. |
| `client_tool`, `request_class` | string | Always (on `main` since 2026-10-09): the two bounded attribution values, `other` / `none` when unknown or absent. Identifiers (session, agent, prompt ids) are never log fields. |
| `deployment` | string | A deployment was selected. |
| `error` | string | Failure. |
| `upstream_status`, `upstream_error_type`, `upstream_retry_after_ms`, `upstream_provider`, `upstream_stream_error` | as above | Failure, same rules as `chat_completion`. |
| `prompt_tokens`, `total_tokens` | int | Success. |
| `cost_usd` | string (decimal) | Success. |

### `cache_cross_instance_check`

One `INFO` line per cache check that completed: after the L1 check and after the L2 check when L1 missed (each only when that layer's backend returned without error), and once per L3-lite check, on a hit after every gate passed or on a miss after the candidate loop (not when the volatile-query bypass or a `lexical_cache_search_failed` error ended the check early). The L3 line reuses the request's L1 key as `cache_key`, since L3 has no exact key of its own. This is a log line and not a metric because `cache_key` is unbounded cardinality. It is the input stream for the offline analysis function in `gateway/internal/telemetry/cachecorrelation`.

| Field | Type | Meaning |
|---|---|---|
| `trace_id`, `span_id` | string | When the request span is valid. |
| `tenant_id` | string | The cache scope (`cache.ScopeKey`) on `L1`/`L2` rows: the virtual key id, or a per-end-user SHA-256 hex derived from it for keys with `cache_scope_to_end_user`. The bare virtual key id on `L3` rows. |
| `cache_key` | string | SHA-256 hex of the exact key: the L1 key on `L1` and `L3` rows, the L2 normalized key on `L2` rows. |
| `cache_layer` | string | `L1`, `L2` or `L3`. |
| `instance_id` | string | `<hostname>:<pid>`. |
| `hit` | bool | |
| `ttl_ms` | int | The layer's configured TTL. |

### Process lifecycle events

| Event (`msg`) | Level | Fields | When |
|---|---|---|---|
| `build_info` | `INFO` | `version`, `commit`, `date`, `go_version`, `platform` | First line at startup, the same fields as `-version` prints. On main since 2026-10-08, not in gateway/v0.17.0. |
| `gateway_starting` | `INFO` | `instance_id` | After configuration is loaded. |
| `gateway listening` | `INFO` | `addr` | The data-plane listener is about to serve. |
| `admin server listening` | `INFO` | `addr`, `mtls` (bool) | Only when an admin listener is configured. |
| `gateway shutting down` | `INFO` | `reason` | A stop signal was received. |
| `gateway_shutdown_forced_with_requests_still_in_flight` | `WARN` | none | The shutdown grace period expired with request handlers still running. |
| `telemetry_metrics_shutdown_flush_failed` | `WARN` | `error` | The final metrics flush failed; non-fatal. |
| `telemetry_shutdown_failed` | `WARN` | `error` | The trace provider's shutdown failed. On main since 2026-10-08, not in gateway/v0.17.0. |
| `gateway exited` | `ERROR` | `error` | The gateway's main loop returned an error; the process exits. |

### Other event names

Every other structured event name in non-test gateway code, grouped by subsystem. Names and fields are informational and may change in any release. [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md) maps each failure to the event it produces.

| Subsystem | Event names |
|---|---|
| Admin API audit | `admin_backup_completed`, `admin_cache_entry_erased`, `admin_config_read`, `admin_deployment_weight_updated`, `admin_prompt_deleted`, `admin_prompt_label_deleted`, `admin_prompt_label_set`, `admin_prompt_upserted`, `admin_prompts_read`, `admin_virtual_key_deleted`, `admin_virtual_key_inflight_read`, `admin_virtual_key_rotated`, `admin_virtual_key_spend_read`, `admin_virtual_key_upserted`, `admin_virtual_keys_read`; `admin_audit_durable_append_failed` |
| Alerting webhooks | `alerting_webhook_delivery_failed`, `alerting_webhook_id_generation_failed`, `alerting_webhook_marshal_failed`, `alerting_webhook_signing_secret_not_base64`, `alerting_webhook_signing_secret_not_whsec_prefixed`, `alerting_webhook_url_env_unset` |
| Anomaly detection | `anomaly_detected_fallback_rate_shift`, `anomaly_detected_finish_reason_shift` |
| Budget | `budget_backend_unavailable`, `budget_persist_failed`, `budget_redis_addr_and_persist_path_both_set`, `budget_redis_backend_unavailable`, `budget_threshold_crossed`, `budget_warn_threshold_crossed` |
| Cache | `cache_cross_instance_check`, `lexical_cache_search_failed` |
| Config propagation | `configpropagation_apply_failed`, `configpropagation_marshal_failed`, `configpropagation_payload_unmarshal_failed`, `configpropagation_publish_failed`, `configpropagation_subscribe_stopped` |
| Credential reload | `credential_reload_read_failed`, `credential_reload_rotated`, `bedrockguard_credential_reload_read_failed`, `bedrockguard_credential_reload_rotated`, `embedsim_credential_reload_read_failed`, `embedsim_credential_reload_rotated` |
| Guardrails | `guardrail_bedrock_guardrails_enabled`, `guardrail_blocked_postcall`, `guardrail_blocked_postcall_streaming_audit_only`, `guardrail_blocked_precall`, `guardrail_config_unknown_action`, `guardrail_config_unknown_category`, `guardrail_detector_error`, `guardrail_embedsim_enabled`, `guardrail_fail_open`, `guardrail_verdict_blocked`, `guardrail_verdict_warn`, `embedsim_corpus_seeded` |
| Health probes | `health_probe_deployment_recovered`, `health_probe_deployment_unhealthy` |
| Idempotency | `idempotency_complete_failed`, `idempotency_complete_marshal_failed`, `idempotency_fail_failed` |
| Identity and persistence | `identity_persist_failed`, `identity_redis_addr_and_persist_path_both_set`, `persist_store_corrupt_backup_failed`, `persist_store_open_failed`, `persist_store_reset`, `startup_virtual_key_overridden_by_persisted_store` |
| Rate limiting | `ratelimit_backend_unavailable`, `ratelimit_backend_unavailable_fallback_hop`, `ratelimit_tpm_backend_unavailable`, `deployment_ratelimit_backend_unavailable`, `deployment_tpm_backend_unavailable`, `embeddings_ratelimit_backend_unavailable` |
| Streaming | `stream_duplicate_index_after_finish`, `stream_missing_usage`, `streaming_midstream_reservation_topup_exhausted`, `streaming_near_duplicate_collision`, `streaming_runaway_guard_triggered` |
| Request lines | `chat_completion`, `embeddings`, `gatewayevents_marshal_failed` |
| Startup and encoding (prose `msg`, not snake_case) | `automemlimit: could not set GOMEMLIMIT from cgroup`; `deployment's AWS access key ID env var is not set; calls to this deployment will fail`, `deployment's AWS secret access key env var is not set; calls to this deployment will fail`, `deployment's upstream API key env var is not set; calls to this deployment will fail`; `bedrock guardrails access key ID env var is not set; calls will fail`, `bedrock guardrails secret access key env var is not set; calls will fail`; `embedsim access key ID env var is not set; calls will fail`, `embedsim secret access key env var is not set; calls will fail`; `credential file could not be read or is empty; calls will fail` (all `WARN`, startup); `encoding chat completion response`, `encoding embeddings response` (`ERROR`, the response write failed); `encoding models list` (`ERROR`; on main since 2026-10-08, not in gateway/v0.17.0) |

## Not available today

- A Prometheus `/metrics` endpoint. Metrics leave the process only through the configured exporter; scrape them from an OTLP collector.
- A `kelvran.deprecation.used` counter and a `deprecated_surface_used` log line. [`docs/VERSIONING.md`](../VERSIONING.md) reserves both; nothing is deprecated yet, so neither exists.
- Metrics for health-probe transitions, fallback hops (a span event only), OTLP export failures, admin audit-append failures, or credential-reload failures. Each has a log line only.
- A `budget_fail_open` field on the `GatewayDecisionEvent`. The signal exists only as the `kelvran.budget.fail_open` counter and the `budget_backend_unavailable` log line.
- Live spend-velocity (CUSUM) detection. `gateway/internal/telemetry/spendvelocity` is a standalone analysis package, not wired into the request path.
- Live cross-instance cache correlation. `gateway/internal/telemetry/cachecorrelation` is an offline analysis function over `cache_cross_instance_check` lines; no process runs it.
- Telemetry for the evals deployable is not documented here.

## Related pages

- [`docs/operations/TELEMETRY.md`](../operations/TELEMETRY.md): the design narrative, dashboards and alerting guidance behind these signals.
- [`docs/operations/FAILURE-MODES.md`](../operations/FAILURE-MODES.md): which metric and log event each dependency failure produces.
- [`config.md`](config.md): every configuration key, including the `telemetry` block.
- [`error-codes.md`](error-codes.md): the client-facing error vocabulary that `error.type` and `Outcome` classify.
- [`data-plane-api.md`](data-plane-api.md): the routes the otelhttp server span (operation `gateway.http`) covers.
- [`../how-to/deploy/docker-compose.md`](../how-to/deploy/docker-compose.md): the bundled OTLP collector, Prometheus, Tempo and Grafana profile.
- [`../how-to/troubleshooting.md`](../how-to/troubleshooting.md): reading these signals during an incident.
- [`../how-to/caching.md`](../how-to/caching.md) and [`../explanation/cache-gate.md`](../explanation/cache-gate.md): what the `kelvran.cache.*` signals measure.
- RFCs: [`2026-09-02-otel-tracing-agent-run-id.md`](../rfcs/2026-09-02-otel-tracing-agent-run-id.md), [`2026-09-05-gateway-ratelimit-fail-open-metric.md`](../rfcs/2026-09-05-gateway-ratelimit-fail-open-metric.md), [`2026-09-07-gateway-genai-metrics.md`](../rfcs/2026-09-07-gateway-genai-metrics.md), [`2026-09-07-cache-cross-instance-telemetry.md`](../rfcs/2026-09-07-cache-cross-instance-telemetry.md), [`2026-09-10-gateway-cache-savings-metric.md`](../rfcs/2026-09-10-gateway-cache-savings-metric.md).
