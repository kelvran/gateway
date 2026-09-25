package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/budget"
	budgetboltstore "github.com/kelvran/gateway/gateway/internal/budget/boltstore"
	"github.com/kelvran/gateway/gateway/internal/identity"
	identityboltstore "github.com/kelvran/gateway/gateway/internal/identity/boltstore"
	"github.com/kelvran/gateway/gateway/internal/prompt"
	promptboltstore "github.com/kelvran/gateway/gateway/internal/prompt/boltstore"
)

// This file is the one deliberate exception to backup.go's own "leaf
// package" doc comment: proving Restore's real load-bearing claim --
// that a restored file loads correctly through the exact real
// constructor cmd/gateway's own startup path uses (identity/budget/
// prompt's own boltstore.Open), not merely a raw bolt.Open read-check --
// requires importing all three concrete boltstore packages from test
// code. None of those packages import this one, so no import cycle
// exists; go-arch-lint's own excludeFiles rule (gateway/.go-arch-lint.yml)
// already exempts every _test.go file from the dependency-direction
// check this would otherwise trip.

// TestRestoreThroughRealIdentityConstructor is the exact missing proof
// an audit found: a backup produced by a real CopyFile call restores
// and reloads correctly through identity/boltstore.Open -- the real
// constructor cmd/gateway's own startup path uses -- not just a raw
// bolt.Open read-check.
func TestRestoreThroughRealIdentityConstructor(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "identity-source.db")
	source, err := identityboltstore.Open(sourcePath)
	if err != nil {
		t.Fatalf("identityboltstore.Open (source): %v", err)
	}
	wantKey := identity.VirtualKey{ID: "vk-restore-proof", KeyHash: "deadbeef"}
	if err := source.Save(context.Background(), wantKey); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "identity-backup.db")
	if err := CopyFile(source.DB(), backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing source store: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "identity-restored.db")
	if err := Restore(backupPath, destPath, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := identityboltstore.Open(destPath)
	if err != nil {
		t.Fatalf("identityboltstore.Open (restored destination): %v", err)
	}
	defer func() { _ = restored.Close() }()

	keys, err := restored.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := keys[wantKey.ID]
	if !ok {
		t.Fatalf("restored store has no entry for %q, want the round-tripped virtual key", wantKey.ID)
	}
	if got.KeyHash != wantKey.KeyHash {
		t.Errorf("restored KeyHash = %q, want %q", got.KeyHash, wantKey.KeyHash)
	}
}

// TestRestoreThroughRealBudgetConstructor mirrors
// TestRestoreThroughRealIdentityConstructor for budget/boltstore.Open.
func TestRestoreThroughRealBudgetConstructor(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "budget-source.db")
	source, err := budgetboltstore.Open(sourcePath)
	if err != nil {
		t.Fatalf("budgetboltstore.Open (source): %v", err)
	}
	wantState := budget.State{Spent: decimal.NewFromFloat(42.5), BilledCount: 7}
	if err := source.Save(context.Background(), "key-restore-proof", wantState); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "budget-backup.db")
	if err := CopyFile(source.DB(), backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing source store: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "budget-restored.db")
	if err := Restore(backupPath, destPath, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := budgetboltstore.Open(destPath)
	if err != nil {
		t.Fatalf("budgetboltstore.Open (restored destination): %v", err)
	}
	defer func() { _ = restored.Close() }()

	states, err := restored.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := states["key-restore-proof"]
	if !ok {
		t.Fatal("restored store has no entry for \"key-restore-proof\", want the round-tripped budget state")
	}
	if !got.Spent.Equal(wantState.Spent) {
		t.Errorf("restored Spent = %s, want %s", got.Spent, wantState.Spent)
	}
	if got.BilledCount != wantState.BilledCount {
		t.Errorf("restored BilledCount = %d, want %d", got.BilledCount, wantState.BilledCount)
	}
}

// TestRestoreThroughRealPromptConstructor mirrors
// TestRestoreThroughRealIdentityConstructor for prompt/boltstore.Open.
func TestRestoreThroughRealPromptConstructor(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "prompt-source.db")
	source, err := promptboltstore.Open(sourcePath)
	if err != nil {
		t.Fatalf("promptboltstore.Open (source): %v", err)
	}
	wantVersions := []prompt.Prompt{{ID: "prompt-restore-proof", Version: 1, CreatedAt: time.Unix(1758700000, 0).UTC()}}
	if err := source.Save(context.Background(), "prompt-restore-proof", wantVersions); err != nil {
		t.Fatalf("Save: %v", err)
	}

	backupPath := filepath.Join(t.TempDir(), "prompt-backup.db")
	if err := CopyFile(source.DB(), backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing source store: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "prompt-restored.db")
	if err := Restore(backupPath, destPath, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := promptboltstore.Open(destPath)
	if err != nil {
		t.Fatalf("promptboltstore.Open (restored destination): %v", err)
	}
	defer func() { _ = restored.Close() }()

	history, err := restored.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := history["prompt-restore-proof"]
	if !ok {
		t.Fatal("restored store has no entry for \"prompt-restore-proof\", want the round-tripped version history")
	}
	if len(got) != 1 || got[0].Version != 1 {
		t.Errorf("restored version history = %+v, want exactly one entry at version 1", got)
	}
}

// TestRestoreRefusesToOverwriteAnExistingDestinationWithoutForce mirrors
// TestCopyFileRefusesToOverwriteAnExistingDestination -- the exact
// convention this function is required to match.
func TestRestoreRefusesToOverwriteAnExistingDestinationWithoutForce(t *testing.T) {
	db := openTestDB(t)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := CopyFile(db, backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	if err := os.WriteFile(destPath, []byte("a live persist_path file, not to be clobbered"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destPath before Restore: %v", err)
	}

	if err := Restore(backupPath, destPath, false); err == nil {
		t.Fatal("Restore without force against an existing destPath returned nil error, want a refusal-to-overwrite error")
	}

	after, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destPath after Restore: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("destPath content changed despite the refused restore: before %q, after %q", before, after)
	}
}

// TestRestoreOverwritesExistingDestinationWhenForced proves force is a
// real escape hatch, not just a parameter that's accepted and ignored.
func TestRestoreOverwritesExistingDestinationWhenForced(t *testing.T) {
	db := openTestDB(t)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := CopyFile(db, backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	if err := os.WriteFile(destPath, []byte("a stale persist_path file, to be replaced"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := Restore(backupPath, destPath, true); err != nil {
		t.Fatalf("Restore with force=true: %v", err)
	}

	restored, err := bolt.Open(destPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("opening the forced-restore destination: %v", err)
	}
	defer func() { _ = restored.Close() }()
	err = restored.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			t.Fatal("forced-restore destination has no \"data\" bucket -- the stale file, not the backup, is what's present")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the forced-restore destination: %v", err)
	}
}

// TestRestoreRejectsACorruptSourceBeforeTouchingTheDestination proves
// the exact validate-before-touching-destPath ordering Restore's own
// doc comment promises -- not merely that a corrupt source is rejected,
// but that the destination is PROVABLY untouched (mtime and content both
// unchanged) when it is.
func TestRestoreRejectsACorruptSourceBeforeTouchingTheDestination(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "corrupt-backup.db")
	if err := os.WriteFile(backupPath, []byte("this is definitely not a valid bbolt database file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	wantContent := []byte("the live destination file, which must survive untouched")
	if err := os.WriteFile(destPath, wantContent, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	infoBefore, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("Stat destPath before Restore: %v", err)
	}

	// force=true too -- a corrupt source must be rejected before the
	// overwrite decision is even reached, regardless of force.
	if err := Restore(backupPath, destPath, true); err == nil {
		t.Fatal("Restore from a corrupt source returned nil error, want a validation error")
	}

	infoAfter, err := os.Stat(destPath)
	if err != nil {
		t.Fatalf("Stat destPath after Restore: %v", err)
	}
	if infoBefore.ModTime() != infoAfter.ModTime() {
		t.Errorf("destPath mtime changed (%v -> %v) despite the source failing validation", infoBefore.ModTime(), infoAfter.ModTime())
	}
	if infoBefore.Size() != infoAfter.Size() {
		t.Errorf("destPath size changed (%d -> %d) despite the source failing validation", infoBefore.Size(), infoAfter.Size())
	}
	gotContent, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("reading destPath after Restore: %v", err)
	}
	if string(gotContent) != string(wantContent) {
		t.Errorf("destPath content changed despite the source failing validation: got %q, want %q", gotContent, wantContent)
	}

	// The corrupt source itself must also be left alone -- Restore never
	// mutates backupPath.
	gotBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("reading backupPath after Restore: %v", err)
	}
	if string(gotBackup) != "this is definitely not a valid bbolt database file" {
		t.Errorf("backupPath content changed unexpectedly: %q", gotBackup)
	}

	// And no stray temp file should be left behind next to destPath.
	entries, err := os.ReadDir(filepath.Dir(destPath))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(destPath) {
			t.Errorf("unexpected leftover file %q next to destPath -- Restore should clean up its temp file on every error path", e.Name())
		}
	}
}

// TestRestoreRejectsACorruptSourceWhenNoDestinationExistsYet proves the
// validation-first ordering holds even in the common no-live-file case
// (a fresh persist_path that doesn't exist on disk yet) -- Restore must
// still refuse a corrupt source and must never create anything at
// destPath.
func TestRestoreRejectsACorruptSourceWhenNoDestinationExistsYet(t *testing.T) {
	backupPath := filepath.Join(t.TempDir(), "corrupt-backup.db")
	if err := os.WriteFile(backupPath, []byte("also not a valid bbolt database"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "dest.db")

	if err := Restore(backupPath, destPath, false); err == nil {
		t.Fatal("Restore from a corrupt source returned nil error, want a validation error")
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("destDir has %d entries after a rejected Restore, want 0: %v", len(entries), entries)
	}
}

// TestRestoreProducesARestorableDestination is the plain, no-real-store
// baseline: a valid backup restores into a fresh destPath that is itself
// a correct, independently-openable bbolt database with the source's
// data intact -- mirroring TestCopyFileProducesAByteIdenticalRestorableDatabase's
// own shape for Restore.
func TestRestoreProducesARestorableDestination(t *testing.T) {
	db := openTestDB(t)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := CopyFile(db, backupPath); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}

	destPath := filepath.Join(t.TempDir(), "dest.db")
	if err := Restore(backupPath, destPath, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := bolt.Open(destPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("opening the restored destination: %v", err)
	}
	defer func() { _ = restored.Close() }()

	err = restored.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("data"))
		if b == nil {
			t.Fatal("restored destination has no \"data\" bucket")
		}
		if got := string(b.Get([]byte("key"))); got != "value" {
			t.Errorf("restored destination's key = %q, want %q", got, "value")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the restored destination: %v", err)
	}
}
