// Package router selects which Deployment should serve the next request
// for a given canonical model, per docs/rfcs/2026-09-04-weighted-routing.md.
// It replaces dataplane.Pipeline's inline atomic-counter round-robin with
// smooth weighted round-robin (the LVS/IPVS wrr.c algorithm) — closing the
// "weighted" half of PRD.md's v1 routing scope line ("static + weighted
// routing; a single fallback chain"). The fallback half of that line is
// untouched: it already lives in dataplane.go/streaming.go and needs no
// change here.
//
// This package is a shared-kernel leaf (see gateway/ARCHITECTURE.md's
// dependency-direction table) — it must never import
// gateway/internal/gateway/dataplane or gateway/internal/cache.
//
// Deliberately out of scope, per the RFC: usage/latency/cost-based
// routing signals, circuit breaker / health-cooldown tracking, and
// model-group fallback chains.
package router

// Router selects a deployment for a canonical model via smooth weighted
// round-robin. One Router is built once (New) and shared by the whole
// dataplane.Pipeline, mirroring ratelimit.KeyLimiter/budget.Tracker's
// single-instance shape.
type Router struct {
	models map[string]*modelState
}

// New builds a Router from deployments, grouping by Model in the exact
// per-model input order given. Callers (cmd/gateway, via
// controlplane.Load's existing alphabetical-by-name sort) must never pass
// a list re-sorted by weight, or the equal-weight
// degrade-to-plain-round-robin guarantee the RFC proves breaks. A Weight
// of 0 means "unset," normalized to 1.
func New(deployments []Deployment) *Router {
	byModel := map[string][]weightedDeployment{}
	for _, d := range deployments {
		byModel[d.Model] = append(byModel[d.Model], weightedDeployment{name: d.Name, weight: d.Weight})
	}

	models := make(map[string]*modelState, len(byModel))
	for model, deps := range byModel {
		models[model] = newModelState(deps)
	}
	return &Router{models: models}
}

// Select returns the next chosen deployment's Name for model. The second
// return value is false only if no deployment is configured for model at
// all — the same "not found" contract dataplane.Pipeline.nextDeployment
// already has today.
func (r *Router) Select(model string) (string, bool) {
	ms, ok := r.models[model]
	if !ok {
		return "", false
	}
	return ms.next()
}
