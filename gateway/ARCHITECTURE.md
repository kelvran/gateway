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
                             Reader (SSE frame parser)/Writer (SSE frame writer, Flush()-per-chunk) — ACTIVE
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
                             docs/rfcs/2026-09-07-gateway-ratelimit-provider-header-path-dimensions.md;
                             this is a design record only — nothing below this line has changed. Hierarchical
                             scope resolution (org/team/user/session) remains
                             target-only, same boundary as identity's own scope deferral below
/internal/cache            — Cache's public interface — see "Cache Subsystem" below; this is the ONLY
                             package Gateway's request pipeline is allowed to import from Cache
    /port.go                — type Cache interface { Get, Put } — the sole import surface
    /grpc/cache.proto        — contract defined now, unused until/unless Cache is ever extracted
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
                             store below, not a replacement for it
/internal/adapter            — OpenAI/Anthropic/Gemini/Bedrock/self-hosted client wrappers
/internal/costaccounting     — token/$ metering, Decimal-precision ledger — real, per
                             docs/rfcs/2026-09-02-decimal-cost-accounting.md (github.com/shopspring/decimal,
                             the gateway's second external Go dependency family after OTel)
/internal/telemetry          — real OTel spans per request (GenAI semantic-convention attributes,
                             agent_run_id via W3C Baggage) — ACTIVE, per
                             docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md. `api/otel/`'s versioned
                             cross-language contract remains deliberately deferred (OTLP already IS a
                             real wire format for this data; see that RFC's Motivation section). The
                             OTHER half of the shared contract — `api/gatewayevents/v1` (structured
                             per-request decision outcomes, not span data) — is real: `finalize` (see
                             /internal/gateway/dataplane below) is its producer, per
                             docs/rfcs/2026-09-03-api-gatewayevents-contract.md
/internal/mcp                — **NOT BUILT.** Zero code exists (confirmed: no such directory under
                             gateway/internal/), explicitly out of scope for v1 per PRD.md. Intended
                             design: inbound (expose Kelvran's own APIs as MCP tools) + outbound (broker
                             agent tool calls) brokering — shares identity/costaccounting, not a second
                             gateway
/internal/guardrail          — pre/post-call middleware interface; PII/content checks — ACTIVE, per
                             docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md
/internal/admin               — Real, per docs/rfcs/2026-09-05-gateway-admin-api.md: an optional,
                             off-by-default HTTP surface on its own separate net.Listener (never the
                             client-facing gateway's mux/port) exposing read-only config introspection
                             (GET /admin/config — safe to return wholesale, since Config never holds a
                             raw secret) plus the one section made live-mutable in v1, virtual keys
                             (POST/DELETE /admin/virtual_keys/{name}, via a new
                             dataplane.Pipeline.UpsertVirtualKey/DeleteVirtualKey pair built around
                             identity.Verifier becoming an atomic.Pointer). Auth is a deliberately
                             separate, two-tier static bearer credential space from client-facing virtual
                             keys — never delegates to identity.Verifier. An optional second, read-only
                             viewer credential (admin.viewer_token_env) can authenticate GET /admin/config
                             but is structurally rejected by POST/DELETE /admin/virtual_keys/{name}
                             (per-route middleware wrapping); omitting it reproduces the original
                             single-credential behavior exactly. Every successful virtual-key
                             create/delete now writes a structured audit-log entry (key name only, never
                             the credential). Admin mutations are in-memory-only in v1
                             (lost on restart, reverting to config.yaml); every other config section
                             (guardrails, budgets' shape, rate limits, routing, cache, price table,
                             telemetry) stays static-YAML-only, named explicitly as later follow-on work
```

**Dependency direction rules** — enforced by `go-arch-lint` in CI since 2026-09-05 (`gateway/.go-arch-lint.yml`, wired into `.github/workflows/ci.yml`'s `gateway` job and `make lint-gateway`), since Go's `internal/` visibility only catches direct imports, not transitive ones. Previously (until 2026-09-04) this was followed only by manual discipline with nothing to catch a future violation automatically. The rules below also correct two stale package names caught while wiring the linter (`gateway` → the real `internal/gateway/dataplane`/`internal/gateway/controlplane`; `provideradapter` → the real `internal/adapter`), confirmed against the actual import graph (`grep` across every non-test `.go` file), not assumed from this doc's own prior prose:

```
dataplane → cache, adapter, adapter/{anthropic,bedrock,gemini,openai,openaicompat}, streaming,
            budget, ratelimit, router, costaccounting, telemetry, guardrail, identity,
            api/gatewayevents/v1
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
{identity, budget, ratelimit, router, telemetry, adapter, costaccounting, controlplane, guardrail}
          ✗→ dataplane, cache     (shared kernel is a leaf — verified: every one of these packages has
                                  zero internal cross-package imports of its own)
budget    ✗→ identity              (budget tracks by key ID string only — it doesn't need to know what a
                                  VirtualKey is, only that it's a string; keeps both packages independently
                                  testable and reusable)
ratelimit ✗→ identity            (same reasoning as budget above — KeyConfig carries a plain key ID string)
ratelimit/redislimiter ✗→ ratelimit   (the interface (RedisBackend) lives in the consumer package; the
                                  Redis-specific implementation never imports it back — the same pattern
                                  budget/boltstore already established, so go-redis stays out of
                                  ratelimit's own dependency graph in the default, in-memory-only case)
admin → controlplane, dataplane, identity, ratelimit   (the HTTP handler layer for GET /admin/config and
                                  POST/DELETE /admin/virtual_keys/{name}; never imported BY any of those
                                  four — admin is a top-level consumer, not a shared-kernel package)
```

## Request Lifecycle

Every capability is a stage in one linear pipeline against a single canonical schema:

```
[client/agent request, carrying session/agent_run_id if present]
  → auth (resolve virtual key → team/workspace → budget+rpm/tpm+allowed_models record)
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
    health-real-traffic-2026-09-09.md)
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
  → response to client
```

Cache lookup happens *before* the router/provider call so a hit never touches an upstream. On a genuine miss, everything from guardrail pre-call through cache write-back (per `docs/rfcs/2026-09-05-gateway-cache-stampede-protection.md`) is deduplicated across concurrent identical requests via `golang.org/x/sync/singleflight`, keyed on the L1 exact-match key — N concurrent requests for the same not-yet-cached key produce exactly one real upstream call, not N. Single-instance-only (in-process) in v1; a distributed version across multiple gateway instances is named future work. Guardrails wrap the call symmetrically — pre-call is identical on both the buffered and streaming paths (the request text is fully known before any upstream call either way). Guardrail post-call is **enforcement-capable on the buffered path only**: on streaming, every chunk is already flushed to the client (non-buffering, chunk-by-chunk, per the upstream-call line above) strictly before a complete response exists to check, so post-call there is audit-only — it can log a Block-tier finding at elevated severity but cannot withhold content already delivered. Cost/observability finalization is structured to always execute.

## Canonical Schema & Provider Adapters

One canonical internal schema, OpenAI Chat-Completions-shaped — the dialect vLLM/TGI/Ollama/DeepSeek/Together/Groq already speak natively, making self-hosted integration nearly adapter-free. Each adapter is a pure-function pair (`ToProvider()`/`FromProvider()`) that must explicitly own four normalization points that break silently if missed:

1. **Tool-call argument encoding** — on the buffered path, OpenAI/DeepSeek/Qwen return a JSON *string*; Anthropic/Gemini/Bedrock return an already-parsed *object*. On the **streaming** path this splits differently and is a genuinely separate hazard: OpenAI, openaicompat, and Bedrock all send arguments as an *accumulating JSON-string fragment* across chunks (confirmed for Bedrock against the real `ToolUseBlockDelta.Input *string` wire field); Anthropic accumulates a string fragment too (its own `input_json_delta` events); Gemini alone sends a *complete object per chunk*, never fragmented — the one provider where streaming and buffered tool-call shape genuinely match.
2. **System-prompt placement** — in-array `role:"system"` (OpenAI) vs. top-level `system` param (Anthropic/Bedrock) vs. `systemInstruction` (Gemini).
3. **Streaming event shape** — OpenAI's homogeneous `delta.content` fragments vs. Anthropic's typed SSE event sequence (needs a stateful per-stream parser tracking open content blocks / accumulating tool-call indices) vs. Bedrock's binary EventStream encoding (real per docs/rfcs/2026-09-04-bedrock-converse-stream.md — decoded by `bedrock.StreamDecoder`, a genuinely stateless decoder since every Bedrock event is self-describing, unlike Anthropic's). Real for OpenAI, Anthropic, Gemini, openaicompat, and Bedrock (see `/internal/streaming` above and each adapter's `stream.go`).
4. **Unknown-field preservation** — e.g. Gemini's `thoughtSignature` must round-trip verbatim across turns or multi-turn tool use silently breaks. Adapters must never strip fields they don't recognize.

**Corrected 2026-09-10**: Bedrock's Converse API DOES have a real, genuine `additionalModelRequestFields`-style escape hatch, and the canonical schema now has a use for it — this doc's own prior claim ("no such field exists") is now stale, not the code. Structured-output/JSON-schema requests (`ChatRequest.ResponseFormat`, a new canonical field alongside `Tools`) are the first thing to use it: `bedrock.Request.AdditionalModelRequestFields` carries `{"output_config":{"format":{"type":"json_schema","schema":{...}}}}`, live-verified against a real Converse API call — but genuinely restricted to a specific whitelist of Bedrock-hosted Claude models (`adapter.SupportsStructuredOutput`'s Bedrock branch; calling it against an unsupported model, e.g. `global.anthropic.claude-sonnet-5`, returns a clean, real AWS `ValidationException`, not a silent ignore). Anthropic's own direct Messages API expresses the same feature as a top-level `output_config.format` field (no beta header required); OpenAI/openaicompat mirror OpenAI's real `response_format`/`json_schema` shape; Gemini maps it onto `responseMimeType`/`responseSchema` (an OpenAPI-3.0-*subset* dialect, not full JSON Schema — a named fidelity caveat, never translated/validated). v1 is request-shape normalization only — no JSON Schema validation library exists in this module's `go.mod`, and a provider with no native enforcement mechanism simply gets no v1 support, named explicitly rather than silently missing. A new provider's OTHER quirks are still handled the same way the four points above already are, inside that provider's own adapter package via its provider-specific request/response structs (`ToProvider`/`FromProvider`), never by touching the core pipeline or the canonical schema itself — this escape hatch is Bedrock-specific plumbing for one real API capability, not a general-purpose bypass.

**Provider-side opt-in prompt caching** (`Message.CacheControl`/`ContentPart.CacheControl`, per `docs/rfcs/2026-09-07-gateway-provider-prompt-caching.md`) is a fifth normalization point, added after the four above: Anthropic's, Bedrock's, and OpenAI's own opt-in caching mechanisms are each shaped differently — a property attached directly to a specific content block (Anthropic's `cache_control`), a standalone checkpoint block marking a boundary in a content array (Bedrock's `cachePoint`, wired into the same array-based `System`/message-content shape hazard #2 already required), or a coarse, whole-request routing hint with no relationship to *what* is cached at all (OpenAI's `prompt_cache_key`, since OpenAI's own caching is already fully automatic). The canonical schema exposes Anthropic's full per-message/per-content-part granularity as a superset (the same pattern `ContentPart` itself already established for multi-modal content, per `docs/rfcs/2026-09-06-gateway-multimodal-content.md`); each adapter collapses that marker into whatever shape its own provider actually uses, and an unset marker is a silent no-op on every adapter, never an error. Gemini has no request-level field for any of its own caching mechanisms (implicit caching is fully automatic; explicit caching is a wholly separate out-of-band `CachedContent` object-creation API) — its adapter is untouched by this normalization point, confirmed by a dedicated test proving the marker has zero effect on its output, not merely inferred from the absence of a code change.

`ToolDef.CacheControl` (same-day addendum to that RFC) extends the marker to tool *definitions*, not just messages/system — and the two providers' real shapes diverge here in a way worth naming explicitly: Anthropic's `cache_control` on a tool is an inline sibling key on that tool object (mechanically identical to its content-block placement), but Bedrock's `cachePoint` on a tool is **not** an inline field at all — confirmed against AWS's own `Tool` union-type reference, `toolConfig.tools[]` accepts exactly one of `cachePoint`/`systemTool`/`toolSpec` per element, so a cached tool definition is a standalone `{"cachePoint":{...}}` array element sibling to `{"toolSpec":{...}}` elements, the same "standalone checkpoint block" shape Bedrock already uses for `System`/message content, not the Anthropic tool-def shape. OpenAI/openaicompat/Gemini remain excluded, re-verified for tool definitions specifically rather than assumed to inherit the message-level finding: OpenAI's `prompt_cache_key` still has no per-block addressing concept of any kind; Gemini's tool-building code still has no cache-marker field to read.

**`CacheControl` auto-populate for system prompts is now real** (Anthropic, Bedrock only), per `docs/rfcs/2026-09-07-gateway-cache-control-auto-populate.md` — resolving the original RFC's own "should Kelvran ever auto-populate `CacheControl` on a client's behalf" Unresolved Question. Both adapters' `ToProvider` auto-mark an otherwise-unmarked `role:"system"` message with the provider's own default (cheapest) cache marker, scoped to system messages ONLY — never a user/assistant message, a content part, or a tool definition, which stay caller-explicit-only exactly as before. An explicit, caller-supplied `CacheControl` on a system message always wins outright over the auto-populated default. A new per-deployment `DeploymentConfig.DisableCacheControlAutoPopulate` (`disable_cache_control_auto_populate` in YAML), defaulting to `false` (auto-populate ON, matching every deployment configured before this field existed), lets an operator opt a specific deployment out entirely. The opt-out reaches the adapter via a new `adapter.ChatRequest.DisableCacheControlAutoPopulate` field (`json:"-"` — never client-reachable via the request body, since it expresses an operator policy, not a per-call choice) that `dataplane` sets from the resolved `Deployment`'s own field immediately before every `ToProvider` call, mirroring the pre-existing `Model`/`UpstreamModel` per-call-override pattern rather than widening the `Adapter` interface's signature. OpenAI/openaicompat/Gemini are unaffected — none of their `ToProvider` implementations read the new field at all.

## Cache Subsystem

Cache is a package boundary, **not a network hop**, at every stage until (if ever) `docs/decisions/0002-cache-embedded-in-gateway.md`'s extraction triggers fire. Gateway's request pipeline only ever calls `cache.Cache.Get`/`Put` (L1/L2) or `cache.LexicalCache.Search`/`Put` (L3 — a distinct interface, since a similarity search returns zero-to-many scored candidates, not a single hit/miss) — never a concrete implementation. Internally: L1 (exact hash, SHA-256, in-process, LRU-bounded — real, per `internal/cache/key.go`/`internal/cache/inprocess`), L2 (normalized-match, a narrow 3-operation allowlist — outer whitespace trim, Unicode NFC, trailing terminal punctuation strip — real, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`; deliberately narrower than that RFC's own grounding research recommended, since Kelvran's agent traffic can plausibly include pasted code where internal-whitespace/case normalization risks a wrong-answer collision), **L3-lite** (MinHash/shingling lexical near-duplicate matching — never real embedding-based semantic similarity, deferred to a later RFC per `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`'s "why not real embeddings yet" — real, gated by an entity/number/date hard-gate, a per-tenant-partitioned `inprocess.LexicalCache`, a freshness/risk model, and a hard bypass for volatile queries; see `PRD.md`'s scope note that L3 must never ship without the hard-gate). L1, L2, and L3 are each a separate `inprocess` cache instance, independently capacity-bounded (LRU eviction, no unbounded mode) — L3's bound is structurally *per tenant*, unlike L1/L2's single shared cap, since true tenant partitioning for a similarity search is a security requirement (`THREAT_MODEL.md`'s KeyPooling mitigation), not a style choice. Tenant namespace is real for every layer today (`cache.Key()`/`cache.NormalizedKey()`'s leading `tenantID` parameter for L1/L2, `LexicalCache`'s own `tenantID` parameter for L3, per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`), enforced at every hop (lookup, write, retry, fallback) — the design decision that defeats cross-tenant leakage.

## MCP/A2A Subsystem

Shares Gateway's own auth/budget/audit objects rather than being a second gateway with a second config source — inbound (expose Kelvran's own APIs as MCP tools) and outbound (broker agent tool calls to model providers) brokering both flow through `/internal/identity` and `/internal/costaccounting`.

## Guardrails Subsystem

Pre-call and post-call middleware hooks — **real**, per `docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md`: a pure-Go, stdlib-only `internal/guardrail` package (regex/checksum PII+secrets detection — email, phone, US SSN, IBAN with a real mod-97 checksum, credit card with a real Luhn checksum, IP address, API-key/secret prefixes with Shannon-entropy gating — plus a keyword/hidden-Unicode prompt-injection heuristic). Deliberately **not** NER and **not** an ML/third-party moderation model in this pass — no mature, no-cgo, no-model-file Go NER library exists today, the same class of gap Cache L3-lite already found and narrowed around for real embeddings; pluggable to call out to a third-party moderation model later, once that architectural decision gets its own RFC. Category-tiered fail-closed (credential/financial_id/government_id) vs. fail-open-with-logging (contact_info/network_id/prompt_injection), on both the detection axis and the detector-error axis — never a single global default, and never inherited from the rate limiter's own fail-open policy (guardrails has no independent second control the way `budget.Tracker` backstops the rate limiter). Post-call is enforcement-capable on the buffered path; on streaming it is audit-only — every chunk is already flushed to the client before a complete response exists to check, a named, accepted residual risk, not silently glossed over. A guardrail policy/detector version bump forces every existing cache entry (L1/L2 via the cache key hash, L3 via a stored, checked provenance field) to become a real miss, never a silent, unchecked serve of a hit whose provenance predates the change.

## Tech Stack

| Concern | Choice |
|---|---|
| Language/runtime | Go 1.26+ |
| HTTP | `net/http` + `httputil.ReverseProxy`-derived streaming |
| Distributed rate limiting | `github.com/redis/go-redis/v9` + a Lua token-bucket script — **real**, per `docs/rfcs/2026-09-03-distributed-rate-limiting.md` (the fourth external Go dependency; opt-in, isolated to `internal/ratelimit/redislimiter`; unset = in-memory, unchanged) |
| Cache L1/L2/L3 storage | In-process (`internal/cache/inprocess`), LRU-bounded — **real** for all three layers, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md` and `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`. L3 uses pure Go stdlib (`hash/fnv`) for MinHash — zero new `go.mod` entries. Redis remains the target for a future distributed cache backend (a real embedding-based L3's vector index, or L1/L2/L3 shared across gateway instances) — not yet built |
| Control-plane config store | Postgres (`pgx`/`sqlc`) — still the target for real control-plane state |
| Budget-spend restart-durability | `go.etcd.io/bbolt` — **real**, per `docs/rfcs/2026-09-03-budget-persistence.md` (single-instance only; the third external Go dependency, near-zero *new* transitive weight since its one real dependency, `golang.org/x/sys`, is already pulled in via OTel) |
| Observability sink | ClickHouse (`clickhouse-go`); acceptable to start on Postgres/Timescale pre-scale |
| Tracing | OTel Go SDK, GenAI semantic-convention attributes — **real**, per `docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md` (the first external Go dependency this module has ever had; exporters: stdout/OTLP/none) |
| Cost/budget arithmetic | `github.com/shopspring/decimal` — **real**, per `docs/rfcs/2026-09-02-decimal-cost-accounting.md` (the second external Go dependency; zero transitive dependencies) |
| Distribution | Single static binary, scratch/alpine Docker image |

## Cross-Cutting Contract

`gateway` emits OTel spans and `gatewayevents` (cost/usage/decision events) per the versioned schema in the root `api/` directory — `evals` consumes these without any source dependency on this binary's internals.
