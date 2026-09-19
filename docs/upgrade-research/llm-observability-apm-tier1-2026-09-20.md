# LLM-Specific Observability/APM Tier-1 Survey — OTel GenAI Semconv Drift + Backend Fit

Date: 2026-09-20 · grounded directly against this repo's telemetry code and against primary
sources (`opentelemetry.io`, `github.com/open-telemetry/semantic-conventions-genai`,
`github.com/open-telemetry/semantic-conventions`, and each vendor's own current docs) — no
secondary blog write-ups used for any spec-compliance or vendor-integration claim below.

## Ground truth (re-verified directly against this repo before synthesis)

- `gateway/internal/telemetry/telemetry.go` and `result.go` are the entire attribute/metric
  surface — confirmed by direct read, not by grepping a doc. Full current inventory:
  - **`gen_ai.*` span attributes actually set**: `gen_ai.provider.name`, `gen_ai.response.model`,
    `gen_ai.response.id`, `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens`,
    `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`,
    `gen_ai.usage.cache_creation.input_tokens`.
  - **`gen_ai.*` attribute defined but never set anywhere** (real, previously-unflagged gap —
    confirmed by a repo-wide grep returning exactly one hit, the `const` declaration itself):
    `gen_ai.request.stream` (`AttrGenAIRequestStream`, `result.go:24`).
  - **`gen_ai.*` metric attributes** (on the two histograms below): `gen_ai.operation.name`,
    `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`, `error.type`,
    `gen_ai.token.type`.
  - **`gen_ai.*` metrics**: `gen_ai.client.token.usage` (Histogram, `{token}`),
    `gen_ai.client.operation.duration` (Histogram, `s`) — both use the spec's own canonical
    instrument names verbatim, per `telemetry.go`'s own comment, specifically so a generic
    GenAI-aware dashboard needs no Kelvran-specific translation.
  - **`kelvran.*` span attributes**: `kelvran.virtual_key.id`, `kelvran.agent_run_id`,
    `kelvran.cache.hit`, `kelvran.cost.usd`, `kelvran.savings.usd`, `kelvran.deployment.name`,
    `kelvran.cache.layer`, `kelvran.cache.similarity`, `kelvran.cache.age_ms`,
    `kelvran.prompt.id`, `kelvran.prompt.version`,
    `kelvran.response_format.requested_not_enforced`, `kelvran.cost.estimated`.
  - **`kelvran.*` metrics**: `kelvran.ratelimit.fail_open`, `kelvran.streaming.cost_estimated`,
    `kelvran.cache.l3.gate_outcome`, `kelvran.cache.savings_usd`, `kelvran.cache.lookup`,
    `kelvran.llm.spend_usd`, `kelvran.budget.threshold_crossed`, `kelvran.persistence.failed`,
    `kelvran.configpropagation.subscribe_stopped` — each carries one or more of
    `kelvran.fallback.hop.error_class`/`.duration_ms`, `kelvran.cache.l3.gate`/`.outcome`,
    `kelvran.instance.id`, `kelvran.cache.lookup_outcome`, `kelvran.budget.percent_bucket`,
    `kelvran.persistence.store_kind` as dimensions (span-event-only:
    `kelvran.fallback.hop.error_class`/`.duration_ms`, emitted via `span.AddEvent`, not a span
    attribute).
  - **Resource attributes**: `service.name` = `"kelvran-gateway"`, `service.instance.id`.
- **The OTLP pipeline is not merely implemented — it is now live-verified**, superseding this
  same directory's own `observability-monitoring-maturity-2026-09-13.md` (which still described
  it as "implemented and inert") by six days: `gateway/ARCHITECTURE.md`'s Tech Stack table
  (`gateway/ARCHITECTURE.md:592`) states a `grafana/otel-lgtm`-backed `observability`
  Compose service and a provisioned Grafana dashboard now exist, and that real Bedrock pilot
  traffic produced "real, non-empty `kelvran.cache.lookup`/`kelvran.llm.spend_usd` metrics and
  real traces." `docs/operations/TELEMETRY.md` repeats this with more detail, including specific
  live-queried Prometheus metric names (`kelvran_cache_lookup_total`,
  `gen_ai_client_operation_duration_seconds_count`) and a provisioned dashboard UID
  (`kelvran-gateway-overview`) confirmed `"provisioned": true` via Grafana's own API. This means
  the research question's framing ("it already exports via OTLP per `gateway/ARCHITECTURE.md`")
  understates current reality — it isn't just wired, it's the thing currently proven against real
  traffic.

## Executive summary

The OTel GenAI semantic-conventions spec has had zero stabilization events since the last check
nine days ago (`gateway-observability-sre-round4-2026-09-11.md`) — every attribute and metric in
the dedicated `open-telemetry/semantic-conventions-genai` repo remains `stability: development`,
confirmed by downloading and grepping its current `registry.yaml`/`metrics.yaml` directly, and
that repo still has zero tagged releases as of today. So there is still no forced-migration event.
But real churn did happen underneath Kelvran's own code in the interim, in two concrete,
independently checkable ways: the attribute Kelvran calls
`gen_ai.usage.cache_creation.input_tokens` no longer exists anywhere in the current spec files —
`gen_ai.usage.cache_write.input_tokens` is the only cache-write-token attribute defined today, via
a PR merged 2026-08-20 (three weeks *before* round 4's own research date, meaning round 4 was
already citing a stale name when it wrote its own Finding 1) — and the spec independently grew a
`gen_ai.prompt.name`/`.version`/`.variable` attribute family (merged 2026-06-23, months before
Kelvran built its own `kelvran.prompt.id`/`.version` on 2026-09-13) that overlaps semantically with
Kelvran's custom prompt-versioning attributes without either side knowing about the other. Neither
is a compliance failure — both attributes are still `development`-stability, and nothing forces a
rename — but both are real, disclosed, checkable facts a future backlog audit should not have to
rediscover from scratch. Separately, a plain repo grep found `gen_ai.request.stream` declared as a
constant and never once set on any span or metric — a small, real, previously-unflagged gap
distinct from anything either prior research round flagged. On the vendor-survey side: of the four
platforms named, only Langfuse's own current docs confirm both a real native OTLP ingestion
endpoint *and* documented recognition of generic `gen_ai.*` attributes without requiring Kelvran to
adopt a vendor SDK — making it the lowest-friction path onto Kelvran's *already-emitted* OTLP data
specifically. Traceloop also exposes a real, generic OTLP endpoint and has a genuine historical
claim to having seeded the OTel GenAI semconv effort, but its current docs did not, on direct
fetch, quote a `gen_ai.*` attribute table the way Langfuse's did. Arize Phoenix accepts standard
OTLP transport but its own LLM-aware UI is documented as OpenInference-attribute-native, not
`gen_ai.*`-native — a real, disclosed integration-friction risk, not a blocker, since transport-level
OTLP compatibility is confirmed either way. Helicone is the outlier: its complete documentation
index contains no OpenTelemetry/OTLP page at all — its integration model is proxy-routing or an
SDK-wrapper async logger, meaning adopting it would forgo Kelvran's already-built OTLP investment
entirely rather than build on it.

## Findings

### F1 — Zero stabilization since round 4; the "no forced migration" conclusion still holds nine days later (not_yet — same trigger as before)
**Confidence: high** (direct primary-source verification: downloaded and grepped the live
`registry.yaml`/`metrics.yaml` from `open-telemetry/semantic-conventions-genai`, not a claim from
either prior round carried forward unchecked)

`open-telemetry/semantic-conventions-genai` has zero tags and zero GitHub releases as of today
(confirmed via `gh api repos/open-telemetry/semantic-conventions-genai/tags` and `/releases`, both
empty arrays) — the most recent commits are from 2026-09-16 (#518, "Allow conversation ID on tool
execution spans"; #514, a routine lockfile bump). Every single `stability:` field in the
downloaded `model/gen-ai/registry.yaml` (1041 lines) and `model/gen-ai/metrics.yaml` (278 lines) —
every attribute, every enum member, every metric — reads `development`; grep found zero
occurrences of `stable`. The main `open-telemetry/semantic-conventions` repo (last release
v1.44.0, 2026-08-04) now contains only a `model/gen-ai/deprecated/` stub pointing at the new repo,
confirming the June-2026 split round 4 already found is permanent, not a temporary redirect.

**Not yet**: nothing changes here from round 4's own conclusion — no forced-migration event exists
to react to. Named trigger unchanged: the dedicated repo's first tagged release, or a specific
attribute Kelvran uses becoming individually deprecated (which, per F2 below, has now happened
once — just not to anything stable enough to force a change).

### F2 — Real, concrete drift: `gen_ai.usage.cache_creation.input_tokens` (what Kelvran emits) does not exist in the current spec; `gen_ai.usage.cache_write.input_tokens` is the only current name for that concept (build_now, low-cost)
**Confidence: high** (direct diff against the live registry + the merging PR's own description,
not inferred)

`result.go`'s `AttrGenAIUsageCacheCreationInputTokens` constant is set to
`"gen_ai.usage.cache_creation.input_tokens"`, with a comment explicitly citing "the spec's v1.40.0
(Feb 2026)" as the source of this name. A full-text grep of the current
`open-telemetry/semantic-conventions-genai` registry, metrics, and spans files for the string
`cache_creation` returns zero matches anywhere. The only cache-write-token attribute defined today
is `gen_ai.usage.cache_write.input_tokens` (`registry.yaml`, brief: "The number of input tokens
written to a provider-managed cache" — the identical concept), introduced by PR #440 ("Add
span/event attributes for usage breakdown per modality / cache / phases"), merged **2026-08-20** —
three weeks before round 4's own 2026-09-11 research date. Round 4's Finding 1 cited
`cache_creation.input_tokens` as the current name; that citation was already nine days stale (now
thirty) when round 4 itself was written, meaning this drift was missed by the prior research round,
not newly introduced since. `gen_ai.usage.cache_read.input_tokens` (the paired read-side
attribute Kelvran also emits) is unaffected — it still exists under its original name.

Both names remain `development`-stability, so nothing forces this change, and a spec-aware backend
built against an older snapshot may still recognize the old name — but any dashboard or backend
built against the *current* registry will not recognize Kelvran's current
`cache_creation.input_tokens` values as the semantically equivalent
`cache_write.input_tokens` metric without an explicit remap.

**Build now**: rename the constant's string value from `gen_ai.usage.cache_creation.input_tokens`
to `gen_ai.usage.cache_write.input_tokens` in `result.go` — a one-line value change (the Go
identifier `AttrGenAIUsageCacheCreationInputTokens` can stay, or be renamed for consistency; only
the wire-level string matters to any consuming backend). Low cost, no behavior change, closes a
real and now-confirmed gap rather than a guessed one.

### F3 — Real, previously-unflagged gap: `gen_ai.request.stream` is declared and never set (build_now, trivial)
**Confidence: high (direct source verification)** — a repo-wide grep, not a claim needing
external verification.

`AttrGenAIRequestStream = "gen_ai.request.stream"` (`result.go:24`) is declared as a constant.
Grepping all non-test Go source under `gateway/` for either the Go identifier or the literal
string returns exactly one hit: the declaration itself. Neither `RecordChatCompletionResult` nor
`RecordChatCompletionMetrics` ever calls `attribute.Bool(AttrGenAIRequestStream, ...)`. The spec's
own current definition (`registry.yaml`) is a plain boolean, `"Indicates whether the GenAI request
was made in streaming mode"` — trivially derivable at both of dataplane's own call sites
(`HandleChatCompletion` vs. `HandleChatCompletionStream` already branch on exactly this).

**Build now**: set this attribute on the span (and optionally as a metric attribute alongside
`gen_ai.operation.name`) at the existing `RecordChatCompletionResult`/`RecordChatCompletionMetrics`
call sites — the calling code already knows unambiguously which path it's on. Small, additive,
no design decision required.

### F4 — Streaming TTFT/inter-chunk metrics remain the one concretely-missing metric pair; nothing has changed here since round 4 (not_yet — same trigger as before, re-confirmed against the current spec text)
**Confidence: high** (re-verified directly against the current `metrics.yaml`, not carried
forward unchecked from round 4)

`gen_ai.client.operation.time_to_first_chunk` and `gen_ai.client.operation.time_per_output_chunk`
are still both defined, still both `recommended` requirement level, still both `development`
stability, both still histograms with unit `"s"`, and both still explicitly scoped ("SHOULD be
reported for streaming calls and SHOULD NOT be reported otherwise") — identical to round 4's own
description. A fresh grep of `gateway/` confirms neither metric name nor a Go identifier for
either exists anywhere. This is the same real, structurally-applicable gap round 4 named, not a
new one — Kelvran's own `streaming.go` already has the first-chunk-arrival and inter-chunk timing
data in scope, per that round's own analysis.

Two adjacent, closely-related metrics newly worth naming (present in the current registry, not
individually checked by round 4): `gen_ai.response.time_to_first_chunk` — a **span attribute**
(not a metric), double seconds, semantically the per-request twin of the TTFT histogram — and
`gen_ai.response.status` — a six-value enum (`queued`/`in_progress`/`completed`/`incomplete`/
`failed`/`cancelled`) describing a response's lifecycle state, distinct from
`gen_ai.response.finish_reasons`. Both are directly applicable to Kelvran's existing streaming
path and currently unimplemented, but neither was named in round 4 (both already existed then,
per their own unrelated merge dates — this is a coverage gap in that round's own sweep, not new
spec surface).

**Not yet** (unchanged from round 4): implementing the metric pair, or the two additionally-named
attributes above, is real, cheap work with no forcing trigger — folding into whatever session next
touches `streaming.go` is reasonable; there is no reason to treat it as urgent given the sustained
zero-stabilization status from F1.

### F5 — New spec surface since round 4's Feb-2026 snapshot is overwhelmingly agent/tool/retrieval/memory-shaped and does not apply to Kelvran's own gateway role (not_yet, correctly out of scope — no trigger exists yet)
**Confidence: high** (direct enumeration of the current registry against Kelvran's own
architecture, not a general survey claim)

The current registry additionally defines, beyond what round 4 covered:
`gen_ai.conversation.id`/`.compacted`, `gen_ai.agent.id`/`.name`/`.description`/`.version`,
`gen_ai.tool.name`/`.call.id`/`.description`/`.type`/`.call.arguments`/`.call.result`/
`.definitions`, `gen_ai.data_source.id`, `gen_ai.output.type`,
`gen_ai.embeddings.dimension.count`, `gen_ai.retrieval.documents`/`.query.text`/`.top_k`,
`gen_ai.memory.store.id`/`.record.id`/`.record.count`/`.query.text`/`.records`,
`gen_ai.system_instructions`, `gen_ai.input.messages`/`.output.messages`,
`gen_ai.evaluation.name`/`.score.value`/`.score.label`/`.explanation`,
`gen_ai.workflow.name`, `gen_ai.request.reasoning.level`/`.previous_response.id`/
`.stream_cursor`, and per-modality token breakdowns (`gen_ai.usage.text/image/audio.*`). The
corresponding new metrics (`gen_ai.invoke_workflow.duration`, `gen_ai.invoke_agent.duration`/
`.inference_calls`/`.tool_calls`, `gen_ai.execute_tool.duration`, and the server-side
`gen_ai.server.request.duration`/`.time_per_output_token`/`.time_to_first_token` triad) are
similarly agent/tool-orchestration- or model-server-shaped.

None of this applies to Kelvran's own gateway role today: Kelvran doesn't execute tools, run
retrieval, manage an agent's memory, or run a multi-step workflow itself — it routes/caches/bills
chat-completion calls a caller's own agent framework issues. The `gen_ai.server.*` triad
specifically describes instrumentation from the *model-serving system's own* perspective (a note
in `metrics.yaml` frames it as "time-to-last byte or last output token," i.e. the inference
server's view) — genuinely ambiguous whether Kelvran-as-gateway should ever emit these vs. staying
`gen_ai.client.*`-only, since Kelvran is simultaneously a client (to upstream providers) and a
server (to its own callers); this is flagged as an open question below, not resolved here, since no
source consulted this round addresses gateway/proxy placement specifically.

**Not yet**: no named trigger exists for any of this — it's real spec surface, correctly
inapplicable to Kelvran's current feature set, not a currently-missed obligation. Revisit if/when
Kelvran ever executes tools or brokers MCP/A2A calls, since `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`'s "agent_run_id" concept already puts Kelvran adjacent to agent-shaped traffic even without owning any agent logic itself.

### F6 — Real, disclosed design collision (not a bug): the spec's `gen_ai.prompt.*` family predates and semantically overlaps Kelvran's own `kelvran.prompt.*` custom attributes (not_yet — flagged for the next time prompt-management observability is touched)
**Confidence: high** (direct comparison of merge dates and semantics, both independently
confirmed)

The current registry defines `gen_ai.prompt.name` (string), `gen_ai.prompt.version` (string,
examples given include SemVer, date-based, and platform-tag styles, explicitly noting "when a
prompt management system is in use, this SHOULD match the version identifier used by that
system"), and `gen_ai.prompt.variable` (a `template[string]` type for prompt-template variable
values) — added by PR #179, "Add prompt versioning and variable support to GenAI attributes,"
merged **2026-06-23**. Kelvran's own `kelvran.prompt.id`/`kelvran.prompt.version` span attributes
(`result.go`, per `docs/rfcs/2026-09-13-gateway-prompt-management.md`) were added **2026-09-13** —
nearly three months later — under a Kelvran-custom namespace, with no comment anywhere indicating
the spec's own `gen_ai.prompt.*` family was known or considered at the time.

The semantics are close but not identical: Kelvran's `PromptID` is a stable identifier (the spec's
closest analog is arguably `gen_ai.prompt.name`, "the name of the prompt that uniquely identifies
it" — a plausible match), but Kelvran's `PromptVersion` is typed `int` while the spec's
`gen_ai.prompt.version` is explicitly a free-form string ("can follow any versioning scheme... e.g.
SemVer, date-based... or platform-specific tags") — not a strict rename target without a type
change. `gen_ai.prompt.variable` (template variable capture) has no Kelvran equivalent at all
today.

**Not yet**: this is real, disclosed drift worth carrying forward, not an urgent fix — both
namespaces are `development`-stability, coexistence causes no breakage, and migrating
`kelvran.prompt.*` to `gen_ai.prompt.*` would need a type decision (string vs. int version) that
wasn't resolved by anything consulted this round. Named trigger: the next time
`docs/rfcs/2026-09-13-gateway-prompt-management.md`'s own design gets revisited, or if/when
`gen_ai.prompt.*` reaches any stabilization event, whichever comes first.

### F7 — Of the four named LLM-observability platforms, Langfuse is the only one whose own current docs confirm BOTH a real native-OTLP endpoint AND documented `gen_ai.*` attribute recognition without a vendor SDK (build_now for evaluation; not_yet for adoption pending a real backend decision)
**Confidence: high for Langfuse and Traceloop's OTLP-endpoint claims** (direct doc fetch, exact
URLs/headers quoted); **medium for Phoenix's attribute-naming claim** (confirmed OpenInference-
native from Phoenix's own docs, but this round could not independently confirm or refute whether
Phoenix's collector separately recognizes raw `gen_ai.*` attributes for its LLM-aware UI); **high
for Helicone's absence of a native-OTLP path** (its own complete documentation index was checked,
not a single page)

- **Langfuse**: real native ingestion endpoint, `/api/public/otel` (signal-specific:
  `/api/public/otel/v1/traces`), supporting `OTLP over HTTP` in both `HTTP/JSON` and
  `HTTP/protobuf` — explicitly **not** gRPC ("gRPC is not supported yet," per Langfuse's own
  current docs). Auth is Basic Auth via `OTEL_EXPORTER_OTLP_HEADERS`
  (`Authorization=Basic <base64(public:secret)>`), plus a required
  `x-langfuse-ingestion-version=4` header for real-time visibility. Langfuse's docs confirm it maps
  generic `gen_ai.*` attributes — `gen_ai.request.model`/`.response.model` → model,
  `gen_ai.usage.*` → usage/tokens, `gen_ai.usage.cost` → cost — while its own `langfuse.*`
  namespace, if also present, always takes precedence. Two caveats worth flagging: Langfuse's docs
  quote `gen_ai.system` (an older/legacy attribute name), not `gen_ai.provider.name` (the name
  Kelvran actually emits, and the current spec's canonical name per F1's own registry read) — this
  round could not confirm whether Langfuse's mapping table is itself slightly stale on this one
  attribute, or simply didn't happen to show it in the fetched excerpt; and the docs explicitly
  self-disclose that "the Semantic Conventions for GenAI attributes on traces are still evolving,"
  consistent with F1.
- **Traceloop**: also a real, generic OTLP endpoint — `https://api.traceloop.com` (US instance),
  Bearer-token auth (`Authorization: Bearer <TRACELOOP_API_KEY>`), confirmed via a documented
  OpenTelemetry Collector `otlphttp/traceloop` exporter config, i.e. any OTel SDK/Collector can
  send to it, not only the `openllmetry-sdk` package. OpenLLMetry's own GitHub README states "Our
  semantic conventions are now part of OpenTelemetry" and links to the GenAI-semconv discussion —
  a genuine historical claim to having seeded this effort — but this round's direct fetch of
  Traceloop's own docs did not surface an explicit `gen_ai.*` attribute table the way Langfuse's
  did; only a linked-but-unquoted "GenAI Semantic Conventions" contributor page. Treat
  attribute-level compatibility as plausible-and-likely, not independently confirmed this round.
- **Arize Phoenix**: confirmed to accept standard OTLP transport — its own docs show pointing a
  plain OpenTelemetry Go SDK's OTLP/HTTP exporter directly at Phoenix's collector endpoint
  (`http://localhost:6006` locally; `register()` exposes a `protocol` option of `"grpc"` or
  `"http/protobuf"`) with no Phoenix-specific SDK required for that language. But Phoenix's own
  docs are explicit that its SDK "sends traces over OTLP using OpenInference semantic
  conventions" — a distinct, Phoenix/Arize-originated attribute vocabulary, not `gen_ai.*`. The
  fetched page never mentions `gen_ai.*` at all. This means transport-level OTLP compatibility is
  real, but whether Phoenix's LLM-specific UI (token/cost panels, span-kind rendering) recognizes
  Kelvran's raw `gen_ai.*` attributes the same way it recognizes OpenInference attributes is a real
  open question this round did not resolve — plausible it falls back to generic-trace rendering
  only.
- **Helicone**: this round fetched Helicone's own complete documentation index
  (`docs.helicone.ai/llms.txt`) and found **zero** pages referencing OpenTelemetry, OTLP, or
  "otel" anywhere. Its two documented integration models are proxy-routing (calls go through
  Helicone's gateway, OpenAI-SDK-compatible base-URL swap) and an async logger
  (`HeliconeAsyncLogger`) that wraps a fixed list of provider SDKs (OpenAI, Anthropic, Azure
  OpenAI, Cohere, Bedrock, Google AI Platform) directly — neither accepts arbitrary OTLP traces
  from an already-instrumented service. For Kelvran specifically, adopting Helicone would mean
  building a second, parallel integration rather than pointing its existing OTLP pipeline
  anywhere.

**Build now** (as evaluation, not adoption): if/when Kelvran stands up a real LLM-specific
observability backend, Langfuse is the concrete first candidate to prototype against — point the
already-configured `otlp_endpoint` at Langfuse Cloud's (or a self-hosted Langfuse's)
`/api/public/otel/v1/traces` with the documented Basic-Auth header, and verify Kelvran's own
`gen_ai.*`/`kelvran.*` data renders meaningfully, before investing further. **Not yet**: an actual
adoption decision — this remains gated on the same real backend-selection decision
`observability-monitoring-maturity-2026-09-13.md` already covers for the general-purpose
Grafana/Prometheus/Tempo stack (`grafana/otel-lgtm`, already live per this report's own Ground
Truth section); Langfuse/Phoenix/Traceloop would be an LLM-specific *addition* alongside that
general stack, not a replacement for it, since none of the four platforms surveyed here replace
generic infra metrics (`kelvran.ratelimit.fail_open`, `kelvran.persistence.failed`, etc.) the way
Prometheus/Grafana already do.

## Answers to the research question

1. **Current, official OTel GenAI semconv spec version and maturity?** No version number in the
   traditional sense — the dedicated `open-telemetry/semantic-conventions-genai` repo has zero
   tags/releases (confirmed today, same as nine days ago). Every attribute and metric remains
   `development`-stability; nothing has stabilized. The parent `semantic-conventions` repo is at
   v1.44.0 (2026-08-04) but now contains only a deprecated stub for `gen-ai`.
2. **Is Kelvran still spec-compliant, missing anything new, or emitting anything since
   deprecated/renamed?** Not "non-compliant" in any breaking sense (nothing is stable enough to
   force compliance), but real, concrete drift exists: `gen_ai.usage.cache_creation.input_tokens`
   (F2) has no current-spec equivalent under that name (`cache_write.input_tokens` is the current
   name for the same concept); `gen_ai.request.stream` is declared but never emitted (F3); the TTFT
   metric pair remains missing, unchanged from round 4 (F4); and a large surface of new
   agent/tool/retrieval/memory attributes exists but is correctly inapplicable to Kelvran's own
   gateway role today (F5), except for two directly-relevant, currently-unimplemented items
   (`gen_ai.response.time_to_first_chunk` span attribute, `gen_ai.response.status`). Separately,
   Kelvran's own `kelvran.prompt.*` custom namespace overlaps with a spec-native `gen_ai.prompt.*`
   family it predates awareness of (F6).
3. **Lowest-friction path for Kelvran's already-OTLP-emitting data into an LLM-specific
   observability platform?** Langfuse, per its own docs' confirmed native OTLP endpoint plus
   documented `gen_ai.*` attribute recognition (F7) — no vendor SDK required, just pointing the
   existing OTLP exporter at a different endpoint with Basic Auth. Traceloop is a plausible second
   candidate (also a real generic OTLP endpoint) but its `gen_ai.*` compatibility wasn't as
   directly confirmed in this round's fetch. Phoenix accepts OTLP transport but is
   OpenInference-attribute-native for its LLM UI — a real integration-friction risk. Helicone has
   no native OTLP path at all in its own documentation and would require a parallel, from-scratch
   integration.

## Caveats

- This round's Helicone/Phoenix/Traceloop findings came from `WebFetch`-summarized excerpts of
  specific doc pages (plus, for Helicone, its full `llms.txt` index) rather than a full manual
  read of each vendor's entire documentation site — a real, disclosed limitation. Langfuse's
  finding is the strongest of the four (a single page directly quoted exact endpoint paths,
  headers, and an attribute-mapping table); the other three rest on narrower excerpts.
  `mcp__exa__web_search_exa` hit a rate limit mid-research (free-tier MCP quota) and could not be
  used to cross-check Helicone's/OpenLLMetry's attribute-naming claims against a second source —
  flagged rather than silently worked around.
- Langfuse's own docs quote `gen_ai.system` (not `gen_ai.provider.name`) as their example
  provider-style attribute — this may mean Langfuse's mapping table itself lags the current
  `gen_ai.provider.name` rename (per the F1 registry read), or simply that the fetched page's
  example happened to predate it. This round could not distinguish the two without a second,
  deeper Langfuse doc fetch.
- F5's claim that `gen_ai.server.*` metrics describe "the model-serving system's own perspective"
  is this round's own reading of the metric `brief`/`note` text, not a spec passage that explicitly
  rules gateways in or out — flagged as an open question (below), not resolved.
- F2/F3/F6 are original, codebase-grounded findings produced directly by this synthesis pass
  (diffing `result.go`/`telemetry.go` against the freshly-downloaded live registry files), not
  carried forward from either prior research round — they carry no external adversarial-vote
  count, but rest on direct primary-source diffing, the strongest evidence available for a
  spec-vs-code drift claim.

## Open questions

1. Should Kelvran (a gateway that is simultaneously a `gen_ai.client.*`-shaped caller to upstream
   providers and a `gen_ai.server.*`-shaped responder to its own callers) ever emit the
   `gen_ai.server.*` metric triad, or is `gen_ai.client.*`-only the architecturally correct choice
   for a proxy/gateway role specifically? No source consulted this round addresses gateway/proxy
   placement in the spec directly — this is a real design question for whoever next touches
   `telemetry.go`'s metric set.
2. Does Langfuse's documented attribute-mapping table (quoting `gen_ai.system`) actually still
   recognize `gen_ai.provider.name` today, or would Kelvran's spans need a `gen_ai.system` shim
   attribute alongside `gen_ai.provider.name` to render correctly in Langfuse specifically? Worth
   a direct, isolated verification (e.g. a real test span sent to a Langfuse trial project) before
   any adoption decision, not just a docs read.
3. If Kelvran's `kelvran.prompt.id`/`kelvran.prompt.version` were migrated toward the spec's
   `gen_ai.prompt.name`/`.version` (F6), does `PromptVersion`'s `int` type get converted to a
   string at the attribute layer only (keeping the internal type), or does the RFC's own internal
   versioning scheme need to change to match the spec's "any scheme, e.g. SemVer/date-based"
   framing? Not resolved by anything consulted this round.
4. Does Phoenix's LLM-specific UI (token/cost panels, span-kind-aware rendering) degrade
   gracefully to generic-trace display for `gen_ai.*`-only spans, or does it require an
   OpenInference-to-`gen_ai.*` bridge/converter that this round did not find documented? A direct
   test (send one real Kelvran trace to a local Phoenix instance) would answer this faster than
   further doc reading.
