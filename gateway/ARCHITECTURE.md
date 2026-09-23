# gateway — Architecture

Go binary. Contains the Gateway (routing/proxying) and Cache (embedded, internal module) capabilities in one process. This document describes the internals of that binary. For the whole-system view (how `gateway` relates to `evals`), see the root `ARCHITECTURE.md`.

## Package Layout

```
/cmd/gateway              — main binary entrypoint (single static binary)
/internal/gateway
    /controlplane          — config compilation, cert rotation, metrics; infrequent, "slow and smart"
    /dataplane              — accept/filter/forward hot path; continuous, "dumb and fast"
/internal/adapter/{openai,anthropic,gemini,bedrock,openaicompat}
                           — bidirectional (canonical↔native) request/response transformers, one per provider.
                             ("vertex" was named in this list in an earlier pass but was never a real
                             package — a doc-vs-code staleness instance, corrected here per
                             docs/agents/AGENTS_LEARNING.md's catalogued pattern. Vertex AI's OAuth2/
                             service-account credential flow remains a real, separate, unaddressed
                             surface — see docs/rfcs/2026-09-04-gemini-adapter.md's Unresolved
                             Questions — this package list names real Go packages only, not future work.)
                             openai, anthropic, openaicompat (real per docs/rfcs/2026-09-04-openaicompat-
                             adapter.md — for generic self-hosted OpenAI-compatible runtimes: vLLM, Ollama,
                             TGI, llama.cpp, LocalAI), and gemini (real per
                             docs/rfcs/2026-09-04-gemini-adapter.md) all implement
                             streaming.StreamingAdapter (real, stateful per-request StreamDecoder each).
                             bedrock also streams — both its buffered Converse API (real per
                             docs/rfcs/2026-09-04-bedrock-adapter.md) and ConverseStream (real per
                             docs/rfcs/2026-09-04-bedrock-converse-stream.md) — but deliberately does NOT
                             implement streaming.StreamingAdapter: ConverseStream's real wire format is
                             binary (application/vnd.amazon.eventstream), not SSE, so bedrock.StreamDecoder
                             is a concrete, non-interface type decoding eventstream.Message directly, driven
                             by a genuinely separate dispatch — dataplane's streamDeployment special-cases
                             `dep.Provider == "bedrock"` at its own top and forwards to a sibling
                             streamDeploymentBedrock, mirroring the existing per-provider-string-switch
                             convention setUpstreamAuthHeaders/streamUpstreamURL already use, rather than
                             forcing a shared interface across two incompatible wire framings for a single
                             binary-framed implementor. Bedrock's streaming URL is also a path-SEGMENT swap
                             (/converse -> /converse-stream, confirmed against aws-sdk-go-v2's own
                             serializers.go) — a genuinely different derivation from Gemini's colon-suffix
                             swap (:generateContent -> :streamGenerateContent), both handled by the same
                             streamUpstreamURL function. dataplane.ErrStreamingNotSupported (a typed 400,
                             never a silent buffering fallback) is still real code but currently only fires
                             for a future provider added without a streaming implementation, or a
                             misconfigured registry entry for "bedrock" whose value isn't *bedrock.Adapter —
                             every provider actually registered in cmd/gateway/main.go streams today. bedrock
                             is also the first adapter needing a genuine Deployment/config-schema change —
                             AWS SigV4 request signing needs an access-key-id/secret-access-key/region
                             credential shape, not the single bearer-token secret every other provider
                             fits (DeploymentConfig.AccessKeyIDEnv/SecretAccessKeyEnv/SessionTokenEnv/
                             Region; dataplane.go's setUpstreamAuthHeaders now signs over the real request
                             body and can genuinely fail, unlike every other provider's infallible header-
                             setting branch). The real AWS SigV4 service-signing name for Bedrock Runtime
                             is "amazonbedrockfrontendservice", confirmed directly against aws-sdk-go-v2
                             source — not "bedrock," a plausible-sounding but wrong guess the initial
                             grounding research made and this RFC's own research trail corrects.
                             openaicompat is a near-verbatim copy of openai's adapter, deliberately: the
                             wire format itself is uniformly OpenAI-compatible across every self-hosted
                             runtime surveyed while grounding that RFC — real, sourced differences exist
                             only at the response-content level (finish_reason values, tool-calling opt-in
                             gating), not the wire-shape level, and are already handled correctly by the
                             existing design (FinishReason is an open string, not a closed enum; unknown
                             response fields are ignored by default). gemini is a genuine-translation
                             adapter (like anthropic, not a near-copy) — Gemini's real API has no
                             "system"/"tool" role, requires resolving a functionResponse's required "name"
                             field from message history (the canonical role:"tool" message only carries
                             ToolCallID), and — the one real cross-cutting change no prior adapter needed —
                             uses a genuinely different URL (:generateContent vs
                             :streamGenerateContent?alt=sse) for buffered vs. streaming calls, derived at
                             call time by dataplane.go's streamUpstreamURL rather than a second config field
/internal/streaming        — transport-level SSE plumbing, provider-agnostic: canonical ChatCompletionChunk/
                             ChunkChoice/MessageDelta/ToolCallDelta types, the StreamDecoder/StreamingAdapter
                             interfaces every streaming-capable adapter implements against, and the actual
                             Reader (SSE frame parser)/Writer (SSE frame writer, Flush()-per-chunk) — ACTIVE.
                             **Explicitly out of scope for v2, per docs/upgrade-research/gateway-realtime-
                             streaming-2026-09-11.md's Finding 5**: bidirectional/WebSocket realtime
                             (OpenAI Realtime-API-style voice/multimodal streaming). Every real production
                             precedent that research found (LiteLLM, Portkey) is purpose-built for OpenAI's
                             voice use case specifically, with no generalized bidirectional text-completion
                             analog anywhere, and no confirmed Kelvran customer demand for one. This SSE-
                             only design — with its own real resilience (runaway-completion guard, mid-
                             stream budget/TPM reservation top-up) — is unaffected and remains the v2 design.
                             **Added 2026-09-23**, per docs/upgrade-research/streaming-transport-protocol-
                             evolution-2026-09-22.md: re-confirms, doesn't merely re-assert, that SSE
                             remains correct for this layer — every major LLM provider (OpenAI, Anthropic,
                             Gemini, Cohere, Mistral) still streams tokens over SSE in 2026, and no
                             production LLM gateway (Kong, Envoy AI Gateway, LiteLLM, Portkey) has moved
                             its default token-delivery path off it. WebTransport reaching browser
                             "Baseline" status in March 2026 doesn't change this verdict — every source
                             discussing it for AI workloads frames it around bidirectional, partially-
                             unreliable traffic (cloud gaming, live video, collaborative cursors), not the
                             ordered, reliable, one-way delivery this package already provides. gRPC
                             server-streaming for LLM completions is real, shipping infrastructure
                             elsewhere, but oriented at GCP-IAM-authenticated service-to-service traffic
                             (Vertex AI dedicated endpoints), not the API-key-authenticated public surface
                             this package's adapters actually call — a real developer-forum report shows
                             gRPC access to Gemini's own public API-key endpoint failing outright. One
                             separate, code-grounded fact worth naming even though it doesn't change this
                             verdict: `cmd/gateway/main.go`'s client-facing `*http.Server` calls plain
                             `ListenAndServe()` with no `TLSConfig`/h2c wiring, so the Go process itself
                             only ever speaks HTTP/1.1 — whatever HTTP/2 or HTTP/3 a real client sees
                             depends entirely on an ingress/load-balancer this repo does not commit a
                             manifest for (`deploy/k8s/base/` has no `Ingress` resource). Worth
                             investigating against a real deployment's own ingress config if HTTP/2-level
                             behavior (e.g. multiplexing) ever matters — not something to change inside
                             this package, which is unaffected either way.
/internal/router          — **ACTIVE**, per docs/rfcs/2026-09-04-weighted-routing.md: weighted round-robin
                             deployment selection (the LVS/IPVS `wrr.c` smooth-WRR algorithm — O(1) state
                             per deployment, no goroutine, no ticker), closing the "weighted" half of
                             PRD.md's v1 routing scope line ("static + weighted routing; a single fallback
                             chain"). `dataplane.Pipeline.nextDeployment` is now a thin wrapper delegating
                             to `router.Router.Select`; the old inline atomic-counter round-robin and
                             `deploymentsByModel` map it replaced are gone. Equal weights (including the
                             unset/default case) provably degrade to the exact same sequence the old
                             round-robin produced — proven by hand-trace in the RFC, not merely assumed.
                             Active/synthetic health-probing is now real too, per
                             docs/rfcs/2026-09-07-gateway-active-health-probing.md: `router.Router` tracks
                             each deployment's own consecutive-probe-outcome health state
                             (`ReportProbeResult`/`IsHealthy`) and `Select` skips any deployment currently
                             marked unhealthy (failing open — returning a known-bad deployment rather than
                             "no deployment configured" — only if every deployment for a model is
                             unhealthy at once). The N-of-M consecutive thresholds (3 failures to exclude,
                             2 successes to re-include, both configurable) are this package's own
                             adapter-agnostic bookkeeping only; the probe LOOP itself (issuing the actual
                             lightweight synthetic request on a timer, per `health_probe.interval_seconds`,
                             default 300s) lives in `dataplane.Pipeline.ProbeDeployments`/
                             `RunHealthProbeLoop`, since only `dataplane` has access to each deployment's
                             BaseURL/adapter/credentials — this package still has zero I/O and zero
                             `internal/adapter` dependency. **Updated 2026-09-07**: `dataplane.go`'s router
                             step also now supports error-classified, multi-hop fallback chains
                             (`Deployment.FallbackChains`, per
                             docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md) — an
                             explicit, per-deployment, per-error-class (content-policy /
                             context-window-exceeded / generic) ordered target list, which MAY name a
                             deployment serving a different canonical model (e.g. a larger-context
                             fallback for a context-window error). This is a bounded, opt-in extension of
                             the prior "a single fallback chain" scope line — a deployment with no
                             `fallback_chains` configured keeps the exact prior same-model,
                             single-fallback-via-router behavior unchanged, and now composes with
                             health-probing for free: a probe-excluded deployment is never even chosen as
                             the primary pick, so the fallback path (either the new chain or the old
                             single-hop rule) fires less often, not differently. Still not built,
                             deliberately, and now genuinely narrowed rather than a blanket deferral:
                             usage/latency/cost-based selection signals, automatic model-*group*
                             load-balancing (LiteLLM's sense of that term — distinct from this pass's
                             explicit, opt-in per-deployment fallback chains), and — the one class that
                             correctly stays fully deferred, not just narrowed — the TRAFFIC-DERIVED
                             statistical circuit breaker (Envoy-style outlier detection, LiteLLM-style
                             `allowed_fails` cooldown), which genuinely needs a request-volume floor
                             Kelvran has no production traffic yet to calibrate against. None of these are
                             named in PRD.md's v1 allowlist. **Updated 2026-09-08**: the production probe
                             LOOP (`dataplane.RunHealthProbeLoop`) no longer drives `ProbeDeployments`
                             unconditionally on a flat ticker — `probeDueDeployments`/`rescheduleDeployment`
                             (per docs/rfcs/2026-09-08-gateway-health-probe-backoff.md) skip any deployment
                             whose own per-deployment schedule entry isn't due yet: a deployment
                             `router.IsHealthy` currently reports as unhealthy has its own probe interval
                             backed off by 2x per consecutive unhealthy reschedule (capped at 8x the
                             configured interval), resetting to the plain cadence the instant it reports
                             healthy again — reducing probe load against an already-struggling dependency,
                             mirroring AWS's 2015 DynamoDB postmortem. Separately, `router.Router`'s own `HealthConfig` gained a
                             post-recovery weight ramp (`RecoveryRampSteps`/`RecoveryRampInitialPercent`,
                             `health.go`'s `admitRampedTurn`): a just-recovered deployment starts admitted
                             at only a small percentage of its configured `Weight` and ramps linearly to
                             100% over further consecutive successful probes, rather than immediately
                             receiving its full weighted share the instant it crosses the recovery
                             threshold — closing a thundering-herd-on-recovery gap the plain N-of-M
                             threshold model left open. `ProbeDeployments` itself and `router.go`'s own
                             `Select`/`ReportProbeResult` surface are unchanged by either feature.
                             **Updated 2026-09-12**: `Deployment` gained an optional `CostTier` field (a
                             *configured*, operator-set cost-preference band — never a learned or
                             usage-derived signal, and still explicitly NOT what "usage/latency/cost-based
                             selection signals" above refers to). `health.go`'s `activeCostTier`/
                             `selectHealthy` prefer the lowest tier with a currently-healthy deployment,
                             falling through to the next tier only once every deployment in the cheaper one
                             is unhealthy — layered outside `wrr.go`'s own cursor math, the same way the
                             ramp feature is, so a model group with no tier configured (every deployment
                             configured before this existed) is byte-for-byte unchanged. Strict opt-in: a
                             group with even one untiered deployment disables filtering for that whole
                             group. Still explicitly NOT built: automatic price discovery. `CostTier` is
                             per-deployment within one canonical model's existing WRR pool, not a new
                             grouping concept; see DECISIONS.md's `[2026-09-12]` entry for why `PriceTable`
                             (keyed by canonical model, identical for every deployment sharing one `Model`
                             value) couldn't already express per-deployment price differentiation.
                             **Corrected 2026-09-14**: this paragraph previously also named cross-model
                             "virtual model" grouping itself (a single client-facing name spanning several
                             genuinely different real providers/models, the way LiteLLM/Envoy AI Gateway/
                             Kong/Portkey each ship) as NOT BUILT — false. `controlplane.DeploymentConfig`'s
                             own doc comment already states "multiple deployments may share the same
                             Model," with no homogeneity requirement anywhere in config parsing, router
                             construction, or dataplane's routing path — an operator can build exactly this
                             pattern TODAY with zero new code, simply by giving two or more deployments the
                             same `Model` value and different `Provider`/`UpstreamModel`/`BaseURL`. Proven
                             end to end (not just reasoned about) by
                             `TestIntegrationVirtualModelAliasFansOutAcrossProviders`
                             (`cmd/gateway/virtual_model_alias_integration_test.go`): a single alias name
                             fanned out across a real OpenAI-shaped and a real Anthropic-shaped upstream,
                             5/5 over 10 requests. The one real, still-unbuilt half is price
                             differentiation across such an alias's own members, named above — every
                             deployment sharing one alias `Model` is billed at that ONE canonical model's
                             `PriceTable` entry regardless of which real provider actually served the
                             request (`realServingModel` returns `dep.Model`, the shared alias name, never
                             `dep.UpstreamModel`). An operator building a genuinely different-priced alias
                             today must either price it conservatively at the more expensive member's rate
                             (never undercounting) or avoid mixing different-priced models under one alias
                             until per-deployment pricing exists. See
                             docs/upgrade-research/competitor-feature-parity-2026-09-14.md Finding 1.
                             **Added 2026-09-17** (missing from this entry until now, caught by a doc-
                             staleness sweep): `Deployment` gained an optional `Sticky bool` flag, and
                             `router.go` a `stickyGroups map[string]bool` (per-model-group, OR semantics —
                             deliberately different from `CostTier`'s all-or-nothing gate). New
                             `sticky.go`: `hashStickyKey`/`stickyPick` bucket a caller-supplied key (the
                             virtual key's own ID) via a monotonic threshold hash
                             (`hash(key) % 10000 < canaryThreshold`), not an N-way cumulative-range-mod-
                             `sumW` scheme — the latter reintroduces non-monotonicity on every weight edit,
                             the whole point sticky canary routing exists to avoid (a canary weight ramp
                             must only ever ADD newly-bucketed callers, never bounce an already-canary one
                             back to stable). `Router.SelectSticky` is a genuinely separate entry point
                             from `Select` (byte-for-byte unchanged) — `dataplane.Pipeline.
                             nextDeploymentSticky` calls it as `runMissPath`'s and
                             `HandleChatCompletionStream`'s own FIRST pick (never a fallback/re-pick site,
                             which wants "not this one," the opposite of stickiness), falling through to
                             plain `selectHealthy` whenever the sticky pick also fails `exclude`/health/
                             cost-tier filtering — never bypassing those safety gates.
                             **Added 2026-09-23, doc-vs-code staleness fix**: this section previously
                             never credited a real Phase 6 feature shipped earlier the same day —
                             `health.go`'s `admitLatencyThinnedTurn`/`latencyCredit` (mirroring the
                             existing ramp-recovery gate's own accumulator shape) apply a soft,
                             multiplicative de-weighting factor on top of a deployment's configured
                             `Weight`, driven by `dataplane.updateLatencyDeweighting`'s per-deployment
                             EMA of real probe-call latency (`latencyEMAAlpha = 0.3`) — never a hard
                             exclusion, so a slow-but-healthy deployment still receives some traffic,
                             just proportionally less. `Router.SetLatencyFactor` is the one new public
                             entry point; `Select`/`ReportProbeResult` are otherwise unchanged. **Also
                             added 2026-09-23**, per docs/upgrade-research/self-hosted-inference-server-
                             integration-depth-2026-09-22.md: a real, currently-unread signal family
                             exists beyond the latency EMA above — every self-hosted runtime the
                             `openaicompat` adapter targets (vLLM, SGLang, TGI, llama.cpp) exposes its
                             own native queue-depth/KV-cache-utilization telemetry (e.g. vLLM's
                             `vllm:num_requests_waiting`/`vllm:kv_cache_usage_perc`) over a separate
                             `/metrics` (or, for llama.cpp, `/slots`) endpoint this router never scrapes.
                             The deepest coordination tier for this signal (precise KV-cache-event-
                             driven prefix-cache routing, disaggregated prefill/decode orchestration) is
                             deliberately out of scope — that tier is real and production-proven, but
                             lives one architectural layer down, in dedicated sidecar/ext-proc systems
                             (the Kubernetes Gateway API Inference Extension's Endpoint Picker, most
                             visibly packaged as GKE Inference Gateway/llm-d) that Kelvran's own single-
                             process, provider-agnostic shape was never designed to replicate. A
                             shallower tier — scraping the same `/metrics`/`/slots` endpoint as a soft
                             pre-filter feeding this same latency-de-weighting mechanism — is a real,
                             evidence-backed gap (LiteLLM's own GitHub issue #37622 shows its request-
                             count-only `least-busy` strategy routing to a 98.8%-KV-cache-saturated
                             instance while a sibling sat fully idle, purely because it never reads
                             backend state) but is correctly left unbuilt pending a bounded RFC, not
                             built speculatively — named future work, not silently dropped. Separately,
                             the same report resolves "speculative decoding" as a router-relevant
                             concept to NOT APPLICABLE, not merely unbuilt: token-level speculative
                             decoding (a draft model proposing candidates a larger model verifies within
                             one inference engine) has zero wire-visible signal — the client-facing
                             response is identical whether or not the backend used it internally, so
                             there is nothing for this router to coordinate on. The similarly-named but
                             structurally different *response-level* speculative decoding/model
                             cascading (two separate HTTP-addressable endpoints, a cheap model drafts,
                             an expensive one verifies) is the same "cost-aware model cascading"
                             question `DECISIONS.md`'s `[2026-09-14]` and `[2026-09-20]` (third entry)
                             entries already researched twice and correctly left `not_yet` for lack of
                             an accurate request-level confidence/refusal signal — this report found no
                             new evidence changing that verdict. **Also added 2026-09-23**, per
                             docs/upgrade-research/carbon-aware-sustainable-routing-2026-09-22.md: no
                             carbon-aware routing signal exists here, and none is planned — reconfirmed,
                             not merely re-asserted, since two of Kelvran's five adapters (OpenAI,
                             Anthropic) publish zero real energy/carbon data for their own models today,
                             which would leave any such feature flying blind for the two adapters likely
                             carrying the most traffic. The existing, unrelated `CostTier` mechanism
                             above is a reasonable energy proxy already, once built — no separate
                             carbon-specific code path is planned to duplicate it.
/internal/ratelimit        — per-virtual-key token bucket — ACTIVE, per
                             docs/rfcs/2026-09-03-distributed-rate-limiting.md. In-memory by default
                             (single-process); optionally Redis-backed (internal/ratelimit/redislimiter,
                             a Lua script over go-redis, atomic across any number of gateway instances)
                             when `rate_limit.redis_addr` is configured — a Redis backend error fails
                             open (logged, request allowed), since internal/budget's per-key USD cap is
                             an independent backstop. A consumer x model dimension is now real too, per
                             docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md:
                             KeyConfig.PerModel lets one virtual key give a specific model its own,
                             entirely separate RPM bucket (checked before, never alongside, the key's own
                             default bucket) — enforced in BOTH in-memory and Redis mode, unlike TPM
                             (still in-memory-only). Provider/header/path matching (the rest of Kong's own
                             multi-dimensional shape) remains scoped-out future work, deliberately, not yet
                             cheaply addable at checkRateLimit's current (virtual key, model)-only view of
                             a request — the concrete design (what new data checkRateLimit would need, how
                             it would thread through dataplane.Pipeline, per-dimension matching semantics,
                             and an honest effort estimate/recommendation) is now written up in
                             docs/rfcs/2026-09-07-gateway-ratelimit-provider-header-path-dimensions.md.
                             **Corrected 2026-09-22**: that RFC's own path-matching trigger ("Kelvran has
                             more than one HTTP route") DID fire — POST /v1/embeddings shipped 2026-09-17
                             and calls checkRateLimit with the identical (vk, model) signature chat
                             completions uses, no path differentiation. Re-examined per the RFC's own
                             instruction to "revisit this design fresh" rather than left silently stale —
                             found the underlying concern (a shared bucket letting embeddings traffic
                             contend with chat-completion traffic) is already addressable today via the
                             already-shipped PerModel override, keyed on the embedding model's own name,
                             with zero new code — see DECISIONS.md's 2026-09-22 entry. The general
                             N-dimension path/header/provider matching machinery therefore remains
                             deliberately unbuilt, now for a stronger, re-verified reason than "no second
                             route yet." Hierarchical
                             scope resolution (org/team/user/session) remains
                             target-only, same boundary as identity's own scope deferral below.
                             **Added 2026-09-23**, per docs/upgrade-research/edge-native-ai-gateway-
                             deployment-2026-09-22.md: edge-native rate limiting (Cloudflare Workers Rate
                             Limiting API, Cloudflare WAF rate-limiting rules, Vercel's WAF) was evaluated
                             and found to be a LESS globally-accurate model than this package's own
                             Redis/GCRA-backed cross-replica design above — deliberately per-PoP/local and
                             eventually consistent by the vendors' own design, trading accuracy for speed,
                             the opposite tradeoff this package already made. Not an upgrade path to
                             revisit; recorded so it isn't re-litigated from scratch in a future edge/CDN
                             evaluation.
                             **Verified 2026-09-23**, per docs/upgrade-research/incident-postmortems-
                             failure-taxonomy-2026-09-22.md Finding 4 (a real LiteLLM production incident:
                             concurrent cold-start requests each created their own new Redis connection
                             pool via a check-then-create race in RedisCache.init_async_client(), spiking
                             connections from ~14 to a peak of 746 across 2 pods): checked directly
                             against this codebase's own three Redis-client-construction call sites, not
                             assumed. redislimiter.Open, redisbudget.Open, and configpropagation.Open
                             (internal/ratelimit/redislimiter, internal/budget/redisbudget,
                             internal/configpropagation) each call redis.NewClient exactly once inside
                             their own constructor, and every call site (newKeyLimiter/newBudgetTracker/
                             newConfigPublisher inside buildPipeline, plus the config-propagation
                             subscriber's own separate configpropagation.Open call directly in run())
                             executes exactly once, synchronously, on cmd/gateway/main.go's single-
                             threaded startup path (main -> run -> buildPipeline) — before the HTTP
                             server ever accepts a request. No lazy, per-request, or concurrent-cold-start
                             construction path exists for any of the three; LiteLLM's specific bug shape
                             (a Python redis.asyncio per-event-loop check-then-create race) has no
                             structural equivalent here. Confirmed clean — recorded so a future pass
                             doesn't redo this ~30-minute code read from scratch.
                             **Missing from this entry until now**: `ratelimit.ConcurrencyLimiter`
                             (internal/ratelimit/concurrency.go, per
                             docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md) bounds how many of
                             one virtual key's requests may be simultaneously outstanding
                             (`VirtualKeyConfig.MaxConcurrentRequests`) — a genuinely different dimension
                             from this package's own RPM/TPM buckets above, which bound how FAST a key
                             may issue new requests, never how MANY may be outstanding at once. The
                             identical type is reused, unchanged, for the per-deployment aggregate cap
                             (`DeploymentConfig.MaxConcurrentRequests`, wired as `Pipeline.
                             DeploymentConcurrency`, per docs/upgrade-research/gateway-per-deployment-
                             concurrency-2026-09-09.md) — a separate instance, separate scope, sharing
                             only the type. Deliberately scoped to the virtual-key ID only, never
                             `agent_run_id` — that value is client-supplied/unverified (W3C Baggage), so
                             enforcing anything on it would let a client bypass the control by rotating
                             the value per request; see docs/upgrade-research/multi-agent-fanout-
                             concurrency-composition-2026-09-23.md for a fresh, dedicated research pass
                             independently reconfirming this industry-wide (no production LLM gateway
                             safely nests a per-agent-run sub-limit inside a parent key's own allowance
                             either), and DECISIONS.md's matching entry for the full resolved verdict.
                             **Added 2026-09-23**, from that same research round: `AcquireWithRun`/
                             `ReleaseWithRun`/`InFlightByAgentRun` extend `ConcurrencyLimiter` with a
                             read-only, observability-only per-agent-run breakdown of one key's current
                             in-flight count (surfaced via `GET /admin/virtual_keys/{name}/inflight`) —
                             tracked for every key, capped or not, and never consulted by the admission
                             decision above. The pre-existing single-argument `Acquire`/`Release` are now
                             thin wrappers over the new methods with an empty run ID, so every one of
                             their ~15 existing call sites (including the deployment-scoped limiter,
                             which has no agent-run concept at all) is unaffected.
/internal/cache            — Cache's public interface — see "Cache Subsystem" below; this is the ONLY
                             package Gateway's request pipeline is allowed to import from Cache
    /port.go                — type Cache interface { Get, Put, Delete } — the sole import surface.
                             **Corrected 2026-09-14**: this entry omitted `Delete(ctx, key) error`,
                             added the same day to close a real GDPR erasure gap (no prior way to
                             purge one cached entry short of a full flush or waiting out TTL) — real
                             in the active `inprocess` implementation, a typed "not implemented"
                             stub in the two dormant `grpcserver`/`grpcclient` seams, matching every
                             other method there. See `THREAT_MODEL.md`'s Cache Repudiation row for
                             the real, disclosed limitation: `Delete` has zero live callers anywhere
                             outside its own tests today (no admin route, no CLI) — the primitive
                             exists, the operational capability doesn't yet
    /grpc/cache.proto        — **NOT BUILT.** No `.proto` contract exists for this seam —
                               confirmed via a repo-wide `find . -iname '*.proto'` (the only
                               one present is `api/gatewayevents/v1/gatewayevents.proto`,
                               unrelated to Cache). **Corrected 2026-09-12**, correcting this
                               line's own prior "contract defined now, unused" claim — see
                               docs/decisions/0002-cache-embedded-in-gateway.md's own
                               Corrected 2026-09-12 note for the full confirmation trail.
                               `grpcserver`/`grpcclient` below are real, typed Go code only;
                               the wire-format contract itself remains a genuine, disclosed
                               gap until/unless Cache is ever extracted
    /inprocess/              — adapter #1 — ACTIVE
    /grpcserver/             — adapter #2 — DORMANT
    /grpcclient/             — adapter #3 — DORMANT
    /internal/               — cache-private: eviction policy, embedding index, storage engine
/internal/identity          — virtual keys + per-key tenant resolution (real, hash-matched — ACTIVE);
                             teams/hierarchical scope (org -> team -> user -> key -> session) remain
                             target-only, per docs/rfcs/2026-09-02-virtual-keys-budgets.md's scope
                             boundary — zero deps upward
/internal/budget             — per-key cumulative USD spend tracking against an optional cap — ACTIVE.
                             Restart-durable via an optional bbolt-backed store (internal/budget/boltstore,
                             docs/rfcs/2026-09-03-budget-persistence.md) when `budget.persist_path` is
                             configured; pure in-memory (resets on restart) otherwise — single-instance
                             only, a deliberate, bounded stepping stone ahead of the Postgres control-plane
                             store below, not a replacement for it. **Added 2026-09-18**: a shared
                             `admin.on_corrupt_store: fail|reset` config knob (default `fail`, byte-for-
                             byte the original behavior for every config file that doesn't set it)
                             applies identically to this store AND identity's/prompt's own boltstore-
                             backed stores, via a single generic `openPersistStoreWithRecovery` helper
                             in cmd/gateway/main.go. `reset` renames a genuinely corrupt file aside
                             (preserving it for forensics, never deleting) and retries once against the
                             now-clear path. **Corrected same day, an audit finding**: this originally
                             classified ANY open error as corruption-worthy, meaning a permissions
                             misconfiguration or a disk-full error would have been silently reset (real
                             data loss) — fixed to match only bbolt's own three documented corruption
                             sentinels (`ErrInvalid`/`ErrVersionMismatch`/`ErrChecksum`, from
                             `go.etcd.io/bbolt/errors`) via `errors.Is`; any other error is now treated
                             exactly like `fail`, never reset.
/internal/adapter            — OpenAI/Anthropic/Gemini/Bedrock/self-hosted client wrappers
/internal/costaccounting     — token/$ metering, Decimal-precision ledger — real, per
                             docs/rfcs/2026-09-02-decimal-cost-accounting.md (github.com/shopspring/decimal,
                             the gateway's second external Go dependency family after OTel).
                             **Verified 2026-09-22**: this session's own research raised a theoretical
                             cache-token double-counting concern (OTel's `gen_ai.usage.input_tokens`
                             spec-definition includes both cache_read/cache_write subtotals). Checked
                             directly against the code, not assumed: `Calculator.Calculate` already
                             correctly subtracts `CacheReadTokens`/`CacheCreationTokens` from
                             `PromptTokens` before pricing; every adapter (`openai`/`openaicompat`/
                             `anthropic`/`bedrock`/`gemini`) independently populates all three fields
                             from each provider's own already-separate native usage fields, never from
                             a single combined field; and the OTel span emission
                             (`dataplane.go`'s `ChatCompletionResult.InputTokens`) is set directly from
                             `resp.Usage.PromptTokens`, with `CacheReadTokens`/`CacheCreationTokens`
                             emitted as separate, informational sibling attributes, not re-added on
                             top. No double-counting path exists anywhere in this pipeline.
/internal/telemetry          — real OTel spans per request (GenAI semantic-convention attributes,
                             agent_run_id via W3C Baggage) — ACTIVE, per
                             docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md. **Added 2026-09-14**:
                             kelvran.savings.usd (ChatCompletionResult.SavingsUSD) mirrors
                             kelvran.cost.usd's exact string-typed convention, set only on a genuine
                             cache hit — the one cardinality-safe, per-agent-run signal for cache
                             savings, since kelvran.cache.savings_usd (a Prometheus counter,
                             dimensioned by cache layer only) can never carry agent_run_id without an
                             unbounded-cardinality metric label. `api/otel/`'s versioned
                             cross-language contract remains deliberately deferred (OTLP already IS a
                             real wire format for this data; see that RFC's Motivation section). The
                             OTHER half of the shared contract — `api/gatewayevents/v1` (structured
                             per-request decision outcomes, not span data) — is real: `finalize` (see
                             /internal/gateway/dataplane below) is its producer, per
                             docs/rfcs/2026-09-03-api-gatewayevents-contract.md
    /cachecorrelation/       — **Corrected 2026-09-12**: also missing from a prior pass of this tree.
                             telemetry/cachecorrelation, per
                             docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md: a pure,
                             standalone Event{TenantID, Key, InstanceID, Hit, Timestamp, TTL}
                             correlation unit; Analyze([]Event) groups by (tenant, key) and flags a
                             miss as cross-instance-avoidable when a different instance had an open
                             TTL window for the same key. Registered in gateway/.go-arch-lint.yml as
                             its own cache-correlation component — a pure leaf like telemetry itself
                             (stdlib-only, zero project-internal imports). **Standalone analysis
                             unit, NOT wired into the live request pipeline**: dataplane's
                             checkCache/checkLexicalCache already emit the raw
                             cache_cross_instance_check log lines this package's Analyze would
                             consume, but the log-parsing glue that would actually call Analyze
                             against real data is itself deliberately deferred until a genuine
                             multi-instance deployment exists to measure.
    /spendvelocity/          — **Added 2026-09-14**: telemetry/spendvelocity, per
                             docs/upgrade-research/llm-cost-optimization-finops-2026-09-14.md
                             Finding 6: a pure, standalone two-sided CUSUM (cumulative sum)
                             change-point Detector — Observe(x) folds one observation into a
                             running cumulative sum and reports whether a sustained shift (either
                             direction) crossed Target±ThresholdSigma·Sigma. Registered in
                             gateway/.go-arch-lint.yml as its own spend-velocity component — a
                             pure leaf like cache-correlation above (stdlib-only, zero
                             project-internal imports). **Standalone analysis unit, NOT wired into
                             the live request pipeline**: no periodic sampler feeds it from
                             budget.Tracker's own per-key spend yet, and no alarm destination
                             (log line, OTel span event, admin endpoint) has been chosen — both
                             are deliberate product decisions the source research explicitly
                             declined to make on its own, not an oversight.
/internal/mcp                — **NOT BUILT.** Zero code exists (confirmed: no such directory under
                             gateway/internal/), explicitly out of scope for v1 per PRD.md. Intended
                             design: inbound (expose Kelvran's own APIs as MCP tools) + outbound (broker
                             agent tool calls) brokering — shares identity/costaccounting, not a second
                             gateway. **Added 2026-09-23**, per docs/upgrade-research/agent-memory-
                             context-management-2026-09-22.md — a deliberate non-build verdict, not a
                             deferred item: a Kelvran-native cross-request agent memory store (Zep/Mem0-
                             style long-term memory, distinct from this same request's own context
                             window) is explicitly out of scope, consistent with Kelvran's stated
                             stateless-per-request architecture and its own PRD.md non-goals. A caller
                             who wants provider-hosted persistent memory (e.g. OpenAI's Conversations
                             API) already gets it transparently today — Kelvran's canonical schema
                             passes such a request through as an ordinary opaque request/response,
                             requiring zero new code on Kelvran's side, since building a memory store
                             would mean Kelvran itself tracking cross-request state it deliberately
                             does not track anywhere else in this architecture.
/internal/guardrail          — pre/post-call middleware interface; PII/content checks — ACTIVE, per
                             docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md
/internal/alerting           — **Added 2026-09-18**, per docs/upgrade-research/operator-alerting-
                             integrations-2026-09-15.md Finding 4: a direct-from-Go webhook push for
                             operational signals the gateway already computes but never delivered
                             anywhere (today: budget-threshold-crossed). A pure leaf package (zero
                             project-internal imports — see the dependency direction rules below), so
                             any future signal source can depend on it without this package ever
                             knowing about any of them. Implements the Standard Webhooks specification
                             (standardwebhooks.com, verified directly against that spec's own current
                             text before implementing, not assumed): HMAC-SHA256 over
                             "id.timestamp.body", a "v1,<base64>" header shape, a `whsec_`-prefixed
                             base64 secret convention — the identical scheme
                             evals/evals/webhook.py implements in Python, so both deployables' webhook
                             sends are mutually consistent for any receiver verifying either one.
                             `alerting.Notifier.Notify`'s own delivery is async (its own goroutine via
                             `context.WithoutCancel`, never tracked against the shutdown WaitGroup — a
                             disclosed, accepted tradeoff: an in-flight retry can be abandoned mid-
                             backoff on process exit) and bounded (3 attempts, exponential backoff with
                             jitter, giving up rather than looping forever). **Corrected same day, an
                             audit finding**: the retry loop originally treated every failure
                             identically; a malformed webhook URL or a permanent 4xx response from the
                             receiver is now classified as non-retryable and stops the loop immediately,
                             rather than burning the remaining attempts on a failure retrying can never
                             fix — 5xx and transport-level errors are still retried exactly as before.
                             `newAlertNotifier` (cmd/gateway/main.go) returns a genuine nil
                             `alerting.Notifier` interface — never a typed-nil pointer — when
                             unconfigured, so `dataplane.Pipeline.alertNotifier != nil` stays a valid
                             nil-check; wired into `checkBudgetAlertLadder`'s existing threshold-crossed
                             path, gated on the new `alerting:` config section (`config.example.yaml`).
/internal/admin               — Real, per docs/rfcs/2026-09-05-gateway-admin-api.md: an optional,
                             off-by-default HTTP surface on its own separate net.Listener (never the
                             client-facing gateway's mux/port) exposing read-only config introspection
                             (GET /admin/config — safe to return wholesale, since Config never holds a
                             raw secret) plus the two sections made live-mutable in v1: virtual keys
                             (POST/DELETE /admin/virtual_keys/{name}, via a new
                             dataplane.Pipeline.UpsertVirtualKey/DeleteVirtualKey pair built around
                             identity.Verifier becoming an atomic.Pointer) and, per **Corrected
                             2026-09-16** below, a deployment's own routing weight. Auth is a deliberately
                             separate, two-tier static bearer credential space from client-facing virtual
                             keys — never delegates to identity.Verifier. An optional second, read-only
                             viewer credential (admin.viewer_token_env) can authenticate GET /admin/config
                             but is structurally rejected by POST/DELETE /admin/virtual_keys/{name}
                             (per-route middleware wrapping); omitting it reproduces the original
                             single-credential behavior exactly. Every successful virtual-key
                             create/delete now writes a structured audit-log entry (key name only, never
                             the credential). **Added 2026-09-18**: `POST /admin/cache/erase` (Admin
                             tier only, audit-logged) finally gives `cache.Cache.Delete` — real since
                             2026-09-14 but with zero live callers anywhere until now — a real caller,
                             closing the right-to-erasure gap named in
                             docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md
                             Finding 4. Computes the exact same L1/L2 cache keys the normal request path
                             would for a given (virtual key, request) pair and does a Get-then-Delete
                             against each. See the Cache Subsystem section below for this endpoint's own
                             disclosed limitations (L3 is not touched at all; the Get-then-Delete
                             sequence isn't atomic). **Added 2026-09-14**: an opt-in `admin.enable_pprof` flag
                             (default false) mounts `net/http/pprof`'s standard handler set under
                             `/admin/debug/pprof/`, on this same mux, gated behind the admin credential
                             specifically — never the viewer tier — mirroring Envoy Gateway's own shipped
                             `enablePprof` field, per docs/upgrade-research/performance-latency-optimization-2026-09-14.md
                             Finding 3. **Corrected 2026-09-16**: the very next sentence's own
                             "routing... stays static-YAML-only, named explicitly as later follow-on
                             work" claim went stale the moment `POST /admin/deployments/{name}/weight`
                             shipped (router.Router.SetWeight, in-memory-only, reverts to config.yaml on
                             restart exactly like every other admin mutation here) — caught by a live
                             adversarial-audit doc-staleness pass, not silently left wrong. Admin
                             mutations are in-memory-only in v1
                             (lost on restart, reverting to config.yaml); every other config section
                             (guardrails, budgets' shape, rate limits, cache, price table,
                             telemetry) stays static-YAML-only, named explicitly as later follow-on work
/internal/prompt             — **Corrected 2026-09-12**: missing from a prior pass of this tree, a
                             doc-vs-code staleness instance per AGENTS.md's catalogued pattern.
                             Real, ACTIVE, per docs/rfcs/2026-09-13-gateway-prompt-management.md:
                             operator-managed, versioned prompt templates (prompt_id +
                             prompt_version + prompt_variables), CRUD'd only via five new Admin API
                             routes (GET/POST/DELETE /admin/prompts...) and resolved into real
                             adapter.Message content by dataplane's resolvePromptIfSet, which runs
                             immediately after the model-allowlist check and before rate-limiting
                             (see Request Lifecycle below). Global, not tenant-scoped — a resolved
                             design fork, the same category as price_table/deployments/guardrails
                             config, never per-tenant computed state like cache/rate-limit/budget.
                             Substitution is a minimal {{name}} allowlist swap, deliberately not
                             Go's text/template (a real template-injection surface over
                             operator-supplied content otherwise); an unresolved placeholder stays
                             literal, never an error or a silent drop. Version history is
                             append-only; Persister (Load/Save) is an unimplemented seam mirroring
                             internal/budget's own boltstore separation — prompts are
                             in-memory-only, lost on restart, the same disclosed v1 limitation
                             virtual keys already have.
```

**Dependency direction rules** — enforced by `go-arch-lint` in CI since 2026-09-05 (`gateway/.go-arch-lint.yml`, wired into `.github/workflows/ci.yml`'s `gateway` job and `make lint-gateway`), since Go's `internal/` visibility only catches direct imports, not transitive ones. Previously (until 2026-09-04) this was followed only by manual discipline with nothing to catch a future violation automatically. The rules below also correct two stale package names caught while wiring the linter (`gateway` → the real `internal/gateway/dataplane`/`internal/gateway/controlplane`; `provideradapter` → the real `internal/adapter`), confirmed against the actual import graph (`grep` across every non-test `.go` file), not assumed from this doc's own prior prose. **Corrected 2026-09-12**: `dataplane`'s and `admin`'s own lists below were each missing a real edge that `internal/prompt`'s own shipping introduced (per `docs/rfcs/2026-09-13-gateway-prompt-management.md`) and this doc never picked up — `dataplane → prompt`, and `admin → adapter, prompt` (the same commit that added `admin`'s new prompt-CRUD routes also added its first-ever `adapter` import, for those routes' own `[]adapter.Message` request/response bodies) — both re-verified against the real import graph and `gateway/.go-arch-lint.yml`'s own `mayDependOn` entries for `dataplane`/`admin`/`prompt`, not assumed:

```
dataplane → cache, adapter, adapter/{anthropic,bedrock,gemini,openai,openaicompat}, streaming,
            budget, ratelimit, router, costaccounting, telemetry, guardrail, identity, prompt,
            api/gatewayevents/v1, alerting
adapter/{anthropic,bedrock,gemini,openai,openaicompat} → adapter, streaming
streaming → adapter                (canonical ChatCompletionChunk/StreamDecoder types live in adapter)
cache/{inprocess,grpcserver,grpcclient} → cache   (each a real implementation of cache's own interfaces)
cache     ✗→ adapter             (cache is provider-agnostic — keyed on normalized request, not on which
                                  upstream served it; verified: cache has ZERO internal cross-package
                                  imports at all, a stricter, cleaner leaf than an earlier pass of this
                                  doc's own prose implied)
guardrail ✗→ adapter, cache, dataplane   (text in, Verdict out — guardrail has no concept of
                                  pre/post-call or streaming/buffered; that distinction lives in
                                  gateway/dataplane, per docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md)
cache     ✗→ dataplane            (no back-references — this is what makes cache extractable later)
router    ✗→ dataplane, cache     (router.Deployment is its own decoupled type, never dataplane.Deployment —
                                  mirrors ratelimit.KeyConfig's existing decoupling from identity.VirtualKey)
{identity, budget, ratelimit, router, telemetry, adapter, costaccounting, controlplane, guardrail,
 alerting} ✗→ dataplane, cache    (shared kernel is a leaf — verified: every one of these packages has
                                  zero internal cross-package imports of its own)
budget    ✗→ identity              (budget tracks by key ID string only — it doesn't need to know what a
                                  VirtualKey is, only that it's a string; keeps both packages independently
                                  testable and reusable)
ratelimit ✗→ identity            (same reasoning as budget above — KeyConfig carries a plain key ID string)
ratelimit/redislimiter ✗→ ratelimit   (the interface (RedisBackend) lives in the consumer package; the
                                  Redis-specific implementation never imports it back — the same pattern
                                  budget/boltstore already established, so go-redis stays out of
                                  ratelimit's own dependency graph in the default, in-memory-only case)
admin → adapter, controlplane, dataplane, identity, prompt, ratelimit   (the HTTP handler layer for GET
                                  /admin/config, POST/DELETE /admin/virtual_keys/{name}, and the new
                                  prompt-CRUD routes; adapter is pulled in only for those routes' own
                                  []adapter.Message request/response bodies, never for any provider-calling
                                  behavior; never imported BY any of those six — admin is a top-level
                                  consumer, not a shared-kernel package)
```

## Request Lifecycle

Every capability is a stage in one linear pipeline against a single canonical schema. **Corrected
2026-09-17**: this is no longer true without exception — `/v1/embeddings` (`Pipeline.HandleEmbeddings`,
`cmd/gateway/main.go`'s `embeddingsHandler`) runs a materially different, narrower pipeline against a
different canonical schema (`adapter.EmbeddingRequest`, not `ChatRequest`), by its own doc comment's
explicit disclosure: it does NOT thread through cache, idempotency, the per-identity concurrency cap,
fallback chains, or telemetry spans — it reuses only the budget/rate-limit/guardrail primitives and a
plain (non-sticky) `nextDeployment` pick. See the Embeddings entry under Canonical Schema & Provider
Adapters below. The lifecycle diagram immediately following this paragraph describes the
`/v1/chat/completions` pipeline specifically, not every capability this gateway exposes:

```
[client/agent request, carrying session/agent_run_id if present]
  → auth (resolve virtual key → team/workspace → budget+rpm/tpm+allowed_models record)
  → prompt resolution (dataplane.resolvePromptIfSet), immediately after auth's own model-allowlist
    check (isModelAllowed) and before rate-limiting: if req.PromptID is set, resolves the stored,
    versioned template into real adapter.Message content via a minimal {{name}} allowlist
    substitution, never Go's text/template; PromptID and inline Messages both set is a 400
    (ErrPromptAndMessagesBothSet), never a silent merge — see
    docs/rfcs/2026-09-13-gateway-prompt-management.md and /internal/prompt above
  → rate-limit check (hierarchical: org → team → user → key → session)
  → cache lookup, L1 exact hash match → hit → log, return
  → cache lookup, L2 normalized match → hit → log, return
  → cache lookup, L3-lite lexical near-duplicate (MinHash/Jaccard + entity/date hard-gate + freshness gate,
    with a hard volatility bypass — never real embedding-based semantic matching, see Cache Subsystem
    below) → hit → log, return
  → guardrail pre-call (PII/content check)
  → router (weighted round-robin selects a deployment, skipping any deployment
    active/synthetic-probe health-probing has marked unhealthy — per
    docs/rfcs/2026-09-04-weighted-routing.md and
    docs/rfcs/2026-09-07-gateway-active-health-probing.md — plus, on error, an error-classified,
    multi-hop fallback chain if the deployment configures one, else the pre-existing same-model
    single-fallback attempt, per docs/rfcs/2026-09-07-gateway-error-classified-fallback-chains.md;
    still no TRAFFIC-DERIVED statistical circuit breaker, deliberately — though the filter/volume-floor
    primitives for one exist, built but deliberately unwired, per docs/upgrade-research/gateway-router-
    health-real-traffic-2026-09-09.md). **Added 2026-09-14**: immediately after this first pick,
    rerouteToCapableDeploymentIfNeeded checks whether the picked deployment can honor a non-nil
    req.ResponseFormat (adapter.SupportsStructuredOutput) and, if not, walks the rest of the same
    model's pool for a capable alternative before the first real upstream call — best-effort, not a
    hard error; falls through to the original pick unchanged when no capable deployment exists
    anywhere in the pool. See THREAT_MODEL.md's Gateway Elevation-of-Privilege row for the gap this
    closes. **Corrected 2026-09-17**: "weighted round-robin selects a deployment" above describes the
    first pick's FALLBACK behavior, not its only behavior — for a model group with any `Sticky`-flagged
    deployment, the real first pick is `nextDeploymentSticky` → `router.Router.SelectSticky`, which
    deterministically hash-buckets the request by the calling virtual key's own ID into a stable/canary
    side before ever falling through to plain WRR (only on an exclude/health/cost-tier rejection). See
    `/internal/router`'s own entry above for the full mechanism
  → deployment capacity check, PER HOP (every hop, not just the first): the resolved deployment's own
    optional rate_limit/max_concurrent_requests ceiling — a genuinely different scope from the per-key
    checks above, bounding one shared deployment's own AGGREGATE load across every virtual key and every
    fallback hop that converges on it. A rejection here is a *DeploymentCapacityError, mapped to a
    client-facing 503 (never the per-key 429 bucket), per docs/upgrade-research/gateway-per-deployment-
    concurrency-2026-09-09.md — never applied to the synthetic health-probe loop itself
  → provider adapter: canonical → provider-native request translation
  → upstream call (streaming: non-buffering pass-through, chunk-by-chunk, explicit Flush() per chunk;
    each chunk also re-checks a mid-stream runaway-completion ceiling and tops up the request's own
    budget/TPM reservation to match real, growing output — closing the gap where a long stream's real
    cost could silently exceed what a concurrent sibling on the same key could see, per docs/rfcs/2026-
    09-08-gateway-streaming-runaway-completion-guard.md's own follow-on reservation-top-up fix)
  → provider adapter: provider-native response/chunk → canonical translation (stateful per-stream parser)
  → guardrail post-call
  → cache write-back (all layers)
  → cost/observability finalize (OTel span close, Decimal cost calc, budget record, gatewayevents log
    line) — ALWAYS runs, even on error/cancel, via Go `defer` — a partial generation still consumed
    billable output tokens. **Corrected 2026-09-05**: no session- or agent_run_id-level cost roll-up
    happens here or anywhere in the codebase (confirmed: zero matches for roll-up/rollup/session_cost
    across the module) — finalize is purely per-request; aggregating cost across a session/run today
    requires an operator to sum spans or gatewayevents downstream themselves
  → response to client — buffered path only, also carries X-Kelvran-Overhead-Duration-Ms (added
    2026-09-14, docs/rfcs/2026-09-14-gateway-overhead-duration-header.md): total wall-clock time
    minus the real upstream round-trip (including any multi-hop fallback retry/backoff), a context-
    value pointer runMissPath writes into, never a widened HandleChatCompletion return signature.
    0 on a cache hit by construction (no upstream call happened). Cannot be set on the streaming
    path, whose own headers are already sent before the pipeline runs
```

Cache lookup happens *before* the router/provider call so a hit never touches an upstream. On a genuine miss, everything from guardrail pre-call through cache write-back (per `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`) is deduplicated across concurrent identical requests via `golang.org/x/sync/singleflight`, keyed on the L1 exact-match key — N concurrent requests for the same not-yet-cached key produce exactly one real upstream call, not N. Single-instance-only (in-process) in v1; a distributed version across multiple gateway instances is named future work. Guardrails wrap the call symmetrically — pre-call is identical on both the buffered and streaming paths (the request text is fully known before any upstream call either way). Guardrail post-call is **enforcement-capable on the buffered path only**: on streaming, every chunk is already flushed to the client (non-buffering, chunk-by-chunk, per the upstream-call line above) strictly before a complete response exists to check, so post-call there is audit-only — it can log a Block-tier finding at elevated severity but cannot withhold content already delivered. Cost/observability finalization is structured to always execute.

## Canonical Schema & Provider Adapters

One canonical internal schema, OpenAI Chat-Completions-shaped — the dialect vLLM/TGI/Ollama/DeepSeek/Together/Groq already speak natively, making self-hosted integration nearly adapter-free. Each adapter is a pure-function pair (`ToProvider()`/`FromProvider()`) that must explicitly own four normalization points that break silently if missed:

1. **Tool-call argument encoding** — on the buffered path, OpenAI/DeepSeek/Qwen return a JSON *string*; Anthropic/Gemini/Bedrock return an already-parsed *object*. On the **streaming** path this splits differently and is a genuinely separate hazard: OpenAI, openaicompat, and Bedrock all send arguments as an *accumulating JSON-string fragment* across chunks (confirmed for Bedrock against the real `ToolUseBlockDelta.Input *string` wire field); Anthropic accumulates a string fragment too (its own `input_json_delta` events); Gemini alone sends a *complete object per chunk*, never fragmented — the one provider where streaming and buffered tool-call shape genuinely match.
2. **System-prompt placement** — in-array `role:"system"` (OpenAI) vs. top-level `system` param (Anthropic/Bedrock) vs. `systemInstruction` (Gemini).
3. **Streaming event shape** — OpenAI's homogeneous `delta.content` fragments vs. Anthropic's typed SSE event sequence (needs a stateful per-stream parser tracking open content blocks / accumulating tool-call indices) vs. Bedrock's binary EventStream encoding (real per docs/rfcs/2026-09-04-bedrock-converse-stream.md — decoded by `bedrock.StreamDecoder`, a genuinely stateless decoder since every Bedrock event is self-describing, unlike Anthropic's). Real for OpenAI, Anthropic, Gemini, openaicompat, and Bedrock (see `/internal/streaming` above and each adapter's `stream.go`).
4. **Unknown-field preservation** — e.g. Gemini's `thoughtSignature` must round-trip verbatim across turns or multi-turn tool use silently breaks. Adapters must never strip fields they don't recognize.

**Added 2026-09-17 — a second, separate canonical schema now exists for embeddings.** `adapter.EmbeddingRequest`/`EmbeddingResponse` and a new `EmbeddingAdapter` interface (`ToEmbeddingProvider`/`FromEmbeddingProvider`) are deliberately NOT added to the existing `Adapter` interface above — every provider package would otherwise need a dummy/panicking implementation. Only `openai` and `bedrock` implement it today (both have a real native embeddings model; Gemini is deferred for lack of demand signal beyond the default, Anthropic is skipped entirely — no native embeddings model, only a third-party Voyage AI pointer, out of scope for a first pass). Bedrock's Titan `InvokeModel` embeddings endpoint has a real, AWS-enforced single-input-per-call constraint (confirmed against `evals/scripts/validate_embedding_gate.py`, not assumed) — `bedrock.ErrBedrockEmbeddingBatchNotSupported` rejects a multi-input request outright rather than silently truncating it. `Pipeline.HandleEmbeddings` (`/v1/embeddings`) is its own function, never a `HandleChatCompletion` retrofit — see the Request Lifecycle section's own correction above for exactly which pipeline stages it does and doesn't thread through.

**Corrected 2026-09-10**: Bedrock's Converse API DOES have a real, genuine `additionalModelRequestFields`-style escape hatch, and the canonical schema now has a use for it — this doc's own prior claim ("no such field exists") is now stale, not the code. Structured-output/JSON-schema requests (`ChatRequest.ResponseFormat`, a new canonical field alongside `Tools`) are the first thing to use it: `bedrock.Request.AdditionalModelRequestFields` carries `{"output_config":{"format":{"type":"json_schema","schema":{...}}}}`, live-verified against a real Converse API call — but genuinely restricted to a specific whitelist of Bedrock-hosted Claude models (`adapter.SupportsStructuredOutput`'s Bedrock branch; calling it against an unsupported model, e.g. `global.anthropic.claude-sonnet-5`, returns a clean, real AWS `ValidationException`, not a silent ignore). Anthropic's own direct Messages API expresses the same feature as a top-level `output_config.format` field (no beta header required); OpenAI/openaicompat mirror OpenAI's real `response_format`/`json_schema` shape; Gemini maps it onto `responseMimeType`/`responseSchema` (an OpenAPI-3.0-*subset* dialect, not full JSON Schema — a named fidelity caveat, never translated/validated). v1 is request-shape normalization only — no JSON Schema validation library exists in this module's `go.mod`, and a provider with no native enforcement mechanism simply gets no v1 support, named explicitly rather than silently missing. A new provider's OTHER quirks are still handled the same way the four points above already are, inside that provider's own adapter package via its provider-specific request/response structs (`ToProvider`/`FromProvider`), never by touching the core pipeline or the canonical schema itself — this escape hatch is Bedrock-specific plumbing for one real API capability, not a general-purpose bypass.

**Added 2026-09-14** (schema-dialect linter): Bedrock's structured-output schema dialect is a real, narrower subset of Draft 2020-12 than OpenAI's own dialect — AWS documents recursive schemas, external `$ref`, and numeric/string-length constraints (`minimum`/`maximum`/`multipleOf`/`minLength`/`maxLength`) as unsupported, returning a real AWS 400 with no advance warning. `bedrockUnsupportedSchemaFeature` walks the caller's schema tree (top-level, and every nested `properties`/`items`/`$defs` node) for these six keywords before ever sending to AWS, turning an opaque upstream 400 into an actionable Kelvran-side error naming exactly which feature is unsupported — distinct from, and layered on top of, the already-fixed `additionalProperties` auto-injection. `$ref` is rejected unconditionally (Kelvran has no schema-graph-cycle detector to tell a genuinely-recursive reference apart from a merely-local one, so any `$ref` is treated as unsupported — fail-safe, not fail-open).

**Added 2026-09-14**, per `docs/rfcs/2026-09-14-gateway-tool-choice-normalization.md`: `ChatRequest.ToolChoice` (`Mode` ∈ `auto`/`required`/`none`/`tool`, plus `ToolName` and `DisableParallelToolUse`) requests tool-calling forcing behavior, normalized across all five adapters despite genuinely different native semantics — Anthropic's `tool_choice.type` object (errors, never silently downgrades, when `required`/`tool` targets a model AWS/Anthropic document as rejecting forced modes — `adapter.AnthropicModelRejectsForcedToolChoice`); Bedrock Converse's strict `toolChoice` union (errors on `tool` against a model outside AWS's real Claude-3/Nova whitelist — `adapter.BedrockModelSupportsForcedToolChoice` — and errors on `none` outright, since Converse's union has no "forbid tool use" member at all); Gemini's legacy `functionCallingConfig` (mode `"tool"` maps to `ANY` + a one-element `allowedFunctionNames` allow-list, since Gemini's legacy surface has no distinct single-tool-forcing mode); OpenAI/openaicompat's real string-or-object wire union (a hand-written `MarshalJSON` on each package's own `ToolChoiceWire`). Nil (the default) is a silent no-op for every adapter, matching `ResponseFormat`/`CacheControl`'s own established convention.

**Provider-side opt-in prompt caching** (`Message.CacheControl`/`ContentPart.CacheControl`, per `docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`) is a fifth normalization point, added after the four above: Anthropic's, Bedrock's, and OpenAI's own opt-in caching mechanisms are each shaped differently — a property attached directly to a specific content block (Anthropic's `cache_control`), a standalone checkpoint block marking a boundary in a content array (Bedrock's `cachePoint`, wired into the same array-based `System`/message-content shape hazard #2 already required), or a coarse, whole-request routing hint with no relationship to *what* is cached at all (OpenAI's `prompt_cache_key`, since OpenAI's own caching is already fully automatic). The canonical schema exposes Anthropic's full per-message/per-content-part granularity as a superset (the same pattern `ContentPart` itself already established for multi-modal content, per `docs/rfcs/2026-09-06-gateway-multimodal-content.md`); each adapter collapses that marker into whatever shape its own provider actually uses, and an unset marker is a silent no-op on every adapter, never an error. Gemini has no request-level field for any of its own caching mechanisms (implicit caching is fully automatic; explicit caching is a wholly separate out-of-band `CachedContent` object-creation API) — its adapter is untouched by this normalization point, confirmed by a dedicated test proving the marker has zero effect on its output, not merely inferred from the absence of a code change.

`ToolDef.CacheControl` (same-day addendum to that RFC) extends the marker to tool *definitions*, not just messages/system — and the two providers' real shapes diverge here in a way worth naming explicitly: Anthropic's `cache_control` on a tool is an inline sibling key on that tool object (mechanically identical to its content-block placement), but Bedrock's `cachePoint` on a tool is **not** an inline field at all — confirmed against AWS's own `Tool` union-type reference, `toolConfig.tools[]` accepts exactly one of `cachePoint`/`systemTool`/`toolSpec` per element, so a cached tool definition is a standalone `{"cachePoint":{...}}` array element sibling to `{"toolSpec":{...}}` elements, the same "standalone checkpoint block" shape Bedrock already uses for `System`/message content, not the Anthropic tool-def shape. OpenAI/openaicompat/Gemini remain excluded, re-verified for tool definitions specifically rather than assumed to inherit the message-level finding: OpenAI's `prompt_cache_key` still has no per-block addressing concept of any kind; Gemini's tool-building code still has no cache-marker field to read.

**`CacheControl` auto-populate for system prompts is now real** (Anthropic, Bedrock only), per `docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md` — resolving the original RFC's own "should Kelvran ever auto-populate `CacheControl` on a client's behalf" Unresolved Question. Both adapters' `ToProvider` auto-mark an otherwise-unmarked `role:"system"` message with the provider's own default (cheapest) cache marker, scoped to system messages ONLY — never a user/assistant message, a content part, or a tool definition, which stay caller-explicit-only exactly as before. An explicit, caller-supplied `CacheControl` on a system message always wins outright over the auto-populated default. A new per-deployment `DeploymentConfig.DisableCacheControlAutoPopulate` (`disable_cache_control_auto_populate` in YAML), defaulting to `false` (auto-populate ON, matching every deployment configured before this field existed), lets an operator opt a specific deployment out entirely. The opt-out reaches the adapter via a new `adapter.ChatRequest.DisableCacheControlAutoPopulate` field (`json:"-"` — never client-reachable via the request body, since it expresses an operator policy, not a per-call choice) that `dataplane` sets from the resolved `Deployment`'s own field immediately before every `ToProvider` call, mirroring the pre-existing `Model`/`UpstreamModel` per-call-override pattern rather than widening the `Adapter` interface's signature. OpenAI/openaicompat/Gemini are unaffected — none of their `ToProvider` implementations read the new field at all.

**`adapter.CacheControl.TTL` now genuinely reaches Bedrock, fixed 2026-09-14**: `bedrock.CachePoint` previously had no `TTL` field at all, and every constructor hardcoded `{Type: "default"}`, silently dropping a caller's requested 1-hour cache tier and falling back to Bedrock's plain 5-minute default with no error — contradicting `CacheControl.TTL`'s own doc comment, which already claimed Bedrock read this. The forwarded TTL is gated to a real per-model whitelist (`bedrockCacheTTLModelSubstrings`, `gateway/internal/adapter/bedrock/bedrock.go` — a separately-tracked list from the structured-output whitelist in `internal/adapter/capabilities.go`, since AWS documents the two capabilities' model support independently), mirroring `adapter.SupportsStructuredOutput`'s existing per-model gating shape: Bedrock hard-rejects unrecognized fields on models that don't support it, so an ungated `ttl` risks failing the whole request rather than silently omitting one field.

**Reasoning/thinking-block round-tripping** (`Message.ReasoningBlocks`, per `docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md`) is a sixth normalization point, and the first one that was a genuine, live-breaking bug rather than a design gap: Anthropic's Messages API and Bedrock's Anthropic-compatible Claude Messages endpoint return a hard `400` if a prior turn's `thinking`/`redacted_thinking` blocks aren't echoed back byte-for-byte, in original order, on any subsequent turn carrying a tool result — some model tiers cannot disable thinking at all, so this fires unconditionally, not just under specific configuration. `adapter.Message.ReasoningBlocks` is an ordered, additive field (`Sequence`-indexed against `ToolCalls`, never a breaking replacement of `Content`/`ToolCalls`) capturing each opaque block's `Text`/`Signature` (plaintext) or `Data` (provider-encrypted ciphertext — never interpreted, scanned, or logged). Anthropic and Bedrock both fully DROPPED these blocks before this fix (`ContentBlock` had no field for either type, on either the buffered or streaming path); Gemini's bug was a different class entirely — its real `thought`/`thoughtSignature` fields exist, but a thought part's content rides the exact same `text` JSON key an ordinary answer uses, so it was silently MERGED into visible `Content`, indistinguishable from a real answer, rather than dropped. **Corrected 2026-09-13**: OpenAI remains deliberately deferred — it targets the Chat Completions wire shape, which has no reasoning/thinking field of any kind (confirmed against OpenAI's own current API reference; only `usage.completion_tokens_details.reasoning_tokens`, a plain count), and encrypted reasoning items/summaries exist only under the Responses API, which `openai.go` does not target. **openaicompat shipped** (that RFC's Phase 5): field-name verification against each target runtime's live source found the wire name genuinely fragmented — llama.cpp emits `reasoning_content`; vLLM renamed its own field to `reasoning` (accepting the old name only as a request-side backward-compat alias, never emitting it); Ollama's OpenAI-compat layer uses `reasoning`; TGI has none. `openaicompat.go`/`stream.go` capture/replay under both live wire names, as a single flat `ReasoningBlock` at `Sequence: 0` (no interleaving signal exists on this flat wire shape). Two cross-cutting consequences shipped as real code, not left as a design note: Cache L3-lite gained an eighth hard gate (`ReasoningBlocksFingerprint` — see Cache Subsystem below); the guardrail pre-call scan now excludes a Redacted block's opaque `Data` (previously fed straight into the PII/secret regex detectors, a real violation of `ReasoningBlock.Redacted`'s own "never scan" contract), while the post-call scan now includes plaintext `ReasoningBlocks.Text` (previously never scanned at all — see Guardrails Subsystem below).

**Added 2026-09-23**, per `docs/upgrade-research/ai-gateway-api-standardization-2026-09-22.md`: no emerging standardized AI-gateway wire-protocol effort is being tracked as an adoption candidate against this canonical schema today — correctly, not from inattention. The one GA'd standard surveyed doesn't apply to Kelvran's architecture; the one effort that would apply is pre-alpha, unimplemented by any peer; and the peers Kelvran is actually benchmarked against in this repo's own convention (Kong, Envoy AI Gateway, LiteLLM) have each gone their own proprietary way at the layer that matters. A "monitor lightly, revisit in 6-12 months" verdict, not a permanent one — this schema's own OpenAI-Chat-Completions-shaped design (see above) is unaffected either way.

**Added 2026-09-23**, per `docs/upgrade-research/agent-memory-context-management-2026-09-22.md`: checked directly against the code (Finding 1's own open question) whether an inbound `context_management`-shaped field (Anthropic's `context_management.edits`/`compact_20260112`, OpenAI Responses API's `context_management.compact_threshold`) survives anywhere in Kelvran's request path — confirmed clean, the expected result, not a silently-discovered gap. `adapter.ChatRequest` (`internal/adapter/types.go`) has no such field and no generic unknown-field-preservation mechanism at the canonical-schema level (Gemini's `thoughtSignature`, normalization point #4 above, is a NAMED field inside `gemini`'s own provider-specific wire structs, not a passthrough on `ChatRequest` itself); `chatCompletionsHandler` (`cmd/gateway/main.go`) decodes the request body straight into a bare `adapter.ChatRequest` via `json.Unmarshal`, which silently drops any JSON field with no matching struct tag before the request ever reaches an adapter's `ToProvider`. Separately, per that research doc's Finding 3: AWS Bedrock AgentCore Memory is confirmed a distinct AWS service family (`bedrock-agentcore`/`bedrock-agentcore-control`), unrelated to the Converse API `bedrock.go` already calls — nothing for this adapter to normalize, consistent with the "no Kelvran-native memory store" decision (that doc's own Finding 2: LiteLLM's `/v1/memory` was evaluated and explicitly declined) already recorded in that same research doc.

## Cache Subsystem

Cache is a package boundary, **not a network hop**, at every stage until (if ever) `docs/decisions/0002-cache-embedded-in-gateway.md`'s extraction triggers fire. Gateway's request pipeline only ever calls `cache.Cache.Get`/`Put`/`Delete` (L1/L2 — `Delete` added 2026-09-14, see `/internal/cache` above) or `cache.LexicalCache.Search`/`Put` (L3 — a distinct interface, since a similarity search returns zero-to-many scored candidates, not a single hit/miss) — never a concrete implementation. Internally: L1 (exact hash, SHA-256, in-process, LRU-bounded — real, per `internal/cache/key.go`/`internal/cache/inprocess`), L2 (normalized-match, a narrow 3-operation allowlist — outer whitespace trim, Unicode NFC, trailing terminal punctuation strip — real, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`; deliberately narrower than that RFC's own grounding research recommended, since Kelvran's agent traffic can plausibly include pasted code where internal-whitespace/case normalization risks a wrong-answer collision), **L3-lite** (MinHash/shingling lexical near-duplicate matching — never real embedding-based semantic similarity, deferred to a later RFC per `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`'s "why not real embeddings yet" — real, gated by an entity/number/date hard-gate, a **negation-particle hard-gate** (added 2026-09-12, see below), a per-tenant-partitioned `inprocess.LexicalCache`, a freshness/risk model, and a hard bypass for volatile queries; see `PRD.md`'s scope note that L3 must never ship without the hard-gate).

**2026-09-12 addition — negation-particle gate, a narrow correctness fix, NOT a semantic-similarity upgrade.** `dataplane.NegationFingerprint` extracts a small, fixed, closed set of syntactic negation particles/contractions (`not`, `never`, `without`, `n't`-contractions, etc.) from a query, compared via the same exact-set-equality `fingerprintsEqual` the entity/number/date gate already uses. This closes a real, narrow gap: a negation particle inserted or removed between an otherwise lexically near-duplicate query and a cached candidate (e.g. "take X" vs. "don't take X") previously had no gate at all — same empty entity fingerprint on both sides, so the pair could pass every existing check. **What this explicitly does NOT close**, disclosed rather than silently assumed away: an antonym-verb-flip with zero negation particles in either string (e.g. "withhold" vs. "administer" a drug) — `DECISIONS.md`'s `[2026-09-08]` entry already investigated and rejected three designs specifically for that case (a negation-cue gate alone doesn't fire there either; a curated antonym-pair list is unbounded "security theater" for a general-purpose gateway; broadening the entity fingerprint to all content words breaks a load-bearing existing test). That decision stands, undiminished by this addition — see the `[2026-09-12]` entry in `DECISIONS.md` for the full scope boundary.

**Also 2026-09-12 — `ReasoningBlocksFingerprint` gate**, per `docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md`: an eighth L3-lite hard gate, exact-string-equality (mirroring `GuardrailPolicyVersion`/`ResponseFormatFingerprint`/`PromptFingerprint`'s convention immediately above, not the entity/negation gates' set-equality one), closing a real gap where two requests differing only in accumulated `ReasoningBlocks` history could otherwise collide on a high-similarity L3 hit — replayed reasoning content is causally read by the model and can change output, per `AGENTS.md`'s "never weaken the cache hard-gate" rule. L1/L2 already satisfied this incidentally (`adapter.Message` has no custom `MarshalJSON`, so a full-message serialize already includes `ReasoningBlocks`) — this gate closes the one layer (L3's fuzzy similarity search) where that accident didn't apply.

**2026-09-13 — real paraphrase-vs-near-duplicate calibration data, from a live production dry-run, not a design assumption.** `internal/cache/lexical.go`'s own package doc already states L3-lite's scope honestly in prose ("lexical near-duplicate matching via MinHash/Jaccard similarity — never embedding-based semantic similarity"), and `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md` names the same distinction as an unvalidated design choice ("honestly scoped as lexical near-duplicate matching, not semantic paraphrase understanding") — but until this dry-run, neither had a measured number behind it. Two real pairs, both derived from the same ~70-word binary-search explanation: a genuine paraphrase (different wording/word order, same meaning, no new/changed entities/numbers/dates) computed to a real 3-word-shingle Jaccard similarity of ~0.0078 against the original — nowhere near `l3MinSimilarity`'s 0.9 floor (`gateway/internal/gateway/dataplane/dataplane.go`), a clear, correctly-rejected miss; a near-verbatim version of that SAME original with exactly one isolated synonym swap ("efficient" → "effective"), identical word order otherwise, computed to ~0.9143 exact Jaccard, with the real gateway's own 128-value MinHash estimate for that pair landing at 0.9140625 (117/128) — correctly hit, just above the floor. **Confirms**: real, useful typo/near-verbatim/single-word-substitution tolerance. **Does NOT confirm, despite the "near-duplicate" name inviting the assumption**: any tolerance for genuine semantic paraphrase — that traffic shape misses this gate entirely (falls straight through to a real upstream call), by design, not as a bug. No code or gate changed by this finding; see `THREAT_MODEL.md`'s Cache Elevation of Privilege row for the corresponding security-framing update.

L1, L2, and L3 are each a separate `inprocess` cache instance, independently capacity-bounded (LRU eviction, no unbounded mode) — L3's bound is structurally *per tenant*, unlike L1/L2's single shared cap, since true tenant partitioning for a similarity search is a security requirement (`THREAT_MODEL.md`'s KeyPooling mitigation), not a style choice. Tenant namespace is real for every layer today (`cache.Key()`/`cache.NormalizedKey()`'s leading `tenantID` parameter for L1/L2, `LexicalCache`'s own `tenantID` parameter for L3, per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`), enforced at every hop (lookup, write, retry, fallback) — the design decision that defeats cross-tenant leakage.

**2026-09-18 addition — `Pipeline.EraseCacheEntry` finally gives `cache.Cache.Delete` a real caller**, via `POST /admin/cache/erase` (see `/internal/admin` above). Two disclosed, real limitations, not silently narrowed: (1) **L3 is not touched at all** — `LexicalCache` has no `Delete` method on its own interface, and `writeCache` populates L1, L2, AND L3 on every miss, so a byte-identical follow-up request for content this endpoint just reported erased can still be served from cache, from L3 instead of L1 — confirmed empirically by a dedicated regression test, not just reasoned about; an L3 entry's own TTL is the only path to eventual removal until a real `LexicalCache.Delete` exists. (2) **The Get-then-Delete sequence against L1/L2 isn't atomic** — `cache.Cache` has no combined get-and-delete operation, and `inprocess.Cache`'s `Get`/`Delete` each acquire the mutex independently, so a concurrent identical in-flight request's own `writeCache` call landing between this method's Get and Delete (or right after Delete returns) can leave a fresh entry under the same key, invisible to this method's caller. Narrow-window and low-severity (repopulates with a NEW response for a NEW request, never resurrects the erased bytes) — closing it properly needs a new atomic `GetAndDelete` interface method implemented across `inprocess` AND a new RPC for the `grpcclient`/`grpcserver` pair, named as real future work rather than built here.

## MCP/A2A Subsystem

Shares Gateway's own auth/budget/audit objects rather than being a second gateway with a second config source — inbound (expose Kelvran's own APIs as MCP tools) and outbound (broker agent tool calls to model providers) brokering both flow through `/internal/identity` and `/internal/costaccounting`.

## Guardrails Subsystem

Pre-call and post-call middleware hooks — **real**, per `docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md`: a pure-Go, stdlib-only `internal/guardrail` package (regex/checksum PII+secrets detection — email, phone, US SSN, IBAN with a real mod-97 checksum, credit card with a real Luhn checksum, IP address, API-key/secret prefixes with Shannon-entropy gating — plus a keyword/hidden-Unicode prompt-injection heuristic). Deliberately **not** NER in this pass — no mature, no-cgo, no-model-file Go NER library exists today, the same class of gap Cache L3-lite already found and narrowed around for real embeddings. **2026-09-13 addition — an optional ML detector backend is now real**, per `docs/rfcs/2026-09-13-gateway-bedrock-guardrails-ml-detector-design.md`: `guardrail/bedrockguard.Detector` wraps AWS Bedrock Guardrails' standalone `ApplyGuardrail` check (a real live AWS API call, decoupled from model invocation), opt-in via `GuardrailsConfig.BedrockGuardrails`, appended to the engine's detector list only when configured — `DefaultDetectors()`'s regex-only set is unaffected when it isn't. Lives in its own sibling package (`internal/guardrail/bedrockguard`), never inside `internal/guardrail` itself, since that package's own boundary ("never imports adapter, cache, or any provider-specific package") is enforced by a dedicated `go-arch-lint` component, not just documented. **Generalizing this, added 2026-09-14**: `guardrail.Detector` (`internal/guardrail/types.go`) is deliberately I/O-agnostic by design — `Detect(ctx, text) ([]Finding, error)` returns a real error, not just because `bedrockguard` needs one today, but so any FUTURE third-party-moderation provider that genuinely can error over the network can plug in the same way, in its own sibling package, without touching `internal/guardrail` itself. `bedrockguard` is the one real, shipped instance of this pattern, not the limit of it. Proven live against a real, minimal PROMPT_ATTACK-only guardrail in the pilot's AWS account: catches real attack phrasing the regex heuristic misses (e.g. "ignore all previous instructions" — the extra word between verb and target breaks `promptinjection.go`'s own exact-substring combinatoric match). Real AWS classifier behavior at `inputStrength: HIGH` initially produced false positives on imperative output-format instructions ("say X and nothing else") — retuned same-day to `inputStrength: MEDIUM` after a real A/B test against live AWS cleared both false positives with zero observed recall loss across 4 real attack patterns; the pilot now runs at `MEDIUM`. Full account in that RFC's own Status/"New finding" sections. Category-tiered fail-closed (credential/financial_id/government_id) vs. fail-open-with-logging (contact_info/network_id/prompt_injection), on both the detection axis and the detector-error axis — never a single global default, and never inherited from the rate limiter's own fail-open policy (guardrails has no independent second control the way `budget.Tracker` backstops the rate limiter). Post-call is enforcement-capable on the buffered path; on streaming it is audit-only — every chunk is already flushed to the client before a complete response exists to check, a named, accepted residual risk, not silently glossed over. A guardrail policy/detector version bump forces every existing cache entry (L1/L2 via the cache key hash, L3 via a stored, checked provenance field) to become a real miss, never a silent, unchecked serve of a hit whose provenance predates the change.

**2026-09-12 addition**: the pre-call scan now excludes a `ReasoningBlock`'s opaque, provider-encrypted `Data` (Anthropic `redacted_thinking`, Bedrock `redactedContent`) via a dedicated `guardrailScanMessages` — before this fix it was fed straight into the PII/secret regex detectors as part of `serializeMessages`' blanket full-message marshal, a real violation of that field's own "never scan" contract, even though the practical exposure was low (ciphertext rarely resembles a real PII pattern). The post-call scan now includes plaintext `ReasoningBlocks.Text`, previously never scanned at all despite a model's reasoning trace being a real channel for PII/secrets/policy-violating content that never surfaces in visible output — the mirror gap to the pre-call fix. See `docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md`.

**Corrected 2026-09-13**: "fail-open-with-logging" above was only half true until this fix — `Engine.Check` previously logged a `guardrail_verdict_blocked` line only when `Blocked` was true; a Warn-tier category's own findings (prompt_injection/contact_info/network_id) were recorded on `Verdict.Findings` but never logged anywhere, since every one of this Engine's 4 call sites (pre-call/post-call, buffered/streaming) only ever inspects `.Blocked`. Found via a live adversarial evaluation against the real pilot gateway (7 hand-crafted probes mirroring promptfoo's own public plugin taxonomy — indirect-prompt-injection, ASCII-smuggling via Unicode tag characters, system-prompt-override, a Context Compliance Attack, special-token-injection, plus 2 PII probes): 6 of 7 produced zero guardrail log output at all, indistinguishable from the detector never running — including the hidden-Unicode-tag probe, despite `promptinjection.go`'s own doc comment specifically claiming hidden-Unicode detection. `Engine.Check` now also logs `guardrail_verdict_warn` whenever `Findings` is non-empty but `Blocked` is false, giving Warn-tier detections a real audit trail for the first time. The underlying blocking/policy behavior is unchanged — this is pure observability, not a security-behavior change. Separately confirmed as real, unfixed, and correctly out of scope for this fix: the model's own alignment (not Kelvran's gateway) was the only thing that actually refused 5 of the 6 non-PII probes — Kelvran's own guardrail layer provides zero defense against classic jailbreak/CCA/indirect-injection framing today, exactly the gap `docs/rfcs/2026-09-13-gateway-bedrock-guardrails-ml-detector-design.md` scopes a design for.

**Added 2026-09-23**, per `docs/upgrade-research/wasm-plugin-extensibility-2026-09-22.md`, reinforcing rather than revisiting `docs/upgrade-research/guardrail-plugin-extensibility-2026-09-15.md`'s existing "not yet" verdict on a WASM-based plugin/filter-chain model for this package: the newest evidence is harder, not softer — Kong (one of the three systems this research area evaluates against) removed WASM support from Kong Gateway entirely in version 3.11 (July 2025) after two years in beta, replacing it with a native plugin mechanism specifically for performance/memory reasons, and Envoy's own WASM HTTP filter still carries an "experimental" label in its current docs years after introduction. WASM would solve a problem (safe extensibility for a marketplace of untrusted, third-party plugin authors) this single-maintainer, single-binary gateway does not have, at a real, measured performance cost (~50% of native execution speed per Google's own V8 benchmarks) that would collide directly with this package's own per-request body-inspection hot path (`Engine.Check` runs pre-call AND post-call on every request). The `guardrail.Detector` interface's existing sibling-package pattern (`bedrockguard` above) remains the extensibility mechanism for a genuinely new detector — a real Go package, not a sandboxed plugin runtime.

## Tech Stack

| Concern | Choice |
|---|---|
| Language/runtime | Go 1.26+ |
| HTTP | `net/http` + `httputil.ReverseProxy`-derived streaming |
| Distributed rate limiting | `github.com/redis/go-redis/v9` + a Lua token-bucket script — **real**, per `docs/rfcs/2026-09-03-distributed-rate-limiting.md` (the fourth external Go dependency; opt-in, isolated to `internal/ratelimit/redislimiter`; unset = in-memory, unchanged) |
| Cache L1/L2/L3 storage | In-process (`internal/cache/inprocess`), LRU-bounded — **real** for all three layers, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` and `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`. L3 uses pure Go stdlib (`hash/fnv`) for MinHash — zero new `go.mod` entries. Redis remains the target for a future distributed cache backend (a real embedding-based L3's vector index, or L1/L2/L3 shared across gateway instances) — not yet built |
| Control-plane config store | Postgres (`pgx`/`sqlc`) — still the target for real control-plane state |
| Budget-spend restart-durability | `go.etcd.io/bbolt` — **real**, per `docs/rfcs/2026-09-03-budget-persistence.md` (single-instance only; the third external Go dependency, near-zero *new* transitive weight since its one real dependency, `golang.org/x/sys`, is already pulled in via OTel) |
| Observability sink | ClickHouse (`clickhouse-go`) remains the intended future target for a durable, queryable event store at scale. **Corrected 2026-09-14**: a real, live backend already exists for the pilot today — `grafana/otel-lgtm` (OTel Collector + Prometheus + Tempo + Grafana bundled in one image), a new profile-gated `observability` service in `docker-compose.yml`, activated by flipping `telemetry.exporter` to `"otlp"` in the already-fully-implemented exporter code path (`gateway/internal/telemetry/telemetry.go`). Confirmed live: real, non-empty `kelvran.cache.lookup`/`kelvran.llm.spend_usd` metrics and real traces from genuine Bedrock pilot traffic, plus a provisioned Grafana dashboard (`docs/operations/grafana/`). This is explicitly a dev/demo-scoped backend (the image's own documented scope), not the ClickHouse-based durable store this row still names as the eventual production target |
| Tracing | OTel Go SDK, GenAI semantic-convention attributes — **real**, per `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md` (the first external Go dependency this module has ever had; exporters: stdout/OTLP/none) |
| Cost/budget arithmetic | `github.com/shopspring/decimal` — **real**, per `docs/rfcs/2026-09-02-decimal-cost-accounting.md` (the second external Go dependency; zero transitive dependencies) |
| Container-aware memory limit | `github.com/KimMachineGun/automemlimit` — **real**, per `docs/upgrade-research/kubernetes-production-deployment-2026-09-14.md` Finding 5 (the fifth external Go dependency; `cmd/gateway/main.go` calls `memlimit.Set` explicitly at startup, not the package's own blank-import convenience, so it logs via this process's real JSON logger rather than the stdlib default). Reads the real cgroup memory limit and sets `GOMEMLIMIT` to 90% of it automatically — a no-op outside a cgroup limit (bare `docker run`, local dev, CI), since Go has no native cgroup-memory-aware equivalent and exceeding a Kubernetes memory limit triggers a hard OOM-kill, unlike a CPU limit which only throttles |
| Distribution | Single static binary, scratch/alpine Docker image |

## Cross-Cutting Contract

`gateway` emits OTel spans and `gatewayevents` (cost/usage/decision events) per the versioned schema in the root `api/` directory — `evals` consumes these without any source dependency on this binary's internals.
