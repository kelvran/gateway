# RFC: Gateway active/synthetic health-probing for routing

## Status

Accepted, implemented 2026-09-07.

## Context

`gateway/internal/router` (per `docs/rfcs/2026-09-04-weighted-routing.md`) selects a deployment via smooth weighted round-robin and falls back once on a real call's error, but has no notion of a deployment's own health at all — every configured deployment is always eligible for `Select`, forever, regardless of how many times it has just failed. `gateway/ARCHITECTURE.md`'s router section named this a deliberate deferral: "usage/latency/cost-based selection signals, consecutive-failure/cooldown circuit-breaker tracking... none of these are named in `PRD.md`'s v1 allowlist."

`docs/upgrade-research/gateway-2026-09-06.md`'s Finding 2 revisits that deferral with new evidence, not a re-litigation: Kong, Envoy, and LiteLLM all draw a hard line between two genuinely different mechanisms the prior framing conflated into one "circuit breaker" bucket:

- **Active/synthetic health checks** — a background loop the gateway itself runs, issuing a real probe request per configured target on a timer, fully independent of real client traffic. Kong and Envoy both document this as carrying no traffic-volume precondition; LiteLLM's `background_health_checks` (default 300s) is the concrete, currently-shipping example, and its own "Health Check Driven Routing" feature removes a deployment from the routing pool purely on synthetic-probe failure.
- **Traffic-derived statistical detection** — Envoy's outlier detection (Success Rate / Failure Percentage), LiteLLM's `allowed_fails`-based cooldown. Both are computed from real proxied traffic and Envoy's own docs confirm they will not activate below a configured request-volume floor — this is the one class of health signal for which "wait for real production traffic" is genuinely, technically correct.

Only the first class ships now. The second — the traffic-derived statistical circuit breaker — correctly stays deferred exactly as `gateway/ARCHITECTURE.md` already says, since Kelvran has no production traffic yet to calibrate a volume floor against. This RFC narrows, not overturns, that deferral.

Per the user's own decision (recorded in the approved phased upgrade roadmap): the N-of-M threshold is conservative — 3 consecutive failed probes to exclude, 2 consecutive successful probes to re-include — deliberately not LiteLLM's own aggressive single-failure default, since Kelvran has no production experience yet to justify a more aggressive one. Probe interval defaults to 300s, matching LiteLLM's own default cadence. All three values are configurable via YAML.

## Design

### Package split: health-state bookkeeping in `router`, probe I/O in `dataplane`

`router.Router` gains health tracking (`health.go`, new file): a per-deployment consecutive-failure/consecutive-success counter and a current healthy/unhealthy verdict, updated via a new exported `ReportProbeResult(name string, success bool) (healthy, changed bool)`, and consulted by `Select` (via a new `selectHealthy` helper) to skip any currently-unhealthy deployment. This keeps `router` exactly the adapter-agnostic, I/O-free leaf it already was — `ReportProbeResult` only records an outcome reported to it; it has no idea what a probe request even is.

Issuing the actual probe — a lightweight synthetic chat-completion request per deployment — needs each `Deployment`'s `BaseURL`/`Provider`/credentials and the registered `adapter.Adapter`, none of which `router.Deployment` carries (by design, per that type's own decoupling from `dataplane.Deployment`, mirroring `ratelimit.KeyConfig`'s decoupling from `identity.VirtualKey`). `dataplane.Pipeline` already has all of this — `adapters`, `deploymentsByName`, `upstream` — so the probe loop lives there as two new methods:

- **`ProbeDeployments(ctx context.Context)`** — issues one probe per configured deployment, concurrently, each bounded by a 5s timeout, calling `p.callDeployment` directly (the same adapter-translate-then-upstream-call function real client requests use) with a minimal `{Role: "user", Content: "ping"}` message and `MaxTokens: 1`. Bypasses auth/cache/guardrail/budget/rate-limit entirely — a probe is not real client traffic, is never cached, never billed, and belongs to no virtual key. Reports each outcome to `p.router.ReportProbeResult` and logs on a health *transition* only (not every probe), at Warn for a deployment going unhealthy and Info for recovery.
- **`RunHealthProbeLoop(ctx context.Context, interval time.Duration)`** — the production wrapper: a `time.Ticker`-driven loop calling `ProbeDeployments` once per tick until `ctx` is canceled. A no-op if `interval <= 0` (health probing not configured) — matching every other optional subsystem's "zero means disabled" convention (Redis, boltstore, OTel, admin). Does not run a pass immediately at start, so a gateway restart storm never adds a synchronized burst of probe calls on top of real traffic resuming.

`cmd/gateway/main.go` starts `RunHealthProbeLoop` as a goroutine scoped to the same `ctx` (`signal.NotifyContext`) the main/admin HTTP servers already use — canceled by the same SIGTERM/SIGINT that triggers graceful shutdown, no separate stop mechanism needed. `ProbeDeployments` is exported specifically so tests can drive deterministic probe passes without depending on real elapsed time.

### `Select`'s skip logic and its fail-open guarantee

`selectHealthy` calls the existing `modelState.next()` (the smooth-WRR cursor) up to `ms.sumW` times — the schedule's full cycle length (the sum of every deployment's normalized weight in the group), not just `len(deployments)`. This bound is load-bearing, not cosmetic: under a skewed weight configuration (e.g. a weight-50 deployment sharing a model with a weight-1 one), the smooth-WRR schedule can return the high-weight deployment many times in a row before ever reaching the low-weight one — a naive `len(deployments)`-call bound would starve the healthy low-weight deployment from ever being found while the high-weight one is unhealthy. `sumW` calls are always enough, for any weight distribution, since each deployment appears exactly `weight` times per `sumW` consecutive calls.

If every deployment in a model's group is currently unhealthy, `Select` fails **open**: it returns the last candidate examined (`ok=true`) rather than `("", false)` — mirroring `internal/ratelimit`'s own Redis-backend-error fail-open precedent. A known-bad deployment is still a better answer than `dataplane.ErrNoDeployment`, which is what an honest "no eligible deployment" signal would otherwise incorrectly produce for a model that IS configured, just entirely unhealthy right now.

### Interaction with the existing single-fallback mechanism

`dataplane.Pipeline.nextDeployment` is unchanged — it is still a thin wrapper around `router.Router.Select`. `runMissPath`'s existing single-fallback-on-real-error mechanism (call `nextDeployment` again if the first call fails) is therefore also unchanged, but now composes for free: a probe-excluded deployment is never chosen as the *primary* pick in the first place, so the fallback path fires less often, not differently. The two mechanisms stay genuinely complementary — active probing prevents routing to a *known*-bad deployment before ever trying it; the single-fallback retry still covers a fresh, not-yet-probe-detected failure.

### Config surface

New `HealthProbeConfig` in `controlplane`, parsed under a new top-level `health_probe:` YAML section (commented out by default in `config.example.yaml`, matching `admin:`/`cache:`/`guardrails:`/`rate_limit:`'s own optional-section convention):

```yaml
health_probe:
  interval_seconds: 300      # 0 (or omitted) disables probing entirely
  unhealthy_threshold: 3     # <= 0 resolves to router.HealthConfig's own default (3)
  healthy_threshold: 2       # <= 0 resolves to router.HealthConfig's own default (2)
```

The `<= 0 means unset, resolve to a default` resolution lives in `router.HealthConfig.normalized()` (called once, inside `router.New`), not in `controlplane` — mirroring `AdminConfig.ListenAddr`'s own "operational default resolved by the consumer, not a config-shape concern this package owns" precedent. `router.New`'s signature widened to `New(deployments []Deployment, health HealthConfig) *Router`; a zero-valued `HealthConfig{}` (every existing test call site not exercising health behavior) is a safe, fully backward-compatible default — no deployment is ever marked unhealthy until some caller actually invokes `ReportProbeResult`.

## Alternatives considered

**A new `internal/router/health` sub-package issuing the probe itself, importing `internal/adapter` directly.** Considered per the plan's own "or a new `internal/router/health` package" hint. Rejected: `router`'s existing `.go-arch-lint.yml` component (`in: router`, not `router/**`) does not automatically extend to a subdirectory, and giving `router` a new `mayDependOn: [adapter]` dependency-direction rule would be a real, avoidable widening of what has been a zero-dependency, I/O-free package since it was created — `dataplane` already legitimately depends on both `router` and `adapter` today, so hosting the probe I/O there needed zero new package, zero new `go-arch-lint` component, and zero new dependency-direction rule.

**Per-deployment health thresholds (configurable N/M per `deployments:` entry) instead of one Router-wide setting.** Rejected as unneeded scope: the plan's own decision frames this as a single, conservative default, not a per-deployment tuning knob; YAGNI applies until a real need for per-deployment overrides is demonstrated.

**Bounding `selectHealthy`'s skip-search loop at `len(deployments)` calls instead of `sumW`.** Considered first, and caught as wrong by hand-tracing a skewed-weight example (weight 50 vs. weight 1) before writing any test: `len(deployments)` consecutive `next()` calls can return the same high-weight deployment every single time under a sufficiently skewed configuration, never reaching the low-weight healthy one at all. `sumW` (the schedule's real, exact cycle length) is the tight, correct bound — proven by `TestSelectSkipsUnhealthyDespiteHeavilySkewedWeight`.

**Running an immediate probe pass at `RunHealthProbeLoop` startup, before the first tick.** Rejected: a gateway restart (or a fleet-wide rolling restart) would synchronize every instance's first probe burst with real traffic already resuming, adding avoidable load exactly when a service is already under startup pressure. The first pass waits for the first full interval, same as a plain `time.Ticker`'s natural semantics.

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0 issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` → clean — no new component, no new dependency-direction rule (this feature deliberately needed neither; see Alternatives above). `go mod tidy` → empty diff (zero new dependencies). `go test ./... -race` → every package `ok` except the two pre-existing, already-documented, environmental rootless-Docker failures (`TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`; `internal/ratelimit/redislimiter`'s own `TestMain` panic) — confirmed unrelated, this pass never touches either.

New tests, three layers:

- **`internal/router/health_test.go`** (pure, no I/O): `TestSelectExcludesOnlyAfterNConsecutiveFailures`/`TestSelectReincludesOnlyAfterMConsecutiveSuccesses` are the load-bearing N-of-M proofs the plan requires — a deployment stays eligible through N-1 failures and is excluded only on the Nth *consecutive* one, and the mirror-image for recovery. `TestReportProbeResultNonConsecutiveFailuresNeverExclude` proves "consecutive" is enforced literally (an alternating pattern never trips the threshold no matter how many total failures accrue). `TestSelectFailsOpenWhenEveryDeploymentUnhealthy` and `TestSelectSkipsUnhealthyDespiteHeavilySkewedWeight` prove the two correctness properties named in Design above.
- **`internal/gateway/dataplane/health_probe_test.go`** (Pipeline-level, mocked `UpstreamCaller`): `TestProbeDeploymentsExcludesOnlyAfterThresholdThenHandleChatCompletionRoutesAround` and `TestProbeDeploymentsReincludesDeploymentAfterMConsecutiveSuccesses` drive `ProbeDeployments` directly (deterministic, no real elapsed time) and then prove real `HandleChatCompletion` calls (each with distinct content, so every one is a genuine cache miss forcing a fresh routing decision) never reach an excluded deployment's mock upstream, and do reach it again after recovery.
- **`cmd/gateway/health_probe_integration_test.go`** — the plan's own explicitly required integration test: two real deployments for one model, each backed by its own real `httptest.Server` upstream (one always 500s, one always succeeds), driven over real HTTP on both the client and upstream sides (mirroring `cache_stampede_integration_test.go`'s own "prove it end-to-end" precedent). `TestIntegrationHealthProbeRoutesAroundConsistentlyFailingDeployment` confirms 20 real client requests, sent only after the 3rd consecutive probe failure trips the threshold, produce zero calls to the failing upstream and 20 to the healthy one.

Sanity-checked the single most load-bearing line — `ReportProbeResult`'s unhealthy-transition condition (`if h.healthy && h.consecutiveFailures >= r.healthCfg.UnhealthyThreshold`) — by temporarily short-circuiting it to `if false && ...`: all three layers of tests failed for the exact right reason (`internal/router`: 3 tests, "healthy = true, want false"/"should be unhealthy" mismatches; `internal/gateway/dataplane`: both new tests, "still healthy after 3 consecutive probe failures"; `cmd/gateway`: the integration test, "real client requests reaching the excluded 'bad' upstream = 19, want 0"), then reverted — confirmed via `grep` that the revert left zero trace of the temporary break, and the full suite re-passed.
