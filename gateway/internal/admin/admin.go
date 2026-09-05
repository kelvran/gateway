// Package admin implements the gateway's optional, off-by-default admin
// HTTP surface, per docs/rfcs/2026-09-05-gateway-admin-api.md: read-only
// config introspection (GET /admin/config) plus the one section made
// live-mutable in v1, virtual keys (POST/DELETE /admin/virtual_keys/{name}).
//
// This is a deliberately separate credential space from client-facing
// virtual keys (internal/identity) — Handler's own bearer token is
// checked here, directly, and never delegates to identity.Verifier. A
// client's virtual key must never authenticate against this surface, and
// this surface's token must never authenticate against
// /v1/chat/completions. cmd/gateway is responsible for binding this
// Handler to its own separate net.Listener, never the same mux as the
// client-facing gateway — see that RFC's "never internet-facing by
// default" section for why.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// virtualKeyRequest is the POST /admin/virtual_keys/{name} request body.
// Field names deliberately mirror config.yaml's own virtual_keys.<name>
// section (key_hash, budget_usd, budget_reset_interval_seconds,
// allowed_models, rate_limit.{burst,refill_per_second}) — an operator
// already familiar with the static config shape needs no second
// vocabulary for the live-mutation API.
type virtualKeyRequest struct {
	KeyHash                    string            `json:"key_hash"`
	BudgetUSD                  decimal.Decimal   `json:"budget_usd"`
	BudgetResetIntervalSeconds int               `json:"budget_reset_interval_seconds"`
	AllowedModels              []string          `json:"allowed_models"`
	RateLimit                  *rateLimitRequest `json:"rate_limit"`
}

type rateLimitRequest struct {
	Burst           float64 `json:"burst"`
	RefillPerSecond float64 `json:"refill_per_second"`
}

// Handler builds the admin HTTP surface. cfg is the already-loaded,
// secret-free static config (served verbatim by GET /admin/config — see
// the RFC's "why Config is safe to return wholesale" section); pipeline
// is the live dataplane.Pipeline whose virtual keys this surface can
// mutate; token is the already-resolved (non-empty — callers must enforce
// this before constructing a Handler at all, per the RFC's "never starts
// with an empty/bypassable token" rule) admin bearer secret.
func Handler(cfg *controlplane.Config, pipeline *dataplane.Pipeline, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/config", getConfigHandler(cfg))
	mux.HandleFunc("POST /admin/virtual_keys/{name}", upsertVirtualKeyHandler(pipeline))
	mux.HandleFunc("DELETE /admin/virtual_keys/{name}", deleteVirtualKeyHandler(pipeline))
	return requireBearerToken(token, mux)
}

// requireBearerToken wraps next so every request must present
// "Authorization: Bearer <token>" matching token exactly, compared via a
// constant-time comparison — the same timing-safety posture
// internal/identity applies to virtual key lookups, applied here to a
// single static secret instead of a hash-keyed map.
func requireBearerToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const bearerPrefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if len(auth) <= len(bearerPrefix) || auth[:len(bearerPrefix)] != bearerPrefix {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		presented := auth[len(bearerPrefix):]
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			http.Error(w, "invalid admin token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// getConfigHandler serves the real, already-loaded *controlplane.Config
// as JSON, unredacted — see this package's own doc comment and the RFC's
// "why Config is safe to return wholesale" section for why nothing in it
// needs redaction: it holds environment-variable *names* and key
// *hashes*, never a raw secret.
func getConfigHandler(cfg *controlplane.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cfg); err != nil {
			http.Error(w, "encoding config", http.StatusInternalServerError)
		}
	}
}

// upsertVirtualKeyHandler adds a brand-new virtual key, or replaces the
// existing one with the same name, live — see
// dataplane.Pipeline.UpsertVirtualKey's own doc comment for the exact
// ordering guarantee (rate limiter registered before the Verifier swap).
func upsertVirtualKeyHandler(pipeline *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "virtual key name is required", http.StatusBadRequest)
			return
		}

		var req virtualKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.KeyHash == "" {
			http.Error(w, "key_hash is required", http.StatusBadRequest)
			return
		}

		var allowedModels map[string]struct{}
		if len(req.AllowedModels) > 0 {
			allowedModels = make(map[string]struct{}, len(req.AllowedModels))
			for _, m := range req.AllowedModels {
				allowedModels[m] = struct{}{}
			}
		}
		burst, refill := 0.0, 0.0
		if req.RateLimit != nil {
			burst, refill = req.RateLimit.Burst, req.RateLimit.RefillPerSecond
		}

		vk := identity.VirtualKey{
			ID:                  name,
			KeyHash:             req.KeyHash,
			BudgetUSD:           req.BudgetUSD,
			BudgetResetInterval: secondsToDuration(req.BudgetResetIntervalSeconds),
			AllowedModels:       allowedModels,
			RateLimitBurst:      burst,
			RateLimitRefill:     refill,
		}
		rateLimitCfg := ratelimit.KeyConfig{ID: name, Capacity: burst, RefillPerSecond: refill}

		if err := pipeline.UpsertVirtualKey(vk, rateLimitCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteVirtualKeyHandler removes a virtual key, live. 404 if the name
// doesn't match any configured key; 409 if it's the last remaining one
// (dataplane.Pipeline.DeleteVirtualKey's own refusal — never leaves the
// gateway with no client able to authenticate at all).
func deleteVirtualKeyHandler(pipeline *dataplane.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "virtual key name is required", http.StatusBadRequest)
			return
		}

		err := pipeline.DeleteVirtualKey(name)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, dataplane.ErrVirtualKeyNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, dataplane.ErrCannotDeleteLastVirtualKey):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// secondsToDuration mirrors cmd/gateway's own identical conversion for
// the static config path (VirtualKeyConfig.BudgetResetIntervalSeconds ->
// identity.VirtualKey.BudgetResetInterval) — kept as a tiny local helper
// rather than exported from either package solely for this one call site.
func secondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}
