// Package boltstore implements identity.Store over go.etcd.io/bbolt,
// giving admin-API-created/rotated virtual keys restart-durable
// persistence, per docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 2 — mirrors gateway/internal/budget/boltstore's own shape and
// single-instance-only scope exactly (that package is this project's
// proven pattern for exactly this class of problem).
//
// A VirtualKey is stored as its exact JSON encoding, never a hand-rolled
// field-by-field layout — bbolt has no schema to migrate around, and JSON
// keeps this store correct automatically as VirtualKey itself gains
// fields (e.g. PreviousKeyHash/PreviousKeyHashExpiresAt), at the cost of
// needing a decode failure to be treated as real (returned, not
// swallowed) rather than assumed impossible.
package boltstore

import (
	"context"
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/identity"
)

// bucketName is the single bbolt bucket this store uses: virtual key ID ->
// the JSON encoding of that key's identity.VirtualKey.
const bucketName = "virtual_keys"

// Store is a bbolt-backed identity.Store. The zero value is not usable;
// construct with Open.
type Store struct {
	db *bolt.DB
}

// Open opens (creating if absent) the bbolt file at path and ensures the
// virtual_keys bucket exists. bbolt takes an exclusive lock on the file
// for the lifetime of the returned Store — a second process opening the
// same path fails clearly rather than silently corrupting the file,
// mirroring budget/boltstore.Open's identical single-instance-only scope.
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

// DB returns the underlying *bolt.DB, for a caller that needs the raw
// handle for something Store's own interface doesn't expose — e.g.
// backup.CopyFile's live, hot-backup primitive
// (dataplane.Pipeline.BackupStores). Not part of identity.Store; callers
// reach it via a type assertion.
func (s *Store) DB() *bolt.DB {
	return s.db
}

// Load implements identity.Store. ctx is accepted for interface symmetry
// with a future networked Store implementation, but unused here — a
// bbolt transaction is synchronous and fast enough that there is no real
// mid-transaction cancellation point to honor, mirroring
// budget/boltstore.Store.Load's identical reasoning.
func (s *Store) Load(_ context.Context) (map[string]identity.VirtualKey, error) {
	result := map[string]identity.VirtualKey{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.ForEach(func(k, v []byte) error {
			var vk identity.VirtualKey
			if err := json.Unmarshal(v, &vk); err != nil {
				return fmt.Errorf("boltstore: key %q has a corrupt stored virtual key: %w", k, err)
			}
			result[string(k)] = vk
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Save implements identity.Store, upserting vk in one bbolt transaction,
// keyed by vk.ID.
func (s *Store) Save(_ context.Context, vk identity.VirtualKey) error {
	encoded, err := json.Marshal(vk)
	if err != nil {
		return fmt.Errorf("boltstore: encoding virtual key %q: %w", vk.ID, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte(vk.ID), encoded)
	})
}

// Delete implements identity.Store, removing id's entry if present — a
// no-op, not an error, if id was never persisted (mirrors
// bolt.Bucket.Delete's own documented behavior).
func (s *Store) Delete(_ context.Context, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Delete([]byte(id))
	})
}
