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
	// adapter) returns a non-nil err.
	Get(ctx context.Context, key string) (resp []byte, ok bool, err error)
	// Put stores resp under key with the given time-to-live.
	Put(ctx context.Context, key string, resp []byte, ttl time.Duration) error
}
