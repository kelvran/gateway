// Package identity resolves and validates the single configured virtual
// key for this scaffolding pass.
//
// This is real, working code — not a stub — but it is intentionally far
// short of gateway/ARCHITECTURE.md's Data Model (virtual keys resolving
// to team/workspace/budget/rpm-tpm/allowed-model records). That full
// model is Phase 1 work; this pass implements exactly one configured key,
// single-tenant, checked in constant time so a timing side-channel can't
// be used to brute-force it byte by byte.
package identity

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
)

const bearerPrefix = "Bearer "

// ErrMissingHeader is returned when the Authorization header is absent or
// doesn't use the expected "Bearer <key>" scheme.
var ErrMissingHeader = errors.New("identity: missing or malformed Authorization header")

// ErrInvalidKey is returned when the presented key doesn't match the
// configured virtual key.
var ErrInvalidKey = errors.New("identity: invalid virtual key")

// Verifier checks the single configured virtual key against incoming
// requests' Authorization headers.
type Verifier struct {
	key string
}

// NewVerifier constructs a Verifier for the given key, which must be
// loaded by the caller from configuration/environment — never hardcoded
// in source, per AGENTS.md's "Never store secrets in a committed file"
// rule.
func NewVerifier(key string) (*Verifier, error) {
	if key == "" {
		return nil, fmt.Errorf("identity: NewVerifier: key must not be empty")
	}
	return &Verifier{key: key}, nil
}

// Verify checks the raw value of an incoming Authorization header against
// the configured virtual key using a constant-time comparison
// (crypto/subtle.ConstantTimeCompare), so response timing can't be used
// to infer how many leading bytes of a guessed key were correct.
func (v *Verifier) Verify(authorizationHeader string) error {
	if authorizationHeader == "" || !strings.HasPrefix(authorizationHeader, bearerPrefix) {
		return ErrMissingHeader
	}
	presented := strings.TrimPrefix(authorizationHeader, bearerPrefix)

	if subtle.ConstantTimeCompare([]byte(presented), []byte(v.key)) != 1 {
		return ErrInvalidKey
	}
	return nil
}
