package budget

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
)

// fakeStore is a tiny in-memory Store, kept in this package's own test
// file rather than depending on internal/budget/boltstore — the same
// dependency-direction discipline the rest of this codebase already
// follows (e.g. internal/telemetry never imports internal/identity).
type fakeStore struct {
	mu        sync.Mutex
	data      map[string]decimal.Decimal
	saveCalls []struct {
		keyID string
		spent decimal.Decimal
	}
	saveErr error
	closed  bool
}

func newFakeStore(initial map[string]decimal.Decimal) *fakeStore {
	if initial == nil {
		initial = map[string]decimal.Decimal{}
	}
	return &fakeStore{data: initial}
}

func (f *fakeStore) Load(context.Context) (map[string]decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]decimal.Decimal, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) Save(_ context.Context, keyID string, spent decimal.Decimal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls = append(f.saveCalls, struct {
		keyID string
		spent decimal.Decimal
	}{keyID, spent})
	if f.saveErr != nil {
		return f.saveErr
	}
	f.data[keyID] = spent
	return nil
}

func (f *fakeStore) Close() error {
	f.closed = true
	return nil
}

func TestNewTrackerWithStoreHydratesExistingSpend(t *testing.T) {
	store := newFakeStore(map[string]decimal.Decimal{"team-alpha": d("7.5")})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	// Allow must immediately reflect the hydrated spend, without any
	// Record call — a cap of exactly the hydrated amount must reject.
	if tr.Allow("team-alpha", d("7.5"), 0) {
		t.Error("Allow(cap=7.5) after hydrating spend=7.5 = true, want false")
	}
	if !tr.Allow("team-alpha", d("7.51"), 0) {
		t.Error("Allow(cap=7.51) after hydrating spend=7.5 = false, want true")
	}
}

func TestNewTrackerWithStoreLoadErrorPropagates(t *testing.T) {
	store := &erroringLoadStore{err: errors.New("simulated load failure")}
	_, err := NewTrackerWithStore(context.Background(), store, nil)
	if err == nil {
		t.Fatal("NewTrackerWithStore with a failing Load returned nil error")
	}
}

type erroringLoadStore struct{ err error }

func (s *erroringLoadStore) Load(context.Context) (map[string]decimal.Decimal, error) {
	return nil, s.err
}
func (s *erroringLoadStore) Save(context.Context, string, decimal.Decimal) error { return nil }
func (s *erroringLoadStore) Close() error                                        { return nil }

func TestRecordPersistsCumulativeTotalToStore(t *testing.T) {
	store := newFakeStore(nil)
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	tr.Record("team-alpha", d("3"), 0)
	tr.Record("team-alpha", d("4"), 0)

	if len(store.saveCalls) != 2 {
		t.Fatalf("len(saveCalls) = %d, want 2", len(store.saveCalls))
	}
	// Save must receive the cumulative total, not just the delta.
	if !store.saveCalls[0].spent.Equal(d("3")) {
		t.Errorf("first Save spent = %v, want 3", store.saveCalls[0].spent)
	}
	if !store.saveCalls[1].spent.Equal(d("7")) {
		t.Errorf("second Save spent = %v, want 7 (cumulative, not the 4-delta)", store.saveCalls[1].spent)
	}
}

// TestRecordLogsButContinuesOnPersistFailure proves a Save failure is
// logged and does not panic, block, or otherwise fail Record — the
// in-memory state (verified separately below) is already correct;
// docs/rfcs/2026-09-03-budget-persistence.md's whole design point is that
// this is a real but non-fatal degradation, not a request failure.
func TestRecordLogsButContinuesOnPersistFailure(t *testing.T) {
	store := newFakeStore(nil)
	store.saveErr = errors.New("simulated disk failure")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	tr, err := NewTrackerWithStore(context.Background(), store, logger)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	tr.Record("team-alpha", d("5"), 0) // must not panic despite the failing store

	// Allow(cap=5) correctly returns false here too (spend == cap is a
	// strict boundary, per TestAllowBoundaryExactlyAtCap) — that's not
	// what this test is checking. Use a cap strictly above the recorded
	// spend to prove the in-memory update actually happened.
	if !tr.Allow("team-alpha", d("5.01"), 0) {
		t.Error("in-memory spend was not updated despite the persist failure — Allow should still see it")
	}
	if tr.Allow("team-alpha", d("4.99"), 0) {
		t.Error("Allow(cap=4.99) = true after recording spend=5 — in-memory spend was not updated at all")
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("budget_persist_failed")) {
		t.Errorf("log output = %q, want it to contain \"budget_persist_failed\"", logBuf.String())
	}
}

func TestCloseCallsThroughToStore(t *testing.T) {
	store := newFakeStore(nil)
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !store.closed {
		t.Error("Tracker.Close() did not call through to the store's Close()")
	}
}

func TestCloseOnPlainTrackerIsANoOp(t *testing.T) {
	tr := NewTracker()
	if err := tr.Close(); err != nil {
		t.Errorf("Close() on a plain (no-store) Tracker = %v, want nil", err)
	}
}
