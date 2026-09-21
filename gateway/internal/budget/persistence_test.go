package budget

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// fakeStore is a tiny in-memory Store, kept in this package's own test
// file rather than depending on internal/budget/boltstore — the same
// dependency-direction discipline the rest of this codebase already
// follows (e.g. internal/telemetry never imports internal/identity).
type fakeStore struct {
	mu        sync.Mutex
	data      map[string]State
	saveCalls []struct {
		keyID string
		state State
	}
	deleteCalls []string
	saveErr     error
	deleteErr   error
	closed      bool
}

func newFakeStore(initial map[string]State) *fakeStore {
	if initial == nil {
		initial = map[string]State{}
	}
	return &fakeStore{data: initial}
}

func (f *fakeStore) Load(context.Context) (map[string]State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]State, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) Save(_ context.Context, keyID string, state State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveCalls = append(f.saveCalls, struct {
		keyID string
		state State
	}{keyID, state})
	if f.saveErr != nil {
		return f.saveErr
	}
	f.data[keyID] = state
	return nil
}

func (f *fakeStore) Delete(_ context.Context, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls = append(f.deleteCalls, keyID)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.data, keyID)
	return nil
}

func (f *fakeStore) Close() error {
	f.closed = true
	return nil
}

func TestNewTrackerWithStoreHydratesExistingSpend(t *testing.T) {
	store := newFakeStore(map[string]State{"team-alpha": {Spent: d("7.5")}})
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

// TestNewTrackerWithStoreHydratesPeriodStartAndEpochAcrossRestart is the
// regression proof for the rolling-window persistence gap: a restart must
// NOT treat a key with real, persisted rolling-window bookkeeping as a
// fresh window.
func TestNewTrackerWithStoreHydratesPeriodStartAndEpochAcrossRestart(t *testing.T) {
	oneHourAgo := time.Now().Add(-time.Hour)
	store := newFakeStore(map[string]State{
		"team-alpha": {Spent: d("5"), PeriodStart: oneHourAgo, PeriodEpoch: 3, BilledCount: 2},
	})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	_, reserved, _, reservationEpoch, _ := tr.Reserve(context.Background(), "team-alpha", d("100"), 24*time.Hour)
	if !reserved {
		t.Fatal("Reserve did not grant a reservation")
	}
	if reservationEpoch != 3 {
		t.Errorf("reservationEpoch = %d, want 3 (hydrated from the store, not a fresh window)", reservationEpoch)
	}
}

// TestNewTrackerWithStoreLegacyEntryStartsAFreshWindowNotAFabricatedOne
// proves the migration boundary: a legacy (pre-migration, zero PeriodStart)
// entry must NOT be treated as "epoch 0, real window" — its window starts
// fresh at the first observation after this restart, exactly matching
// pre-fix behavior for that one key.
func TestNewTrackerWithStoreLegacyEntryStartsAFreshWindowNotAFabricatedOne(t *testing.T) {
	store := newFakeStore(map[string]State{"team-alpha": {Spent: d("5")}}) // zero PeriodStart == legacy
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	_, reserved, _, reservationEpoch, _ := tr.Reserve(context.Background(), "team-alpha", d("100"), 24*time.Hour)
	if !reserved {
		t.Fatal("Reserve did not grant a reservation")
	}
	if reservationEpoch != 0 {
		t.Errorf("reservationEpoch = %d, want 0 (a legacy entry starts a fresh window, not epoch 3-or-whatever it never had)", reservationEpoch)
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

func (s *erroringLoadStore) Load(context.Context) (map[string]State, error) {
	return nil, s.err
}
func (s *erroringLoadStore) Save(context.Context, string, State) error { return nil }
func (s *erroringLoadStore) Delete(context.Context, string) error      { return nil }
func (s *erroringLoadStore) Close() error                              { return nil }

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
	if !store.saveCalls[0].state.Spent.Equal(d("3")) {
		t.Errorf("first Save spent = %v, want 3", store.saveCalls[0].state.Spent)
	}
	if !store.saveCalls[1].state.Spent.Equal(d("7")) {
		t.Errorf("second Save spent = %v, want 7 (cumulative, not the 4-delta)", store.saveCalls[1].state.Spent)
	}
}

// TestRecordPersistsFullStateIncludingPeriodBookkeeping proves Record's
// persisted State carries the CURRENT periodStart/periodEpoch/billedCount
// alongside spend, not just a bare total — the actual fix for the
// rolling-window persistence gap on Record's own persistence path.
func TestRecordPersistsFullStateIncludingPeriodBookkeeping(t *testing.T) {
	store := newFakeStore(nil)
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	// Establish a real window (first Record call starts periodStart) and
	// one real Reconcile'd cost so billedCount is non-zero.
	tr.Record("team-alpha", d("1"), 24*time.Hour)
	_, reserved, reservedUSD, reservationEpoch, _ := tr.Reserve(context.Background(), "team-alpha", d("100"), 24*time.Hour)
	if !reserved {
		t.Fatal("Reserve did not grant a reservation")
	}
	realCost := d("2")
	tr.Reconcile(context.Background(), "team-alpha", reservedUSD, reservationEpoch, &realCost, 24*time.Hour)

	last := store.saveCalls[len(store.saveCalls)-1].state
	if last.BilledCount != 1 {
		t.Errorf("persisted BilledCount = %d, want 1", last.BilledCount)
	}
	if last.PeriodStart.IsZero() {
		t.Error("persisted PeriodStart is zero, want the real window start")
	}
}

var (
	budgetPersistenceMetricsOnce   sync.Once
	budgetPersistenceMetricsReader *sdkmetric.ManualReader
)

// budgetPersistenceMetricsReaderForTest returns the ManualReader backing
// this test binary's real telemetry.RecordPersistenceFailed counter.
//
// Deliberately a package-level singleton, not a fresh reader per call: the
// go.opentelemetry.io/otel global MeterProvider delegate is bound via a
// process-lifetime sync.Once (internal/global/state.go's own
// delegateMeterOnce, confirmed by reading that source directly) — the
// FIRST otel.SetMeterProvider call in this test binary's entire process
// permanently wires telemetry.go's package-level persistenceFailedCounter
// (itself created once, at package-init, from the global default
// delegating meter) to that call's MeterProvider, and every later
// otel.SetMeterProvider call in the SAME process is a no-op for that
// already-delegated instrument. Under `go test -count>1` — which reruns
// this test function's body in the SAME process N times, since package
// vars (including this one) are never reset between reruns — the original
// "create a brand-new reader and SetMeterProvider every run" form only
// ever observed real data on the first run: every later run's fresh reader
// received zero Add() calls, because the counter was already permanently
// delegated elsewhere. Calling SetMeterProvider exactly once across every
// rerun, and asserting the DELTA a run's own Record() call produced
// (below) rather than an absolute count, is what actually holds regardless
// of how many times this process replays the test.
func budgetPersistenceMetricsReaderForTest() *sdkmetric.ManualReader {
	budgetPersistenceMetricsOnce.Do(func() {
		budgetPersistenceMetricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(budgetPersistenceMetricsReader)))
	})
	return budgetPersistenceMetricsReader
}

// budgetPersistFailedCount reads the CURRENT cumulative
// kelvran.persistence.failed[store_kind=budget] count off reader — a
// snapshot, not a per-call delta; callers diff two snapshots themselves.
func budgetPersistFailedCount(t *testing.T, reader *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	var count int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kelvran.persistence.failed" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("kelvran.persistence.failed data type = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				storeKind, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranPersistenceStoreKind))
				if storeKind.AsString() == "budget" {
					count += dp.Value
				}
			}
		}
	}
	return count
}

// TestRecordLogsButContinuesOnPersistFailure proves a Save failure is
// logged and does not panic, block, or otherwise fail Record — the
// in-memory state (verified separately below) is already correct;
// docs/rfcs/2026-09-03-budget-persistence.md's whole design point is that
// this is a real but non-fatal degradation, not a request failure.
func TestRecordLogsButContinuesOnPersistFailure(t *testing.T) {
	// kelvran.persistence.failed, per
	// docs/upgrade-research/admin-operator-experience-2026-09-14.md: this
	// package's own test binary has no other otel.SetMeterProvider swap
	// (unlike internal/telemetry/internal/gateway/dataplane, each of
	// which already spent their own one-per-test-binary global-meter
	// delegation elsewhere), so this is a real, independent proof that
	// budget.go's own call site actually increments the shared counter
	// with store_kind="budget" — not just that RecordPersistenceFailed
	// itself works in isolation (already proven directly in
	// internal/telemetry's own test suite). See
	// budgetPersistenceMetricsReaderForTest's own doc comment for why this
	// reads a shared reader and asserts a delta, rather than a fresh
	// reader and an absolute count.
	reader := budgetPersistenceMetricsReaderForTest()
	before := budgetPersistFailedCount(t, reader)

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

	if got, want := budgetPersistFailedCount(t, reader)-before, int64(1); got != want {
		t.Errorf("kelvran.persistence.failed[store_kind=budget] delta = %d, want %d", got, want)
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

// TestDeletePurgesAllInMemoryState proves every one of Tracker's 5
// in-memory maps is actually purged, not just spend -- a partial purge
// would leave stale rolling-window/alert-bucket state behind for a
// deleted key's ID, ready to silently resurrect if that same ID is ever
// reused.
func TestDeletePurgesAllInMemoryState(t *testing.T) {
	tr := NewTracker()
	tr.Record("team-alpha", d("5"), time.Hour) // seeds spent + periodStart + periodEpoch
	_, reserved, reservedUSD, epoch, _ := tr.Reserve(context.Background(), "team-alpha", d("100"), time.Hour)
	if !reserved {
		t.Fatal("setup: Reserve did not reserve anything")
	}
	realCost := d("1")
	tr.Reconcile(context.Background(), "team-alpha", reservedUSD, epoch, &realCost, time.Hour) // seeds billedCount
	tr.CheckAndMarkBudgetAlertBucket(context.Background(), "team-alpha", 0.9, 0)               // seeds highestAlertedBucket/Epoch

	if err := tr.Delete("team-alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := tr.SpentUSD(context.Background(), "team-alpha", 0); !got.IsZero() {
		t.Errorf("SpentUSD after Delete = %s, want 0", got)
	}
	// A fresh Reserve for the same ID must behave like a brand-new key
	// (full-headroom cold-start reservation, epoch 0) -- not carry over
	// any of the purged state.
	_, reserved, reservedUSD, epoch, _ = tr.Reserve(context.Background(), "team-alpha", d("100"), time.Hour)
	if !reserved {
		t.Fatal("Reserve after Delete did not reserve anything")
	}
	if epoch != 0 {
		t.Errorf("reservationEpoch after Delete = %d, want 0 (a fresh key, not carried-over rolling-window state)", epoch)
	}
	if !reservedUSD.Equal(d("100")) {
		t.Errorf("reservedUSD after Delete = %s, want 100 (cold-start full-headroom reservation, billedCount purged back to 0)", reservedUSD)
	}
}

// TestDeleteOnKeyWithNoRecordedStateIsANoOp proves Delete never errors
// for an ID it has never seen — the common case when erasing a virtual
// key that was created but never actually billed.
func TestDeleteOnKeyWithNoRecordedStateIsANoOp(t *testing.T) {
	tr := NewTracker()
	if err := tr.Delete("never-seen"); err != nil {
		t.Errorf("Delete on an unrecorded key = %v, want nil", err)
	}
}

// TestDeleteCallsThroughToStore proves the durable half: when a Store is
// configured, Delete removes the persisted entry too, not just the
// in-memory maps.
func TestDeleteCallsThroughToStore(t *testing.T) {
	store := newFakeStore(map[string]State{"team-alpha": {Spent: d("5")}})
	tr, err := NewTrackerWithStore(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("NewTrackerWithStore: %v", err)
	}

	if err := tr.Delete("team-alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(store.deleteCalls) != 1 || store.deleteCalls[0] != "team-alpha" {
		t.Errorf("store.deleteCalls = %v, want exactly [\"team-alpha\"]", store.deleteCalls)
	}
	if _, ok := store.data["team-alpha"]; ok {
		t.Error("store.data still has \"team-alpha\" after Delete — the durable entry was not actually removed")
	}
}
