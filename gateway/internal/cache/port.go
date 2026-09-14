// Package cache defines Cache's public interface — the ONLY package
// Gateway's request pipeline is allowed to import from Cache, per
// gateway/ARCHITECTURE.md's package layout and dependency-direction rules.
//
// This package must never import internal/adapter or anything
// provider-specific: Cache is keyed on a normalized request, not on which
// upstream served it, per docs/decisions/0002-cache-embedded-in-gateway.md.
package cache

import (
	"context"
	"time"
)

// Cache is the sole interface Gateway's dataplane pipeline depends on.
// Concrete implementations (inprocess, and the dormant grpcserver/
// grpcclient extraction seam) live in sibling packages and are never
// referenced by their concrete type outside their own package and the
// wiring code that constructs one.
//
// Get/Put deal only in value objects (raw bytes, strings, durations) —
// never a pointer into anything outside this package — so a future
// network-adapter implementation (grpcclient) can satisfy this interface
// without leaking in-process memory across a process boundary.
type Cache interface {
	// Get looks up a previously cached response by key. ok is false and
	// err is nil for a cache miss (never-set key or expired entry) —
	// only a genuine failure (e.g. a backend error in a future network
	// adapter) returns a non-nil err. writtenAt is the time of the most
	// recent Put for key (zero value when ok is false) — added per
	// docs/upgrade-research/cache-2026-09-06.md Finding 6, closing the
	// telemetry asymmetry with LexicalCandidate.WrittenAt (L3's own
	// equivalent), so cache-hit-provenance age reporting is uniform
	// across all three layers instead of L3-only.
	Get(ctx context.Context, key string) (resp []byte, writtenAt time.Time, ok bool, err error)
	// Put stores resp under key with the given time-to-live.
	Put(ctx context.Context, key string, resp []byte, ttl time.Duration) error
	// Delete removes key immediately, if present — never-set is a no-op,
	// not an error. Added 2026-09-14 to close a real, previously-open
	// gap: without this, there was no way to service a GDPR Article 17
	// erasure request against one specific cached entry short of
	// flushing the entire cache or waiting out its TTL, even though the
	// product's own design already accepts that a message which passes
	// guardrail checks (or predates a guardrail rule) can still be
	// cached. See
	// docs/upgrade-research/ai-compliance-regulatory-readiness-2026-09-14.md
	// Finding 4. Deliberately does NOT solve "find every cached entry
	// that might belong to data subject X" — that needs a write-time
	// PII-provenance index, a real design commitment named `not_yet` in
	// that same finding, not a small addition. Delete only removes an
	// exact key the caller already knows.
	Delete(ctx context.Context, key string) error
}
