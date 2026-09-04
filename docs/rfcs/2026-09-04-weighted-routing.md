- **Status**: accepted
- **Date**: 2026-09-04
- **Author(s)**: Claude (agentic session), grounded via a 5-angle dynamic-workflow research pass + independent synthesis re-verification against the live repo

## Summary

Give each `Deployment` a `Weight`, and replace `dataplane.Pipeline`'s inline atomic-counter round-robin (`nextDeployment`, `gateway/internal/gateway/dataplane/dataplane.go:617-633`) with a new `gateway/internal/router` package implementing smooth weighted round-robin (the nginx/LVS `wrr.c` algorithm). This closes the "weighted" half of `PRD.md`'s v1 routing scope line — *"static + weighted routing; a single fallback chain"* — the one part of that line not yet built. The existing single-fallback mechanism in `HandleChatCompletion`/`streamDeploymentWithFallback` is untouched: it already satisfies "a single fallback chain" and needs no chain-depth change, only a weight-aware deployment selector underneath it.

## Motivation

`gateway/ARCHITECTURE.md`'s Package Layout section already documents this as a live, named gap (corrected 2026-09-04, commit `ee99115`): real routing today is ~15 lines of plain round-robin, with "no weighting" called out explicitly as missing. `PRD.md`'s own v1 scope line commits to weighted routing as an in-scope v1 capability — it is not aspirational or deferred, unlike usage/latency/cost-based routing and circuit breakers, which the same ARCHITECTURE.md gap description bundles together with weighting without distinguishing "owed for v1" from "deferred to v2." This RFC exists specifically to make that distinction explicit and to close only the part PRD.md actually commits to.

A grounding research pass (5 parallel angles: current-code audit, routing-strategy precedent from LiteLLM/Portkey/Kong/Envoy, circuit-breaker precedent from Envoy/Hystrix/resilience4j, a PRD.md scope-boundary check, and a config/integration design pass) plus an independent synthesis re-verification found:

- **PRD.md, Scope — v1 (line 26), quoted verbatim**: *"static + weighted routing; a single fallback chain."* An affirmative allowlist — weighted routing is in scope; nothing else routing-related is named.
- `ARCHITECTURE.md`'s Request Lifecycle section (line 116) still describes the router step as doing "load-balance / fallback-chain / circuit-breaker" — stale, aspirational language that contradicts the same file's own corrected Package Layout entry. This RFC's implementation pass also fixes that inconsistency.
- Of the four routing signals real gateways implement (weighted, least-busy/usage-based, latency-based, cost-based), only **weighted** requires no live per-request tracked signal at all — it is pure static configuration, the simplest and most-recommended default in every system surveyed (LiteLLM's own docs recommend `simple-shuffle`/weighted over every stateful strategy "for best performance in production"; Portkey's load-balance mode offers *only* weighted-random; Envoy's plain weighted-round-robin needs no runtime state beyond the static weight). This makes weighted routing the correct, minimal-risk v1 slice — no live production traffic is needed to validate a stateful tracking mechanism, because there isn't one.

## Detailed Design

### Scope

**In scope**: static + weighted round-robin deployment selection within one canonical model's deployment set.

**Explicitly out of scope for this RFC** (see Alternatives Considered and PRD.md's own allowlist):
- Usage/latency/cost-based routing signals.
- Circuit breaker / consecutive-failure cooldown / health tracking.
- Model-*group* fallback chains (falling back to a different canonical model). The existing 1-hop, same-model fallback already satisfies PRD's "a single fallback chain" and is unchanged by this RFC.
- Any new config knobs for the above (no `router:` config section — there is nothing to tune yet).

### Config: `Weight` on `DeploymentConfig`

`gateway/internal/gateway/controlplane/config.go`'s `DeploymentConfig` (currently `Name, Model, Provider, UpstreamModel, BaseURL, APIKeyEnv`) gains one field:

```go
// Weight controls this deployment's share of routing selection among
// deployments serving the same Model, per PRD.md's "static + weighted
// routing" v1 scope line. Zero (unset in YAML) means "use the default
// weight of 1" — normalized in internal/router, not here, matching this
// file's existing looseness. A negative value is a config error.
Weight int
```

Parsed in the existing deployments loop, validated non-negative (a config error, not a silent clamp). `gateway/config.example.yaml` gets one commented illustration; no new top-level section.

### Selection algorithm: smooth weighted round-robin

The nginx/LVS `wrr.c` cursor algorithm — O(1) per-selection state per model, no memory proportional to weight (unlike naive weight-expansion, which also produces bursty `A,A,A,B,B,B` traffic rather than a smooth interleave for any weight > 1):

```go
func (m *modelState) next() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.deps)
	if n == 0 {
		return "", false
	}
	for {
		m.i = (m.i + 1) % n
		if m.i == 0 {
			m.cw -= m.gcd
			if m.cw <= 0 {
				m.cw = m.maxW
			}
		}
		if m.deps[m.i].weight >= m.cw {
			return m.deps[m.i].name, true
		}
	}
}
```

**Backward-compatibility proof, traced by hand, not assumed**: for `n` deployments all at the default weight (`gcd = maxW = 1`), starting from `i=-1, cw=0`, every call's inner loop returns on its very first check (`weight[i]=1 >= cw`, which is always exactly `1` immediately after any `cw` reset). This produces the sequence `0, 1, 2, ..., n-1, 0, 1, 2, ...` — **byte-identical** to today's real `nextDeployment`, which does `idx := counter.Add(1)-1; deps[idx%n]` starting from `counter=0`. Confirmed decisively, not merely expected, for **any** uniform weight `W` (not just `W=1`): `gcd=maxW=W`, and the same reasoning holds. `TestHandleChatCompletionFallsBackOnUpstreamError` (`dataplane_test.go:181`, 2 default-weight deployments, asserts exactly 2 calls in `primary, secondary` order) needs zero changes under this replacement.

**The one implementation constraint this proof depends on**: the per-model deployment list must preserve `controlplane.Load`'s existing alphabetical-by-name sort order unchanged — never re-sorted by weight (a common micro-optimization in some WRR implementations that would silently break the exact-match guarantee).

### New package: `gateway/internal/router`

A new shared-kernel leaf package (joins `ratelimit`/`budget`/`telemetry`/`provideradapter`/`costaccounting` in the dependency-direction table — must never import `dataplane` or `cache`), matching this codebase's "many small files, narrow interface" convention:

- `router.go` — `Router` type (`mu sync.Mutex`, `models map[string]*modelState`), `New(deployments []Deployment) *Router`, `Select(model string) (name string, ok bool)`.
- `deployment.go` — `type Deployment struct { Name, Model string; Weight int }`, decoupled from `dataplane.Deployment`, mirroring `ratelimit.KeyConfig`'s existing decoupling from `identity.VirtualKey`.
- `wrr.go` — `modelState` and the `next()` method above, plus `gcd`/`maxW` precomputed once per model group in `New`.
- `router_test.go` — the decisive backward-compat test (N deployments, all weight 0/1, `3N` calls, assert byte-identical to `[0..N-1]` repeated) and a genuinely-weighted case (e.g. weights `2,1`) asserting a smooth, non-bursty interleave.

Weight normalization (`0` → `1`) happens in `router.New`, keeping `controlplane`'s parser dumb and routing semantics owned by `router`.

### `dataplane.go` change points

- `Pipeline` struct: remove `deploymentsByModel map[string][]Deployment`, `rrMu sync.Mutex`, `rrCounters map[string]*atomic.Uint64`. Add `deploymentsByName map[string]Deployment` and `router *router.Router`.
- `Config` struct: add `Router *router.Router`, required (not optional) — matching every other dependency in this struct (`Guardrails`, `Budget`, etc., all required concrete pointers).
- `NewPipeline`: add `cfg.Router == nil` to the required-field switch; replace the `byModel` construction with `byName := map[string]Deployment{}`; drop `rrCounters` from the returned struct literal.
- `nextDeployment` (`dataplane.go:617-633`): entire body replaced by a thin wrapper, signature unchanged (`(Deployment, bool)`), so `HandleChatCompletion` and `streamDeploymentWithFallback` need **zero** call-site changes:
  ```go
  func (p *Pipeline) nextDeployment(model string) (Deployment, bool) {
  	name, ok := p.router.Select(model)
  	if !ok {
  		return Deployment{}, false
  	}
  	dep, ok := p.deploymentsByName[name]
  	return dep, ok
  }
  ```
- Imports: add `internal/router`; remove `sync`/`sync/atomic` (confirmed via grep: their only uses in this file were `rrMu`/`rrCounters`, both removed).

`streaming.go` needs **no changes at all** — it calls the same `nextDeployment` method.

### `cmd/gateway/main.go` wiring

`buildPipeline`'s deployment loop grows a second, parallel `[]router.Deployment` slice built in the same loop, then `depRouter := router.New(routerDeployments)`, wired into `dataplane.Config{Router: depRouter}`.

### `gateway/ARCHITECTURE.md` updates

The `/internal/router` Package Layout entry flips from "not built... target shape... not built yet" to an ACTIVE entry naming what shipped and what's still deferred (usage/latency/cost-based selection, circuit breaker, model-group chains). The Request Lifecycle line (116)'s stale "circuit-breaker" mention is removed — it never matched the corrected Package Layout entry and does not match this RFC's scope either.

## Drawbacks

- `Select`'s per-model mutex is a strictly larger critical section than today's single lock-free atomic increment — negligible in practice (in-memory arithmetic over a handful of deployments per model), but a real, honest complexity increase, the same tradeoff `TokenBucket`'s own mutex-guarded refill+consume already accepts for correctness.
- A permanently-failing deployment still receives its full weighted share of traffic forever — this RFC does not add health/failure awareness (deliberately; see Alternatives Considered). The existing single-fallback-per-request mechanism is the only failure response, unchanged.
- Two parallel deployment representations now exist briefly at wiring time (`dataplane.Deployment` for execution, `router.Deployment` for selection) — a deliberate decoupling (matching `ratelimit.KeyConfig`/`identity.VirtualKey`'s existing precedent) rather than a shared type, at the cost of `cmd/gateway` building two slices from one config loop instead of one.

## Alternatives Considered

1. **Naive weight-expansion** (repeat each deployment `Weight` times into a virtual list, reuse today's counter/modulo over that list) — rejected: for any weight > 1 it produces bursts (`A,A,A,B,B,B` rather than a smooth interleave), silently defeating the actual purpose of weighting, and its memory cost is `O(Σweight)` — a config typo (`weight: 1000000`) becomes a real memory hazard. Smooth WRR is O(1) per deployment regardless of weight magnitude.
2. **Ship a circuit breaker in the same pass** — researched in detail (Envoy outlier detection, Hystrix, resilience4j — all real, well-understood designs that would fit this codebase's `TokenBucket`-style mutex+lazy-eval shape with no new goroutine) and explicitly rejected for *this* RFC: not named in PRD's v1 allowlist, and this session's established discipline is to narrow to exactly what's committed (Cache L2 → 3 safe ops, Cache L3 → MinHash not real embeddings, Guardrails → regex not NER/ML) rather than build ahead of a stated need. A future RFC can propose it against `gateway/ARCHITECTURE.md`'s still-open gap description once there's a concrete trigger (real traffic showing sustained deployment failures, per this session's own "no telemetry to validate a policy against" caution).
3. **Model-group fallback chains** — rejected: PRD says "a single fallback chain" (singular); `ARCHITECTURE.md` explicitly names "no model-group fallback chain" as the intended v1 boundary, and the existing 1-hop same-model fallback already satisfies it.
4. **Do nothing** (leave plain round-robin) — the acknowledged status quo; rejected because PRD.md's v1 scope line explicitly commits to weighted routing, making this the one part of the routing gap that is not optional future work.

## Unresolved Questions

- Whether `Weight` should also be surfaced on `dataplane.Deployment` for observability (e.g. logging a request's effective weight) — this RFC says no, keeping the wire-level execution struct unchanged; weight is purely a `router`-package selection concern.
- Whether `router.New` should defensively re-validate non-negative weights at construction (in addition to `controlplane.Load`'s parse-time check) — likely yes, cheap, consistent with "narrow interface, don't trust the caller blindly," but not load-bearing; left to implementation judgment.
- When (not whether) a follow-up RFC should propose the circuit breaker / health-tracking half of `ARCHITECTURE.md`'s still-open `/internal/router` gap — deliberately left open; no production traffic exists yet to calibrate failure thresholds/cooldowns against, the same caution this session applied when originally scoping this feature.
