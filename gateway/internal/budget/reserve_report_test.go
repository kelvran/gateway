package budget

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/shopspring/decimal"
)

// Round-5 item 10 phase 2a: Reserve reports the spend it decided against,
// in one lock acquisition in memory and one script in Redis, so the
// decision event's budget_spent_usd no longer needs its own read.
func TestReserveWithSpendReportsThePriorSpendInMemory(t *testing.T) {
	tr := NewTracker()
	ctx := context.Background()
	cap := decimal.NewFromInt(10)

	r1, err := tr.ReserveWithSpend(ctx, "k", cap, 0)
	if err != nil || !r1.Allowed || !r1.Reserved || !r1.SpentBeforeUSD.IsZero() || !r1.ReservedUSD.Equal(cap) {
		t.Fatalf("first reservation = %+v (%v), want allowed, reserved, spent-before 0, the full 10 USD headroom", r1, err)
	}
	cost := decimal.NewFromInt(2)
	after, known := tr.Reconcile(ctx, "k", r1.ReservedUSD, r1.Epoch, &cost, 0)
	if !known || !after.Equal(cost) {
		t.Fatalf("Reconcile reports (%s, %v), want (2, true)", after, known)
	}
	r2, err := tr.ReserveWithSpend(ctx, "k", cap, 0)
	if err != nil || !r2.SpentBeforeUSD.Equal(cost) {
		t.Fatalf("second reservation reports spent-before %s (%v), want 2: the spend before THIS reservation", r2.SpentBeforeUSD, err)
	}
	if !r2.ReservedUSD.Equal(cost) {
		t.Fatalf("second reservation = %s, want the 2 USD mean", r2.ReservedUSD)
	}
	// The five-value Reserve is the same decision without the spend: with r2
	// still in flight the window holds 4 USD over one settlement, so the mean
	// -- and the reservation -- is 4 USD (FAILURE-MODES, known gaps).
	allowed, reserved, reservedUSD, epoch, err := tr.Reserve(ctx, "k", cap, 0)
	if err != nil || !allowed || !reserved || !reservedUSD.Equal(decimal.NewFromInt(4)) || epoch != r2.Epoch {
		t.Fatalf("Reserve = (%v, %v, %s, %d, %v), want (true, true, 4, r2's epoch, nil)", allowed, reserved, reservedUSD, epoch, err)
	}
}

func TestReserveWithSpendReportsThePriorSpendFromTheRedisBackend(t *testing.T) {
	fake := &fakeRedisBudgetBackend{reserveFunc: func(_ context.Context, _ string, capNanoUSD, _ int64) (bool, int64, int64, int64, error) {
		if capNanoUSD <= 0 {
			return true, 0, 7, 5_000_000_000, nil
		}
		return true, 1_000_000_000, 7, 3_000_000_000, nil
	}}
	tr := NewRedisTracker(fake, slog.New(slog.DiscardHandler))
	ctx := context.Background()

	r, err := tr.ReserveWithSpend(ctx, "k", decimal.NewFromInt(10), 0)
	if err != nil || !r.Allowed || !r.Reserved || r.Epoch != 7 || !r.ReservedUSD.Equal(decimal.NewFromInt(1)) || !r.SpentBeforeUSD.Equal(decimal.NewFromInt(3)) {
		t.Fatalf("capped reservation = %+v (%v), want reserved 1 USD, epoch 7, spent-before 3 USD", r, err)
	}
	r, err = tr.ReserveWithSpend(ctx, "k", decimal.Zero, 0)
	if err != nil || !r.Allowed || r.Reserved || r.Epoch != 7 || !r.SpentBeforeUSD.Equal(decimal.NewFromInt(5)) {
		t.Fatalf("uncapped reservation = %+v (%v), want allowed, not reserved, epoch 7, spent-before 5 USD", r, err)
	}
	fake.reserveFunc = func(context.Context, string, int64, int64) (bool, int64, int64, int64, error) {
		return false, 0, 0, 0, errors.New("redis down")
	}
	if _, err := tr.ReserveWithSpend(ctx, "k", decimal.NewFromInt(10), 0); err == nil {
		t.Fatal("a backend error must surface so the dataplane can fail open")
	}
}

func TestReconcileReportsTheSettledSpend(t *testing.T) {
	fake := &fakeRedisBudgetBackend{adjustFunc: func(context.Context, string, int64, int64) (int64, error) {
		return 9_000_000_000, nil
	}}
	tr := NewRedisTracker(fake, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	cost := decimal.NewFromInt(1)

	after, known := tr.Reconcile(ctx, "k", decimal.NewFromInt(4), 7, &cost, 0)
	if !known || !after.Equal(decimal.NewFromInt(9)) {
		t.Fatalf("Redis Reconcile reports (%s, %v), want (9, true): the backend's post-adjust spend", after, known)
	}
	fake.adjustFunc = func(context.Context, string, int64, int64) (int64, error) {
		return 0, errors.New("redis down")
	}
	if after, known := tr.Reconcile(ctx, "k", decimal.NewFromInt(4), 7, &cost, 0); known || !after.IsZero() {
		t.Fatalf("Redis Reconcile on a backend error reports (%s, %v), want (0, false): the spend is unknown", after, known)
	}

	mem := NewTracker()
	_, _, reservedUSD, epoch, err := mem.Reserve(ctx, "k", decimal.NewFromInt(10), 0)
	if err != nil {
		t.Fatal(err)
	}
	if after, known := mem.Reconcile(ctx, "k", reservedUSD, epoch, &cost, 0); !known || !after.Equal(cost) {
		t.Fatalf("in-memory Reconcile reports (%s, %v), want (1, true)", after, known)
	}
	if after, known := mem.Reconcile(ctx, "k", decimal.Zero, epoch, nil, 0); !known || !after.Equal(cost) {
		t.Fatalf("in-memory release reports (%s, %v), want the unchanged 1 USD and true", after, known)
	}
}
