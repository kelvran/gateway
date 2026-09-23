// Package identity resolves incoming requests' bearer tokens against a set
// of statically-configured virtual keys.
//
// Per docs/rfcs/2026-09-02-virtual-keys-budgets.md, virtual keys are
// identified by the SHA-256 hash of the actual secret, not an environment
// variable name: unlike a provider API key (a third party's credential
// Kelvran must protect on someone else's behalf), a virtual key is a
// credential Kelvran itself issues, so the config only ever needs to
// verify a presented token matches one it issued — it never needs to
// recover the raw secret from config at all.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const bearerPrefix = "Bearer "

// ErrMissingHeader is returned when the Authorization header is absent or
// doesn't use the expected "Bearer <key>" scheme.
var ErrMissingHeader = errors.New("identity: missing or malformed Authorization header")

// ErrInvalidKey is returned when the presented key doesn't match any
// configured virtual key.
var ErrInvalidKey = errors.New("identity: invalid virtual key")

// ErrDuplicateKeyHash is returned by NewVerifier when two configured
// virtual keys hash to the same value — a config error, not a runtime one.
var ErrDuplicateKeyHash = errors.New("identity: duplicate virtual key hash in config")

// VirtualKey is one statically-configured tenant: its identity, its
// spending cap, its optional model allow-list, and its own rate-limit
// knobs. See docs/rfcs/2026-09-02-virtual-keys-budgets.md for the full
// design and gateway/ARCHITECTURE.md's Data Model note this implements.
type VirtualKey struct {
	// ID uniquely identifies this key within the config file — never the
	// secret itself. Used as the map key for per-key rate limiting
	// (internal/gateway/dataplane) and per-key budget tracking
	// (internal/budget), and as the tenant dimension in cache keys
	// (internal/cache.Key).
	ID string
	// KeyHash is the hex-encoded SHA-256 digest of the actual secret
	// bearer token, from config. Never the raw secret.
	KeyHash string
	// BudgetUSD is this key's cumulative spending cap. Zero (or negative)
	// means unlimited. Decimal, not float64, per
	// docs/rfcs/2026-09-02-decimal-cost-accounting.md.
	BudgetUSD decimal.Decimal
	// BudgetResetInterval, when positive, makes BudgetUSD a rolling window
	// (e.g. 30*24*time.Hour for a "monthly" budget) rather than a
	// lifetime-of-the-process cap — see internal/budget.Tracker's own
	// resetIfNeeded. Zero (the default) preserves the original,
	// never-resets behavior exactly.
	BudgetResetInterval time.Duration
	// BudgetWarnPercent, when positive, is the fraction of BudgetUSD (e.g.
	// 0.8 for 80%) at which dataplane logs a budget_warn_threshold_crossed
	// warning on every billable completion while spend remains at or
	// above it — log-only, per docs/rfcs/2026-09-05-gateway-budget-warn-
	// threshold.md: no new API surface, the request is never rejected or
	// altered. Expressed as a percentage of BudgetUSD rather than a
	// second absolute USD value so it stays proportional automatically if
	// BudgetUSD is ever changed — an absolute threshold would silently
	// drift out of sync with the cap it's meant to warn about. Zero (the
	// default) disables the warning entirely; meaningless when BudgetUSD
	// itself is zero/unlimited (nothing to warn a percentage of).
	BudgetWarnPercent float64
	// AllowedModels restricts this key to a subset of configured models.
	// Empty or nil means every configured model is allowed.
	AllowedModels map[string]struct{}
	// AllowedRegions restricts this key to deployments whose own
	// Deployment.Region is in this set — a data-residency constraint, per
	// docs/upgrade-research/data-residency-regional-routing-2026-09-15.md's
	// confirmed finding that a same-model fallback could otherwise silently
	// cross a deployment's region boundary with no override available.
	// Empty or nil means no constraint — mirrors AllowedModels's own
	// "empty means unrestricted" convention exactly. When a constraint IS
	// set, a candidate deployment whose own Region is "" (true for every
	// non-Bedrock provider today — Region is documented as required only
	// for Bedrock) does NOT satisfy it: this fails closed deliberately,
	// since there is no way to positively prove a non-Bedrock deployment's
	// residency, and treating an unknown region as automatically compliant
	// would silently defeat the exact guarantee this field exists to
	// provide. See dataplane.isRegionAllowed for the enforcement logic.
	AllowedRegions map[string]struct{}
	// AllowedSourceCIDRs restricts this key to requests whose resolved
	// client source IP falls within at least one of these CIDR blocks.
	// Empty or nil means no constraint, mirroring AllowedModels's/
	// AllowedRegions's own "empty means unrestricted" convention exactly.
	// Pre-parsed into *net.IPNet (not raw strings) at construction time,
	// the same reason AllowedModels/AllowedRegions are pre-resolved into
	// sets rather than re-parsed per request. Checked ONCE, at request-
	// auth resolution (dataplane.HandleChatCompletion/HandleEmbeddings/
	// HandleChatCompletionStream, immediately after Verify succeeds) --
	// unlike AllowedModels/AllowedRegions, source IP doesn't vary across
	// the dataplane's several fallback-candidate call sites, so there is
	// no need to duplicate this check at each of them. Resolved from the
	// real TCP peer address (http.Request.RemoteAddr) by default -- a
	// client-supplied header (X-Forwarded-For) is deliberately NOT
	// trusted here, since trusting a spoofable header by default would
	// make the allowlist itself spoofable. See dataplane.isSourceIPAllowed
	// for the enforcement logic.
	AllowedSourceCIDRs []*net.IPNet
	// CacheScopeToEndUser, when true, folds the caller-supplied
	// X-Kelvran-End-User-Id request header into this key's own L1/L2
	// response-cache partitioning (cache.Key/NormalizedKey's endUserID
	// parameter, and cache.ScopeKey for Cache.Get/Put/Delete's own
	// tenantID parameter) — closing the cross-user cache-sharing gap this
	// session's response-cache-compliance-risk research found: without
	// it, a response generated for one end user behind this virtual key
	// can be served verbatim to a DIFFERENT end user behind the same key,
	// empirically demonstrated by real 2026 cross-tenant cache attacks.
	// False (the default, and every virtual key configured before this
	// field existed) is a silent no-op — byte-for-byte unchanged cache
	// behavior, since existing tenants' cache hit-rate/cost economics
	// must not change without an explicit decision to enable this.
	//
	// The header itself is caller-supplied and unauthenticated — the
	// same trust class as the existing AgentRunId field (a correlation
	// signal, not a security credential); Kelvran does not authenticate
	// individual end users today, only virtual keys. When true but the
	// header is ABSENT on a given request, dataplane fails closed: that
	// entry gets its own request-unique scope (never shared with any
	// other request, past or future) rather than silently falling back
	// to tenant-only scoping, which would defeat the whole point of
	// enabling this flag.
	CacheScopeToEndUser bool
	// RateLimitBurst and RateLimitRefill configure this key's own
	// token-bucket rate limiter (see internal/ratelimit.TokenBucket).
	RateLimitBurst  float64
	RateLimitRefill float64
	// MaxConcurrentRequests bounds how many of this key's requests may be
	// simultaneously in flight, per
	// docs/rfcs/2026-09-07-gateway-retry-storm-mitigation.md's design
	// (b). <= 0 means unlimited. Mirrors RateLimitBurst/RateLimitRefill
	// above: kept here for the same documentation/consistency reason
	// those two fields are (checkConcurrency itself only ever reads
	// vk.ID, resolving the actual cap from the separately-constructed
	// ratelimit.ConcurrencyLimiter, exactly like checkRateLimit already
	// does for RateLimitBurst/RateLimitRefill via ratelimit.KeyLimiter).
	MaxConcurrentRequests int
	// PreviousKeyHash and PreviousKeyHashExpiresAt back grace-period key
	// rotation, per docs/upgrade-research/admin-operator-experience-2026-09-14.md
	// Finding 1: rotation was previously a manual delete-then-recreate
	// with no continuity, breaking any in-flight caller using the old
	// secret. When PreviousKeyHash is non-empty, NewVerifier indexes it
	// alongside KeyHash so BOTH hashes resolve to this same VirtualKey —
	// Verify additionally requires time.Now().Before(PreviousKeyHashExpiresAt)
	// for a match via PreviousKeyHash specifically, so an expired old
	// hash simply fails closed once the grace period elapses, with no
	// separate cleanup sweep needed. Only ONE generation back is ever
	// tracked — a second rotation while one is already pending overwrites
	// this pair, dropping the now-doubly-old hash — a deliberate limit,
	// not a bug: see dataplane.Pipeline.RotateVirtualKey's own doc
	// comment.
	PreviousKeyHash          string
	PreviousKeyHashExpiresAt time.Time
	// BillingSubjectID is an opaque, operator-supplied external billing
	// identifier, per docs/upgrade-research/billing-monetization-
	// integration-2026-09-15.md — never read by any enforcement path in
	// this codebase (budget.Reserve/Reconcile never touch it); purely
	// metadata threaded onto GatewayDecisionEvent for a future export
	// consumer (a billing platform's usage-ingestion API) to key off of.
	// Deliberately vendor-neutral, not named after any one platform's
	// own vocabulary (Stripe's customer_id, Metronome's external_id,
	// etc.) — every surveyed vendor names this differently, and no
	// vendor has been chosen yet. Empty (the default) means this key has
	// no known billing subject — every key configured before this field
	// existed.
	BillingSubjectID string
}

// Store persists admin-API-mutated virtual keys durably across process
// restarts, per docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 2 — mirrors budget.Store's own shape and optionality. Without
// one configured, an admin-created or -rotated virtual key reverts to
// whatever config.yaml declares on the next restart, exactly as before
// this feature existed. See internal/identity/boltstore (single-process)
// and internal/identity/redisstore (shared across replicas) for the two
// real implementations.
//
// Store alone answers "what does a freshly (re)started replica load,"
// never "how does an already-running replica learn about another
// instance's live admin mutation" — even with a shared redisstore, two
// replicas each holding their own already-built *Verifier never
// re-consult Store mid-flight; Verify resolves purely against that
// LOCAL, in-memory structure. dataplane.Pipeline's own
// internal/configpropagation wiring (TypeVirtualKeyUpsert/
// TypeVirtualKeyDelete) is the separate, complementary mechanism that
// closes THAT gap — without it, a stale replica would keep
// authenticating a revoked/rotated credential, or keep rejecting a
// brand-new one, until its own restart.
//
// Deliberately scoped to VirtualKey alone, not the paired
// ratelimit.KeyConfig a live Upsert/Rotate call also carries — identity
// stays a dependency-free leaf (no internal/ratelimit import), matching
// gateway/ARCHITECTURE.md's dependency rules. A hydrated key's
// RateLimitBurst/RateLimitRefill (both real VirtualKey fields) ARE
// restored; any PerModel/TPM override the same key might have had via
// config.yaml's own rate_limit.per_model section is NOT, since that shape
// lives only in ratelimit.KeyConfig — a real, disclosed v1 scope limit,
// not an oversight. An operator relying on per-model/TPM overrides for an
// admin-mutated key must re-apply them via another Upsert after a
// restart.
type Store interface {
	Load(ctx context.Context) (map[string]VirtualKey, error)
	Save(ctx context.Context, vk VirtualKey) error
	Delete(ctx context.Context, id string) error
	Close() error
}

// Verifier resolves a presented bearer token against the set of configured
// virtual keys, matched by the SHA-256 hash of the presented token — an
// O(1) map lookup, not a linear scan, since a cryptographic hash's output
// is already uniformly distributed and unrelated to prefix-matching the
// original token; there is nothing for a timing side-channel to reveal
// beyond "was this exact 32-byte digest present," which leaks nothing
// about the raw secret itself.
type Verifier struct {
	keys map[string]*VirtualKey // hex key hash -> resolved VirtualKey
}

// NewVerifier constructs a Verifier for the given virtual keys, which must
// be loaded by the caller from configuration — never hardcoded in source,
// per AGENTS.md's "Never store secrets in a committed file" rule (though
// per this package's own doc comment, KeyHash is not itself a secret).
func NewVerifier(keys []VirtualKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("identity: NewVerifier: at least one virtual key is required")
	}

	byHash := make(map[string]*VirtualKey, len(keys))
	seenID := make(map[string]bool, len(keys))
	for i := range keys {
		k := keys[i]
		if k.ID == "" {
			return nil, fmt.Errorf("identity: NewVerifier: virtual key at index %d has an empty ID", i)
		}
		if seenID[k.ID] {
			return nil, fmt.Errorf("identity: NewVerifier: duplicate virtual key ID %q", k.ID)
		}
		seenID[k.ID] = true

		normalizedHash, err := normalizeKeyHash(k.KeyHash)
		if err != nil {
			return nil, fmt.Errorf("identity: NewVerifier: virtual key %q: %w", k.ID, err)
		}
		k.KeyHash = normalizedHash
		if _, exists := byHash[normalizedHash]; exists {
			return nil, fmt.Errorf("%w: virtual key %q", ErrDuplicateKeyHash, k.ID)
		}

		// PreviousKeyHash, when set, indexes to this SAME VirtualKey
		// alongside KeyHash — see that field's own doc comment. Normalized
		// and duplicate-checked identically to KeyHash, since it's a real,
		// still-potentially-valid credential for the remainder of its
		// grace period, not inert metadata.
		if k.PreviousKeyHash != "" {
			normalizedPrevHash, err := normalizeKeyHash(k.PreviousKeyHash)
			if err != nil {
				return nil, fmt.Errorf("identity: NewVerifier: virtual key %q: previous_key_hash: %w", k.ID, err)
			}
			k.PreviousKeyHash = normalizedPrevHash
			if _, exists := byHash[normalizedPrevHash]; exists {
				return nil, fmt.Errorf("%w: virtual key %q (previous_key_hash)", ErrDuplicateKeyHash, k.ID)
			}
			byHash[normalizedPrevHash] = &k
		}

		byHash[normalizedHash] = &k
	}

	return &Verifier{keys: byHash}, nil
}

// normalizeKeyHash validates that hash is a well-formed hex-encoded
// SHA-256 digest (64 hex chars) and returns it lower-cased, so hash
// comparisons never depend on the config file's own casing.
func normalizeKeyHash(hash string) (string, error) {
	decoded, err := hex.DecodeString(hash)
	if err != nil {
		return "", fmt.Errorf("key_hash %q is not valid hex: %w", hash, err)
	}
	if len(decoded) != sha256.Size {
		return "", fmt.Errorf("key_hash %q must decode to %d bytes (a SHA-256 digest), got %d", hash, sha256.Size, len(decoded))
	}
	return strings.ToLower(hash), nil
}

// Verify checks the raw value of an incoming Authorization header against
// the configured virtual keys and returns the one that matches.
//
// Lookup is a direct map hit on the presented token's own SHA-256 digest,
// not a loop over every configured key — a cryptographic hash's output is
// already uniformly distributed and unrelated to prefix-matching the
// original token, so there is nothing for a timing side-channel to reveal
// beyond "was this exact digest present," and computing that digest
// itself takes the same time regardless of how many keys are configured
// or which one (if any) matches.
func (v *Verifier) Verify(authorizationHeader string) (*VirtualKey, error) {
	if authorizationHeader == "" || !strings.HasPrefix(authorizationHeader, bearerPrefix) {
		return nil, ErrMissingHeader
	}
	presented := strings.TrimPrefix(authorizationHeader, bearerPrefix)

	sum := sha256.Sum256([]byte(presented))
	presentedHash := hex.EncodeToString(sum[:])

	key, ok := v.keys[presentedHash]
	if !ok {
		return nil, ErrInvalidKey
	}
	// A match via PreviousKeyHash specifically (never via the current
	// KeyHash) is only valid for the remainder of its grace period — see
	// VirtualKey.PreviousKeyHash's own doc comment. presentedHash equals
	// exactly one of key.KeyHash/key.PreviousKeyHash here, since that's
	// the only way v.keys[presentedHash] could have resolved to key at
	// all (both are normalized identically to presentedHash's own
	// hex.EncodeToString(sha256) shape by NewVerifier).
	if presentedHash == key.PreviousKeyHash && !time.Now().Before(key.PreviousKeyHashExpiresAt) {
		return nil, ErrInvalidKey
	}
	return key, nil
}

// Keys returns every VirtualKey this Verifier was constructed with, in no
// particular order. Safe to call concurrently — a Verifier's own map is
// built once by NewVerifier and never mutated afterward, so reads here
// never race with anything (including a caller that goes on to build a
// brand-new Verifier from a modified copy of this slice, per
// docs/rfcs/2026-09-05-gateway-admin-api.md's live virtual-key mutation).
func (v *Verifier) Keys() []VirtualKey {
	// A key mid-rotation (PreviousKeyHash set) occupies TWO entries of
	// v.keys — one for KeyHash, one for PreviousKeyHash — both pointing to
	// the SAME *VirtualKey (see that field's own doc comment). Deduping
	// by pointer identity here is required, not defensive: without it,
	// every caller that rebuilds a fresh key list from Keys() (Upsert/
	// Delete/RotateVirtualKey in dataplane.go) would silently double that
	// key's entry, which identity.NewVerifier then rejects outright as a
	// duplicate ID.
	keys := make([]VirtualKey, 0, len(v.keys))
	seen := make(map[*VirtualKey]bool, len(v.keys))
	for _, k := range v.keys {
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, *k)
	}
	return keys
}
