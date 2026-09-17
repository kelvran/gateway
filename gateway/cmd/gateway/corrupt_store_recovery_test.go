package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	budgetboltstore "github.com/kelvran/gateway/gateway/internal/budget/boltstore"
)

// TestOpenPersistStoreWithRecoveryFailModePropagatesErrorUnchanged proves
// mode == "fail" (the default) is byte-for-byte the original, pre-
// existing behavior: the open error is returned completely unchanged,
// and the corrupt path is never touched (no rename attempted) --
// exactly what every config file written before this feature existed
// still gets.
func TestOpenPersistStoreWithRecoveryFailModePropagatesErrorUnchanged(t *testing.T) {
	wantErr := errors.New("simulated corruption")
	fakeOpen := func(path string) (string, error) { return "", wantErr }

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, []byte("not-a-real-bolt-file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := openPersistStoreWithRecovery(path, "fail", logger, fakeOpen)
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want %v unchanged", err, wantErr)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("original file at %q was touched/removed: %v", path, statErr)
	}
	if buf.Len() != 0 {
		t.Errorf("expected zero log output in fail mode, got: %s", buf.String())
	}
}

// TestOpenPersistStoreWithRecoveryResetModeRenamesAndRetries is the
// load-bearing proof for mode == "reset": the corrupt file is renamed
// aside (never deleted -- preserving forensic evidence per the
// research finding's own framing), and a second open attempt against
// the now-clear path succeeds.
func TestOpenPersistStoreWithRecoveryResetModeRenamesAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, []byte("corrupt-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	calls := 0
	fakeOpen := func(p string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("simulated corruption")
		}
		// Second call (post-reset): the path must be clear of the
		// original corrupt bytes for this to be a REAL proof, not just
		// a call-count coincidence.
		if _, statErr := os.Stat(p); statErr == nil {
			return "", errors.New("path still occupied by the corrupt file -- reset did not actually clear it")
		}
		return "fresh-store", nil
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	got, err := openPersistStoreWithRecovery(path, "reset", logger, fakeOpen)
	if err != nil {
		t.Fatalf("openPersistStoreWithRecovery: %v", err)
	}
	if got != "fresh-store" {
		t.Errorf("got %q, want the fresh store from the second open call", got)
	}
	if calls != 2 {
		t.Fatalf("open called %d times, want exactly 2 (original attempt + one retry)", calls)
	}

	// The corrupt file must exist somewhere on disk still -- renamed,
	// never deleted.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var foundBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "store.db.corrupt-") {
			foundBackup = true
			data, readErr := os.ReadFile(filepath.Join(filepath.Dir(path), e.Name()))
			if readErr != nil {
				t.Fatalf("reading backup file: %v", readErr)
			}
			if string(data) != "corrupt-bytes" {
				t.Errorf("backup file content = %q, want the original corrupt bytes preserved verbatim", data)
			}
		}
	}
	if !foundBackup {
		t.Errorf("no store.db.corrupt-* backup file found in %q -- the corrupt file was not preserved", filepath.Dir(path))
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "persist_store_open_failed") || !strings.Contains(logOutput, "persist_store_reset") {
		t.Errorf("expected both persist_store_open_failed and persist_store_reset log lines, got: %s", logOutput)
	}
}

// TestOpenPersistStoreWithRecoveryResetModeFallsBackToOriginalErrorOnRenameFailure
// proves a rename failure (e.g. the corrupt file's own parent directory
// disappearing between the failed open and the reset attempt) surfaces
// the ORIGINAL open error, not the rename error -- and never attempts a
// second open call, since retrying against a path the rename couldn't
// clear would just reproduce the identical failure.
func TestOpenPersistStoreWithRecoveryResetModeFallsBackToOriginalErrorOnRenameFailure(t *testing.T) {
	// A path inside a directory that doesn't exist at all -- os.Rename
	// against it fails immediately and deterministically, with no
	// flakiness risk (unlike simulating a permissions error, which
	// behaves differently across platforms/CI runners).
	path := filepath.Join(t.TempDir(), "does-not-exist-dir", "store.db")

	wantErr := errors.New("simulated corruption")
	calls := 0
	fakeOpen := func(p string) (string, error) {
		calls++
		return "", wantErr
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	_, err := openPersistStoreWithRecovery(path, "reset", logger, fakeOpen)
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want the original open error surfaced after a rename failure", err)
	}
	if calls != 1 {
		t.Errorf("open called %d times, want exactly 1 -- a failed rename must never trigger a retry", calls)
	}
	if !strings.Contains(buf.String(), "persist_store_corrupt_backup_failed") {
		t.Errorf("expected a persist_store_corrupt_backup_failed log line, got: %s", buf.String())
	}
}

// TestOpenPersistStoreWithRecoverySucceedsOnFirstAttemptNeverTouchesDisk
// proves the common, non-corrupt case (a path that opens cleanly on the
// first attempt, including the "path doesn't exist yet" case bbolt
// itself already handles as "create fresh") is completely unaffected
// by either mode -- no rename, no log output, regardless of
// onCorruptStore's value.
func TestOpenPersistStoreWithRecoverySucceedsOnFirstAttemptNeverTouchesDisk(t *testing.T) {
	for _, mode := range []string{"fail", "reset"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			fakeOpen := func(p string) (string, error) {
				calls++
				return "opened-cleanly", nil
			}
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			got, err := openPersistStoreWithRecovery("/some/path.db", mode, logger, fakeOpen)
			if err != nil {
				t.Fatalf("openPersistStoreWithRecovery: %v", err)
			}
			if got != "opened-cleanly" {
				t.Errorf("got %q, want the first successful open's own result", got)
			}
			if calls != 1 {
				t.Errorf("open called %d times, want exactly 1", calls)
			}
			if buf.Len() != 0 {
				t.Errorf("expected zero log output on a clean open, got: %s", buf.String())
			}
		})
	}
}

// TestOpenPersistStoreWithRecoveryRealBoltCorruption is the one true
// end-to-end proof, against the real boltstore.Open a production
// persist_path actually uses -- not just a fake open function. A
// genuinely invalid bbolt file (arbitrary non-bbolt bytes, real disk
// I/O, real bbolt error path) is reset into a real, usable,
// freshly-created *boltstore.Store.
func TestOpenPersistStoreWithRecoveryRealBoltCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	if err := os.WriteFile(path, []byte("this is definitely not a valid bbolt database file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Confirm the premise first: a real boltstore.Open against this
	// file genuinely fails, so this test isn't accidentally vacuous.
	if _, err := budgetboltstore.Open(path); err == nil {
		t.Fatal("premise failed: budgetboltstore.Open succeeded against a deliberately-invalid file")
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	store, err := openPersistStoreWithRecovery(path, "reset", logger, budgetboltstore.Open)
	if err != nil {
		t.Fatalf("openPersistStoreWithRecovery: %v", err)
	}
	defer func() { _ = store.Close() }()

	// A real, usable store -- prove it by actually calling a real
	// method on it, not just checking err == nil.
	if _, err := store.Load(context.Background()); err != nil {
		t.Errorf("Load on the reset store: %v", err)
	}
}
