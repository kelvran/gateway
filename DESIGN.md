# Kelvran — Design

> **Dated 2026-09-02. Frozen.** This is a one-time, whole-system design sketch written before any code existed. It captures the *reasoning* behind the foundational decisions below. For the *current* state of the system as it's actually built, see `ARCHITECTURE.md` — that document supersedes this one on any point where they'd disagree. The three decisions below are formalized as ADRs in `docs/decisions/`; smaller decisions made later live in `DECISIONS.md`. Open questions this document raises but doesn't resolve are tracked as `docs/rfcs/` candidates once the corresponding work starts.

## Whole-System Sketch

Kelvran is one monorepo containing two deployables that share a single versioned contract and nothing else.

```
                        ┌─────────────────────────────────────────┐
                        │              gateway  (Go)                │
  client / agent  ───▶  │  auth → rate-limit → cache lookup          │ ───▶  OpenAI / Anthropic /
  request               │  (L1 exact → L2 normalized → L3 semantic)  │       Gemini / Bedrock /
                        │  → router → provider adapter → stream      │       self-hosted (vLLM/…)
                        │  → guardrail → cache write-back            │
                        │  → cost/OTel finalize                      │
                        └──────────────────┬──────────────────────────┘
                                           │  OTel spans + gatewayevents
                                           │  (versioned contract, api/)
                                           ▼
                        ┌─────────────────────────────────────────┐
                        │              evals  (Python)               │
                        │  ingestion ← production trace sample       │
                        │  rollout: sandboxed agent execution        │
                        │  judge: LLM-as-judge + stats (CI, pass@k)  │
                        └─────────────────────────────────────────┘
```

`evals` never sits in the request path. It calls `gateway`'s own API to run offline eval rollouts against candidate models/prompts/routes, and it samples `gateway`'s production telemetry online to catch regressions. `gateway`'s Cache is not a third component in this diagram — it's a package inside the `gateway` binary, called in-process, never a network hop. That last point is itself one of the three foundational decisions below, so it's worth explaining why.

## Decision 1: One monorepo, two deployables — not one, not three

**The question:** should Gateway, Cache, and Evals be one combined project, or three fully separate ones?

**The reasoning:** Neither extreme holds up. A single deployable is impossible outright — Go and Python cannot share a runtime, so Evals can never literally merge into the Gateway process. And the research found the one real company that *did* unify gateway+cache+observability+evals+optimization into one team/codebase/business (TensorZero) was the one company in the whole survey that shut down — a go-to-market failure, but one the technical over-unification directly enabled. Three fully separate projects fares no better: every comparable product surveyed (LiteLLM, Portkey, TensorZero, Bifrost, Kong AI Gateway, Helicone) keeps its gateway and cache fused in one process with zero exceptions, and every "split early" case study in the broader architecture literature (Segment's 140+ microservices, InVision, Istio's control plane) describes a team that split early, paid a real cost, and reversed it.

The actual boundary is forced by one fact nobody disputes: Go and Python can't share a runtime. So: **`gateway/`** is one Go binary (Gateway + embedded Cache), **`evals/`** is one Python service, and both live in **one repository** so the thing that must be versioned atomically across the language boundary — the OTel/proto contract — never drifts the way Portkey's own OSS-and-enterprise-fork split did for two years before a public remerge.

**When this changes:** only on evidence, never on anticipation. See `docs/decisions/0001-monorepo-two-deployables.md` for the full decision record and the concrete triggers that would justify revisiting it.

## Decision 2: Cache is embedded in Gateway, not a standalone service

**The question:** should Cache be its own deployable/service, given it's conceptually distinct from routing?

**The reasoning:** Cache's own natural architecture — a front door that terminates auth, resolves tenant identity, and runs its own upstream router — is 60-70% a duplicate of Gateway. Standing it up as a separate service means running two auth/routing surfaces that will inevitably drift out of sync (this is exactly the failure mode Portkey lived through). It's built instead as an internal Go package with a narrow interface (`cache.Cache { Get, Put }`) that takes value objects in and out, with two *dormant* adapters (`grpcserver`/`grpcclient`) already stubbed alongside the *active* `inprocess` one. If a real trigger ever fires — a second, independent caller that needs Cache without going through Gateway; a measured resource-curve divergence; a team that's genuinely blocked by Gateway's release cadence — extraction is a one-line change in `cmd/gateway/main.go` (swap the adapter), not a rewrite. That reversibility is the entire point of designing the seam now, while it's free.

**When this changes:** see `docs/decisions/0002-cache-embedded-in-gateway.md` for the concrete extraction triggers — none of which is "it would be architecturally cleaner."

## Decision 3: Go for Gateway/Cache, Python for Evals — never one language for uniformity's sake

**The question:** should the whole system converge on one language for a smaller hiring pool and simpler tooling?

**The reasoning:** Gateway is an I/O-bound reverse proxy handling thousands of concurrent streaming connections — Go's goroutines and mature concurrent GC are proven at exactly this scale (Traefik, Caddy, Kong's newer tooling), and LiteLLM's own mid-life pivot from a pure-Python hot path to a Rust core is the visible cost of *not* starting compiled. Cache inherits Go by virtue of being embedded in the same binary. Evals is a fundamentally different workload — I/O-bound sandboxed rollout orchestration plus a statistics/LLM-judge ecosystem (bootstrap resampling, Bayesian model comparison, judge SDKs) that has no real Go equivalent — and every comparable product in this exact space (Langfuse, Braintrust, DeepEval, Ragas) is Python or TypeScript for exactly this reason.

Forcing this onto one language would trade a real, already-present technical mismatch for an illusory uniformity benefit. The mechanism that keeps a two-language system safe in every precedent studied is a **versioned contract crossing the boundary, never a shared library** — `api/otel/` and `api/gatewayevents/` as protobuf, generated into both Go and Python bindings, with `buf breaking` in CI so an incompatible schema change fails the build automatically rather than drifting silently.

**Refinement, not a language change:** a narrow, profiled Rust FFI island inside Cache (embedding-similarity/tokenization math specifically) is a legitimate later addition — the pattern vLLM's own Semantic Router uses — but only behind a stable interface, only once profiling proves it's the bottleneck, and never touching auth/routing/quota logic. See `docs/decisions/0003-go-python-split.md`.

## Shared-Contract Concept

There is no shared database and no shared library between `gateway` and `evals`. There is exactly one shared surface: `api/`, containing versioned protobuf definitions for the OTel semantic-convention schema and the cost/usage event schema (`gatewayevents`). Both sides generate bindings from the same source; neither side imports the other's source code. `buf breaking` runs in CI against this directory specifically, because a schema change that silently breaks the other language is a worse failure mode than getting the deployable-count decision wrong.

## Open Design Questions

These are flagged here, not resolved — they become `docs/rfcs/` entries once the corresponding feature work actually starts, per `PRD.md`'s scope boundary:

- **Semantic-cache risk gating**: the exact shape of the freshness/risk model that replaces a bare similarity threshold (candidates and prior art are in the parent workspace's `ai-infra-research/cache.md` §3, §5) — not designed here, only flagged as necessary before L3 caching ships.
- **Skeptic-panel protocol**: independent-refutation vs. pairwise-comparison judge panels for Evals — research favors independent refutation, but the exact panel-size/quorum rule is an RFC, not a v1 decision.
- **Virtual-key/budget data model**: sketched at a high level in `ARCHITECTURE.md`'s data-model section once it exists; the exact schema (hierarchical scope resolution: org → team → user → key → session) is implementation detail settled during Gateway's Phase 1 build, not here.
