// Package boltstore implements budget.Store over go.etcd.io/bbolt, giving
// internal/budget.Tracker restart-durable persistence, per
// docs/rfcs/2026-09-03-budget-persistence.md.
//
// Spend is stored as the exact decimal string (spent.String()), never a
// numeric encoding of any kind — bbolt has no notion of a "float column"
// to misuse, but the discipline is stated explicitly anyway, matching the
// same precision-preservation reasoning as
// docs/rfcs/2026-09-02-decimal-cost-accounting.md's YAML-parser fix and
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md's string-typed cost
// attribute: the boundary between Kelvran's money type and any external
// representation must never round-trip through anything but decimal text.
package boltstore

import (
	"context"
	"fmt"

	"github.com/shopspring/decimal"
	bolt "go.etcd.io/bbolt"
)

// bucketName is the single bbolt bucket this store uses: keyID -> the
// exact decimal string of that key's cumulative spend.
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
func (s *Store) Load(_ context.Context) (map[string]decimal.Decimal, error) {
	result := map[string]decimal.Decimal{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.ForEach(func(k, v []byte) error {
			d, err := decimal.NewFromString(string(v))
			if err != nil {
				return fmt.Errorf("boltstore: key %q has a corrupt stored spend value %q: %w", k, v, err)
			}
			result[string(k)] = d
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Save implements budget.Store, upserting keyID's cumulative spend in one
// bbolt transaction.
func (s *Store) Save(_ context.Context, keyID string, spent decimal.Decimal) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte(keyID), []byte(spent.String()))
	})
}
