// Package admin implements the gateway's optional, off-by-default admin
// HTTP surface, per docs/rfcs/2026-09-05-gateway-admin-api.md: read-only
// config introspection (GET /admin/config) plus the one section made
// live-mutable in v1, virtual keys (POST/DELETE /admin/virtual_keys/{name}).
//
// This is a deliberately separate credential space from client-facing
// virtual keys (internal/identity) — Handler's own bearer tokens are
// checked here, directly, and never delegate to identity.Verifier. A
// client's virtual key must never authenticate against this surface, and
// this surface's tokens must never authenticate against
// /v1/chat/completions. cmd/gateway is responsible for binding this
// Handler to its own separate net.Listener, never the same mux as the
// client-facing gateway — see that RFC's "never internet-facing by
// default" section for why.
//
// Two credential tiers exist, per
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md: Credentials.Admin
// (required, full read/write) and an optional Credentials.Viewer
// (read-only — GET /admin/config only, never a write route). Every
// successful virtual-key create/delete is logged (name, never the
// credential/key_hash value) via the Logger passed to Handler.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// Credentials holds the admin surface's two credential tiers, per
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md. Admin is required
// (callers must enforce this is non-empty before constructing a Handler
// at all, same as the pre-existing single-token contract). Viewer is
// optional — an empty Viewer means no viewer tier is configured, and
// GET /admin/config then requires Admin exactly as it always has.
// Viewer, when set, can never authenticate a write (POST/DELETE
// /admin/virtual_keys/{name}) — those routes always require Admin.
type Credentials struct {
	Admin  string
	Viewer string
}

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
	BudgetWarnPercent          float64           `json:"budget_warn_percent"`
	AllowedModels              []string          `json:"allowed_models"`
	RateLimit                  *rateLimitRequest `json:"rate_limit"`
}

type rateLimitRequest struct {
	Burst              float64 `json:"burst"`
	RefillPerSecond    float64 `json:"refill_per_second"`
	TPMCapacity        float64 `json:"tpm_capacity"`
	TPMRefillPerSecond float64 `json:"tpm_refill_per_second"`
	// PerModel mirrors config.yaml's rate_limit.per_model section — see
	// controlplane.VirtualKeyConfig.PerModelRateLimits' doc comment and
	// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md. An
	// upsert is a full replace of this key's rate-limit configuration,
	// never a partial merge — like every other field on this request
	// struct — so omitting per_model on an update to an already-overridden
	// key clears its overrides, exactly as omitting rate_limit entirely
	// already resets burst/refill to zero (which UpsertVirtualKey's
	// caller then resolves to the gateway's own default).
	PerModel map[string]perModelRateLimitRequest `json:"per_model"`
}

// perModelRateLimitRequest is one model's per-model RPM override within a
// rateLimitRequest. Burst/RefillPerSecond are both required and must be
// positive — see upsertVirtualKeyHandler's validation, mirroring
// controlplane.parsePerModelRateLimits' identical rule for the static
// config file, so an operator gets the same validation regardless of
// which of the two surfaces they use. TPMCapacity/TPMRefillPerSecond are
// the optional per-model TPM override (must be set together or neither),
// mirroring controlplane.ModelRateLimitConfig's identical fields.
type perModelRateLimitRequest struct {
	Burst              float64 `json:"burst"`
	RefillPerSecond    float64 `json:"refill_per_second"`
	TPMCapacity        float64 `json:"tpm_capacity"`
	TPMRefillPerSecond float64 `json:"tpm_refill_per_second"`
}

// Handler builds the admin HTTP surface. cfg is the already-loaded,
// secret-free static config (served verbatim by GET /admin/config — see
// the RFC's "why Config is safe to return wholesale" section); pipeline
// is the live dataplane.Pipeline whose virtual keys this surface can
// mutate; creds holds the already-resolved (Admin non-empty — callers
// must enforce this before constructing a Handler at all, per the RFC's
// "never starts with an empty/bypassable token" rule) credential tiers;
// logger records a structured audit entry on every successful virtual-key
// create/delete (never the credential/secret value itself), per
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md.
func Handler(cfg *controlplane.Config, pipeline *dataplane.Pipeline, creds Credentials, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /admin/config", requireEitherBearerToken(creds, getConfigHandler(cfg)))
	mux.Handle("POST /admin/virtual_keys/{name}", requireBearerToken(creds.Admin, upsertVirtualKeyHandler(pipeline, logger)))
	mux.Handle("DELETE /admin/virtual_keys/{name}", requireBearerToken(creds.Admin, deleteVirtualKeyHandler(pipeline, logger)))
	return mux
}

// requireBearerToken wraps next so every request must present
// "Authorization: Bearer <token>" matching token exactly, compared via a
// constant-time comparison — the same timing-safety posture
// internal/identity applies to virtual key lookups, applied here to a
// single static secret instead of a hash-keyed map.
func requireBearerToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			http.Error(w, "invalid admin token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireEitherBearerToken wraps next so a request authenticates with
// EITHER creds.Admin OR, when configured (non-empty), creds.Viewer — used
// only for the read-only GET /admin/config route. Write routes
// (POST/DELETE /admin/virtual_keys/{name}) always use requireBearerToken
// with creds.Admin specifically, never this function, per
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md.
func requireEitherBearerToken(creds Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		presentedBytes := []byte(presented)
		if subtle.ConstantTimeCompare(presentedBytes, []byte(creds.Admin)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if creds.Viewer != "" && subtle.ConstantTimeCompare(presentedBytes, []byte(creds.Viewer)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "invalid admin token", http.StatusUnauthorized)
	})
}

// bearerToken extracts the raw token from a well-formed
// "Authorization: Bearer <token>" header — shared by
// requireBearerToken/requireEitherBearerToken so the malformed-header
// check stays in exactly one place.
func bearerToken(r *http.Request) (string, bool) {
	const bearerPrefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(bearerPrefix) || auth[:len(bearerPrefix)] != bearerPrefix {
		return "", false
	}
	return auth[len(bearerPrefix):], true
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
// logger records name (never the presented credential or key_hash) on
// every successful upsert.
func upsertVirtualKeyHandler(pipeline *dataplane.Pipeline, logger *slog.Logger) http.HandlerFunc {
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
		var tpmCapacity, tpmRefill float64
		var perModel map[string]ratelimit.ModelRateLimit
		if req.RateLimit != nil {
			burst, refill = req.RateLimit.Burst, req.RateLimit.RefillPerSecond
			tpmCapacity, tpmRefill = req.RateLimit.TPMCapacity, req.RateLimit.TPMRefillPerSecond
			if len(req.RateLimit.PerModel) > 0 {
				perModel = make(map[string]ratelimit.ModelRateLimit, len(req.RateLimit.PerModel))
				for model, mrl := range req.RateLimit.PerModel {
					if mrl.Burst <= 0 || mrl.RefillPerSecond <= 0 {
						http.Error(w, fmt.Sprintf("rate_limit.per_model.%s must set positive burst and refill_per_second", model), http.StatusBadRequest)
						return
					}
					if (mrl.TPMCapacity > 0) != (mrl.TPMRefillPerSecond > 0) {
						http.Error(w, fmt.Sprintf("rate_limit.per_model.%s.tpm_capacity/tpm_refill_per_second must both be set, or neither", model), http.StatusBadRequest)
						return
					}
					perModel[model] = ratelimit.ModelRateLimit{
						Capacity:           mrl.Burst,
						RefillPerSecond:    mrl.RefillPerSecond,
						TPMCapacity:        mrl.TPMCapacity,
						TPMRefillPerSecond: mrl.TPMRefillPerSecond,
					}
				}
			}
		}

		vk := identity.VirtualKey{
			ID:                  name,
			KeyHash:             req.KeyHash,
			BudgetUSD:           req.BudgetUSD,
			BudgetResetInterval: secondsToDuration(req.BudgetResetIntervalSeconds),
			BudgetWarnPercent:   req.BudgetWarnPercent,
			AllowedModels:       allowedModels,
			RateLimitBurst:      burst,
			RateLimitRefill:     refill,
		}
		rateLimitCfg := ratelimit.KeyConfig{
			ID:                 name,
			Capacity:           burst,
			RefillPerSecond:    refill,
			TPMCapacity:        tpmCapacity,
			TPMRefillPerSecond: tpmRefill,
			PerModel:           perModel,
		}

		if err := pipeline.UpsertVirtualKey(vk, rateLimitCfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("admin_virtual_key_upserted", "name", name, "authorized_by", "admin")
		w.WriteHeader(http.StatusNoContent)
	}
}

// deleteVirtualKeyHandler removes a virtual key, live. 404 if the name
// doesn't match any configured key; 409 if it's the last remaining one
// (dataplane.Pipeline.DeleteVirtualKey's own refusal — never leaves the
// gateway with no client able to authenticate at all). logger records
// name on every successful delete.
func deleteVirtualKeyHandler(pipeline *dataplane.Pipeline, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "virtual key name is required", http.StatusBadRequest)
			return
		}

		err := pipeline.DeleteVirtualKey(name)
		switch {
		case err == nil:
			logger.Info("admin_virtual_key_deleted", "name", name, "authorized_by", "admin")
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
