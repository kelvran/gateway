// Package router selects which Deployment should serve the next request
// for a given canonical model, per docs/rfcs/2026-09-04-weighted-routing.md,
// and tracks each deployment's active/synthetic-probe health, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md. It replaces
// dataplane.Pipeline's inline atomic-counter round-robin with smooth
// weighted round-robin (the LVS/IPVS wrr.c algorithm) — closing the
// "weighted" half of PRD.md's v1 routing scope line ("static + weighted
// routing; a single fallback chain"). The fallback half of that line is
// untouched: it already lives in dataplane.go/streaming.go and needs no
// change here.
//
// This package is a shared-kernel leaf (see gateway/ARCHITECTURE.md's
// dependency-direction table) — it must never import
// gateway/internal/gateway/dataplane or gateway/internal/cache. Health
// tracking here is deliberately adapter-agnostic and I/O-free: it only
// records outcomes reported to it (see health.go's ReportProbeResult) —
// issuing the actual probe request is dataplane.Pipeline's job, since
// only dataplane has access to each Deployment's BaseURL/adapter/API key.
//
// Corrected 2026-09-24, per docs/upgrade-research/gateway-performance-
// optimization-2026-09-24.md Finding 3: latency- and cost-based routing
// signals are NOT out of scope — both are real and live today.
// SetLatencyFactor (health.go) records a soft de-weighting percentage
// per deployment from each deployment's own rolling-average probe
// latency relative to its model-group peers, applied via a
// Bresenham-style thinning gate (admitLatencyThinnedTurn); activeCostTier/
// costTiers (health.go, populated from each Deployment.CostTier at
// New()) similarly biases selection toward a cheaper tier when one is
// healthy. Both are traffic-INDEPENDENT signals — probe-derived or
// static config, never learned from real request-serving traffic — so
// neither needed the traffic-volume floor this doc comment originally,
// correctly, gated something else on.
//
// What DOES remain out of scope, per the weighted-routing RFC and
// narrowed (not overturned) by the health-probing RFC: model-group
// fallback chains, and the traffic-DERIVED statistical circuit breaker
// (Envoy-style outlier detection, LiteLLM-style allowed_fails cooldown),
// plus the more sophisticated online-learning latency/cost scoring
// (EWMA, Bayesian posteriors) comparator gateways ship — that class
// genuinely needs a traffic-volume floor Kelvran doesn't have
// production data for yet, and correctly stays deferred per
// gateway/ARCHITECTURE.md.
package router

import (
	"fmt"
	"sync"
)

// Router selects a deployment for a canonical model via smooth weighted
// round-robin, skipping any deployment ReportProbeResult has marked
// unhealthy. One Router is built once (New) and shared by the whole
// dataplane.Pipeline, mirroring ratelimit.KeyLimiter/budget.Tracker's
// single-instance shape.
type Router struct {
	// modelsMu guards models itself (the map's own identity per key —
	// i.e. read/write of the map, not each *modelState's own internal
	// fields, which stay guarded by that modelState's own mu per
	// wrr.go). Needed once SetWeight can replace a model's *modelState
	// value live, live-mutating a map concurrently with Select's own
	// unguarded read would otherwise be a real data race.
	modelsMu sync.RWMutex
	models   map[string]*modelState

	healthMu  sync.Mutex
	healthCfg HealthConfig
	health    map[string]*deploymentHealth

	// costTiers maps deployment Name -> configured CostTier (see
	// Deployment.CostTier's own doc comment). Populated once, at New()
	// time, from the exact same input deployments used to build models —
	// read-only afterward, safe for concurrent access without a lock,
	// exactly like models itself.
	costTiers map[string]int
	// stickyDeployments maps deployment Name -> its configured Sticky
	// flag (see Deployment.Sticky's own doc comment) — mirrors
	// costTiers's exact shape and the same "populated once at New(),
	// read-only afterward" contract.
	stickyDeployments map[string]bool
	// stickyGroups maps Model -> whether ANY deployment in that model's
	// group has Sticky set (see sticky.go's SelectSticky/stickyPick for
	// why this is checked once per Select call rather than recomputed).
	// Deliberately OR, not AND: sticky routing is a per-model-group
	// toggle an operator turns on for one canary/stable pair, not a
	// per-deployment tuning knob every member must separately opt into —
	// unlike costTiers, where the "every deployment in the group must be
	// tiered, or filtering is off" rule is intentionally the opposite.
	stickyGroups map[string]bool
}

// New builds a Router from deployments, grouping by Model in the exact
// per-model input order given. Callers (cmd/gateway, via
// controlplane.Load's existing alphabetical-by-name sort) must never pass
// a list re-sorted by weight, or the equal-weight
// degrade-to-plain-round-robin guarantee the RFC proves breaks. A Weight
// of 0 means "unset," normalized to 1.
//
// health configures the N-of-M consecutive-probe thresholds Select
// applies (see health.go) — the zero value is a valid, safe default: no
// deployment is ever marked unhealthy until some caller actually invokes
// ReportProbeResult, so a Router built with a zero HealthConfig behaves
// identically to this package's pre-health-probing behavior until (and
// unless) probing is separately wired up (see
// dataplane.Pipeline.RunHealthProbeLoop).
func New(deployments []Deployment, health HealthConfig) *Router {
	byModel := map[string][]weightedDeployment{}
	costTiers := make(map[string]int, len(deployments))
	stickyDeployments := make(map[string]bool, len(deployments))
	stickyGroups := map[string]bool{}
	for _, d := range deployments {
		byModel[d.Model] = append(byModel[d.Model], weightedDeployment{name: d.Name, weight: d.Weight})
		costTiers[d.Name] = d.CostTier
		stickyDeployments[d.Name] = d.Sticky
		if d.Sticky {
			stickyGroups[d.Model] = true
		}
	}

	models := make(map[string]*modelState, len(byModel))
	for model, deps := range byModel {
		models[model] = newModelState(deps)
	}
	return &Router{
		models:            models,
		healthCfg:         health.normalized(),
		health:            map[string]*deploymentHealth{},
		costTiers:         costTiers,
		stickyDeployments: stickyDeployments,
		stickyGroups:      stickyGroups,
	}
}

// Select returns the next chosen deployment's Name for model, skipping
// any deployment currently marked unhealthy (see selectHealthy in
// health.go) and any name present in exclude (nil-safe: a nil map's
// lookups always report false, so every existing caller passing nil is
// unaffected). The second return value is false only if no deployment
// is configured for model at all, or every configured deployment is
// either unhealthy or excluded — the same "not found" contract
// dataplane.Pipeline.nextDeployment already has today.
//
// exclude exists specifically for a same-model WRR fallback re-pick
// (dataplane.go/streaming.go's own "else if" branch, after
// nextDeployment's FIRST pick already failed): Select's own cursor
// (modelState.next, wrr.go) is one shared, mutex-protected sequence
// across every concurrent caller for this model — under real
// concurrency, an odd number of OTHER requests' own next() calls can
// land between this request's first and second pick, which (with two
// equal-weight deployments, a strict alternation) makes the second
// pick land back on the exact same name the first pick already failed
// on. A sequential test never exercises this: two back-to-back calls
// from the same goroutine, with no other caller interleaved, always
// alternate. Confirmed live: a 20-concurrent-request burst against a
// deliberately-broken deployment produced exactly this failure mode
// for 3 of 20 requests (no fallback attempted at all, the original
// error surfaced directly) before this fix.
func (r *Router) Select(model string, exclude map[string]bool) (string, bool) {
	r.modelsMu.RLock()
	ms, ok := r.models[model]
	r.modelsMu.RUnlock()
	if !ok {
		return "", false
	}
	return r.selectHealthy(ms, exclude)
}

// SetWeight live-mutates deploymentName's own weight within model's
// routing group to weight (weight <= 0 normalizes to 1, the same "unset"
// convention newModelState itself already applies) — every OTHER
// deployment in the group keeps its existing weight, in the same input
// order newModelState requires for its degrade-to-plain-round-robin
// proof (see modelState's own doc comment). Rebuilding the whole group
// necessarily resets THIS model's own WRR cursor (i/cw) — there is no
// principled way to carry a cursor's meaning forward across a changed
// weight distribution — but every OTHER model's cursor, and every
// deployment's health state (health.go's own map, keyed by deployment
// name, never by model), is completely untouched: a concurrent Select
// call already holding the OLD *modelState value keeps using it to
// completion, simply abandoned (not mutated in place) once this method
// installs the new one.
//
// Returns an error, changing nothing, if model has no configured
// deployment group at all, or deploymentName is not a member of it —
// this only ever adjusts an EXISTING deployment's weight, never adds or
// removes one (that needs a real config reload/restart, exactly like
// every other deployment-topology change today).
func (r *Router) SetWeight(model, deploymentName string, weight int) error {
	r.modelsMu.Lock()
	defer r.modelsMu.Unlock()

	ms, ok := r.models[model]
	if !ok {
		return fmt.Errorf("router: no deployment group configured for model %q", model)
	}

	deps := make([]weightedDeployment, len(ms.deps))
	copy(deps, ms.deps)
	found := false
	for i := range deps {
		if deps[i].name == deploymentName {
			deps[i].weight = weight
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("router: deployment %q is not a member of model %q's routing group", deploymentName, model)
	}

	r.models[model] = newModelState(deps)
	return nil
}
