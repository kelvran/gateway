package budget

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// TestReconcileRedisModeBackendErrorIncrementsPersistenceFailedCounter: a
// Redis-mode Reconcile whose Adjust fails is a durable-ledger write that was
// lost (the reservation stays debited at the full headroom and the real cost
// is never recorded), so it must count under kelvran.persistence.failed
// exactly like a bbolt Save failure does — before 2026-10-08 it was Warn-only.
func TestReconcileRedisModeBackendErrorIncrementsPersistenceFailedCounter(t *testing.T) {
	reader := budgetPersistenceMetricsReaderForTest()
	before := budgetPersistFailedCount(t, reader)

	backend := &fakeRedisBudgetBackend{
		adjustFunc: func(context.Context, string, int64, int64) error {
			return errors.New("simulated redis outage")
		},
	}
	tr := NewRedisTracker(backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	realCost := d("1")
	tr.Reconcile(context.Background(), "k", d("5"), 0, &realCost, 0)

	if got := budgetPersistFailedCount(t, reader) - before; got != 1 {
		t.Errorf("kelvran.persistence.failed[store_kind=budget] delta after a failed Redis reconcile = %d, want 1", got)
	}
}
