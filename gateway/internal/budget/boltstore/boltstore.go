// Package boltstore implements budget.Store over go.etcd.io/bbolt, giving
// internal/budget.Tracker restart-durable persistence, per
// docs/rfcs/2026-09-03-budget-persistence.md.
//
// Each key's full budget.State (spend plus rolling-window bookkeeping) is
// JSON-encoded — decimal.Decimal has its own MarshalJSON/UnmarshalJSON
// that round-trips through its exact decimal string form, never a
// float64, preserving the same precision-preservation discipline as
// docs/rfcs/2026-09-02-decimal-cost-accounting.md's YAML-parser fix and
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md's string-typed cost
// attribute: the boundary between Kelvran's money type and any external
// representation must never round-trip through anything but decimal
// text, JSON-framed or not.
//
// Load transparently reads a pre-existing bucket entry written before
// this file understood budget.State (a bare spent.String() byte string,
// with no JSON object framing at all) — see Load's own comment. This
// makes the schema widening lazy and automatic: no separate migration
// tool, no downtime, no operator action required on an existing
// deployment. The very next Save for that key rewrites it in the new
// format.
package boltstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/budget"
)

// bucketName is the single bbolt bucket this store uses: keyID -> a
// JSON-encoded budget.State (or, for a not-yet-migrated legacy entry, a
// bare decimal string — see Load).
const bucketName = "spend"

// Store is a bbolt-backed budget.Store. The zero value is not usable;
// construct with Open.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if absent) the bbolt file at path and ensures the
// spend bucket exists. bbolt takes an exclusive lock on the file for the
// lifetime of the returned Store — a second process opening the same path
// fails clearly rather than silently corrupting the file, per the RFC's
// explicit single-instance-only scope.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("boltstore: opening %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("boltstore: creating bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying bbolt file handle (and its exclusive
// lock).
func (s *Store) Close() error {
	return s.db.Close()
}

// Load implements budget.Store. ctx is accepted for interface symmetry
// with a future networked Store implementation, but unused here — a
// bbolt transaction is synchronous and fast enough that there is no real
// mid-transaction cancellation point to honor.
//
// Each entry is tried as JSON-encoded budget.State first. A bucket entry
// written before this file understood State — a bare decimal string with
// no JSON object framing — reliably fails that unmarshal (a bare JSON
// number literal can't decode into a struct), so on that specific failure
// this falls back to decimal.NewFromString and treats it as a legacy
// entry: budget.State{Spent: parsed}, with PeriodStart left at its zero
// value — exactly this store's pre-migration behavior for that one key.
// See budget.NewTrackerWithStore's own doc comment for how a zero
// PeriodStart is interpreted. Only if BOTH decodes fail is this a
// genuinely corrupt value.
func (s *Store) Load(_ context.Context) (map[string]budget.State, error) {
	result := map[string]budget.State{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.ForEach(func(k, v []byte) error {
			var state budget.State
			if jsonErr := json.Unmarshal(v, &state); jsonErr == nil {
				result[string(k)] = state
				return nil
			}
			if d, decErr := decimal.NewFromString(string(v)); decErr == nil {
				result[string(k)] = budget.State{Spent: d}
				return nil
			}
			return fmt.Errorf("boltstore: key %q has a corrupt stored spend value %q", k, v)
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Save implements budget.Store, upserting keyID's full budget.State
// (JSON-encoded) in one bbolt transaction — including for a key whose
// existing entry was still in the legacy bare-decimal-string format,
// which this unconditionally rewrites in the new format.
func (s *Store) Save(_ context.Context, keyID string, state budget.State) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("boltstore: encoding state for key %q: %w", keyID, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte(keyID), encoded)
	})
}

// Delete implements budget.Store, mirroring
// internal/identity/boltstore.Store.Delete's identical pattern exactly.
func (s *Store) Delete(_ context.Context, keyID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Delete([]byte(keyID))
	})
}
