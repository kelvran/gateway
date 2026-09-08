# RFC: Per-deployment backoff for the active health-probe loop

## Status

Accepted, implemented 2026-09-08.

## Context

`evals/tests/fixtures/regression_corpus_routing_chaos.json`'s
`chaos-health-probe-no-backoff-on-repeated-failure` case (added
2026-09-08, revision 1) documented an honest, verified FAIL: reading
`RunHealthProbeLoop`/`ProbeDeployments`
(`gateway/internal/gateway/dataplane/dataplane.go`) end to end confirmed
the background active-probe loop drives every configured deployment off
a single flat `time.NewTicker(interval)`, unconditionally, forever — a
deployment that has already failed 50 consecutive probes is probed
again on the very next tick at the exact same fixed cadence as a
perfectly healthy one. `router.ReportProbeResult`
(`gateway/internal/router/health.go`) tracks `consecutiveFailures`
purely to decide the `healthy` bool; it feeds no backoff/cooldown signal
back to the probe loop at all. The case's own citation names this the
same structural shape as AWS's 2015 DynamoDB postmortem: continued
fixed-rate probe traffic against an already-degraded dependency is
itself a real (if bounded-per-probe, since `healthProbeCallTimeout`/
`healthProbeMaxTokens` already cap each individual probe's own cost)
contributor to sustained overload.

This RFC closes that gap: a persistently-unhealthy deployment's own
probe cadence grows the longer it stays unhealthy, capped, and reverts
to the plain configured cadence the instant it recovers.

### Constraint: `router` stays the adapter-agnostic, I/O-free leaf it already is

`gateway/internal/router`'s own package doc comment is explicit that
this package "is a shared-kernel leaf... deliberately adapter-agnostic
and I/O-free," recording only outcomes reported to it via
`ReportProbeResult`/exposing them via `IsHealthy` — issuing the probe
and scheduling when the next one happens are both `dataplane`'s job.
`IsHealthy` (already exported, already used by `fallback.go`) is
sufficient to read a deployment's current health verdict without adding
any probe-scheduling concept to `router` at all — this RFC touches only
`gateway/internal/gateway/dataplane`, exactly as the plan anticipated
and confirmed necessary.

## Design

### Per-deployment `nextProbeAt`/`backoff` schedule, owned by `Pipeline`

Two new `Pipeline` fields: `probeSchedule map[string]*healthProbeSchedule`
(guarded by a new, dedicated `probeMu` — never `p.probeMu` reused for
anything else, mirroring `router.Router.healthMu`'s own
per-concern-scoped-lock precedent) and `now func() time.Time` (real
`time.Now` in production, overridden directly by white-box tests —
mirroring `internal/budget.Tracker.now`'s identical injectable-clock
convention, chosen over inventing a new pattern). `healthProbeSchedule`
is two fields: `nextProbeAt time.Time` and `backoff time.Duration` (zero
= "not backed off").

### `probeDueDeployments`: the new, backoff-aware entry point `RunHealthProbeLoop` drives

`ProbeDeployments` (existing, exported) is **unchanged** — it still
probes every configured deployment unconditionally on every call,
concurrently, each bounded by `healthProbeCallTimeout`. It stays exactly
what its own doc comment already said it was for: a "force a full probe
pass right now" primitive, useful for tests that need deterministic,
elapsed-time-independent passes (its own two existing tests,
`health_probe_test.go`, keep passing completely unmodified — zero
change to their behavior or assertions).

A new, unexported `probeDueDeployments(ctx, interval)` is what
`RunHealthProbeLoop` now calls each tick instead. It:

1. Reads `p.now()` once, then — holding `probeMu` only long enough to
   read the map, never across any I/O — collects every deployment whose
   own `probeSchedule` entry is either absent (never probed via this
   path before — always due, matching `RunHealthProbeLoop`'s pre-existing
   "first pass happens after the first interval elapses" contract) or
   whose `nextProbeAt` has already arrived.
2. Probes every DUE deployment concurrently (the same `sync.WaitGroup`
   pattern `ProbeDeployments` already used, preserved exactly for
   whichever subset is due this tick — not narrowed to sequential probes
   just because the plan raised the question of restructuring).
3. Immediately after each one's own `probeOneDeployment` call (and thus
   its own `ReportProbeResult` call) completes, reschedules that single
   deployment via `rescheduleDeployment` — reading the FRESH
   post-probe `router.IsHealthy` verdict, never a stale pre-probe one.

### `rescheduleDeployment`: the backoff math itself

Healthy (including a deployment that just recovered on the probe that
preceded this call): `backoff` resets to 0, `nextProbeAt = now +
interval` — the plain configured cadence, unconditionally. This is the
line that makes "backoff must never apply to a currently-healthy
deployment" literally true: the branch is selected by `IsHealthy`,
which is exactly the same boolean `router.Select` already treats as the
line between eligible and excluded.

Unhealthy: if `backoff` is currently 0 (this deployment's first
unhealthy reschedule since its last healthy one), seed it to `interval`;
then double it (`healthProbeBackoffGrowthFactor = 2`), capped at
`healthProbeBackoffMaxMultiplier * interval` (`= 8`) — so the sequence
of successive backed-off intervals is `2x, 4x, 8x, 8x, 8x, ...`, never
regressing below `2x` once unhealthy, never exceeding `8x`.

**Why a multiplier of the configured interval, not a fixed absolute
cap.** The health-probe interval itself is an operator-tunable value
(300s default, per `docs/rfcs/2026-09-07-gateway-active-health-probing.md`)
— a fixed absolute cap (e.g. "5 minutes, always") could land at or below
an operator's own configured cadence for an operator who configures a
longer interval than the default, inverting the entire point of backing
off (a "backed-off" interval that isn't even longer than the plain one).
Scaling the cap to the configured interval instead guarantees the backed-
off cadence is always strictly longer, by a known, constant factor,
regardless of what an operator configures. At the documented 300s
default, 8x means a deployment that has stayed unhealthy long enough to
fully back off is checked roughly every 40 minutes instead of every 5 —
a real, material reduction in Kelvran's own probe load against an
already-struggling dependency, while still bounding how long a genuine
recovery can go unnoticed to a human-reasonable window.

**Why deterministic doubling, not jittered.** This codebase already has
an established exponential-backoff idiom
(`internal/ratelimit.EqualJitterBackoff`/`RetryBackoff`), used for the
CLIENT-facing `Retry-After` signal on rejected requests — deliberately
jittered there, because jitter's whole value is desynchronizing many
independent CLIENT callers converging on the same rejected resource at
once. That rationale doesn't transfer here: `RunHealthProbeLoop` is a
single per-gateway-instance background loop probing its own configured
deployment list; there is no independent-caller-desynchronization
problem to solve. Plain deterministic doubling is simpler and exactly
provable against a fake clock — see the new tests below — with no loss
of real benefit.

### Preserved, not narrowed: "every due deployment probed concurrently, each independently timeout-bounded"

The plan's own question — "should `ProbeDeployments` now probe
deployments individually on their own schedule, rather than all at once
every tick" — is answered as: yes, ELIGIBILITY becomes per-deployment
(`probeDueDeployments`'s filtering step), but every deployment that IS
due on a given tick is still probed exactly as concurrently, and still
exactly as independently `healthProbeCallTimeout`-bounded, as before. No
deployment's probe ever blocks on another's, whether or not backoff is
in play for either.

## Alternatives considered

**Tracking backoff inside `router.deploymentHealth` (health.go) instead
of a new `dataplane`-owned schedule.** Rejected for the same reason the
original active-probing RFC put probe I/O in `dataplane` and not
`router` in the first place: `router`'s own package doc comment states
it "must never" gain an I/O or probe-scheduling concept, and scheduling
WHEN the next probe fires is exactly that — `router` continuing to know
nothing beyond "healthy or not, as of the last reported outcome" is the
correct, unchanged boundary. `IsHealthy` already gives `dataplane`
everything it needs to read that state without widening `router`'s own
responsibilities.

**A backoff step counter (int) instead of directly tracking the
`backoff time.Duration` value.** Considered — mathematically equivalent,
computing `interval * growthFactor^step` each time via a small capped
loop or `math.Pow`. Rejected as needless indirection: tracking the
duration itself directly is simpler, avoids a `math` import/float
round-trip, and is exactly as easy to reason about and test.

**Backing off ONCE a deployment starts accumulating failures below the
unhealthy threshold, not only once it actually crosses into
`IsHealthy() == false`.** Considered, per the plan's own "(or has
accumulated some number of consecutive failures below the unhealthy
threshold)" phrasing. Rejected: the same plan explicitly requires
"backoff must never apply to a currently-healthy deployment," and
`router.IsHealthy` — the one boolean this whole feature (and
`router.Select`'s own routing decision) already treats as the
authoritative "healthy or not" answer — reports `true` for a deployment
still below the unhealthy threshold. Backing off a deployment `IsHealthy`
calls healthy would directly contradict that stated requirement, and
would also duplicate the N-of-M threshold's own job (deciding how many
failures are tolerated) with a second, parallel notion of "not quite
healthy yet" that `router` doesn't have and isn't being asked to gain.
Gating strictly on `IsHealthy` keeps this feature a clean, additive
layer on top of the existing threshold model, never a second one beside
it.

## Verification

`go build ./... && go vet ./...` clean. `golangci-lint run ./...` → `0
issues`. `go run github.com/fe3dback/go-arch-lint@v1.18.0 check` →
clean — no new component, no new dependency-direction rule (`router` is
untouched by this change entirely). `gofmt -l .` empty. `go mod tidy` →
empty diff. `go test ./... -race` → every package `ok` except the two
pre-existing, already-documented, environment-only rootless-Docker
failures (`TestIntegrationTwoGatewayInstancesShareOneRedisRateLimit`;
`internal/ratelimit/redislimiter`'s own `TestMain` panic) — confirmed
unrelated, this pass never touches either.

New tests, `gateway/internal/gateway/dataplane/health_probe_backoff_test.go`,
using a fake clock (`p.now` overridden directly, mirroring
`internal/budget.Tracker.now`'s own test convention) so backoff growth
is proven against exact elapsed-time thresholds rather than real sleeps:

- `TestProbeDueDeploymentsBacksOffIncreasinglyWhileDeploymentStaysUnhealthy`
  — the plan's own required proof: a deployment that keeps failing every
  probe is skipped at the plain interval once backed off, and only
  probed again once the FULL backed-off window (2x, then 4x) has
  elapsed — asserted on real upstream call counts, not just internal
  schedule state.
- `TestProbeDueDeploymentsProbesHealthyDeploymentEveryIntervalWithNoBackoff`
  — the mirror-image proof: a deployment that stays healthy is probed on
  every single tick at the plain interval, never skipped, across 6
  consecutive ticks.
- `TestProbeDueDeploymentsRecoveredDeploymentResumesNormalIntervalImmediately`
  — a deployment backed off while unhealthy, once it crosses the
  M-consecutive-success recovery threshold, has its very next schedule
  entry show zero carried-over backoff and a `nextProbeAt` exactly one
  plain interval away — never a lingering, still-large backoff — both by
  inspecting the schedule directly and by confirming the actual next
  probe call only fires at that exact plain-interval boundary.

Sanity-checked by breaking: temporarily short-circuited
`rescheduleDeployment`'s healthy/unhealthy branch to always take the
no-backoff path (`if true || p.router.IsHealthy(name)`), confirmed the
two backoff-growth tests failed for the exact right reason (extra,
premature probe calls the test asserts must not happen), confirmed the
"healthy, no backoff" test was correctly unaffected by this specific
break (it never exercised the unhealthy branch at all), then reverted
and confirmed `git diff --stat` showed the temporary line gone with no
residual trace, and the full suite re-passed.

`evals/tests/fixtures/regression_corpus_routing_chaos.json`'s
`chaos-health-probe-no-backoff-on-repeated-failure` case bumped to
revision 2, `output`/`reference` updated to describe this now-fixed
behavior, re-verified against the real fixed code above (not edited to
match hoped-for behavior).
