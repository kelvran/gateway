// Package credentialstate holds the domain-agnostic primitives behind
// every credential-hot-reload mechanism in this gateway: an atomically-
// swappable snapshot of resolved credential values, the *File config
// convention that names where to re-read them from disk, and the shared
// file-read helper both the initial resolution and every subsequent
// reload use identically. A pure leaf package (no internal imports) —
// see .go-arch-lint.yml — so that dataplane, bedrockguard, and embedsim
// can each depend on it directly without needing to depend on each
// other, per that file's own component graph.
//
// This package owns ONLY the primitive: an atomic holder plus a file
// read. Deciding WHEN to re-read (a ticker loop), WHAT to iterate (one
// Deployment's fields vs. a single Detector's own cfg), and HOW to
// react to a change (a structured log line naming the field, never the
// value) stays the responsibility of whichever package actually holds
// the credential — see dataplane/credential_reload.go,
// guardrail/bedrockguard/credential_reload.go, and
// guardrail/embedsim/credential_reload.go for three independent,
// intentionally-not-shared reload loops built on this one shared
// primitive.
package credentialstate

import (
	"os"
	"strings"
	"sync/atomic"
)

// Credentials is an immutable snapshot of a set of resolved credential
// values. Not every field applies to every holder: a subsystem
// authenticating via AWS SigV4 (bedrockguard.Detector,
// embedsim.BedrockEmbedder, a bedrock-provider Deployment) populates
// AccessKeyID/SecretAccessKey/SessionToken and leaves APIKey empty; a
// subsystem authenticating via a single bearer secret (a non-bedrock
// Deployment) populates APIKey only. Never logged in full — every
// caller that logs a rotation event names the field that changed, never
// its value.
type Credentials struct {
	APIKey          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Files is the set of on-disk file paths a holder may configure as its
// credential source, one-for-one with Credentials' own fields. An empty
// string means that particular credential still comes from the
// holder's own construction-time-resolved value (an env var, or a
// literal) and is never hot-reloaded.
type Files struct {
	APIKey          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// HasAny reports whether f configures at least one file-based
// credential source — the single gate every caller uses to decide
// whether to start a reload loop at all, and for that loop to skip a
// holder that didn't opt in.
func (f Files) HasAny() bool {
	return f.APIKey != "" || f.AccessKeyID != "" || f.SecretAccessKey != "" || f.SessionToken != ""
}

// NewState builds a shared, atomically-swappable credential holder,
// seeded with initial — the same values the caller already resolved
// once at construction time (an env var, or an initial file read via
// ReadFile), so the very first read behaves identically whether or not
// a rotation has happened yet.
func NewState(initial Credentials) *atomic.Pointer[Credentials] {
	state := &atomic.Pointer[Credentials]{}
	state.Store(&initial)
	return state
}

// ReadFile reads path and returns its trimmed contents — the leading/
// trailing whitespace strip matters because a Kubernetes projected
// Secret volume's file (and most operator-written rotation scripts)
// commonly ends in a trailing newline that isn't part of the actual
// credential value.
func ReadFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
