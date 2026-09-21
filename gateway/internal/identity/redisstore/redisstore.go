// Package redisstore implements identity.Store over Redis, giving
// admin-API-created/rotated virtual keys durable persistence that is
// genuinely shared across every gateway replica pointed at the same
// Redis instance — unlike internal/identity/boltstore, which takes an
// EXCLUSIVE file lock for the lifetime of the process, and is exactly
// why deploy/k8s/base/deployment.yaml is pinned to replicas: 1 +
// strategy: Recreate today.
//
// Unlike internal/budget/redisbudget, this package needs no atomic Lua
// scripting: identity has no per-request enforcement decision that must
// run INSIDE Redis for cross-replica correctness (the Verify hot path is
// a pure read against a LOCAL, already-resolved *identity.Verifier, per
// identity.go's own doc comment) — Store here is pure at-rest
// persistence, restart/cold-start durability only. Real-time cross-
// replica convergence of a LIVE admin mutation (Upsert/Delete/Rotate)
// while every replica keeps running is a SEPARATE concern this package
// does not address at all, closed instead by dataplane.Pipeline's own
// configpropagation wiring (pub/sub) — the two mechanisms are
// complementary, not redundant: this package answers "what does a
// freshly (re)started replica load," configpropagation answers "how
// does an already-running replica learn about another instance's live
// mutation without restarting."
//
// One Redis Hash holds every virtual key (field = ID, value = its exact
// JSON encoding) — mirrors boltstore's own "store the exact JSON
// encoding, never a hand-rolled field-by-field layout" convention
// exactly, for the identical reason: no field-by-field schema to keep in
// sync as identity.VirtualKey itself gains fields over time.
//
// Unlike internal/budget/redisbudget/internal/ratelimit/redislimiter,
// this package DOES import internal/identity directly — identity.Store's
// own interface requires the concrete VirtualKey type in its signature,
// so there is no primitive-only shape to satisfy structurally without
// it; mirrors internal/identity/boltstore's identical real import for
// the identical reason.
package redisstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/identity"
)

// hashKey is the single Redis key (a Hash) every virtual key lives
// under, field-keyed by virtual key ID.
const hashKey = "identity:virtual_keys"

// Store is a Redis-backed implementation of identity.Store. The zero
// value is not usable — construct with Open.
type Store struct {
	client *redis.Client
}

// Open constructs a Store against the Redis server described by opts
// (go-redis's own canonical connection-options type — see
// internal/ratelimit/redislimiter.Open's identical doc comment for why
// AUTH/TLS support lives here as the full redis.Options, not a bare
// addr string). go-redis dials lazily, mirroring
// internal/budget/redisbudget.Open's identical "never fails on an
// unreachable addr" contract.
func Open(opts redis.Options) (*Store, error) {
	return &Store{client: redis.NewClient(&opts)}, nil
}

// Load implements identity.Store, decoding every field in the shared
// Hash. A single corrupt entry's decode failure is returned (never
// silently skipped) — mirrors boltstore.Store.Load's identical
// fail-loud convention, since a partially-loaded key set on startup is
// a worse failure mode than a clearly-reported one.
func (s *Store) Load(ctx context.Context) (map[string]identity.VirtualKey, error) {
	raw, err := s.client.HGetAll(ctx, hashKey).Result()
	if err != nil {
		return nil, fmt.Errorf("redisstore: loading virtual keys: %w", err)
	}
	result := make(map[string]identity.VirtualKey, len(raw))
	for id, encoded := range raw {
		var vk identity.VirtualKey
		if err := json.Unmarshal([]byte(encoded), &vk); err != nil {
			return nil, fmt.Errorf("redisstore: virtual key %q has a corrupt stored value: %w", id, err)
		}
		result[id] = vk
	}
	return result, nil
}

// Save implements identity.Store, upserting vk's exact JSON encoding
// into the shared Hash, keyed by its own ID.
func (s *Store) Save(ctx context.Context, vk identity.VirtualKey) error {
	encoded, err := json.Marshal(vk)
	if err != nil {
		return fmt.Errorf("redisstore: encoding virtual key %q: %w", vk.ID, err)
	}
	if err := s.client.HSet(ctx, hashKey, vk.ID, encoded).Err(); err != nil {
		return fmt.Errorf("redisstore: saving virtual key %q: %w", vk.ID, err)
	}
	return nil
}

// Delete implements identity.Store, removing id's field from the shared
// Hash — a no-op, not an error, if id was never persisted (mirrors
// redis.Client.HDel's own documented behavior, and boltstore.Store.Delete's
// identical convention).
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := s.client.HDel(ctx, hashKey, id).Err(); err != nil {
		return fmt.Errorf("redisstore: deleting virtual key %q: %w", id, err)
	}
	return nil
}

// Close closes the underlying Redis client.
func (s *Store) Close() error {
	return s.client.Close()
}
