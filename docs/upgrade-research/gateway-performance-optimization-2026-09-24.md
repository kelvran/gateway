# Kelvran Gateway — 2026 Performance/Latency-Optimization Upgrade Research & Prioritized Roadmap

**Date:** 2026-09-24
**Scope:** `gateway/` — connection pooling/keep-alive tuning, HTTP/2 or HTTP/3 to upstream providers, streaming-response buffer sizing, provider-selection/routing algorithms that account for real latency (vs. static weights), speculative/parallel multi-provider racing, and request coalescing beyond simple cache-stampede protection. Grounded first against `gateway/internal/router/router.go` and `health.go` (the real, currently-shipping WRR + active-probe health + latency-de-weighting + ramp-recovery mechanisms), `gateway/cmd/gateway/main.go`'s upstream HTTP transport/client construction, and `gateway/ARCHITECTURE.md`'s Tech Stack section, per this round's explicit brief. Deliberately excludes ground the sibling report `docs/upgrade-research/performance-latency-optimization-2026-09-14.md` already covered in depth — `net/http/pprof`, `GOMEMLIMIT`/`GOGC`, the L3 lexical-cache LSH question, and existing `go test -bench` baselines are not re-litigated here; where this round's findings overlap with that report's own connection-pooling finding, the current, now-changed code state is what's checked, not the 09-14 snapshot.

**Method:** Adversarial multi-source research (3-vote verification per claim) against real, currently-shipping 2026 practice: Envoy (core proxy docs + Envoy AI Gateway), agentgateway (Solo.io), Kong AI Gateway, LiteLLM, Ramp's own internal-LLM-gateway engineering writeup, the Go standard library's own source/issue tracker, a general-purpose Go singleflight-coalescing library, and an SSE-reverse-proxy engineering blog. 25 claims were put to adversarial vote: 14 confirmed, 11 refuted. This document adds the Kelvran-specific "why it matters here / is this actually a gap once the live code is read / effort-and-traffic-gate tier" layer the raw adversarial verification deliberately does not do — several of this round's initially-plausible "gaps" turned out, on reading `router.go`/`health.go`/`main.go` directly, to already be shipped; those are recorded below as explicit non-gap confirmations, not omitted, per this round's own instruction not to assume a gap exists without checking first.

---

## Executive summary

Grounding this round's brief against Kelvran's own live code overturned more assumed gaps than it confirmed. Kelvran's gateway already ships automatic HTTP/2-over-ALPN to every upstream provider (Go's `http.DefaultTransport.Clone()`, untouched `ForceAttemptHTTP2`), an already-tuned per-host idle-connection pool (`upstreamMaxIdleConnsPerHost = 100`, deliberately raised from Go's default of 2 specifically to avoid the `golang/go#13801` connection-churn failure mode — the fix the 2026-09-14 sibling report itself recommended), a real, live latency-aware soft de-weighting signal in the router (`Router.SetLatencyFactor`), an SSE writer that already flushes after every chunk with zero extra buffering between the upstream reader and the client write (a stronger backpressure property than the bounded-channel pattern this round's own best comparator source recommends), and `singleflight`-based request coalescing for exact-duplicate buffered requests. The one genuinely confirmed, narrow gap on the request-coalescing axis is that this same coalescing is deliberately, self-disclosedly absent on the streaming path. HTTP/3/QUIC to upstream providers is a real absence, but it fails the "worth building" bar on its own economics, not on a traffic-volume gate: no LLM provider Kelvran actually calls speaks QUIC today, so Envoy's own 300ms-race-then-fallback mechanism has no real target to race against. The "next level" routing techniques surfaced in comparators (agentgateway's live Power-of-Two-Choices EWMA scoring, Ramp's Thompson-Sampling deadline-aware routing) are real and would meaningfully sharpen Kelvran's existing latency signal — but, like Kelvran's own already-deferred statistical circuit breaker, both are online-learning techniques that need real production traffic to mean anything, landing them in the same "track, don't build yet" bucket Kelvran already uses for that analogous deferral. No comparator was found racing multiple LLM providers in parallel for one logical completion — plausibly because, unlike TCP/QUIC connection racing, racing LLM providers means paying for N completions, a cost multiplier at odds with this project's own cost-optimization thesis.

---

## Findings, ranked

### 1. Request coalescing is real for buffered requests — and deliberately, self-disclosedly absent for streaming (Small–Medium)

**What:** `dataplane.Pipeline.runMissPath` already deduplicates concurrent identical **buffered** cache misses via `golang.org/x/sync/singleflight` (`missGroup singleflight.Group`), keyed on the exact-match `l1Key` (which itself bakes in the tenant ID) — only the first caller for a given key runs the real guardrail+router+upstream body; every other concurrent caller blocks and receives the same result. `streaming.go`'s own doc comment states this plainly: *"The streaming path has no singleflight coalescing (unlike runMissPath's buffered miss path) — every completed stream is its own real, unshared upstream call."* This is not an oversight this research discovered — it is a named, accepted v1 scope limit already in the code — but it is a real gap against this round's own research question ("request coalescing beyond simple cache-stampede protection"), since the buffered-path mechanism *is* exactly that pattern, just not extended to streaming.

**Why it matters for Kelvran specifically:** a multi-agent fan-out pattern (N parallel agents issuing the identical prompt against the same model, a plausible shape for exactly the kind of agentic workloads Kelvran targets) currently pays for N full upstream streaming completions today, with no coalescing safety net at all on that path — the same "cache-miss storm causing redundant expensive upstream calls" `THREAT_MODEL.md` already names for the buffered path, just unmitigated here.

**Consistent with settled decisions?** Extends, not reopens, `runMissPath`'s own documented v1 scope limit — the buffered-path mechanism (`golang.org/x/sync/singleflight`) is already Kelvran's chosen tool for exactly this problem; this is "use the same tool somewhere it isn't yet," not a new design decision.

**2026 best practice grounding:**
- `mopo3ula/dedup` (a real, MIT-licensed, currently-maintained Go library) implements the identical general pattern Kelvran's own `runMissPath` already uses — only one concurrent identical request becomes the "original" that executes the real work; every other waiter blocks and receives the same result [github.com/mopo3ula/dedup — confirmed 3-0, verified directly against the library's own source, not just its README]. This confirms Kelvran's *existing* buffered-path technique is itself the current, standard 2026 shape for this problem — the gap here is coverage, not technique.

**Concrete next step:** a streaming-specific coalescing design needs a **broadcast**, not a single `Do()` return value — N concurrent identical streaming callers need to each receive the *same sequence* of SSE chunks as they arrive, not just one shared final result, meaningfully more involved than the buffered case's "share one value" shape. Worth a small design spike (a fan-out `chan ChatCompletionChunk` per in-flight `l1Key`, each follower subscribing) before committing to a full RFC.

**Effort:** Small–Medium for the design spike; Medium for the real implementation (a genuinely different mechanism from `singleflight.Group`, not a drop-in reuse). **Traffic-gate check:** this does *not* need the "wait for real production traffic volume" gate Kelvran applies elsewhere — the value of coalescing here depends on traffic *shape* (how often genuinely-identical concurrent streaming requests occur), not *volume*, and that shape question could be answered by simply logging near-duplicate-key collisions in production before building the fix, a much cheaper instrumentation-first step than either building blind or waiting for volume.

---

### 2. HTTP/3 (QUIC) to upstream providers is a real absence — but not worth building: no target speaks it (Large, do-not-build-now)

**What:** `main.go`'s `newUpstreamTransport`/`newDeploymentTLSTransport` both clone `http.DefaultTransport`, which is HTTP/1.1/HTTP/2-only — Go's standard library has no built-in HTTP/3/QUIC client at all (would require an external library, e.g. `golang.org/x/net/http3` wrapping `quic-go`, plus Envoy-style connection-racing logic Kelvran would have to build itself). This is a genuine, currently-real absence, not a misreading of the code.

**Why it matters for Kelvran specifically:** the research question asks specifically about HTTP/3 to upstream *providers* — the honest answer is that this gap's "worth it" calculus depends entirely on whether Kelvran's own 5 adapters' real upstream endpoints (OpenAI, Anthropic, Bedrock, Gemini, and OpenAI-compatible self-hosted targets) serve inference traffic over QUIC today. They do not, as far as this research and general knowledge of these providers' current API surfaces can confirm — all are served over HTTPS via HTTP/1.1 or HTTP/2, not QUIC.

**Consistent with settled decisions?** Not previously scoped anywhere (no RFC or `ARCHITECTURE.md` row addresses HTTP/3), so this isn't a revisit of a prior decision — it's a genuinely new question this research round was the first to ask directly.

**2026 best practice grounding:**
- Envoy races a QUIC handshake against a TCP fallback with a fixed 300ms head start for QUIC, falling back to TCP only if QUIC hasn't established by then, and still preferring QUIC if it completes later [envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/connection_pooling — confirmed 3-0, cross-checked stable across multiple Envoy doc versions and a merged PR narrowing one mobile-only edge case].
- This mechanism exists specifically because Envoy is a *general-purpose* proxy that may have upstreams speaking QUIC (e.g. some CDNs, some Google services) — it is not evidence that LLM-provider APIs specifically have adopted QUIC.

**Concrete next step:** none, for now. If Kelvran ever adds a deployment target that genuinely serves inference over HTTP/3 (worth a one-line check against each new adapter's provider docs going forward, not a standing research task), revisit; until then this stays a documented non-gap-with-a-reason rather than a silently-dropped question.

**Effort:** Large (external QUIC library, connection-racing logic, a new code path parallel to `newUpstreamTransport`) for zero currently-real benefit. **Traffic-gate check:** does *not* apply here — this isn't blocked on Kelvran's own traffic volume at all, it's blocked on upstream providers never having adopted QUIC for inference endpoints, a precondition entirely outside Kelvran's control.

---

### 3. Provider-selection routing already accounts for real latency, not just static weights — the sophistication gap to close (EWMA/Bayesian online learning, deadline-aware scoring) is real but needs the same production-traffic gate Kelvran already applies to its statistical circuit breaker (Large, track-not-build)

**What:** `router.go`'s `Router` already implements smooth weighted round-robin (the LVS/IPVS `wrr.go` algorithm) **plus** a live, currently-shipping latency-aware signal: `health.go`'s `SetLatencyFactor` records a soft de-weighting percentage per deployment (clamped to `[latencyFactorFloorPercent=10, 100]`, applied via a Bresenham-style thinning gate in `admitLatencyThinnedTurn`), fed by "each deployment's own rolling-average probe latency relative to its model-group peers" per that method's own doc comment. This is a real, already-answered "yes" to half of this round's research question — Kelvran's routing is not purely static weights today.

**A genuine internal finding, surfaced only by reading the code directly (per this round's own instruction):** `router.go`'s own package-level doc comment (lines 20–27) still states *"Deliberately out of scope... usage/latency/cost-based routing signals"* — stale relative to the same file's own `SetLatencyFactor` and `activeCostTier`/`costTiers`, both real, live, latency- and cost-based routing mechanisms. This is the exact "doc-vs-code staleness" gotcha class `AGENTS.md`'s own Gotchas section already tracks (recurred 6+ times prior to this finding) — flagged here, not fixed, since this is a research deliverable rather than a code change, but worth a trivial drive-by doc fix whenever `router.go` is next touched.

**Why it matters for Kelvran specifically:** `SetLatencyFactor`'s own signal is a manually-fed percentage, computed by whatever caller invokes it (the health-probe loop in `dataplane`) — the `router` package itself stays deliberately I/O-free and clock-free (per its own doc comment) and has no online-learning model of its own. The genuinely next-level techniques found in comparators go further: a live, continuously-updated statistical/Bayesian model computed *from real traffic*, and (Ramp's case) a deadline-aware routing decision, neither of which Kelvran has today.

**Consistent with settled decisions?** Narrows, doesn't reopen, `router.go`'s own explicit deferral of "the traffic-derived statistical circuit breaker (Envoy-style outlier detection, LiteLLM-style `allowed_fails` cooldown)... that class genuinely needs a traffic-volume floor Kelvran doesn't have production data for yet." The online-learning routing techniques below are the same *category* of mechanism (a live statistical model fit to real traffic) as that already-deferred circuit breaker, so the same reasoning — and the same deferral — applies.

**2026 best practice grounding:**
- agentgateway's LLM load balancer uses Power-of-Two-Choices: sample two providers at random (with replacement, so the same provider can be sampled twice, preventing starvation of the lowest-scored endpoint), score each via `score = health / (1 + latency_penalty)` where `latency_penalty = request_latency * (1 + pending_requests * 0.1)`, and route to the better-scored one [docs.solo.io/agentgateway/2026.7.1/llm/load-balancing — confirmed 3-0, independently verified against the project's own live Rust source (`loadbalancer.rs`), not just its docs].
- That scoring combines two independently-tracked EWMA signals (health: 1.0/success, 0.0/failure; latency: successes only, to avoid fast-error-response skew), both using smoothing factor α=0.3 [same source — confirmed 3-0, byte-for-byte matched against the live `Ewma`/`score()` implementation].
- Ramp's internal LLM gateway goes further still: Thompson Sampling over a Normal-Inverse-Gamma conjugate prior on log-latency (updated online via Redis-pooled sufficient statistics), combined with an EWMA of genuine provider-side failure probability, into a per-candidate `P(bad_outcome) = P(failure) + (1 − P(failure)) · P(latency > deadline)` — a deadline-aware routing decision, not merely a latency-aware one — then ranked by that risk score combined with relative cost [zenml.io/llmops-database (secondary, aggregating Ramp's own primary engineering post) — confirmed 3-0 on both the model and the formula, independently cross-checked against Ramp's own primary source].

**Concrete next step (not now — a forward design note, matching this round's own "track, don't build" bucket):** if/when real production traffic exists to calibrate against, agentgateway's simpler EWMA+P2C shape is the better starting point for Kelvran specifically than Ramp's heavier Bayesian NIG-prior approach — it needs no new external dependency (Kelvran has no Redis-backed statistics store today outside the opt-in rate-limiter, per `ARCHITECTURE.md`'s Tech Stack table) and is a natural extension of the health-probe loop that already feeds `SetLatencyFactor`, rather than a new subsystem.

**Effort:** Large (a genuine online-learning subsystem, whichever shape is chosen). **Traffic-gate check:** yes, explicitly — an EWMA or Bayesian posterior computed only from Kelvran's own synthetic health probes (the only traffic that exists pre-production) would be learning from artificial signal, not real request-serving latency variance, so this needs the identical "wait for real production data" gate `router.go`'s own doc comment already applies to the statistical circuit breaker — this is not a new deferral, it's the same one, one layer up the sophistication ladder.

---

### 4. Connection pooling and keep-alive tuning — already correctly implemented; no action (confirmed non-gap)

**What:** `main.go`'s `newUpstreamTransport` clones `http.DefaultTransport` and raises `MaxIdleConnsPerHost` from Go's stdlib default of 2 to 100 (`upstreamMaxIdleConnsPerHost`), with an explicit doc comment citing `golang/go#13801`'s ephemeral-port-exhaustion failure mode as the reason — exactly the fix `docs/upgrade-research/performance-latency-optimization-2026-09-14.md`'s own Finding 1 recommended, already shipped since that report. One shared `*http.Transport` is reused across both the buffered and streaming `http.Client`s (`newUpstreamTransport`'s own doc comment), and `IdleConnTimeout`/`MaxIdleConns`/dial and TLS-handshake timeouts all inherit `http.DefaultTransport`'s own tuned stdlib defaults (90s idle timeout, 100 global idle-conn cap, 30s dial timeout) rather than a bare, defaults-free `&http.Transport{}`.

**Why it matters for Kelvran specifically:** this round's research question asked whether Kelvran's gateway lacks connection-pool tuning comparable to LiteLLM/Kong — it does not; the mechanism (raise the per-host idle cap, reuse one Transport) is the same *genre* of fix both comparators ship.

**Consistent with settled decisions?** Directly confirms — does not reopen — `performance-latency-optimization-2026-09-14.md`'s Finding 1 is real and already implemented, not merely recommended.

**2026 best practice grounding:**
- LiteLLM's default async transport (`aiohttp`, not `httpx`) already has connection-pool limiting via `AIOHTTP_CONNECTOR_LIMIT` (default 1000, env-overridable) — the gap that PR was addressing was specific to LiteLLM's separate `httpx`-based transport path, not a blanket absence of pooling across LiteLLM as a whole [github.com/BerriAI/litellm/pull/22538 — confirmed 2-1, independently verified against LiteLLM's own live `main`-branch source, not just the PR's self-description; note the PR itself was never merged, so LiteLLM's `httpx` path specifically may still lack this today].
- Kong Gateway's own general docs confirm upstream keepalive pooling is on by default (`upstream_keepalive_pool_size` defaults to 512, `upstream_keepalive_max_requests` defaults to 10000, `upstream_keepalive_idle_timeout` defaults to 60s) — Kong AI Gateway's Forward Proxy feature disabling keepalive specifically implies normal (non-forward-proxy) mode reuses upstream connections by default, consistent with that general pooling behavior [developer.konghq.com/ai-gateway/forward-proxy/ + developer.konghq.com/gateway/configuration/ — confirmed 2-1, corroborated against Kong's own general Gateway config docs and a maintainer's own GitHub discussion answer].

**Concrete next step:** none. `MaxIdleConns` (the *global*, cross-host idle cap) still inherits the cloned default of 100, identical to `MaxIdleConnsPerHost`'s own new value — with more than one upstream host configured, the global cap could in principle bind before any single host's per-host cap does. Not raised to a genuine problem by this research (Kelvran has 5 adapters, not hundreds of hosts), but worth a one-line note for whoever next revisits this constant: raise `MaxIdleConns` in the same pass if `MaxIdleConnsPerHost` is ever raised further.

**Effort:** None (already shipped).

---

### 5. HTTP/2 to upstream providers — already automatic via Go's stdlib ALPN negotiation; no action (confirmed non-gap)

**What:** `http.DefaultTransport.(*http.Transport).Clone()` (the exact construction `newUpstreamTransport`/`newDeploymentTLSTransport` both use) inherits `ForceAttemptHTTP2: true` and a fresh, independently-fired `sync.Once` that configures HTTP/2-over-TLS automatically on first use — neither function's own doc comment claims otherwise (*"every other stdlib default (dial timeouts, TLS handshake timeout, HTTP/2 support) is preserved"*), and neither overrides `TLSNextProto` to disable it. Every upstream call over TLS (all 5 of Kelvran's provider adapters) already negotiates HTTP/2 automatically wherever the provider's endpoint supports it, falling back to HTTP/1.1 otherwise, with zero code Kelvran had to write for this.

**Why it matters for Kelvran specifically:** this round's research question named HTTP/2-to-upstream explicitly as a possible gap; reading `main.go` directly (per this round's own instruction) shows it isn't one.

**Consistent with settled decisions?** No prior doc claims otherwise; this is a first-time direct check, and it confirms rather than revisits anything.

**2026 best practice grounding:**
- Envoy supports automatic upstream protocol selection via ALPN, picking between HTTP/2 and HTTP/1.1 per connection without the operator statically pinning one protocol per upstream — the same outcome (auto-negotiate the best available protocol, no static pin) Go's stdlib `ForceAttemptHTTP2`+ALPN mechanism already gives Kelvran, via a different implementation [envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/connection_pooling — confirmed 2-1, cross-checked against the Envoy v3 API proto docs for `AutoHttpConfig`].

**Concrete next step:** none.

**Effort:** None (already shipped, via the language runtime, not custom code).

---

### 6. SSE streaming buffer sizing and backpressure — already correct, and structurally stronger than the best-practice pattern this research found; no action (confirmed non-gap)

**What:** `internal/streaming/writer.go`'s `Writer.WriteChunk`/`WriteDone` both call `sw.flusher.Flush()` immediately after every single write, with `NewWriter` failing fast if the wrapped `http.ResponseWriter` doesn't implement `http.Flusher` at all — there is no batching path anywhere in the type. Separately, `streamDeployment` (`dataplane/streaming.go`) is a fully **synchronous** single-goroutine pump: it blocks on `reader.Next()` (an upstream body read), then blocks on `sw.WriteChunk` (a client write), in a tight loop — there is no channel, buffer, or separate goroutine pair between the upstream reader and the downstream writer at all.

**Why it matters for Kelvran specifically:** this round's research question named streaming-buffer-sizing and backpressure explicitly; the honest answer is Kelvran's design has **zero** extra buffering to size in the first place — a slow client simply stalls the same goroutine that's reading from upstream, which is a stronger, not weaker, backpressure guarantee than a bounded channel (which, however small, still permits some multiple of one chunk's worth of memory to accumulate per stalled stream, across however many concurrent stalled streams exist).

**Consistent with settled decisions?** Directly confirms, does not reopen, `performance-latency-optimization-2026-09-14.md`'s own Finding 2 ("the SSE streaming writer already flushes after every chunk; this is not a gap") — this round's research adds the backpressure/buffering half that report didn't specifically examine, and it also checks out clean.

**2026 best practice grounding:**
- Without an explicit flush call, Go's `http.ResponseWriter` buffers writes through multiple internal layers (a `bufio.Writer`, a `chunkWriter`, a connection-level buffer) and delivers SSE events to the client in batches rather than immediately — exactly the failure mode Kelvran's own explicit per-chunk `Flush()` calls avoid [preto.ai/blog/streaming-sse-proxy/ — confirmed 3-0, independently corroborated directly against Go's own `net/http/server.go` source].
- The same source recommends a bounded (not unbounded) in-memory channel between the upstream reader and downstream writer, sized to a fixed number of in-flight events, specifically to prevent OOM under slow-client conditions [preto.ai/blog/streaming-sse-proxy/ — confirmed 2-1]. Kelvran's synchronous, no-channel-at-all design achieves the same goal (bounded memory under slow clients) with an even tighter bound (a few KB of stdlib/OS write-buffer slack per stream, not a configurable N-event channel) — this is a design difference worth being explicit about, not a gap: Kelvran didn't miss the bounded-channel pattern, it picked a simpler design with an equal-or-better property.

**Concrete next step:** none.

**Effort:** None (already shipped).

---

### 7. Speculative/parallel multi-provider racing for a single logical completion — genuinely unresolved, likely absent industry-wide for cost reasons, not a research miss (Unscoped)

**What the research was asked to find:** whether any comparator gateway races a request against multiple LLM providers in parallel and takes the first (or best) completion, the way Envoy races a QUIC handshake against a TCP fallback at the transport layer.

**What the research found:** no comparator gateway confirmed doing this for LLM completions specifically. Envoy's own connection-racing mechanism (Finding 2, above) races *transport-layer handshakes* — an operation that costs essentially nothing extra to attempt twice — not application-layer LLM completions, which are individually metered and billed per token.

**Why this matters for Kelvran specifically:** this is a real, still-open question the research brief asked directly and could not answer with verified evidence — recommend treating this as "no confirmed comparator practice," not "confirmed absent everywhere," if revisited. The a priori economic reasoning (racing N providers for one logical request means paying for N real completions, not one — a direct multiplier on the exact cost metric Kelvran's own budget/cost-tracking subsystems exist to minimize) suggests this technique may be structurally unattractive for a cost-optimizing LLM gateway specifically, unlike TCP/QUIC racing where the "wasted" side of the race is nearly free — but this is inference, not a verified finding, and should be labeled as such if repeated.

**Effort:** N/A — needs its own dedicated research pass (specifically targeting whether any gateway offers this as an opt-in, cost-aware feature — e.g. gated to only race when the cost delta is small, or only for a subset of "urgent" requests) before any implementation decision, and even then the traffic-gate question would need answering: this technique's *value* (is the latency win worth 2x+ spend) is unknowable without real request-latency-variance data Kelvran doesn't have yet.

---

## Top 3 do next

1. **Extend `singleflight`-style coalescing to the streaming path** (Finding 1, Small–Medium design spike / Medium build). The only genuinely actionable gap this round confirmed — closes a real, already-self-disclosed scope limit in `streaming.go`'s own doc comment, and does not need to wait for a production-traffic-volume gate (only for a small amount of instrumentation to check traffic *shape* first).

2. **Fix `router.go`'s stale package-level doc comment** (surfaced inside Finding 3). A trivial, zero-risk, one-comment-block correction — the comment currently claims latency/cost-based routing signals are "deliberately out of scope," while the same file's `SetLatencyFactor`/`activeCostTier` are real and live. Matches `AGENTS.md`'s own named "doc-vs-code staleness" Gotcha pattern exactly; cheap to fix the moment anyone is back in this file for Finding 1's coalescing work or any other reason.

3. **Track (do not build) the EWMA/Thompson-Sampling online-learning routing upgrade** (Finding 3). This is the most substantial technique surfaced this round, but it is explicitly gated on real production traffic — the same gate `router.go`'s own statistical-circuit-breaker deferral already uses. Worth recording agentgateway's simpler EWMA+P2C shape as the preferred future direction over Ramp's heavier Bayesian approach now, so the choice doesn't have to be re-researched from scratch once traffic actually exists.

**Worth flagging, not immediately building:** HTTP/3 to upstream providers (Finding 2) and speculative multi-provider racing (Finding 7) are both real research questions this round answered as "not worth it right now" for two *different* reasons worth keeping distinct — HTTP/3 fails because no target provider speaks it (an external precondition, not a Kelvran-side gate), while multi-provider racing fails (tentatively) on its own cost economics for an LLM gateway specifically, not on any traffic-volume gate at all.

---

## Caveats

- Of the 14 confirmed claims, 5 rest on split 2-1 votes rather than unanimous 3-0 (LiteLLM's aiohttp-vs-httpx pooling asymmetry; Kong's forward-proxy keepalive contrastive implicature; Envoy's ALPN protocol-selection claim; the original `golang/go#27816` bug report itself; preto.ai's bounded-channel recommendation) — all five are still rated high-confidence per their own verifier evidence (independent corroboration against live source code or a second primary doc, in every case), flagged here for transparency, matching the sibling 2026-09-06 report's own convention.
- The LiteLLM `AIOHTTP_CONNECTOR_LIMIT` claim (Finding 4) describes a real, currently-live asymmetry (aiohttp pooled, httpx not), but the specific PR proposing to close the httpx-side gap (#22538) was never merged — LiteLLM's `httpx` transport path may still lack pool-size limiting today; this document does not claim that gap is closed on LiteLLM's side, only that the asymmetry itself is real.
- Time-sensitivity: agentgateway's docs are pinned to version `2026.7.1` (roughly two months old relative to this research date); the Ramp engineering post's own exact publish date could not be independently confirmed (only the ZenML database entry's 2026 dating), though the technical content was independently verified against Ramp's own primary source directly. The `golang/go` issue/CL pair (Findings 5/6 in the raw claim set) describes Go 1.12-era (2019) stdlib work — confirmed still true against current Go `master` source, so its age doesn't weaken the finding, only dates the *history* being cited.
- This document's "why it matters for Kelvran" / "consistent with settled decisions" / effort-and-traffic-gate framing was authored by synthesizing the raw adversarially-verified claims (which do not themselves make Kelvran-specific judgment calls) against a direct, first-hand reading of `router.go`, `health.go`, `main.go`, `streaming.go`, `writer.go`, and `dataplane.go` — not assumed from the research brief's own framing of what might be a gap. Four of this round's seven findings turned out to be non-gaps precisely because of this grounding step; a version of this report that skipped reading the live code first would have wrongly presented Findings 4–6 as open gaps.

## Open questions

- Is duplicate-identical-streaming-request traffic (the shape question Finding 1's fix depends on) actually common enough in Kelvran's real usage to justify the streaming-coalescing build? No usage data exists yet — recommend instrumenting near-duplicate-key detection in production before committing to the fix, cheaper than either building blind or waiting indefinitely.
- If/when real traffic justifies the online-learning routing upgrade (Finding 3), should Kelvran adopt agentgateway's simpler EWMA+Power-of-Two-Choices shape or Ramp's heavier Bayesian Thompson-Sampling+deadline-aware shape — and does Kelvran's `ChatRequest` even have (or need) a caller-specified deadline field for the latter's deadline-aware scoring to apply at all?
- Does any of Kelvran's 5 real adapters' upstream provider endpoints (OpenAI, Anthropic, Bedrock, Gemini, OpenAI-compatible self-hosted) serve inference over HTTP/3/QUIC today, which would invalidate Finding 2's "no target" verdict? Not exhaustively checked against each provider's current API surface — checked only against general knowledge of these providers' present transport choices.
- Is multi-provider parallel racing (Finding 7) genuinely absent industry-wide for cost reasons, or did this round's research simply fail to surface an existing opt-in/cost-gated implementation? Needs a dedicated, narrower follow-up pass specifically targeting this question before treating the absence as confirmed.
