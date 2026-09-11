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
// Deliberately out of scope, per the weighted-routing RFC and narrowed
// (not overturned) by the health-probing RFC: usage/latency/cost-based
// routing signals, model-group fallback chains, and the traffic-derived
// statistical circuit breaker (Envoy-style outlier detection,
// LiteLLM-style allowed_fails cooldown) — that class genuinely needs a
// traffic-volume floor Kelvran doesn't have production data for yet, and
// correctly stays deferred per gateway/ARCHITECTURE.md. Only the
// traffic-independent active-probe half is real here.
package router

import "sync"

// Router selects a deployment for a canonical model via smooth weighted
// round-robin, skipping any deployment ReportProbeResult has marked
// unhealthy. One Router is built once (New) and shared by the whole
// dataplane.Pipeline, mirroring ratelimit.KeyLimiter/budget.Tracker's
// single-instance shape.
type Router struct {
	models map[string]*modelState

	healthMu  sync.Mutex
	healthCfg HealthConfig
	health    map[string]*deploymentHealth

	// costTiers maps deployment Name -> configured CostTier (see
	// Deployment.CostTier's own doc comment). Populated once, at New()
	// time, from the exact same input deployments used to build models —
	// read-only afterward, safe for concurrent access without a lock,
	// exactly like models itself.
	costTiers map[string]int
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
	for _, d := range deployments {
		byModel[d.Model] = append(byModel[d.Model], weightedDeployment{name: d.Name, weight: d.Weight})
		costTiers[d.Name] = d.CostTier
	}

	models := make(map[string]*modelState, len(byModel))
	for model, deps := range byModel {
		models[model] = newModelState(deps)
	}
	return &Router{
		models:    models,
		healthCfg: health.normalized(),
		health:    map[string]*deploymentHealth{},
		costTiers: costTiers,
	}
}

// Select returns the next chosen deployment's Name for model, skipping
// any deployment currently marked unhealthy (see selectHealthy in
// health.go). The second return value is false only if no deployment is
// configured for model at all — the same "not found" contract
// dataplane.Pipeline.nextDeployment already has today.
func (r *Router) Select(model string) (string, bool) {
	ms, ok := r.models[model]
	if !ok {
		return "", false
	}
	return r.selectHealthy(ms)
}
