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
// This package also provides Restore, the missing operator-facing
// counterpart CopyFile never had: an OFFLINE restore primitive for
// putting a backup file back in place as a store's persist_path. Unlike
// CopyFile, Restore is NOT safe to run against a persist_path a live
// gateway process still has open — restoring a bbolt file out from
// under a running process's own exclusive file lock and in-memory state
// is a real concurrent-access hazard this package deliberately does not
// attempt to solve. The intended shape is strictly: stop the gateway
// process, run a restore (cmd/gateway's -restore-store/-restore-from
// flags), then start the process again with the restored file already
// in place — see docs/operations/DEPLOY.md's "Local Bbolt Persistence:
// Backup & Restore" section.
//
// A leaf package deliberately: no imports from dataplane/identity/
// budget/prompt internals — it operates on raw file paths and a
// *bolt.DB handle, exactly like every existing boltstore package's own
// db.Update/db.View calls do, so callers (dataplane.Pipeline.
// BackupStores for CopyFile; cmd/gateway's restore mode for Restore)
// reach it without either direction depending on the other. (This
// package's own tests are the one deliberate exception — see
// restore_test.go's doc comment for why proving Restore's real-
// constructor round trip requires importing identity/budget/prompt's
// boltstore packages from test code specifically.)
package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

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

// validationOpenTimeout bounds how long Restore's own pre-flight
// validation open (below) may block. bbolt's own DefaultOptions.Timeout
// is 0 — a locked file blocks the open call forever rather than ever
// returning an error — which would hang an operator's one-shot restore
// invocation indefinitely against a backupPath that, unexpectedly, still
// has another process holding it open. A short, fixed timeout instead
// fails fast with bolt's own ErrTimeout.
const validationOpenTimeout = 2 * time.Second

// Restore puts a bbolt backup file back in place as destPath — the
// missing counterpart to CopyFile: the exact recovery step an operator
// needs after a corrupted persist_path file, using a backup CopyFile (or
// POST /admin/backup, which calls it) already produced.
//
// Two safety properties, in this order:
//
//  1. backupPath MUST be a well-formed, independently-openable bbolt
//     database — verified by actually opening it, read-only, via a raw
//     bolt.Open (not merely checking that a file exists at that path) —
//     BEFORE destPath is touched in any way. This is the exact gap an
//     audit found in this package's own pre-existing tests: they proved
//     a backup file opens via bolt.Open, but never that it survives a
//     restore through the real, disk-format-and-schema-aware
//     constructor an app package (identity/budget/prompt's own
//     boltstore.Open) actually uses at startup — see restore_test.go.
//  2. destPath is never silently overwritten. If a file already exists
//     at destPath, Restore refuses (returning an error) unless force is
//     true — mirroring CopyFile's own refuses-to-overwrite convention
//     (see TestCopyFileRefusesToOverwriteAnExistingDestination) for the
//     identical reason: clobbering a live persist_path with a stale
//     backup by accident is a real footgun, not a hypothetical one.
//
// The copy itself is atomic: backupPath's bytes are written to a
// temporary file in destPath's own directory first, then moved into
// place with a single os.Rename — so a crash or power loss mid-copy
// leaves either the original destPath (untouched) or the complete
// restored file at destPath, never a half-written one. The temp file is
// always cleaned up, including on every error path.
//
// Restore is an OFFLINE operation — see this package's own doc comment
// for why destPath must not be a persist_path any running gateway
// process currently has open.
func Restore(backupPath, destPath string, force bool) error {
	validation, err := bolt.Open(backupPath, 0o600, &bolt.Options{ReadOnly: true, Timeout: validationOpenTimeout})
	if err != nil {
		return fmt.Errorf("backup: %s is not a valid, openable bbolt database, refusing to restore from it: %w", backupPath, err)
	}
	if err := validation.Close(); err != nil {
		return fmt.Errorf("backup: closing validation handle for %s: %w", backupPath, err)
	}

	if !force {
		if _, err := os.Stat(destPath); err == nil {
			return fmt.Errorf("backup: %s already exists, refusing to overwrite (pass force to override)", destPath)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("backup: checking %s: %w", destPath, err)
		}
	}

	src, err := os.Open(backupPath)
	if err != nil {
		return fmt.Errorf("backup: opening %s to restore: %w", backupPath, err)
	}
	defer func() { _ = src.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".restoring-*")
	if err != nil {
		return fmt.Errorf("backup: creating a temp file next to %s: %w", destPath, err)
	}
	tmpPath := tmp.Name()
	// Removing an already-renamed-away tmpPath is a harmless no-op
	// (os.Remove on a nonexistent path), so this unconditional deferred
	// cleanup is correct whether Restore ultimately succeeds or fails.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: copying %s to %s: %w", backupPath, tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: flushing %s to disk: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: closing %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("backup: setting permissions on %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("backup: moving %s into place at %s: %w", tmpPath, destPath, err)
	}
	return nil
}
