> **For agentic executors:** work through this task-by-task, checking off each step as it's done. Don't skip ahead — a later task may depend on an earlier one's actual output, not just its description.

---

**Goal:** Replace `dataplane.Pipeline`'s inline round-robin (`nextDeployment`) with a new `gateway/internal/router` package implementing weighted round-robin, closing the "weighted" half of `PRD.md`'s v1 routing scope line.

**Architecture:** New leaf package `gateway/internal/router` (smooth WRR selection, no dependency on `dataplane`/`cache`). `dataplane.Pipeline` gains a required `Router *router.Router` dependency and `nextDeployment` becomes a thin wrapper delegating to it. `controlplane.DeploymentConfig` gains a `Weight int` field. `cmd/gateway`'s `buildPipeline` constructs the `router.Router` alongside the existing `[]dataplane.Deployment` slice.

**Tech Stack:** No new dependency — pure Go stdlib (`sync`), matching `internal/ratelimit`'s existing shape.

**Spec:** `docs/rfcs/2026-09-04-weighted-routing.md`.

**Global Constraints:**
- `nextDeployment`'s signature (`(Deployment, bool)`) must not change — `HandleChatCompletion` and `streamDeploymentWithFallback` get zero call-site edits.
- Equal-weight (including the default, unset-weight) behavior must be byte-identical to today's round-robin sequence — no existing test's exact call-order assertions may need updating.
- `router` package must never import `gateway/internal/gateway/dataplane` or `gateway/internal/cache` (dependency-direction rule).
- No circuit breaker, no health tracking, no model-group fallback chains — out of scope per the RFC.

---

## Phase 1: `internal/router` package

### Task 1: `Deployment` type and smooth-WRR core

**Files:**
- Create: `gateway/internal/router/deployment.go`
- Create: `gateway/internal/router/wrr.go`
- Create: `gateway/internal/router/router.go`
- Test: `gateway/internal/router/router_test.go`

**Interfaces:**
- Consumes: nothing (leaf package).
- Produces: `router.Deployment{Name, Model string; Weight int}`, `router.New(deployments []Deployment) *Router`, `(*Router).Select(model string) (name string, ok bool)`.

**Steps:**
- [x] `deployment.go`: define `Deployment struct { Name, Model string; Weight int }`.
- [x] `wrr.go`: define `modelState struct { mu sync.Mutex; deps []weightedDeployment; i, cw, gcd, maxW int }` where `weightedDeployment struct { name string; weight int }`. Implement `next() (string, bool)` per the RFC's algorithm. Compute `gcd`/`maxW` once when a `modelState` is built (weight `0` normalized to `1` first).
- [x] `router.go`: define `Router struct { models map[string]*modelState }`. `New(deployments []Deployment) *Router` groups by `Model`, preserving input order exactly (never re-sort). `Select(model string) (string, bool)` looks up the model's `modelState` and calls `next()`; returns `("", false)` if the model has no `modelState` at all.
- [x] `router_test.go`, backward-compat test: construct with N=3 deployments, all `Weight: 0`, call `Select` 9 times, assert the returned name sequence is exactly `[0,1,2,0,1,2,0,1,2]` (by index into the input slice).
- [x] `router_test.go`, weighted test: construct with 2 deployments, weights `2` and `1`, call `Select` 6 times, assert the interleave is smooth (`A,B,A,A,B,A` per the nginx algorithm's real trace) — not bursty (`A,A,B,B,A,A`).
- [x] `router_test.go`: `Select` on an unconfigured model returns `("", false)`.
- [x] `go build ./... && go vet ./...` from `gateway/`.
- [x] `go test ./internal/router/...` — new tests pass.

---

## Phase 2: Config plumbing

### Task 1: `Weight` on `DeploymentConfig`

**Files:**
- Modify: `gateway/internal/gateway/controlplane/config.go`
- Modify: `gateway/config.example.yaml`
- Test: `gateway/internal/gateway/controlplane/config_test.go` (existing file — add cases, don't create a new one if it already exists; check first)

**Interfaces:**
- Consumes: nothing new.
- Produces: `DeploymentConfig.Weight int`, available to `cmd/gateway`.

**Steps:**
- [x] Add `Weight int` field to `DeploymentConfig` with the doc comment from the RFC.
- [x] In `Load`'s deployments loop, parse `dep.Weight, _ = getInt(depMap, "weight")`; return a `controlplane:` error if negative.
- [x] Add a commented `# weight: 1` line under one example deployment in `config.example.yaml`.
- [x] Add/extend a config test: a deployment with `weight: 3` parses to `Weight: 3`; a deployment with `weight: -1` (or any negative) returns an error; a deployment with no `weight` key parses to `Weight: 0` (the unset-default sentinel, normalized later by `router.New`, not here).
- [x] `go build ./... && go test ./internal/gateway/controlplane/...`.

---

## Phase 3: `dataplane` integration

### Task 1: `Pipeline`/`Config`/`nextDeployment` rewire

**Files:**
- Modify: `gateway/internal/gateway/dataplane/dataplane.go`

**Interfaces:**
- Consumes: `router.Router` (constructed by the caller, `cmd/gateway`).
- Produces: unchanged `nextDeployment(model string) (Deployment, bool)` signature.

**Steps:**
- [x] Add import `"github.com/kelvran/gateway/gateway/internal/router"`.
- [x] `Config` struct: add `Router *router.Router` field with a doc comment (required, mirrors `Guardrails`).
- [x] `Pipeline` struct: remove `deploymentsByModel`, `rrMu`, `rrCounters`; add `deploymentsByName map[string]Deployment` and `router *router.Router`.
- [x] `NewPipeline`: add `case cfg.Router == nil: return nil, fmt.Errorf("dataplane: Config.Router is required")` to the switch. Replace the `byModel` construction loop with a `byName := map[string]Deployment{}` loop (`byName[d.Name] = d`). Update the returned struct literal: drop `rrCounters`, add `deploymentsByName: byName` and `router: cfg.Router`.
- [x] Rewrite `nextDeployment` per the RFC's wrapper shown above.
- [x] Remove the now-dead `"sync"` and `"sync/atomic"` imports (verify via grep that `rrMu`/`rrCounters` were their only uses in this file before removing — check `checkRateLimit`/other methods don't use `sync` for something else first).
- [x] `go build ./...` — confirm it fails everywhere `Config{}` is constructed without `Router` (expected; Task 2 below fixes each).

### Task 2: Update every `Config{}` construction site

**Files:**
- Modify: `gateway/cmd/gateway/main.go`
- Modify: `gateway/internal/gateway/dataplane/dataplane_test.go`
- Modify: `gateway/internal/gateway/dataplane/lexical_cache_test.go`
- Modify: `gateway/internal/gateway/dataplane/streaming_test.go`
- Modify: `gateway/internal/gateway/dataplane/guardrail_test.go`
- Modify: `gateway/internal/gateway/dataplane/gatewayevents_test.go`

**Interfaces:**
- Consumes: `router.New(...)`.
- Produces: nothing new — every existing helper's signature stays the same.

**Steps:**
- [x] `main.go`'s `buildPipeline`: build a parallel `routerDeployments := make([]router.Deployment, 0, len(cfg.Deployments))` in the existing deployment loop (`routerDeployments = append(routerDeployments, router.Deployment{Name: d.Name, Model: d.Model, Weight: d.Weight})`); after the loop, `depRouter := router.New(routerDeployments)`; add `Router: depRouter` to the `dataplane.Config{}` literal; add the `router` import.
- [x] In `dataplane_test.go`, add a small package-level test helper (visible to every sibling `_test.go` file in `package dataplane`):
  ```go
  // testRouter builds a router.Router from deployments, weight 0 for
  // every one (normalized to 1 by router.New) — preserving today's exact
  // round-robin sequence, per docs/rfcs/2026-09-04-weighted-routing.md's
  // degrade proof.
  func testRouter(deployments []Deployment) *router.Router {
  	rd := make([]router.Deployment, 0, len(deployments))
  	for _, d := range deployments {
  		rd = append(rd, router.Deployment{Name: d.Name, Model: d.Model})
  	}
  	return router.New(rd)
  }
  ```
  Add the `router` import to this file.
- [x] `dataplane_test.go`'s `newTestPipelineWithKeysAndBudget`: add `Router: testRouter(deployments)` to its `Config{}` literal.
- [x] `lexical_cache_test.go`'s `newTestPipelineWithCacheL3`: bind its inline `Deployments` literal to a local `deployments := []Deployment{...}` variable first (if not already), then add `Router: testRouter(deployments)`.
- [x] `streaming_test.go`'s two helpers (`newStreamingTestPipeline`, `newStreamingTestPipelineWithKeysAndBudget`): bind the resolved `Deployments` value (currently an inline IIFE) to a local variable first, then reference it in both `Deployments:` and `Router: testRouter(...)`.
- [x] `guardrail_test.go`'s `TestCacheHitsAreForcedMissesAfterGuardrailPolicyVersionChanges`: it already has a local `deployments` variable — add `Router: testRouter(deployments)`.
- [x] `gatewayevents_test.go`'s 4 call sites: for the 2 using `newTestPipelineWithKeysAndLogger`, update that helper (mirrors `newTestPipelineWithKeysAndBudget`) to add `Router: testRouter(deployments)`. For the 2 direct `NewPipeline(Config{...})` calls (`TestGatewayEventRateLimitFailOpenTrueWhenBackendErrors`, `TestGatewayEventBudgetSpentUsdReflectsRealPriorSpend`, `TestGatewayEventStreamingFallbackFalseAfterFirstChunkSent` — check exact count when editing), bind each inline `Deployments` literal to a local variable first, then add `Router: testRouter(...)`.
- [x] `go build ./... && go vet ./...` from `gateway/` — must be clean.
- [x] `go test ./...` from `gateway/` — every existing test passes unmodified in its assertions (no test's expected call sequence/count should need changing).
- [x] `go test -race ./...` from `gateway/`.

---

## Phase 4: Docs, verify, ship

### Task 1: Documentation updates

**Files:**
- Modify: `gateway/ARCHITECTURE.md`
- Modify: `gateway/changelog/unreleased.md`
- Modify: `DECISIONS.md`
- Modify: `docs/agents/LOGS.md`
- Modify: `STATUS.md`

**Steps:**
- [x] `gateway/ARCHITECTURE.md`: flip the `/internal/router` Package Layout entry to ACTIVE, naming what shipped (weighted round-robin) and what's still deferred (usage/latency/cost-based selection, circuit breaker, model-group chains). Update the dependency-direction table to include `router` in the shared-kernel-leaf line. Remove the stale "circuit-breaker" mention from the Request Lifecycle section (line 116).
- [x] `gateway/changelog/unreleased.md`: add an `## Added` entry per this repo's existing verbose, precise style (see prior entries for the exact voice/detail level).
- [x] `DECISIONS.md`: append one line (after re-reading the true tail first).
- [x] `docs/agents/LOGS.md`: append a full dated entry (files touched / intent / decisions / verification / next steps).
- [x] `STATUS.md`: update Current Phase / Last Completed Task / Next Action.

### Task 2: Full verification and ship

**Steps:**
- [x] From `gateway/`: `golangci-lint run ./...` — clean.
- [x] From repo root: `make verify` — clean (both deployables).
- [x] `git add` the exact touched files (no `git add -A`); commit with a `feat(gateway):` conventional-commit message.
- [x] Push; watch the real CI run (`gh run watch` / `gh run view`) to green.
- [x] Final `STATUS.md` commit confirming the exact commit SHA and CI run ID.

## Scope Gate

This is architecturally-scoped work (a new package, a new required `Pipeline` dependency, a config schema change) — correctly warranting this plan + `docs/rfcs/2026-09-04-weighted-routing.md`, not a one-line `DECISIONS.md` entry alone.
