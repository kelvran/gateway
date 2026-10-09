// Package adminapi holds the request and response body types of the
// gateway's admin HTTP API — the wire shapes of every /admin/* route — as
// exported types, so that internal/admin (the server) and internal/cli
// (the kelvran command-line client, RFC-3) encode and decode exactly the
// same JSON without a second copy of the shapes.
//
// It is a leaf: stdlib, shopspring/decimal and the root adapter package
// only (adapter.ChatRequest and adapter.Message appear on the cache-erase
// and prompt bodies). Nothing here is public API in the docs/VERSIONING.md
// sense — every package under gateway/internal/ is in-module only; the
// public contract is the JSON on the wire, documented in
// docs/reference/admin-api.md and pinned byte-for-byte by
// TestWireShapesMatchGolden.
//
// Moved here from internal/admin on 2026-10-10 (RFC-3 decision 2,
// docs/rfcs/2026-10-09-gateway-kelvran-cli-and-single-user-mode.md).
// internal/admin keeps unexported aliases to these types, so its handlers
// and tests are unchanged and the JSON is byte-identical to before the
// move. Field names on the virtual-key types deliberately mirror
// config.yaml's own virtual_keys.<name> section (key_hash, budget_usd,
// budget_reset_interval_seconds, allowed_models,
// rate_limit.{burst,refill_per_second}) — an operator already familiar
// with the static config shape needs no second vocabulary for the live
// mutation API, and the CLI keeps the same names.
package adminapi

import (
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// VirtualKeyRequest is the POST /admin/virtual_keys/{name} request body.
// An upsert is a full replace, never a merge: a field omitted from the
// body is stored at its zero value.
type VirtualKeyRequest struct {
	KeyHash                    string          `json:"key_hash"`
	BudgetUSD                  decimal.Decimal `json:"budget_usd"`
	BudgetResetIntervalSeconds int             `json:"budget_reset_interval_seconds"`
	BudgetWarnPercent          float64         `json:"budget_warn_percent"`
	AllowedModels              []string        `json:"allowed_models"`
	// AllowedRegions restricts this key to deployments in a subset of
	// regions (Deployment.Region), per
	// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md
	// — mirrors AllowedModels's own convention exactly.
	AllowedRegions []string `json:"allowed_regions"`
	// AllowedSourceCIDRs restricts this key to requests whose resolved
	// client source IP falls within at least one of these CIDR blocks —
	// mirrors AllowedModels/AllowedRegions' own convention exactly. See
	// identity.VirtualKey.AllowedSourceCIDRs' own doc comment.
	AllowedSourceCIDRs []string `json:"allowed_source_cidrs"`
	// CacheScopeToEndUser mirrors identity.VirtualKey.CacheScopeToEndUser's
	// own doc comment exactly.
	CacheScopeToEndUser bool `json:"cache_scope_to_end_user"`
	// AttributionIDsDisabled is the per-key identifier-capture opt-out
	// (13a); omitted means capture ON, which is why the wire field is the
	// negative `attribution_capture_ids_disabled`.
	AttributionIDsDisabled bool              `json:"attribution_capture_ids_disabled"`
	RateLimit              *RateLimitRequest `json:"rate_limit"`
}

// RotateVirtualKeyRequest is the POST /admin/virtual_keys/{name}/rotate
// request body, per docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 1. GracePeriodSeconds <= 0 rotates with no grace period at all
// -- the old secret stops working immediately, identical in effect to a
// delete-then-recreate but atomic and without the intervening window
// where the key doesn't exist at all.
type RotateVirtualKeyRequest struct {
	NewKeyHash         string `json:"new_key_hash"`
	GracePeriodSeconds int    `json:"grace_period_seconds"`
}

// RateLimitRequest is VirtualKeyRequest's rate_limit section.
type RateLimitRequest struct {
	Burst              float64 `json:"burst"`
	RefillPerSecond    float64 `json:"refill_per_second"`
	TPMCapacity        float64 `json:"tpm_capacity"`
	TPMRefillPerSecond float64 `json:"tpm_refill_per_second"`
	// PerModel mirrors config.yaml's rate_limit.per_model section — see
	// controlplane.VirtualKeyConfig.PerModelRateLimits' doc comment and
	// docs/rfcs/2026-09-07-gateway-multi-dimensional-rate-limits.md. An
	// upsert is a full replace of this key's rate-limit configuration,
	// never a partial merge — like every other field on the request
	// struct — so omitting per_model on an update to an already-overridden
	// key clears its overrides, exactly as omitting rate_limit entirely
	// resets burst/refill to zero before the upsert handler's own
	// ratelimit.ResolveKeyRateLimit call resolves that zero pair to
	// ratelimit.DefaultKeyBurstCapacity/DefaultKeyRefillPerSecond.
	PerModel map[string]PerModelRateLimitRequest `json:"per_model"`
}

// PerModelRateLimitRequest is one model's per-model RPM override within a
// RateLimitRequest. Burst/RefillPerSecond are both required and must be
// positive — see the upsert handler's validation, mirroring
// controlplane.parsePerModelRateLimits' identical rule for the static
// config file, so an operator gets the same validation regardless of
// which of the two surfaces they use. TPMCapacity/TPMRefillPerSecond are
// the optional per-model TPM override (must be set together or neither),
// mirroring controlplane.ModelRateLimitConfig's identical fields.
type PerModelRateLimitRequest struct {
	Burst              float64 `json:"burst"`
	RefillPerSecond    float64 `json:"refill_per_second"`
	TPMCapacity        float64 `json:"tpm_capacity"`
	TPMRefillPerSecond float64 `json:"tpm_refill_per_second"`
}

// AuditEntryResponse is one element of GET /admin/audit's response array
// -- a direct field-for-field mirror of auditstore.Entry, kept as its own
// type rather than exposing that package's type on the wire, matching the
// admin package's convention of a dedicated response shape per route.
type AuditEntryResponse struct {
	Time   time.Time         `json:"time"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// BackupResponse is POST /admin/backup's response body -- the filenames
// actually written.
type BackupResponse struct {
	Files []string `json:"files"`
}

// UpdateDeploymentWeightRequest is POST /admin/deployments/{name}/weight's
// request body.
type UpdateDeploymentWeightRequest struct {
	Weight int `json:"weight"`
}

// EraseCacheEntryRequest carries the exact request-defining fields the
// ORIGINAL request used, plus the virtual key ID it was made under (no
// live Authorization header to resolve one from here) -- see
// dataplane.Pipeline.EraseCacheEntry's own doc comment for why this
// shape is required, not just convenient. adapter.ChatRequest is
// embedded anonymously so its fields (Model/Messages/Temperature/
// MaxTokens/ResponseFormat/PromptID/PromptVersion/PromptLabel) flatten
// into this same JSON object rather than nesting under a sub-key.
type EraseCacheEntryRequest struct {
	VirtualKeyID string `json:"virtual_key_id"`
	// EndUserID targets the exact end-user-scoped entry a request with
	// CacheScopeToEndUser enabled and this same header value would have
	// been cached under -- see dataplane.Pipeline.EraseCacheEntry's own
	// doc comment. Empty (the default) targets the tenant-only-scoped
	// entry, correct for every virtual key that never enabled that flag.
	EndUserID string `json:"end_user_id"`
	adapter.ChatRequest
}

// EraseCacheEntryResponse is POST /admin/cache/erase's response body.
type EraseCacheEntryResponse struct {
	L1Found bool `json:"l1_found"`
	L2Found bool `json:"l2_found"`
	// L3Skipped is always true -- named explicitly in the response
	// itself, not just a code comment, so a caller relying on this
	// endpoint for compliance purposes can't miss that L3 isn't
	// touched. See dataplane.Pipeline.EraseCacheEntry's own doc comment.
	L3Skipped bool `json:"l3_skipped"`
}

// VirtualKeySpendResponse is GET /admin/virtual_keys/{name}/spend's body.
// PercentUsed is 0 whenever BudgetUSD is zero/unlimited (nothing to
// divide by) — never a fabricated 100% or a divide-by-zero.
type VirtualKeySpendResponse struct {
	SpentUSD                   string  `json:"spent_usd"`
	BudgetUSD                  string  `json:"budget_usd"`
	BudgetResetIntervalSeconds int     `json:"budget_reset_interval_seconds"`
	PercentUsed                float64 `json:"percent_used"`
}

// VirtualKeyListEntry is one entry in GET /admin/virtual_keys's list
// response -- deliberately NEVER includes KeyHash, mirroring
// VirtualKeySpendResponse's own "safe subset, never the secret" rule.
// AllowedModels/AllowedRegions are converted from identity.VirtualKey's
// own map[string]struct{} into a sorted []string, matching
// controlplane.VirtualKeyConfig's identical existing JSON convention
// (config.go sorts these at load time too) rather than marshaling a Go
// map directly.
type VirtualKeyListEntry struct {
	ID                         string   `json:"id"`
	BudgetUSD                  string   `json:"budget_usd"`
	BudgetResetIntervalSeconds int      `json:"budget_reset_interval_seconds"`
	BudgetWarnPercent          float64  `json:"budget_warn_percent"`
	AllowedModels              []string `json:"allowed_models,omitempty"`
	AllowedRegions             []string `json:"allowed_regions,omitempty"`
	AllowedSourceCIDRs         []string `json:"allowed_source_cidrs,omitempty"`
	CacheScopeToEndUser        bool     `json:"cache_scope_to_end_user,omitempty"`
	AttributionIDsDisabled     bool     `json:"attribution_capture_ids_disabled,omitempty"`
	RateLimitBurst             float64  `json:"rate_limit_burst,omitempty"`
	RateLimitRefill            float64  `json:"rate_limit_refill_per_second,omitempty"`
	BillingSubjectID           string   `json:"billing_subject_id,omitempty"`
}

// VirtualKeyInFlightResponse is GET /admin/virtual_keys/{name}/inflight's
// body: the key's current in-flight load broken down by agent_run_id. The
// value is observability-only and is never consulted by any admission or
// throttling decision — see ratelimit.ConcurrencyLimiter's package doc.
type VirtualKeyInFlightResponse struct {
	TotalInFlight int            `json:"total_in_flight"`
	ByAgentRunID  map[string]int `json:"by_agent_run_id"`
}

// PromptRequest is the POST /admin/prompts/{id} request body: the raw
// canonical message list a caller wants stored as the next version. Unlike
// VirtualKeyRequest there is no static config.yaml section to mirror
// (prompts are Admin-API-only).
type PromptRequest struct {
	Messages []adapter.Message `json:"messages"`
}

// PromptResponse is what every read/write prompt route returns.
type PromptResponse struct {
	ID        string            `json:"id"`
	Version   int               `json:"version"`
	Messages  []adapter.Message `json:"messages"`
	CreatedAt time.Time         `json:"created_at"`
}

// SetPromptLabelRequest is the PUT /admin/prompts/{id}/labels/{label}
// request body -- Version <= 0 means "whichever version is currently
// latest," resolved to a concrete number at call time (see
// prompt.Store.SetLabel's own doc comment).
type SetPromptLabelRequest struct {
	Version int `json:"version"`
}

// LabelResponse is the PUT /admin/prompts/{id}/labels/{label} response.
type LabelResponse struct {
	PromptID  string    `json:"prompt_id"`
	Label     string    `json:"label"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}
