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
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"sort"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/prompt"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// Credentials holds the admin surface's three credential tiers. Admin is
// required (callers must enforce this is non-empty before constructing a
// Handler at all, same as the pre-existing single-token contract). Viewer
// and CostViewer are both optional — an empty value means that tier isn't
// configured. Viewer, per docs/rfcs/2026-09-09-gateway-admin-viewer-role.md,
// is full read access (GET /admin/config, prompts) but never a write.
// CostViewer, per docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md's
// narrow-third-tier recommendation, is narrower still: it authenticates
// ONLY GET /admin/virtual_keys/{name}/spend, never config, prompts, or any
// write route — a credential an operator can hand to a billing/finance
// consumer without granting it any visibility into deployment topology,
// prompt content, or model/rate-limit configuration.
type Credentials struct {
	Admin      string
	Viewer     string
	CostViewer string
}

// virtualKeyRequest is the POST /admin/virtual_keys/{name} request body.
// Field names deliberately mirror config.yaml's own virtual_keys.<name>
// section (key_hash, budget_usd, budget_reset_interval_seconds,
// allowed_models, rate_limit.{burst,refill_per_second}) — an operator
// already familiar with the static config shape needs no second
// vocabulary for the live-mutation API.
type virtualKeyRequest struct {
	KeyHash                    string          `json:"key_hash"`
	BudgetUSD                  decimal.Decimal `json:"budget_usd"`
	BudgetResetIntervalSeconds int             `json:"budget_reset_interval_seconds"`
	BudgetWarnPercent          float64         `json:"budget_warn_percent"`
	AllowedModels              []string        `json:"allowed_models"`
	// AllowedRegions restricts this key to deployments in a subset of
	// regions (Deployment.Region), per
	// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md
	// — mirrors AllowedModels's own convention exactly.
	AllowedRegions []string          `json:"allowed_regions"`
	RateLimit      *rateLimitRequest `json:"rate_limit"`
}

// rotateVirtualKeyRequest is the POST /admin/virtual_keys/{name}/rotate
// request body, per docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 1. GracePeriodSeconds <= 0 rotates with no grace period at all
// -- the old secret stops working immediately, identical in effect to a
// delete-then-recreate but atomic and without the intervening window
// where the key doesn't exist at all.
type rotateVirtualKeyRequest struct {
	NewKeyHash         string `json:"new_key_hash"`
	GracePeriodSeconds int    `json:"grace_period_seconds"`
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
	// resets burst/refill to zero before upsertVirtualKeyHandler's own
	// ratelimit.ResolveKeyRateLimit call resolves that zero pair to
	// ratelimit.DefaultKeyBurstCapacity/DefaultKeyRefillPerSecond — a
	// round-3 backlog audit found this comment's prior wording ("which
	// UpsertVirtualKey's caller then resolves to the gateway's own
	// default") was FALSE: no such resolution existed anywhere on this
	// path, and a key created with no rate_limit section got a permanent,
	// never-refilling zero-capacity bucket instead.
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

// auditLogger gates every admin audit-log line behind
// cfg.Admin.EnableAuditLog, so an operator can disable Kelvran's own
// admin-mutation audit trail (e.g. because a separate compliance pipeline
// already captures the same events) without a per-call-site conditional
// at each of this package's own logging points. Every logger call in this
// package IS an audit entry (confirmed: this file has zero Warn/Error/Debug
// calls, only Info) — so wrapping the single Info method here is
// sufficient to gate all of them, not just some.
type auditLogger struct {
	logger  *slog.Logger
	enabled bool
}

func (a auditLogger) Info(msg string, args ...any) {
	if !a.enabled {
		return
	}
	a.logger.Info(msg, args...)
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
// docs/rfcs/2026-09-09-gateway-admin-viewer-role.md — unless
// cfg.Admin.EnableAuditLog is false, per that field's own doc comment.
func Handler(cfg *controlplane.Config, pipeline *dataplane.Pipeline, creds Credentials, logger *slog.Logger) http.Handler {
	audit := auditLogger{logger: logger, enabled: cfg.Admin.EnableAuditLog}
	mux := http.NewServeMux()
	mux.Handle("GET /admin/config", requireEitherBearerToken(creds, getConfigHandler(cfg, audit)))
	mux.Handle("GET /admin/virtual_keys", requireEitherBearerToken(creds, listVirtualKeysHandler(pipeline, audit)))
	mux.Handle("POST /admin/virtual_keys/{name}", requireBearerToken(creds.Admin, upsertVirtualKeyHandler(pipeline, audit)))
	mux.Handle("DELETE /admin/virtual_keys/{name}", requireBearerToken(creds.Admin, deleteVirtualKeyHandler(pipeline, audit)))
	mux.Handle("POST /admin/virtual_keys/{name}/rotate", requireBearerToken(creds.Admin, rotateVirtualKeyHandler(pipeline, audit)))
	// Deliberately its own middleware call, not requireEitherBearerToken:
	// this is the one route CostViewer authenticates, alongside Admin and
	// Viewer (both of which already see strictly more elsewhere on this
	// mux, so neither loses anything by also being able to read this
	// narrower view) — see requireAnyBearerToken's own doc comment.
	mux.Handle("GET /admin/virtual_keys/{name}/spend", requireAnyBearerToken(
		getVirtualKeySpendHandler(pipeline, audit),
		tokenTier{creds.Admin, "admin"},
		tokenTier{creds.Viewer, "viewer"},
		tokenTier{creds.CostViewer, "cost_viewer"},
	))
	// Prompt/template management, per this feature's own design: prompts
	// are GLOBAL, operator-managed config (the same category as
	// price_table/deployments/guardrails config above) -- reads are
	// viewer-or-admin, like GET /admin/config; writes are admin-only,
	// like the virtual-key routes above.
	mux.Handle("GET /admin/prompts", requireEitherBearerToken(creds, listPromptsHandler(pipeline, audit)))
	mux.Handle("GET /admin/prompts/{id}", requireEitherBearerToken(creds, getPromptHandler(pipeline, audit)))
	mux.Handle("GET /admin/prompts/{id}/versions/{version}", requireEitherBearerToken(creds, getPromptVersionHandler(pipeline, audit)))
	mux.Handle("POST /admin/prompts/{id}", requireBearerToken(creds.Admin, upsertPromptHandler(pipeline, audit)))
	mux.Handle("DELETE /admin/prompts/{id}", requireBearerToken(creds.Admin, deletePromptHandler(pipeline, audit)))
	// Prompt label management (promote/rollback), per
	// internal/prompt.Store.SetLabel's own doc comment -- reuses the
	// Admin tier exactly, the same convention every other write route on
	// this mux already follows (no precedent exists for a narrower
	// "labels-only" tier, and no named demand for one yet). Rollback is
	// SetLabel to an OLDER version, not a separate route.
	mux.Handle("PUT /admin/prompts/{id}/labels/{label}", requireBearerToken(creds.Admin, setPromptLabelHandler(pipeline, audit)))
	mux.Handle("DELETE /admin/prompts/{id}/labels/{label}", requireBearerToken(creds.Admin, deletePromptLabelHandler(pipeline, audit)))
	// Live bbolt backup, per cfg.Admin.BackupDir's own doc comment --
	// admin-only (a write-shaped, disk-touching operation, same tier as
	// every other write route on this mux), always registered (unlike
	// EnablePprof's conditional mount above) so an unconfigured caller
	// gets an informative 501 from backupHandler itself, not a bare 404
	// indistinguishable from a typo'd path.
	mux.Handle("POST /admin/backup", requireBearerToken(creds.Admin, backupHandler(cfg, pipeline, audit)))
	// Deployment weight live-mutation, per
	// docs/upgrade-research/admin-operator-experience-2026-09-14.md --
	// admin-only, same tier as every other write route on this mux: a
	// deployment's routing weight is an operational lever, not read-only
	// reporting.
	mux.Handle("POST /admin/deployments/{name}/weight", requireBearerToken(creds.Admin, updateDeploymentWeightHandler(pipeline, audit)))
	mux.Handle("POST /admin/cache/erase", requireBearerToken(creds.Admin, eraseCacheEntryHandler(pipeline, audit)))
	// pprof, per cfg.Admin.EnablePprof's own doc comment — off by
	// default, admin-credential-gated (never the viewer tier: profiling
	// data is a stronger information-disclosure/DoS-surface signal than
	// anything the read-only viewer tier exposes elsewhere on this mux),
	// mounted only on this already off-by-default, loopback-default,
	// bearer-token-protected mux -- never a second listener, never a
	// second credential space.
	if cfg.Admin.EnablePprof {
		mux.Handle("GET /admin/debug/pprof/", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Index)))
		mux.Handle("GET /admin/debug/pprof/cmdline", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Cmdline)))
		mux.Handle("GET /admin/debug/pprof/profile", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Profile)))
		mux.Handle("GET /admin/debug/pprof/symbol", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Symbol)))
		mux.Handle("POST /admin/debug/pprof/symbol", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Symbol)))
		mux.Handle("GET /admin/debug/pprof/trace", requireBearerToken(creds.Admin, http.HandlerFunc(pprof.Trace)))
		// Named profiles registered against the DefaultServeMux by pprof's
		// own package init() (goroutine, heap, threadcreate, block, mutex,
		// allocs) -- Handler() looks them up by name via pprof.Handler,
		// the same indirection net/http/pprof's own docs recommend for
		// mounting under a custom prefix instead of DefaultServeMux.
		for _, name := range []string{"goroutine", "heap", "threadcreate", "block", "mutex", "allocs"} {
			mux.Handle("GET /admin/debug/pprof/"+name, requireBearerToken(creds.Admin, pprof.Handler(name)))
		}
	}
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
// for every read-only route (GET /admin/config, GET /admin/prompts and
// its two sibling routes). Write routes (POST/DELETE
// /admin/virtual_keys/{name}, POST/DELETE /admin/prompts/{id}) always use
// requireBearerToken with creds.Admin specifically, never this function,
// per docs/rfcs/2026-09-09-gateway-admin-viewer-role.md.
//
// Stashes WHICH tier authenticated into the request's context (never the
// credential value itself) via contextWithCredentialTier, so a read
// handler's own audit-log line can record "admin" vs. "viewer" without
// re-deriving it or re-comparing the token a second time.
func requireEitherBearerToken(creds Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		presentedBytes := []byte(presented)
		if subtle.ConstantTimeCompare(presentedBytes, []byte(creds.Admin)) == 1 {
			next.ServeHTTP(w, r.WithContext(contextWithCredentialTier(r.Context(), "admin")))
			return
		}
		if creds.Viewer != "" && subtle.ConstantTimeCompare(presentedBytes, []byte(creds.Viewer)) == 1 {
			next.ServeHTTP(w, r.WithContext(contextWithCredentialTier(r.Context(), "viewer")))
			return
		}
		http.Error(w, "invalid admin token", http.StatusUnauthorized)
	})
}

// tokenTier pairs a credential token with the tier name it represents --
// requireAnyBearerToken's own building block.
type tokenTier struct {
	token string
	tier  string
}

// requireAnyBearerToken wraps next so a request authenticates with ANY of
// pairs' non-empty tokens, stashing whichever tier matched into the
// request's context exactly like requireEitherBearerToken does. Used only
// for the new GET /admin/virtual_keys/{name}/spend route — every existing
// route keeps using requireBearerToken/requireEitherBearerToken,
// unchanged. A zero-value token in pairs (a tier that was never
// configured, e.g. an unset CostViewer) never matches any presented
// credential, mirroring requireEitherBearerToken's own "Viewer, when
// empty, authenticates nothing" convention — skipped explicitly rather
// than compared, since bearerToken already guarantees a non-empty
// presented value whenever ok is true, so an empty pair.token could only
// ever match a request bearerToken would have already rejected, making
// the comparison itself pointless, not just redundant.
func requireAnyBearerToken(next http.Handler, pairs ...tokenTier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := bearerToken(r)
		if !ok {
			http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
			return
		}
		presentedBytes := []byte(presented)
		for _, pair := range pairs {
			if pair.token == "" {
				continue
			}
			if subtle.ConstantTimeCompare(presentedBytes, []byte(pair.token)) == 1 {
				next.ServeHTTP(w, r.WithContext(contextWithCredentialTier(r.Context(), pair.tier)))
				return
			}
		}
		http.Error(w, "invalid admin token", http.StatusUnauthorized)
	})
}

// credentialTierContextKey is a private type so no other package can
// collide with or forge this context value — the same "unexported key
// type" idiom the Go standard library's own context doc comment
// recommends.
type credentialTierContextKey struct{}

// contextWithCredentialTier/credentialTierFromContext stash and retrieve
// which credential tier ("admin"/"viewer") authenticated the current
// request — set only by requireEitherBearerToken, read only by the 4
// read-route audit-log call sites below. credentialTierFromContext
// returns "" (never a fabricated default) if the context has no such
// value at all, e.g. a direct unit-test call to a handler that bypasses
// the middleware entirely.
func contextWithCredentialTier(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, credentialTierContextKey{}, tier)
}

func credentialTierFromContext(ctx context.Context) string {
	tier, _ := ctx.Value(credentialTierContextKey{}).(string)
	return tier
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
//
// logger records this read (route + which credential tier authenticated,
// never the credential value or the response body) — a round-3 backlog-
// audit finding: every admin WRITE was already audit-logged, but reading
// the full deployment topology/price table/every virtual key's budget-
// and-model shape via this route left zero trace an operator could ever
// detect after the fact, exactly the recon signal THREAT_MODEL.md's own
// "a compromised admin credential IS a full privilege escalation" row
// names as the accepted residual risk this closes visibility into.
func getConfigHandler(cfg *controlplane.Config, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(cfg); err != nil {
			http.Error(w, "encoding config", http.StatusInternalServerError)
			return
		}
		logger.Info("admin_config_read", "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// maxAdminIdentifierLen bounds a caller-supplied identifier (a virtual
// key name or prompt id) accepted at write time by upsertVirtualKeyHandler/
// upsertPromptHandler. **Fixed 2026-09-17, real bug**: neither handler
// validated length or character content on this path parameter at all
// before this check existed -- it becomes a map key in every in-memory
// store this identifier touches (identity.Verifier, budget.Tracker,
// ratelimit.KeyLimiter, prompt.Store) and is logged on every future
// request that references it. 256 is far beyond any realistic name/id
// while still bounding an operator mistake or a pathologically long
// value to a known, finite cost -- checked only at WRITE time (upsert),
// never at read/delete/rotate, where an oversized value just fails an
// ordinary "not found" lookup with no growth risk.
const maxAdminIdentifierLen = 256

// validateAdminIdentifier rejects an empty or oversized identifier --
// see maxAdminIdentifierLen's own doc comment.
func validateAdminIdentifier(id, fieldName string) error {
	if len(id) > maxAdminIdentifierLen {
		return fmt.Errorf("%s exceeds %d characters", fieldName, maxAdminIdentifierLen)
	}
	return nil
}

// upsertVirtualKeyHandler adds a brand-new virtual key, or replaces the
// existing one with the same name, live — see
// dataplane.Pipeline.UpsertVirtualKey's own doc comment for the exact
// ordering guarantee (rate limiter registered before the Verifier swap).
// logger records name (never the presented credential or key_hash) on
// every successful upsert.
func upsertVirtualKeyHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "virtual key name is required", http.StatusBadRequest)
			return
		}
		if err := validateAdminIdentifier(name, "virtual key name"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		// **Fixed 2026-09-17, real bug**: this handler performed no
		// validation at all on budget_usd/budget_reset_interval_seconds/
		// budget_warn_percent before this check existed. A negative
		// budget_reset_interval_seconds becomes a negative time.Duration
		// (secondsToDuration below), which budget.Tracker's own
		// resetIfNeeded compares via now.Sub(start) >= resetInterval --
		// trivially true on every single check against a negative
		// duration, silently disabling the rolling-window mechanism
		// entirely (a permanent reset, not a "no window" no-op) rather
		// than erroring on an operator mistake. A negative budget_usd is
		// equally nonsensical against IsPositive()'s own "positive means
		// enforced, non-positive means unlimited" convention (line 639
		// below) -- silently landing in the "unlimited" bucket for the
		// wrong reason. budget_warn_percent outside [0, 100] is a
		// non-fatal but equally confusing operator mistake worth
		// rejecting up front rather than producing an alert threshold
		// that can never fire (>100) or fires immediately (<0).
		if req.BudgetUSD.IsNegative() {
			http.Error(w, "budget_usd must not be negative", http.StatusBadRequest)
			return
		}
		if req.BudgetResetIntervalSeconds < 0 {
			http.Error(w, "budget_reset_interval_seconds must not be negative", http.StatusBadRequest)
			return
		}
		if req.BudgetWarnPercent < 0 || req.BudgetWarnPercent > 100 {
			http.Error(w, "budget_warn_percent must be between 0 and 100", http.StatusBadRequest)
			return
		}

		var allowedModels map[string]struct{}
		if len(req.AllowedModels) > 0 {
			allowedModels = make(map[string]struct{}, len(req.AllowedModels))
			for _, m := range req.AllowedModels {
				allowedModels[m] = struct{}{}
			}
		}
		var allowedRegions map[string]struct{}
		if len(req.AllowedRegions) > 0 {
			allowedRegions = make(map[string]struct{}, len(req.AllowedRegions))
			for _, reg := range req.AllowedRegions {
				allowedRegions[reg] = struct{}{}
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
		// A round-3 backlog-audit finding: without this call, a key
		// created/updated with no rate_limit section (the common,
		// no-throttling-needed case) got Capacity=0/RefillPerSecond=0 --
		// a TokenBucket that never refills above zero, so Allow() denies
		// every request against it forever. Shares the exact resolution
		// cmd/gateway's static-config path already applies, per
		// ResolveKeyRateLimit's own doc comment.
		burst, refill = ratelimit.ResolveKeyRateLimit(burst, refill)

		vk := identity.VirtualKey{
			ID:                  name,
			KeyHash:             req.KeyHash,
			BudgetUSD:           req.BudgetUSD,
			BudgetResetInterval: secondsToDuration(req.BudgetResetIntervalSeconds),
			BudgetWarnPercent:   req.BudgetWarnPercent,
			AllowedModels:       allowedModels,
			AllowedRegions:      allowedRegions,
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
func deleteVirtualKeyHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
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

// rotateVirtualKeyHandler issues a new secret for name, live, keeping the
// old one valid for the requested grace period -- see
// dataplane.Pipeline.RotateVirtualKey's own doc comment for the exact
// mechanism. 404 if name doesn't match any configured key. logger records
// name and the grace period (never either key hash) on every successful
// rotation, mirroring upsertVirtualKeyHandler's identical "log
// identifiers, not secret material" discipline.
func rotateVirtualKeyHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "virtual key name is required", http.StatusBadRequest)
			return
		}

		var req rotateVirtualKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.NewKeyHash == "" {
			http.Error(w, "new_key_hash is required", http.StatusBadRequest)
			return
		}

		gracePeriod := secondsToDuration(req.GracePeriodSeconds)
		err := pipeline.RotateVirtualKey(name, req.NewKeyHash, gracePeriod)
		switch {
		case err == nil:
			logger.Info("admin_virtual_key_rotated", "name", name, "grace_period_seconds", req.GracePeriodSeconds, "authorized_by", "admin")
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, dataplane.ErrVirtualKeyNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}
}

// virtualKeySpendResponse is what GET /admin/virtual_keys/{name}/spend
// returns -- deliberately narrower than virtualKeyRequest or the config
// route: no key hash, no rate-limit config, no allowed-models list, so a
// CostViewer credential can't recon anything about a key beyond its
// spend, per docs/upgrade-research/multi-tenancy-access-control-2026-09-14.md's
// own "pair the narrow tier with an equally narrow route" design.
// PercentUsed is 0 whenever BudgetUSD is zero/unlimited (nothing to
// divide by) — never a fabricated 100% or a divide-by-zero.
// backupResponse is POST /admin/backup's response body -- the filenames
// actually written, per backupHandler's own doc comment.
type backupResponse struct {
	Files []string `json:"files"`
}

// backupHandler backs up every configured, bbolt-backed durable store to
// cfg.Admin.BackupDir, via dataplane.Pipeline.BackupStores -- see that
// method's own doc comment for the exact per-store skip/error semantics.
// Returns 501 (not registered/disabled, per this route's own doc
// comment above) when BackupDir is unset -- the common no-persistence
// case, distinct from a real backup failure (500).
func backupHandler(cfg *controlplane.Config, pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.Admin.BackupDir == "" {
			http.Error(w, "admin.backup_dir is not configured", http.StatusNotImplemented)
			return
		}
		backedUp, err := pipeline.BackupStores(cfg.Admin.BackupDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONResponse(w, backupResponse{Files: backedUp})
		logger.Info("admin_backup_completed", "files", backedUp, "authorized_by", "admin")
	}
}

// updateDeploymentWeightRequest is POST
// /admin/deployments/{name}/weight's own request body.
type updateDeploymentWeightRequest struct {
	Weight int `json:"weight"`
}

// eraseCacheEntryRequest carries the exact request-defining fields the
// ORIGINAL request used, plus the virtual key ID it was made under (no
// live Authorization header to resolve one from here) -- see
// dataplane.Pipeline.EraseCacheEntry's own doc comment for why this
// shape is required, not just convenient. adapter.ChatRequest is
// embedded anonymously so its fields (Model/Messages/Temperature/
// MaxTokens/ResponseFormat/PromptID/PromptVersion/PromptLabel) flatten
// into this same JSON object rather than nesting under a sub-key.
type eraseCacheEntryRequest struct {
	VirtualKeyID string `json:"virtual_key_id"`
	adapter.ChatRequest
}

type eraseCacheEntryResponse struct {
	L1Found bool `json:"l1_found"`
	L2Found bool `json:"l2_found"`
	// L3Skipped is always true -- named explicitly in the response
	// itself, not just a code comment, so a caller relying on this
	// endpoint for compliance purposes can't miss that L3 isn't
	// covered. Confirmed to matter in practice, not just a theoretical
	// gap: a byte-identical follow-up request for the erased content
	// CAN still be served from L3 with zero new upstream call, since
	// the original write populated all three layers -- see
	// dataplane.Pipeline.EraseCacheEntry's own doc comment.
	L3Skipped bool `json:"l3_skipped"`
}

// updateDeploymentWeightHandler live-mutates name's own routing weight,
// via dataplane.Pipeline.UpdateDeploymentWeight -- see that method's own
// doc comment for the exact in-memory-only, restart-reverts-to-config
// scope. Negative weight is rejected outright (400), mirroring
// controlplane's own identical "has a negative weight" parse-time check
// -- 0 is accepted and means "unset," per Deployment.Weight's own
// long-standing convention, not a special case introduced here. 404 if
// name doesn't match any configured deployment. logger records name and
// the new weight on every successful update.
func updateDeploymentWeightHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "" {
			http.Error(w, "deployment name is required", http.StatusBadRequest)
			return
		}

		var req updateDeploymentWeightRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Weight < 0 {
			http.Error(w, fmt.Sprintf("weight must be non-negative, got %d", req.Weight), http.StatusBadRequest)
			return
		}

		err := pipeline.UpdateDeploymentWeight(r.Context(), name, req.Weight)
		switch {
		case err == nil:
			logger.Info("admin_deployment_weight_updated", "name", name, "weight", req.Weight, "authorized_by", "admin")
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, dataplane.ErrDeploymentNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// eraseCacheEntryHandler services a real GDPR Article 17 erasure
// request against a specific cached response, via
// dataplane.Pipeline.EraseCacheEntry -- see that method's own doc
// comment for the exact scope (L1+L2 only, never L3, and the caller
// must already know the original request's own defining fields). Never
// 404s for a KNOWN virtual key with no matching cache entry -- Delete is
// an idempotent no-op on an already-absent key, so "nothing was found"
// is a real, successful 200 response (l1_found/l2_found both false),
// not an error.
//
// **Fixed, a real gap an audit found**: virtual_key_id was only checked
// for non-empty, never validated against any REAL configured virtual
// key -- unlike getVirtualKeySpendHandler's own GetVirtualKey/404
// pattern. Since virtualKeyID is purely a cache-key namespace component
// (see EraseCacheEntry's own implementation), a typo'd ID could never
// erase another tenant's real entry, but it DID silently report a
// successful 200 with l1_found/l2_found both false -- indistinguishable
// from "this key genuinely has nothing cached" from the exact same
// typo an operator has no other signal to catch. For a GDPR Article 17
// erasure request specifically, that false-negative ("we said we
// erased it, but the real target's data was never touched") is a real
// operational safety gap, not a cosmetic one. Now validated the same
// way every other admin route names/keys by virtual key ID already
// does: 404 if virtual_key_id doesn't match any configured key at all.
func eraseCacheEntryHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req eraseCacheEntryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.VirtualKeyID == "" {
			http.Error(w, "virtual_key_id is required", http.StatusBadRequest)
			return
		}
		if _, ok := pipeline.GetVirtualKey(req.VirtualKeyID); !ok {
			http.Error(w, fmt.Sprintf("virtual key %q not found", req.VirtualKeyID), http.StatusNotFound)
			return
		}

		result, err := pipeline.EraseCacheEntry(r.Context(), req.VirtualKeyID, req.ChatRequest)
		switch {
		case err == nil:
			logger.Info("admin_cache_entry_erased", "virtual_key_id", req.VirtualKeyID, "model", req.Model, "l1_found", result.L1Found, "l2_found", result.L2Found, "authorized_by", "admin")
			writeJSONResponse(w, eraseCacheEntryResponse{L1Found: result.L1Found, L2Found: result.L2Found, L3Skipped: true})
		case errors.Is(err, dataplane.ErrPromptAndMessagesBothSet),
			errors.Is(err, dataplane.ErrPromptLabelAndVersionBothSet),
			errors.Is(err, dataplane.ErrPromptResolutionFailed):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

type virtualKeySpendResponse struct {
	SpentUSD                   string  `json:"spent_usd"`
	BudgetUSD                  string  `json:"budget_usd"`
	BudgetResetIntervalSeconds int     `json:"budget_reset_interval_seconds"`
	PercentUsed                float64 `json:"percent_used"`
}

// virtualKeyListEntry is one entry in GET /admin/virtual_keys's list
// response -- deliberately NEVER includes KeyHash, mirroring
// virtualKeySpendResponse's own "safe subset, never the secret" rule.
// AllowedModels/AllowedRegions are converted from identity.VirtualKey's
// own map[string]struct{} into a sorted []string, matching
// controlplane.VirtualKeyConfig's identical existing JSON convention
// (config.go sorts these at load time too) rather than marshaling a Go
// map directly.
type virtualKeyListEntry struct {
	ID                         string   `json:"id"`
	BudgetUSD                  string   `json:"budget_usd"`
	BudgetResetIntervalSeconds int      `json:"budget_reset_interval_seconds"`
	BudgetWarnPercent          float64  `json:"budget_warn_percent"`
	AllowedModels              []string `json:"allowed_models,omitempty"`
	AllowedRegions             []string `json:"allowed_regions,omitempty"`
	RateLimitBurst             float64  `json:"rate_limit_burst,omitempty"`
	RateLimitRefill            float64  `json:"rate_limit_refill_per_second,omitempty"`
	BillingSubjectID           string   `json:"billing_subject_id,omitempty"`
}

func sortedKeysOf(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func virtualKeyToListEntry(vk identity.VirtualKey) virtualKeyListEntry {
	return virtualKeyListEntry{
		ID:                         vk.ID,
		BudgetUSD:                  vk.BudgetUSD.String(),
		BudgetResetIntervalSeconds: int(vk.BudgetResetInterval.Seconds()),
		BudgetWarnPercent:          vk.BudgetWarnPercent,
		AllowedModels:              sortedKeysOf(vk.AllowedModels),
		AllowedRegions:             sortedKeysOf(vk.AllowedRegions),
		RateLimitBurst:             vk.RateLimitBurst,
		RateLimitRefill:            vk.RateLimitRefill,
		BillingSubjectID:           vk.BillingSubjectID,
	}
}

// listVirtualKeysHandler serves every configured virtual key's safe,
// non-secret metadata -- closes a real gap an end-to-end audit found
// (docs/upgrade-research/sdk-dashboard-buildstatus-tier1-2026-09-20.md):
// every virtual-key admin route was scoped to a single already-known
// {name}, with no way to discover what keys exist at all. Same
// read tier as GET /admin/config/GET /admin/prompts (viewer-or-admin) --
// this is a config-shaped read, broader than the narrower CostViewer
// tier GET .../spend also accepts.
func listVirtualKeysHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := pipeline.ListVirtualKeys()
		entries := make([]virtualKeyListEntry, 0, len(keys))
		for _, vk := range keys {
			entries = append(entries, virtualKeyToListEntry(vk))
		}
		writeJSONResponse(w, entries)
		logger.Info("admin_virtual_keys_read", "count", len(entries), "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// getVirtualKeySpendHandler serves name's current spend against its
// budget cap -- closes the "no live cost/budget view" gap named in
// docs/upgrade-research/admin-operator-experience-2026-09-14.md Finding 4:
// today the only way to see this is a full GET /admin/config read, which
// exposes every virtual key's budget/model/rate-limit shape at once, far
// more than a cost-reporting consumer needs. 404 if name doesn't match
// any configured key. logger records this read (name + credential tier,
// never spend/budget figures) mirroring every other read route's audit
// convention.
func getVirtualKeySpendHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		vk, ok := pipeline.GetVirtualKey(name)
		if !ok {
			http.Error(w, fmt.Sprintf("virtual key %q not found", name), http.StatusNotFound)
			return
		}
		spent := pipeline.SpentUSD(vk.ID, vk.BudgetResetInterval)
		var percentUsed float64
		if vk.BudgetUSD.IsPositive() {
			percentUsed, _ = spent.Div(vk.BudgetUSD).Float64()
		}
		writeJSONResponse(w, virtualKeySpendResponse{
			SpentUSD:                   spent.String(),
			BudgetUSD:                  vk.BudgetUSD.String(),
			BudgetResetIntervalSeconds: int(vk.BudgetResetInterval.Seconds()),
			PercentUsed:                percentUsed,
		})
		logger.Info("admin_virtual_key_spend_read", "name", name, "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// promptRequest is the POST /admin/prompts/{id} request body -- unlike
// virtualKeyRequest, there is no static config.yaml section to mirror
// here (prompts are Admin-API-only, per this feature's own design), so
// this shape is simply the raw canonical message list a caller wants
// stored as the next version.
type promptRequest struct {
	Messages []adapter.Message `json:"messages"`
}

// promptResponse is what every read/write prompt route returns --
// mirrors virtualKeyRequest's own "never expose the internal package
// type directly across the HTTP boundary" convention.
type promptResponse struct {
	ID        string            `json:"id"`
	Version   int               `json:"version"`
	Messages  []adapter.Message `json:"messages"`
	CreatedAt time.Time         `json:"created_at"`
}

func promptToResponse(p prompt.Prompt) promptResponse {
	return promptResponse{ID: p.ID, Version: p.Version, Messages: p.Messages, CreatedAt: p.CreatedAt}
}

// writeJSONResponse encodes v as the response body -- shared by every
// prompt read/write route below, mirroring getConfigHandler's own
// identical encode-or-500 pattern.
func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encoding response", http.StatusInternalServerError)
	}
}

// listPromptsHandler serves the latest version of every stored prompt,
// per pipeline.ListPrompts (already sorted by ID). logger records this
// read (route + credential tier, never any prompt content) — see
// getConfigHandler's own doc comment for why every prompt-read route was
// a real, previously-silent audit-logging gap.
func listPromptsHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prompts := pipeline.ListPrompts()
		responses := make([]promptResponse, 0, len(prompts))
		for _, p := range prompts {
			responses = append(responses, promptToResponse(p))
		}
		writeJSONResponse(w, responses)
		logger.Info("admin_prompts_read", "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// getPromptHandler serves id's latest version, or 404 if id is unknown.
func getPromptHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		p, ok := pipeline.GetPrompt(id, 0)
		if !ok {
			http.Error(w, fmt.Sprintf("prompt %q not found", id), http.StatusNotFound)
			return
		}
		writeJSONResponse(w, promptToResponse(p))
		logger.Info("admin_prompts_read", "id", id, "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// getPromptVersionHandler serves one specific historical version of id,
// or 404 if id or that version is unknown, or 400 if version isn't a
// positive integer.
func getPromptVersionHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		versionStr := r.PathValue("version")
		version, convErr := strconv.Atoi(versionStr)
		if convErr != nil || version <= 0 {
			http.Error(w, fmt.Sprintf("invalid version %q: must be a positive integer", versionStr), http.StatusBadRequest)
			return
		}
		p, ok := pipeline.GetPrompt(id, version)
		if !ok {
			http.Error(w, fmt.Sprintf("prompt %q version %d not found", id, version), http.StatusNotFound)
			return
		}
		writeJSONResponse(w, promptToResponse(p))
		logger.Info("admin_prompts_read", "id", id, "version", version, "authorized_by", credentialTierFromContext(r.Context()))
	}
}

// upsertPromptHandler creates a new version of id, live -- see
// dataplane.Pipeline.UpsertPrompt/prompt.Store.Upsert's own doc comments
// for the exact version-bump rule. logger records id and the resulting
// version (never the prompt's own message content) on every successful
// upsert, mirroring upsertVirtualKeyHandler's identical "log identifiers,
// not payload content" discipline.
func upsertPromptHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "prompt id is required", http.StatusBadRequest)
			return
		}
		if err := validateAdminIdentifier(id, "prompt id"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req promptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.Messages) == 0 {
			http.Error(w, "messages is required and must be non-empty", http.StatusBadRequest)
			return
		}
		// Defense-in-depth for a round-3 backlog-audit finding: a prompt
		// template's own inline Parts[].Data/MediaType previously never
		// passed the same client-declared-MIME-type-spoof check every
		// directly-client-supplied message must already pass. Rejecting
		// it here, at write time, fails fast for a prompt author (a
		// less-trusted operator than one touching source code, per this
		// feature's own RFC) rather than only ever being caught later, at
		// every future request-time resolution of this same prompt.
		if err := adapter.ValidateContentParts(req.Messages); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A stored prompt template's Messages gets resolved into every
		// FUTURE request that references it (internal/prompt.Store.Resolve)
		// -- an excessively long template multiplies its own cost across
		// every future call, the same resource-exhaustion shape
		// adapter.ValidateMessageCount already bounds for a direct,
		// one-shot client request (cmd/gateway/main.go).
		if err := adapter.ValidateMessageCount(req.Messages); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		p, err := pipeline.UpsertPrompt(id, req.Messages)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logger.Info("admin_prompt_upserted", "id", id, "version", p.Version, "authorized_by", "admin")
		writeJSONResponse(w, promptToResponse(p))
	}
}

// deletePromptHandler removes every version of id. 404 if id doesn't
// match any stored prompt. logger records id on every successful delete.
func deletePromptHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "" {
			http.Error(w, "prompt id is required", http.StatusBadRequest)
			return
		}

		err := pipeline.DeletePrompt(id)
		switch {
		case err == nil:
			logger.Info("admin_prompt_deleted", "id", id, "authorized_by", "admin")
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, prompt.ErrPromptNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// setPromptLabelRequest is the PUT /admin/prompts/{id}/labels/{label}
// request body -- Version <= 0 means "whichever version is currently
// latest," resolved to a concrete number at call time (see
// prompt.Store.SetLabel's own doc comment).
type setPromptLabelRequest struct {
	Version int `json:"version"`
}

// labelResponse mirrors promptResponse's own "never expose the internal
// package type directly across the HTTP boundary" convention.
type labelResponse struct {
	PromptID  string    `json:"prompt_id"`
	Label     string    `json:"label"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

func labelToResponse(l prompt.Label) labelResponse {
	return labelResponse{PromptID: l.PromptID, Label: l.Name, Version: l.Version, UpdatedAt: l.UpdatedAt}
}

// setPromptLabelHandler points id's named label at the requested
// version, live -- the single primitive that serves both promote (a
// newer version) and rollback (an older one). 404 if id or the requested
// version doesn't exist. logger records id/label/version (never any
// prompt content), mirroring upsertPromptHandler's identical discipline.
func setPromptLabelHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		label := r.PathValue("label")
		if id == "" || label == "" {
			http.Error(w, "prompt id and label are both required", http.StatusBadRequest)
			return
		}
		if err := validateAdminIdentifier(label, "label"); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var req setPromptLabelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		l, err := pipeline.SetPromptLabel(id, label, req.Version)
		switch {
		case err == nil:
			logger.Info("admin_prompt_label_set", "id", id, "label", label, "version", l.Version, "authorized_by", "admin")
			writeJSONResponse(w, labelToResponse(l))
		case errors.Is(err, prompt.ErrPromptNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}
}

// deletePromptLabelHandler removes id's named label entirely -- distinct
// from setPromptLabelHandler with an older version (which keeps the
// label, just moved). 404 if id has no such label.
func deletePromptLabelHandler(pipeline *dataplane.Pipeline, logger auditLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		label := r.PathValue("label")

		err := pipeline.DeletePromptLabel(id, label)
		switch {
		case err == nil:
			logger.Info("admin_prompt_label_deleted", "id", id, "label", label, "authorized_by", "admin")
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, prompt.ErrPromptNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
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
