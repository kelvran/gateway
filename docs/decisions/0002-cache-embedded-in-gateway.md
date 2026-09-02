# 0002 — Cache is embedded in Gateway, not a standalone service

- **Status**: accepted
- **Date**: 2026-09-02
- **Deciders**: project founder

## Context and Problem Statement

Cache (multi-layer LLM response caching) is conceptually distinct from Gateway (routing/proxying). Should it be built as its own deployable/service, or as a module inside the Gateway binary?

## Decision Drivers

- Cache's own natural architecture — a front door that terminates auth, resolves tenant identity, and runs its own upstream router — is 60-70% a duplicate of Gateway's architecture.
- Every real product surveyed that ships both capabilities (LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Braintrust, Helicone) embeds caching in-process. Zero exceptions found.
- A standalone Cache with its own auth/tenant/routing surface duplicating Gateway's is exactly the shape of failure that produced Portkey's ~2-year OSS/enterprise-fork drift before a public remerge.
- Extraction should stay possible without being a rewrite, if a real trigger ever fires.

## Considered Options

1. Standalone Cache service, called by Gateway over the network on every request.
2. Cache as an internal Go package, called in-process, with no seam for future extraction.
3. Cache as an internal Go package behind a narrow interface, with dormant network adapters already stubbed for future extraction.

## Decision Outcome

**Option 3.** `cache.Cache` is defined as an interface (`Get`, `Put`) taking value objects with typed errors and explicit `context.Context` — never a pointer into Gateway's internals. Three adapters implement it: `inprocess` (active), `grpcserver` and `grpcclient` (both dormant, wrapping/calling the same logic over gRPC using a `.proto` contract that's defined now but unused). If Cache is ever extracted, the only required change is swapping the adapter wired in `cmd/gateway/main.go` — Gateway's request-pipeline code, tests, and mocks never touch a concrete Cache implementation, only the interface.

## Consequences

**Positive**: no duplicated auth/routing surface to drift; Cache's storage/eviction/embedding-index internals stay entirely private (`cache/internal/`) and can be rewritten (e.g. a Rust FFI island for embedding-similarity math) without touching Gateway at all; extraction later is a one-line change, not a rewrite.

**Negative**: Cache cannot serve a caller that bypasses Gateway entirely until it's actually extracted — accepted, because that scenario is one of the explicit extraction triggers below, not a day-one requirement.

## Revisit Triggers (extract Cache only when one of these fires, with evidence)

1. A specific person/pair owns Cache full-time and is measurably blocked by Gateway's release cadence.
2. Production telemetry shows Cache's resource profile (embedding-index/cache RAM) diverging from Gateway's (connection/request load) enough that co-located replication forces over-provisioning one dimension to satisfy the other.
3. A second, independent caller needs Cache in front of a traffic path that doesn't route through this Gateway at all — the one trigger that isn't really a choice.
4. A profiled CPU-bound bottleneck in embedding-similarity/tokenization math — try a Rust FFI island inside the same binary first; only escalate to a network split if that doesn't resolve it.

**Not a trigger**: "it would be architecturally cleaner," "we might need to scale it differently someday." Full reasoning: parent workspace's `ai-infra-research/decision-single-vs-separate.md` §5.
