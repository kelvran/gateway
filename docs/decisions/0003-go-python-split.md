# 0003 — Go for Gateway/Cache, Python for Evals

- **Status**: accepted
- **Date**: 2026-09-02
- **Deciders**: project founder

## Context and Problem Statement

Three languages were on the table across the whole system: Rust, Go, and Python. Should the system pick one language for consistency, or split by component? If split, which language for which component?

## Decision Drivers

- Gateway/Cache is an I/O-bound reverse proxy handling many concurrent streaming connections — a workload where Go's goroutines and mature concurrent GC are proven at real scale (Traefik, Caddy, Kong's newer tooling), and where Python's interpreter overhead is a documented ceiling (LiteLLM's own self-reported ~7.5ms/request Python-path overhead, and its own subsequent migration of the hot path toward Rust).
- Rust's raw-performance ceiling is real (Pingora, TensorZero, agentgateway) but its borrow-checker/async-ecosystem tax buys headroom a solo/small-team build won't hit first — the value here is in provider-adapter breadth and correctness, not shaving sub-millisecond runtime overhead.
- Evals is a fundamentally different workload — I/O-bound sandboxed rollout orchestration plus a statistics/LLM-judge ecosystem (bootstrap resampling, Bayesian model comparison, judge SDKs) with no real Go or Rust equivalent. Every comparable product in this space (Langfuse, Braintrust, DeepEval, Ragas, METR's Hawk) is Python or TypeScript.
- Forcing one language across the whole system trades a real, present technical mismatch (Evals' Python-only ecosystem) for an illusory uniformity benefit — every polyglot precedent surveyed (Monzo, SoundCloud, vLLM's own Semantic Router: Go orchestration + a bound Rust library) manages the cross-language boundary with a versioned contract, never shared code, and that mechanism is what actually keeps a two-language system safe.

## Considered Options

1. All-Python (fastest initial build velocity, but LiteLLM's own mid-life pivot away from a pure-Python hot path is the visible cost of this choice for a gateway specifically).
2. All-Go (workable for Gateway/Cache; would force Evals' stats/judge logic into a much thinner ecosystem than Python's).
3. All-Rust (highest performance ceiling; steepest ramp for a solo/small-team build; smallest hiring pool).
4. Go for Gateway/Cache, Python for Evals, connected by a versioned contract (chosen).

## Decision Outcome

**Option 4.** Go 1.25+ for `gateway/` (net/http + `httputil.ReverseProxy`-derived streaming, goroutine-per-connection, `context.Context` carrying session/OTel baggage). Python for `evals/` (asyncio/Ray for orchestration, native provider SDKs, numpy/scipy/scikit-learn for statistics). The two communicate only through `api/` — versioned protobuf for the OTel schema and cost/usage events, generated into both languages' native bindings, checked by `buf breaking` in CI. **Corrected 2026-10-08**: the toolchain and library specifics in this paragraph recorded the 2026-09-02 intent and have since drifted from the code — the decision itself (Option 4, and the `api/` contract boundary) is unchanged and still exactly as built. Go: the floor is `go 1.26.8` (`gateway/go.mod`), CI pins the same (`go-version: "1.26.8"` in `.github/workflows/ci.yml`), and the release image builds on `golang:1.27.1-alpine` (`gateway/Dockerfile`) — "Go 1.25+" is no longer a floor that builds. Streaming: nothing in `gateway/` imports `net/http/httputil` and `ReverseProxy` appears nowhere in any `.go` file (repo-wide grep); upstream calls and the SSE relay are hand-rolled over `net/http` (upstream: `NewHTTPUpstreamCaller`/`NewHTTPUpstreamStreamCaller` in `gateway/internal/gateway/dataplane/dataplane.go`; client-facing relay: `HandleChatCompletionStream` in `gateway/internal/gateway/dataplane/streaming.go`, served by `handleStreamingChatCompletion` in `gateway/cmd/gateway/main.go`), with `httputil` named only in a `dataplane.go` comment as pattern precedent for an idle-timeout. Orchestration: Ray was evaluated and rejected on 2026-09-07 (`DECISIONS.md`'s `[2026-09-07]` asyncio-over-Ray entry; `docs/rfcs/2026-09-07-evals-sandbox-pool-deferred-decision.md`; `evals/ARCHITECTURE.md`'s Tech Stack "Orchestration" row) — `evals/` is `asyncio` only (`evals/evals/cli.py` drives judge panels with `asyncio.run`/`asyncio.gather`; `evals/evals/rollout/scheduler.py` is deliberately sequential), and `ray` appears nowhere in `evals/pyproject.toml` or `evals/uv.lock`. Statistics: `numpy`/`scipy`/`scikit-learn` are not dependencies either (absent from `evals/pyproject.toml` and `evals/uv.lock`, never imported); `evals/evals/stats.py` is pure standard library (`math`, `random`, `statistics.NormalDist`).

**Refinement, not an exception to this decision**: a narrow, profiled Rust FFI island inside Cache specifically (embedding-similarity/tokenization math) is a legitimate later addition if profiling proves it's a bottleneck — the pattern vLLM's own Semantic Router uses (Go orchestration + a bound Rust `candle`-style library) — but only behind a stable interface, and never touching auth/routing/quota/spend logic. A real community attempt to port exactly that class of logic (LiteLLM's auth/rate-limiting) to Rust surfaced 11 open security findings, including auth-bypass and spend-limit-bypass issues — a concrete reason this boundary is a hard rule, not a guideline.

## Consequences

**Positive**: each component is built in the language its own workload actually rewards; no forced ecosystem compromise on either side; the contract boundary is enforceable in CI (`go-arch-lint`/`import-linter` within each language, `buf breaking` across the boundary).

**Negative**: two toolchains to maintain (Go + Python), two sets of CI/lint config, a contributor needs familiarity with whichever side they're touching — accepted, since the alternative (forcing one language) costs more in ecosystem mismatch than it saves in tooling uniformity.

## Revisit Triggers

None expected for the Go/Python split itself — this is treated as settled, not "no stage" in the scale table. The only expected evolution is the narrow Rust-FFI-island refinement inside Cache, gated strictly on profiled evidence, never adopted preemptively. Full reasoning and the language-by-scale-stage table: parent workspace's `ai-infra-research/decision-single-vs-separate.md` §3.
