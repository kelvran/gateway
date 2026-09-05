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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	// AllowedModels restricts this key to a subset of configured models.
	// Empty or nil means every configured model is allowed.
	AllowedModels map[string]struct{}
	// RateLimitBurst and RateLimitRefill configure this key's own
	// token-bucket rate limiter (see internal/ratelimit.TokenBucket).
	RateLimitBurst  float64
	RateLimitRefill float64
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
	return key, nil
}
