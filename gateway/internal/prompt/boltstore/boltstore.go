// Package boltstore implements prompt.Persister over go.etcd.io/bbolt,
// giving admin-API-managed prompt templates restart-durable persistence.
// Mirrors gateway/internal/identity/boltstore and
// gateway/internal/budget/boltstore file-for-file -- this repo's own
// proven single-instance-durability pattern -- rather than waiting on the
// Postgres control-plane store gateway/ARCHITECTURE.md's Tech Stack table
// names as the eventual real target: that decision has no trigger of its
// own yet, and prompt.Persister has already shipped unimplemented for a
// full RFC cycle (see its own doc comment).
//
// A prompt ID's entire version history is stored as one JSON-encoded
// []prompt.Prompt value, matching Persister.Save's own "always the whole
// history for one id" unit of work -- the same granularity
// identity/boltstore.Save uses for one virtual key.
package boltstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
	berrors "go.etcd.io/bbolt/errors"

	"github.com/kelvran/gateway/gateway/internal/prompt"
)

// bucketName is the single bbolt bucket this store uses: prompt ID ->
// the JSON encoding of that ID's []prompt.Prompt version history.
const bucketName = "prompts"

// Store is a bbolt-backed prompt.Persister. The zero value is not usable;
// construct with Open.
type Store struct {
	db *bolt.DB
}

// openLockTimeout bounds how long Open waits for bbolt's exclusive file
// lock. bbolt's default (Options.Timeout == 0) waits forever, so a second
// gateway process pointed at the same persist_path -- or a restart racing a
// predecessor that has not released the file yet -- hung at startup with no
// log line and no exit code. One second is far above the milliseconds a
// clean close takes to release the lock and far below any supervisor's
// start timeout. The resulting error wraps bbolt's ErrTimeout, which
// cmd/gateway's openPersistStoreWithRecovery never mistakes for corruption,
// so on_corrupt_store: reset never renames a merely locked file aside.
const openLockTimeout = time.Second

// Open opens (creating if absent) the bbolt file at path and ensures the
// prompts bucket exists. bbolt takes an exclusive lock on the file for
// the lifetime of the returned Store -- a second process opening the same
// path fails clearly rather than silently corrupting the file, mirroring
// identity/boltstore.Open's identical single-instance-only scope.
func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: openLockTimeout})
	if err != nil {
		if errors.Is(err, berrors.ErrTimeout) {
			return nil, fmt.Errorf("boltstore: opening %s: another process holds the file lock (waited %s): %w", path, openLockTimeout, err)
		}
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

// DB returns the underlying *bolt.DB — see
// identity/boltstore.Store.DB's identical doc comment.
func (s *Store) DB() *bolt.DB {
	return s.db
}

// Load implements prompt.Persister. ctx is accepted for interface
// symmetry with a future networked Persister, but unused here -- a bbolt
// transaction is synchronous and fast enough that there is no real
// mid-transaction cancellation point to honor, mirroring the sibling
// boltstore packages' identical reasoning.
func (s *Store) Load(_ context.Context) (map[string][]prompt.Prompt, error) {
	result := map[string][]prompt.Prompt{}
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.ForEach(func(k, v []byte) error {
			var versions []prompt.Prompt
			if err := json.Unmarshal(v, &versions); err != nil {
				return fmt.Errorf("boltstore: prompt %q has a corrupt stored version history: %w", k, err)
			}
			result[string(k)] = versions
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Save implements prompt.Persister, upserting id's entire version history
// in one bbolt transaction. A nil or empty versions is prompt.Store's own
// documented deletion signal (see Store.Delete's doc comment) -- handled
// here by removing id's bucket entry entirely, not by storing an empty
// JSON array, so a deleted prompt doesn't accumulate as dead weight in
// the file forever.
func (s *Store) Save(_ context.Context, id string, versions []prompt.Prompt) error {
	if len(versions) == 0 {
		return s.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(bucketName))
			return b.Delete([]byte(id))
		})
	}
	encoded, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("boltstore: encoding prompt %q: %w", id, err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte(id), encoded)
	})
}
