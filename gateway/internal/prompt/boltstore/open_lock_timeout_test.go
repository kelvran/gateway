package boltstore

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	berrors "go.etcd.io/bbolt/errors"
)

// TestOpenFailsWithinTheLockTimeoutWhenAnotherStoreHoldsTheFile pins the
// operator-facing contract this package's Open doc comment makes: a second
// opener of the same file (an operator starting two gateway processes on
// one persist_path, or a restart racing a not-yet-exited predecessor) gets
// a clear error, promptly. bbolt's own default (Options.Timeout == 0)
// blocks the second opener forever instead, so without an explicit
// timeout the gateway would hang at startup with no log line and no exit
// code. The 10 s ceiling is deliberately far above the configured timeout
// so the assertion never flakes on a loaded machine; the point is "returns
// with ErrTimeout", not the exact duration.
func TestOpenFailsWithinTheLockTimeoutWhenAnotherStoreHoldsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	type result struct {
		store *Store
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s, openErr := Open(path)
		done <- result{store: s, err: openErr}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			_ = r.store.Close()
			t.Fatal("second Open on a file another Store holds succeeded, want an error")
		}
		if !errors.Is(r.err, berrors.ErrTimeout) {
			t.Fatalf("second Open error = %v, want one wrapping bbolt ErrTimeout", r.err)
		}
		if !strings.Contains(r.err.Error(), "another process holds the file lock") || !strings.Contains(r.err.Error(), path) {
			t.Fatalf("second Open error = %q, want the path and the lock-holder explanation wrapped around ErrTimeout", r.err.Error())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Open on a file another Store holds did not return within 10s (bbolt blocks forever when Options.Timeout is 0)")
	}
}
