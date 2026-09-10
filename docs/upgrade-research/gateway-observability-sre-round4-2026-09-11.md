# Gateway Observability/SRE Research — Round 4 (2026-09-11)

Scope: `gateway/`'s observability lens — OTel GenAI semantic conventions, SLOs, distributed
tracing, sampling, log/trace/metric correlation. Grounded against `gateway/ARCHITECTURE.md`,
`docs/operations/TELEMETRY.md`, `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md`,
`docs/rfcs/2026-09-07-gateway-genai-metrics.md`, and direct reads of
`gateway/internal/gateway/dataplane/{dataplane,fallback}.go`; externally verified against the
live OTel GenAI semantic-conventions repo, `sre.google/workbook`, `opentelemetry.io`, and
production-LLM-gateway observability writeups (MLflow, dev.to, John Hodge, Particula), each
claim adversarially 3-vote-verified before inclusion here.

## Executive Summary

The OTel GenAI semantic-conventions spec has moved house (a full repo split to
`open-telemetry/semantic-conventions-genai` in v1.42.0, June 2026) and grown new surface
(retrieval spans, cache-token attributes, `gen_ai.agent.version`, streaming-latency metrics)
since Kelvran's `gen_ai.*` implementation was last verified — but every single attribute,
span, and metric in the spec, old or new, remains "Development" status with zero stabilization
events, so there is no forced-migration gap, only an optional-enrichment one (streaming TTFT
metrics are the one concretely worth adding now). Kelvran's own docs assert SLOs need real
production traffic to set — Google's own SRE Workbook explicitly contradicts this and prescribes
exactly the situation Kelvran is in (no traffic, no logs) with a first-pass judgment-picked
target methodology, making a minimal SLO definition buildable today, though a full error-budget
*policy* genuinely is not (it needs a real on-call rotation and organizational sign-off Kelvran
doesn't have yet). Tracing sampling is correctly deferred per OTel's own stated volume threshold
(~1000+ traces/sec) that Kelvran is nowhere near, and a low-risk config knob is cheap to add now
even though flipping it isn't needed yet. Direct code reading surfaced two real, previously
unflagged gaps that no amount of spec-reading would have found: `fallback_chains`'
intermediate-hop failures are invisible in all telemetry (only the *first* and *final* deployment
attempts are ever recorded, never a middle hop that also failed), and the gateway's own
`trace_id` is only correlatable from the plain-text `chat_completion` log line via a doubly-nested
JSON string, while every other structured log line (guardrail blocks, budget warnings, rate-limit
fail-open, streaming runaway guards) carries no trace correlation at all.

## Findings

### Finding 1 — GenAI semconv spec: real churn, zero stabilization, one concretely missing metric pair
**Confidence: high** (10 sources, all 3-0 or high-vote, cross-verified against live primary registry)

The spec repo itself relocated: `open-telemetry/semantic-conventions` deprecated all
`gen_ai.*`/`openai.*`/`mcp.*` content in v1.42.0 (2026-06-12) and moved it to a dedicated
`open-telemetry/semantic-conventions-genai` repo, which as of today has zero tagged releases.
Within that new repo, every one of the ~118 `gen_ai.*` attribute/enum stability declarations,
every span, and every metric — including the two Kelvran already emits
(`gen_ai.client.token.usage`, `gen_ai.client.operation.duration`) — is still marked `Development`;
none have graduated to `Stable`. So research question 1's premise (has anything stabilized) is
answered directly: no, and there is no forced-migration event to react to. But real additions
landed in v1.40.0 (Feb 2026) — retrieval spans, cache-token attributes
(`gen_ai.usage.cache_read.input_tokens`/`cache_creation.input_tokens`), and `gen_ai.agent.version`
— none of which apply to Kelvran today (no RAG retrieval, no provider-side prompt-caching
integration yet, no agent-versioning concept in the gateway itself). What *does* directly apply:
the spec separately defines `gen_ai.client.operation.time_to_first_chunk` (TTFT) and
`gen_ai.client.operation.time_per_output_chunk` as their own Development-status histograms,
distinct from the two Kelvran ships. Confirmed via direct grep of
`gateway/internal/telemetry/telemetry.go` and a repo-wide search: zero matches for either metric
name or Go identifier anywhere in `gateway/`. Kelvran already has live streaming infrastructure
(`streaming.go`, `streamaccumulator.go`, `streamrunaway.go`), so this is a real, structurally
applicable, currently-absent metric pair — not a moot point.

The spec also explicitly names a small attribute subset (`gen_ai.operation.name`,
`gen_ai.provider.name`, `gen_ai.request.model`, `server.address`, `server.port`) that
instrumentations SHOULD set at span-*creation* time specifically because they're
sampling-relevant — directly useful input for Finding 3's sampling-knob design, since it tells you
which attributes must be set before, not after, the sampling decision point.

**Build now**: add `gen_ai.client.operation.time_to_first_chunk` and
`gen_ai.client.operation.time_per_output_chunk` histograms to the existing streaming path,
following the same pattern as `operationDurationHistogram`. Low implementation cost (the timing
data — first-chunk arrival, inter-chunk gaps — is already observable inside `streaming.go`'s
existing loop), and TTFT is frequently the single most user-visible latency number for a
streaming LLM gateway, more so than total operation duration.
**Not yet**: a full attribute-name re-sync against the new registry, or migrating off the RFC's
deliberately-hardcoded string constants onto `otel/semconv`'s incubating GenAI package. Named
trigger: the `semantic-conventions-genai` repo's first tagged release (currently zero), or the
addition of a new provider/adapter whose well-known `gen_ai.provider.name` value isn't already
covered by the existing remap table — either event is cheap to react to when it actually happens,
and reacting early risks chasing a still-actively-churning target (most recent commit as of this
writing: 2026-09-10).

### Finding 2 — A minimal SLO definition is buildable today; Kelvran's own docs overstate the traffic prerequisite
**Confidence: high** (4 sources, all against the primary Google SRE Workbook, 3-0/2-1 votes)

`docs/operations/TELEMETRY.md` states outright: *"None of these have concrete target numbers
yet — those get set once there's real production traffic to baseline against, not guessed at
now."* Google's own SRE Workbook explicitly rejects that framing for exactly Kelvran's situation
(a greenfield service with no historical logs/metrics): it prescribes standing up a low-fidelity
data source (even a synthetic health-check ping) as a stopgap rather than waiting, states plainly
that *"your first attempt at an SLI and SLO doesn't have to be correct; the most important goal is
to get something in place and measured"* and to *"set up a feedback loop so you can improve,"* and
its own canonical example SLO document derived several SLO categories (correctness, completeness,
freshness) not from measurement at all but from author judgment, then simply verified the service
was already meeting them — a direct, real precedent for a judgment-picked first-pass target.
Crucially, the Workbook draws a hard line between this minimal SLO-definition step and a full
error-budget *policy*, which additionally requires organization-wide stakeholder sign-off and the
on-call engineers' agreement that the target is achievable without burnout — Kelvran genuinely
lacks both of those (no users, no on-call rotation yet), so that heavier artifact is correctly out
of scope, but the lighter one is not.

**Build now**: a first-pass SLO definition using metrics Kelvran already emits — e.g. a rounded
p99 latency target on `gen_ai.client.operation.duration` per provider, and an error-rate budget
using the existing `error.type` span/metric attribute — explicitly labeled as a judgment-picked
draft (per the Workbook's own transparency requirement) to be revised once real traffic exists.
This directly upgrades `docs/operations/TELEMETRY.md`'s "Key SLIs/SLOs" section from bare metric
names to an actual number, which is dashboard-worthy today.
**Not yet**: a formal error-budget policy (named authors/reviewers/approvers, escalation path,
organizational commitment to use the budget for prioritization decisions). Named trigger: Kelvran
has its first real deployed user/tenant and a designated on-call owner who can credibly agree the
target is achievable — before that point there's no one to get sign-off from.

### Finding 3 — Always-on tracing is correctly deferred; OTel's own threshold quantifies exactly when to revisit
**Confidence: high** (5 sources: OTel primary docs 3-0 x3, MLflow blog 3-0/2-1)

The tracing RFC's own Unresolved Questions section already flags this, unprompted: *"Sampling:
this RFC ships with the SDK's default `ParentBased(AlwaysSample())` — every request is traced.
Revisit if/when trace volume becomes a real cost concern; no evidence of that yet."* OTel's own
sampling guidance gives that vague trigger a concrete number: consider sampling once a system
generates roughly 1000+ traces/second, and explicitly says it's fine to avoid sampling entirely at
tens of traces/second or fewer — Kelvran, pre-launch with no real traffic, is nowhere near either
threshold, so the RFC's existing deferral is correct, not stale.

When that trigger does fire, the right design per both OTel's own docs and a July-2026 MLflow
observability-strategy piece is a **hybrid**, not flat, sampler: head-based (trace-ID + percentage,
decided at span-creation, keeps whole traces intact with no missing spans) for the bulk of
traffic, combined with tail-based/adaptive rules that always keep errors, slow requests, and rare
operations. The MLflow piece adds an LLM/agent-specific argument worth carrying into Kelvran's
design directly: agent runs are non-reproducible (the same prompt rarely produces an identical
output twice), so a dropped trace on an `agent_run_id`-bearing request can be the *only* record of
why the agent did what it did — meaning any future sampler should default to full-fidelity for
traced agent runs specifically and only sample the plain, non-agentic request volume.

**Build now**: a `telemetry.sampling` config knob (e.g. `ratio: 1.0` default, wired through the
existing `Config` struct alongside `Exporter`/`OTLPEndpoint`) that defaults to today's
`AlwaysSample()` behavior — cheap to add, zero behavior change, and removes the need for a future
breaking config-shape change when sampling is actually needed.
**Not yet**: actually implementing a non-trivial (head+tail hybrid, agent-run-aware) sampling
policy. Named trigger: sustained request volume approaching OTel's own ~100-1000+ traces/sec
guidance band, or a real, measured OTLP backend cost/storage line item — neither exists yet, and
building the policy before either exists means designing against a guess.

### Finding 4 — Real gap: `fallback_chains` intermediate-hop failures are invisible in all telemetry
**Confidence: high (direct source verification)** — grounded directly in
`gateway/internal/gateway/dataplane/{dataplane,fallback}.go`, not the adversarial web-research
swarm; no span/log instrumentation exists to check against externally, so this is a codebase fact,
not a claim needing external verification.

The tracing design does produce one coherent trace per request across a fallback walk — there is
no disconnected-spans-per-hop problem, and `zero` `span.AddEvent` calls exist anywhere in
`dataplane.go`, confirming no attempt was made (successfully or not) at richer per-hop
instrumentation. The real gap is different and more subtle: `runMissPath` (dataplane.go)
constructs exactly one `fallbackInfo{happened, from, reason}` value, and it's set **once**, from
only the *original* (first) deployment's failure — `fallback.from = originalDep.Name`,
`fallback.reason = originalErr.Error()` — before handing off to `attemptFallbackChain`
(fallback.go), which then walks the full configured target list internally with its own loop,
consecutive-failure counter, and per-hop capability/rate-limit/capacity checks, but reports back
only a single winning `(dep, resp, err)` triple. Concretely: for a chain `dep1 → dep2 → dep3` where
dep1 and dep2 both fail before dep3 succeeds, the resulting span attributes, the
`GatewayDecisionEvent`, and every log line will show "started at dep1, dep1 failed for reason X,
served by dep3" — dep2's attempt and its specific failure reason are silently dropped from every
telemetry surface (span, `gatewayevents_v1`, and logs alike). An operator debugging "why did this
request take 3x longer than expected" or "why is dep2 unhealthy" from traces alone cannot see that
dep2 was even tried.

**Build now**: emit a `span.AddEvent("fallback_hop", attrs)` (or append to a repeated field on
`GatewayDecisionEvent`) inside `attemptFallbackChain`'s loop for every real attempt, not just the
first and the winner — capturing deployment name, error, and elapsed time per hop. This is a
small, additive, low-risk change (the loop already has all the needed data in scope at the point
each attempt fails) and directly serves the cost/observability story `PRD.md` already commits to
("why did this agent run cost $X" implies "why did this specific request take 3 hops").
**Not yet**: nothing else here needs deferral — this is a real, cheap-to-fix gap with no
meaningful trigger to wait for, unlike Findings 1-3. Flagging it as "build now" without a
speculative future condition attached.

### Finding 5 — Real gap: `trace_id` correlation exists but is one level too deeply buried for most log backends; most log lines have none at all
**Confidence: high (direct source verification)** — grounded directly in
`gateway/internal/gateway/dataplane/dataplane.go`'s `finalize`/`logRequest` functions.

`GatewayDecisionEvent` (the `gatewayevents_v1` structured decision line) genuinely does carry
`TraceId`/`SpanId`, populated from `span.SpanContext()` at `finalize()` — so the underlying data
exists. But `logRequest` only attaches it by `protojson.Marshal`-ing the whole event and appending
the result as a **string value** of a `gatewayevents_v1` field inside the outer `slog` JSON line
(`fields = append(fields, "gatewayevents_v1", string(encoded))`). The practical consequence: a log
backend doing standard trace-log correlation (e.g. Grafana's Loki↔Tempo derived-fields pattern,
which expects a directly-parseable top-level `trace_id` label/field) cannot join on it without a
second, nested JSON-parse pass — parse the outer `slog` line, then re-parse the *string value* of
its `gatewayevents_v1` field as JSON again to reach `trace_id`. That's a real, checkable gap
against "usable in a real backend," not a hypothetical one. Worse: this double-nested `trace_id`
only appears on the `chat_completion` log line (success and error paths in `logRequest`) — every
other structured log line in the gateway (`ratelimit_backend_unavailable`,
`ratelimit_backend_unavailable_fallback_hop`, `deployment_ratelimit_backend_unavailable`,
`lexical_cache_search_failed`, `guardrail_blocked_precall`/`_postcall`,
`health_probe_deployment_unhealthy`, `budget_warn_threshold_crossed`,
`streaming_runaway_guard_triggered`, `streaming_midstream_reservation_topup_exhausted`,
`stream_missing_usage`, `budget_persist_failed`, `gatewayevents_marshal_failed`) is emitted via a
plain `slog.Logger.Warn/Info` call with zero trace context — not even `WarnContext`/`InfoContext`
threading a `context.Context` that a custom `slog.Handler` could pull `trace_id` from
automatically. Confirmed via full grep: 17 total `slog.*`-related call sites in `gateway/internal`,
zero of which include a `trace_id`/`span_id` field, and only one (`telemetry_metrics_shutdown_flush_failed`)
uses the `*Context` variant at all (and that one has no request-scoped span to pull from anyway).

**Build now**: (a) promote `trace_id`/`span_id` to top-level `fields` in `logRequest` (a one-line
change — the values are already computed at that call site via `event.TraceId`/`event.SpanId`),
so a log backend can join without double-parsing; (b) thread `ctx` into the handful of
mid-pipeline `Warn` calls (guardrail blocks, rate-limit fail-open, streaming runaway guards —
all already have a live `ctx`/`span` in scope) and extract `trace.SpanContextFromContext(ctx)` to
attach `trace_id` there too. Both are small, mechanical, low-risk additive changes with an
existing pattern to copy (`logRequest` already does the extraction once).
**Not yet**: standing up an actual Loki/Tempo (or equivalent) backend to prove the correlation
works end-to-end — per `docs/operations/TELEMETRY.md`, no backend has been chosen yet, so there's
nothing live to validate against beyond the log-shape fix above. Named trigger: first real
observability-backend selection/deployment (currently "shape only, no backend chosen").

### Finding 6 — Additional 2026 LLM-gateway observability-survey items worth naming
**Confidence: medium** (single/blog-tier sources per item, but each independently sound and
consistent with mainstream practice)

- The MLflow "full-fidelity tracing for agentic workloads" argument (Finding 3) generalizes beyond
  sampling: it's really an argument that *any* future data-retention/dropping policy (sampling,
  log rotation, span-attribute truncation) should carve out an exception for `agent_run_id`-bearing
  traces specifically, since Kelvran's own differentiator (`PRD.md`'s "why did this agent run cost
  $X") depends on that exact data surviving. **Not yet** actionable as a concrete change (no
  retention/rotation policy exists yet to carve an exception into), but worth keeping in mind the
  next time such a policy is designed — named trigger: whenever a log/span retention or rotation
  policy is first proposed, agent-run traces should be reviewed for an explicit carve-out at that
  same time, not after.
- Provider/OTel attribute churn is a standing maintenance cost, not a one-time fix: the spec's most
  recent commit as of this writing was yesterday (2026-09-10), and the repo split itself (June
  2026) is a reminder that "verified against the registry" has a shelf life. **Not yet** worth a
  recurring calendar job or CI check given the project's explicit skip-CI-version-matrix decision
  from prior research rounds, but worth a note in `docs/agents/MEMORY.md`-style tracking that the
  authoritative source moved repos, so a future check doesn't waste time on the old, now-redirected
  location.

## Caveats

- Findings 4 and 5 are original, codebase-grounded findings produced directly by this synthesis
  pass (reading `dataplane.go`/`fallback.go` line-by-line), not part of the 20 adversarially
  3-vote-verified web-research claims supplied upstream — they carry no external vote count, but
  are verified against the actual current source, which is the strongest possible evidence for a
  claim about what the code does today.
- The GenAI semantic-conventions space is explicitly called out by the task as "actively evolving"
  — Finding 1's "zero stabilization" conclusion is current as of 2026-09-11 (commits as recent as
  2026-09-10 were checked) but has no long shelf life; the next check should re-verify against
  whatever repo/location is authoritative at that time, not assume this one persists.
  Confirmed twice independently, one refuted claim tried to assert `gen_ai.system` →
  `gen_ai.provider.name` happened in a specific dated version (v1.37.0) and was voted down 1-2 —
  that rename's *existence* isn't in question (Kelvran's own RFC already implements the
  post-rename name), only the specific version/date attribution was unverifiable, so it's omitted
  above rather than asserted with a possibly-wrong version number.
- Two other claims were refuted and are deliberately excluded: a "single span per full logical
  operation across retries" spec requirement (0-3, not a real spec mandate — Finding 4 above is
  based on direct code reading instead, not this refuted spec claim) and a claim that Google's
  canonical example SLO document derived *all* its targets from measured data (0-3 — it actually
  derived several from author judgment, which is what Finding 2 relies on).
- Finding 6's items are lower-confidence (blog/single-source) by nature of being forward-looking
  "worth naming" items rather than checkable current-state facts — treat them as things to revisit
  when their named trigger fires, not as an immediate backlog item.

## Open Questions

- If/when a log backend is chosen (Finding 5's "not yet" trigger), does that backend's own
  trace-log correlation feature (e.g. Loki's Tempo derived-fields, Datadog's log-trace linking)
  have a specific top-level field-naming requirement that should shape exactly how `trace_id` gets
  promoted, rather than picking a name arbitrarily now?
- Finding 4's fix (per-hop span events) will change `GatewayDecisionEvent`'s effective information
  content if extended to the protobuf contract rather than just span events — does that count as a
  breaking `api/` change requiring a `buf breaking`-gated RFC, or is an additive-only repeated
  field safe under the existing contract-evolution rules in `AGENTS.md`?
- Does Kelvran want its first SLO draft (Finding 2) published in `docs/operations/TELEMETRY.md`
  directly, or as a separate `SLO.md`/decision doc, given `SECURITY.md`'s precedent of marking
  aspirational pre-release targets explicitly as non-SLA?
- For Finding 3's future sampling policy, should the `agent_run_id`-presence carve-out (always
  full-fidelity for agent-run-bearing traces) be a hardcoded rule or its own config knob — this
  wasn't resolved by any source consulted and is a real product design choice for whoever builds
  the sampler when the volume trigger fires.
