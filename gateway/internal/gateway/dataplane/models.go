package dataplane

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/kelvran/gateway/gateway/internal/identity"
)

// Model catalog for GET /v1/models (plan item 6 of the 2026-10-08
// discoverability round; live defect: every OpenAI-compatible model-picker
// UI, the OpenAI and Anthropic SDKs' models.list() and Claude Code's
// provider discovery call this route, and Kelvran had none).
//
// The catalog is the set of CANONICAL model names a virtual key may send as
// `model` -- one entry per distinct Deployment.Model, never one per
// deployment, because routing, allowlists and the price table are all
// keyed by the canonical name and a client must never learn, or be able
// to pick, an individual deployment. It is derived from
// p.deploymentsByName at request time (a map the admin API never adds to
// or removes from -- weight changes do not alter membership), so it needs
// no lock, exactly like ReadinessSummary's own read of the same map.
//
// Auth is required and fails closed like every data route. The reason is
// not secrecy of the names -- /readyz already lists them unauthenticated
// -- but that the list is PER KEY: a key restricted by AllowedModels sees
// only what it may call, so a model picker never offers a choice that
// would 403. No rate limit, budget or guardrail runs here: this is an
// in-memory read with no upstream call and no tokens.

// ModelMetadata is the optional operator-supplied display metadata for one
// canonical model, from controlplane's top-level `models:` section.
type ModelMetadata struct {
	// DisplayName is what model pickers show; the handler falls back to
	// the canonical id when this is empty.
	DisplayName string
	// Description is free text for pickers that show one (Claude Code).
	Description string
}

// ModelInfo is one canonical model as GET /v1/models reports it to the
// calling virtual key.
type ModelInfo struct {
	// ID is the canonical model name -- what the client sends as `model`.
	ID string
	// Kind is "chat" or "embedding" (Deployment.Kind, with the zero value
	// a directly-built Deployment carries read as "chat", the same default
	// controlplane.Load applies).
	Kind string
	// Providers is the sorted, de-duplicated set of provider names whose
	// deployments serve this model. Reported, never selectable.
	Providers []string
	// DisplayName and Description are the operator metadata, "" when unset.
	DisplayName string
	Description string
}

// CatalogLoadedAt is when this process built its catalog (NewPipeline).
// It backs the `created` field every entry reports: "when this instance
// loaded its catalog", not a model's release date (which no provider
// reports uniformly) -- replicas restarted at different times therefore
// report different values, documented as such.
func (p *Pipeline) CatalogLoadedAt() time.Time { return p.catalogLoadedAt }

// HandleListModels authenticates the request exactly as
// HandleChatCompletion does (bearer, then source-IP allowlist, with the
// same wrapped sentinels so cmd/gateway's error envelope maps them the
// same way), then returns the canonical models the key may call, sorted
// by id.
func (p *Pipeline) HandleListModels(ctx context.Context, authorizationHeader, remoteAddr string) ([]ModelInfo, error) {
	// The same trace fields logRequest carries (the handler's otelhttp span
	// is on ctx), so a 401 here correlates like a chat 401 does.
	fields := traceLogFields(ctx)
	vk, verifyErr := p.verifier.Load().Verify(authorizationHeader)
	if verifyErr != nil {
		// The error names the failure class only (identity's sentinels); the
		// presented credential never reaches a log field. An expired key is
		// named, as logRequest names it: the operator needs to know which key
		// is failing (RFC-3 decision 4).
		fields = append(fields, "error", verifyErr.Error())
		if expired := expiredKeyFromErr(verifyErr); expired != nil {
			fields = append(fields, "virtual_key_id", expired.ID, "key_expired_at", expired.ExpiresAt.UTC().Format(time.RFC3339))
		}
		p.logger.Warn("list_models_auth_failed", fields...)
		return nil, fmt.Errorf("dataplane: auth: %w", verifyErr)
	}
	if !isSourceIPAllowed(vk, resolveClientIP(remoteAddr)) {
		p.logger.Warn("list_models_source_ip_not_allowed", append(fields, "virtual_key_id", vk.ID)...)
		return nil, fmt.Errorf("%w: %q", ErrSourceIPNotAllowed, remoteAddr)
	}
	models := p.listModelsFor(vk)
	p.logger.Info("list_models", append(fields, "virtual_key_id", vk.ID, "models", len(models))...)
	return models, nil
}

// listModelsFor groups the configured deployments by canonical model,
// keeps the models vk may call, merges each model's providers, and sorts
// the result by id so pagination over it is stable.
func (p *Pipeline) listModelsFor(vk *identity.VirtualKey) []ModelInfo {
	byModel := map[string]*ModelInfo{}
	providers := map[string]map[string]struct{}{}
	for _, dep := range p.deploymentsByName {
		if !isModelAllowed(vk, dep.Model) {
			continue
		}
		if _, seen := byModel[dep.Model]; !seen {
			meta := p.modelMetadata[dep.Model]
			byModel[dep.Model] = &ModelInfo{ID: dep.Model, Kind: deploymentKind(dep), DisplayName: meta.DisplayName, Description: meta.Description}
			providers[dep.Model] = map[string]struct{}{}
		}
		providers[dep.Model][dep.Provider] = struct{}{}
	}
	out := make([]ModelInfo, 0, len(byModel))
	for id, info := range byModel {
		for provider := range providers[id] {
			info.Providers = append(info.Providers, provider)
		}
		sort.Strings(info.Providers)
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// deploymentKind reads Deployment.Kind with the "" -> "chat" default
// controlplane.Load applies, for Deployments built directly (tests).
func deploymentKind(dep Deployment) string {
	if dep.Kind == "" {
		return "chat"
	}
	return dep.Kind
}
