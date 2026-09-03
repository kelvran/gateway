# gateway — Architecture

Go binary. Contains the Gateway (routing/proxying) and Cache (embedded, internal module) capabilities in one process. This document describes the internals of that binary. For the whole-system view (how `gateway` relates to `evals`), see the root `ARCHITECTURE.md`.

## Package Layout

```
/cmd/gateway              — main binary entrypoint (single static binary)
/internal/gateway
    /controlplane          — config compilation, cert rotation, metrics; infrequent, "slow and smart"
    /dataplane              — accept/filter/forward hot path; continuous, "dumb and fast"
/internal/adapter/{openai,anthropic,gemini,bedrock,vertex,openaicompat}
                           — bidirectional (canonical↔native) request/response transformers, one per provider.
                             openai and anthropic additionally implement streaming.StreamingAdapter (real,
                             stateful per-request StreamDecoder each) — gemini/bedrock/openaicompat do not,
                             and a streaming request routed to one of them returns a typed
                             dataplane.ErrStreamingNotSupported rather than silently buffering.
/internal/streaming        — transport-level SSE plumbing, provider-agnostic: canonical ChatCompletionChunk/
                             ChunkChoice/MessageDelta/ToolCallDelta types, the StreamDecoder/StreamingAdapter
                             interfaces every streaming-capable adapter implements against, and the actual
                             Reader (SSE frame parser)/Writer (SSE frame writer, Flush()-per-chunk) — ACTIVE
/internal/router          — load-balancing strategies (weighted, usage-based, latency-based, cost-based),
                             cooldowns/circuit-breaker, deployment-level and model-group fallback chains
/internal/ratelimit        — per-virtual-key token bucket — ACTIVE, per
                             docs/rfcs/2026-09-03-distributed-rate-limiting.md. In-memory by default
                             (single-process); optionally Redis-backed (internal/ratelimit/redislimiter,
                             a Lua script over go-redis, atomic across any number of gateway instances)
                             when `rate_limit.redis_addr` is configured — a Redis backend error fails
                             open (logged, request allowed), since internal/budget's per-key USD cap is
                             an independent backstop. Hierarchical scope resolution (org/team/user/session)
                             remains target-only, same boundary as identity's own scope deferral below
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
/internal/provideradapter    — OpenAI/Anthropic/Gemini/Bedrock/self-hosted client wrappers
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
/internal/mcp                — inbound (expose Kelvran's own APIs as MCP tools) + outbound (broker agent
                             tool calls) brokering — shares identity/costaccounting, not a second gateway
/internal/guardrail          — pre/post-call middleware interface; PII/content checks
/internal/admin               — control-plane API: declarative config, live no-restart mutation
```

**Dependency direction rules** (enforced in CI via `go-arch-lint`, since Go's `internal/` visibility only catches direct imports, not transitive ones):

```
gateway  → cache → { identity, telemetry, costaccounting }
gateway  → { identity, budget, ratelimit, provideradapter, costaccounting, telemetry }
cache    ✗→ provideradapter     (cache is provider-agnostic — keyed on normalized request, not on which
                                  upstream served it)
cache    ✗→ gateway              (no back-references — this is what makes cache extractable later)
{identity, budget, ratelimit, telemetry, provideradapter, costaccounting} ✗→ gateway, cache   (shared kernel is a leaf)
budget   ✗→ identity              (budget tracks by key ID string only — it doesn't need to know what a
                                  VirtualKey is, only that it's a string; keeps both packages independently
                                  testable and reusable)
ratelimit ✗→ identity            (same reasoning as budget above — KeyConfig carries a plain key ID string)
ratelimit/redislimiter ✗→ ratelimit   (the interface (RedisBackend) lives in the consumer package; the
                                  Redis-specific implementation never imports it back — the same pattern
                                  budget/boltstore already established, so go-redis stays out of
                                  ratelimit's own dependency graph in the default, in-memory-only case)
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
  → router (load-balance / fallback-chain / circuit-breaker selects a healthy deployment)
  → provider adapter: canonical → provider-native request translation
  → upstream call (streaming: non-buffering pass-through, chunk-by-chunk, explicit Flush() per chunk)
  → provider adapter: provider-native response/chunk → canonical translation (stateful per-stream parser)
  → guardrail post-call
  → cache write-back (all layers)
  → cost/observability finalize (OTel span close, Decimal cost calc, session roll-up) — ALWAYS runs,
    even on error/cancel, via Go `defer` — a partial generation still consumed billable output tokens
  → response to client
```

Cache lookup happens *before* the router/provider call so a hit never touches an upstream. Guardrails wrap the call symmetrically. Cost/observability finalization is structured to always execute.

## Canonical Schema & Provider Adapters

One canonical internal schema, OpenAI Chat-Completions-shaped — the dialect vLLM/TGI/Ollama/DeepSeek/Together/Groq already speak natively, making self-hosted integration nearly adapter-free. Each adapter is a pure-function pair (`ToProvider()`/`FromProvider()`) that must explicitly own four normalization points that break silently if missed:

1. **Tool-call argument encoding** — OpenAI/DeepSeek/Qwen return a JSON *string*; Anthropic/Gemini/Bedrock return an already-parsed *object*.
2. **System-prompt placement** — in-array `role:"system"` (OpenAI) vs. top-level `system` param (Anthropic/Bedrock) vs. `systemInstruction` (Gemini).
3. **Streaming event shape** — OpenAI's homogeneous `delta.content` fragments vs. Anthropic's typed SSE event sequence (needs a stateful per-stream parser tracking open content blocks / accumulating tool-call indices) vs. Bedrock's binary EventStream encoding. Real for OpenAI and Anthropic (see `/internal/streaming` above and each adapter's `stream.go`); Bedrock's EventStream case remains a documented hazard, not yet implemented.
4. **Unknown-field preservation** — e.g. Gemini's `thoughtSignature` must round-trip verbatim across turns or multi-turn tool use silently breaks. Adapters must never strip fields they don't recognize.

Each adapter offers an `additionalModelRequestFields`-style escape hatch (mirroring Bedrock's own Converse API pattern) so a new provider's quirk never requires touching the core pipeline.

## Cache Subsystem

Cache is a package boundary, **not a network hop**, at every stage until (if ever) `docs/decisions/0002-cache-embedded-in-gateway.md`'s extraction triggers fire. Gateway's request pipeline only ever calls `cache.Cache.Get`/`Put` (L1/L2) or `cache.LexicalCache.Search`/`Put` (L3 — a distinct interface, since a similarity search returns zero-to-many scored candidates, not a single hit/miss) — never a concrete implementation. Internally: L1 (exact hash, SHA-256, in-process, LRU-bounded — real, per `internal/cache/key.go`/`internal/cache/inprocess`), L2 (normalized-match, a narrow 3-operation allowlist — outer whitespace trim, Unicode NFC, trailing terminal punctuation strip — real, per `docs/rfcs/2026-09-03-cache-l2-normalized-match.md`; deliberately narrower than that RFC's own grounding research recommended, since Kelvran's agent traffic can plausibly include pasted code where internal-whitespace/case normalization risks a wrong-answer collision), **L3-lite** (MinHash/shingling lexical near-duplicate matching — never real embedding-based semantic similarity, deferred to a later RFC per `docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md`'s "why not real embeddings yet" — real, gated by an entity/number/date hard-gate, a per-tenant-partitioned `inprocess.LexicalCache`, a freshness/risk model, and a hard bypass for volatile queries; see `PRD.md`'s scope note that L3 must never ship without the hard-gate). L1, L2, and L3 are each a separate `inprocess` cache instance, independently capacity-bounded (LRU eviction, no unbounded mode) — L3's bound is structurally *per tenant*, unlike L1/L2's single shared cap, since true tenant partitioning for a similarity search is a security requirement (`THREAT_MODEL.md`'s KeyPooling mitigation), not a style choice. Tenant namespace is real for every layer today (`cache.Key()`/`cache.NormalizedKey()`'s leading `tenantID` parameter for L1/L2, `LexicalCache`'s own `tenantID` parameter for L3, per `docs/rfcs/2026-09-02-virtual-keys-budgets.md`), enforced at every hop (lookup, write, retry, fallback) — the design decision that defeats cross-tenant leakage.

## MCP/A2A Subsystem

Shares Gateway's own auth/budget/audit objects rather than being a second gateway with a second config source — inbound (expose Kelvran's own APIs as MCP tools) and outbound (broker agent tool calls to model providers) brokering both flow through `/internal/identity` and `/internal/costaccounting`.

## Guardrails Subsystem

Pre-call and post-call middleware hooks, independently swappable. Ships with a basic PII/NER + regex classifier at v1; pluggable to call out to a third-party moderation model later. Fail-closed for regulated content categories, fail-open (with logging) for low-stakes ones.

## Tech Stack

| Concern | Choice |
|---|---|
| Language/runtime | Go 1.25+ |
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
