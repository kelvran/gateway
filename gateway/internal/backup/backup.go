// Package backup provides a live, safe-to-run-while-serving-traffic
// bbolt backup primitive, per
// docs/upgrade-research/state-durability-operational-recovery-2026-09-15.md:
// go.etcd.io/bbolt (already vendored, v1.5.0) ships a documented
// Tx.CopyFile method whose own doc comment states "a reader transaction
// is maintained during the copy so it is safe to continue using the
// database while a copy is in progress" — backed by bbolt's MVCC design
// (a read transaction sees a consistent snapshot; concurrent writers
// allocate new pages rather than overwriting ones a live reader still
// references). Confirmed directly against the vendored source
// (go.etcd.io/bbolt@v1.5.0/tx.go), not assumed.
//
// A leaf package deliberately: no imports from dataplane/identity/
// budget/prompt internals — it operates on a raw *bolt.DB handle,
// exactly like every existing boltstore package's own db.Update/db.View
// calls do, so callers (dataplane.Pipeline.BackupStores) reach it via a
// type assertion against whichever concrete boltstore.Store
// implementation is actually configured, never a direct dependency in
// either direction.
package backup

import (
	"fmt"
	"os"

	bolt "go.etcd.io/bbolt"
)

// DBBacked is implemented by any concrete boltstore.Store type (identity/
// budget/prompt each have their own, all with an identical one-line `DB()
// *bolt.DB { return s.db }` accessor) — the type-assertion target a
// caller (dataplane.Pipeline.BackupStores) uses to reach the raw handle
// CopyFile needs from whichever store interface value (identity.Store/
// budget.Store/prompt.Persister) it's actually holding, without any of
// those packages needing to import bbolt themselves or this package
// needing to import any of them.
type DBBacked interface {
	DB() *bolt.DB
}

// CopyFile backs up db to a new file at destPath (0o600), refusing to
// overwrite an existing file at that path — a backup call must never
// silently clobber a prior backup; the caller is expected to pass a
// fresh, timestamped destination each time. Safe to call while db is
// actively serving real reads and writes, per this package's own doc
// comment.
func CopyFile(db *bolt.DB, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup: %s already exists, refusing to overwrite", destPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup: checking %s: %w", destPath, err)
	}
	return db.View(func(tx *bolt.Tx) error {
		return tx.CopyFile(destPath, 0o600)
	})
}
