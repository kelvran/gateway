package budget

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Round-5 item 10 phase 2c-1: the Redis-mode unit conversions and reply
// mappings Tracker.ReserveWithSpend and Tracker.Reconcile apply are public
// pure functions, so a fused call over a shared Redis client reuses them.

func TestPlanReserveConvertsTheCapAndTheResetInterval(t *testing.T) {
	capNano, resetMs := PlanReserve(decimal.RequireFromString("12.5"), 30*time.Second)
	if capNano != 12_500_000_000 || resetMs != 30_000 {
		t.Fatalf("PlanReserve(12.5 USD, 30 s) = (%d, %d), want (12 500 000 000, 30 000)", capNano, resetMs)
	}
	if capNano, resetMs = PlanReserve(decimal.Zero, 0); capNano != 0 || resetMs != 0 {
		t.Fatalf("PlanReserve(uncapped, no reset) = (%d, %d), want (0, 0)", capNano, resetMs)
	}
}

func TestMapReservationAppliesTheUncappedAndRefusedRules(t *testing.T) {
	cap10 := decimal.NewFromInt(10)
	cases := []struct {
		name     string
		cap      decimal.Decimal
		allowed  bool
		reserved int64
		epoch    int64
		before   int64
		want     Reservation
	}{
		{"admitted", cap10, true, 1_000_000_000, 7, 3_000_000_000, Reservation{Allowed: true, Reserved: true, ReservedUSD: decimal.NewFromInt(1), Epoch: 7, SpentBeforeUSD: decimal.NewFromInt(3)}},
		{"refused", cap10, false, 0, 7, 10_000_000_000, Reservation{SpentBeforeUSD: decimal.NewFromInt(10)}},
		{"uncapped is allowed and reserves nothing", decimal.Zero, true, 0, 7, 5_000_000_000, Reservation{Allowed: true, Epoch: 7, SpentBeforeUSD: decimal.NewFromInt(5)}},
		{"uncapped ignores a stray refusal flag", decimal.Zero, false, 0, 7, 0, Reservation{Allowed: true, Epoch: 7, SpentBeforeUSD: decimal.Zero}},
	}
	for _, c := range cases {
		got := MapReservation(c.cap, c.allowed, c.reserved, c.epoch, c.before)
		if got.Allowed != c.want.Allowed || got.Reserved != c.want.Reserved || got.Epoch != c.want.Epoch || !got.ReservedUSD.Equal(c.want.ReservedUSD) || !got.SpentBeforeUSD.Equal(c.want.SpentBeforeUSD) {
			t.Errorf("%s: MapReservation = %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestPlanReconcileIsTheSignedDeltaAndTheBilledFlag(t *testing.T) {
	cost := decimal.RequireFromString("0.4")
	neg := decimal.RequireFromString("-1")
	cases := []struct {
		name       string
		reserved   decimal.Decimal
		real       *decimal.Decimal
		wantDelta  int64
		wantBilled bool
	}{
		{"billed below the reservation", decimal.NewFromInt(1), &cost, -600_000_000, true},
		{"billed above the reservation", decimal.RequireFromString("0.1"), &cost, 300_000_000, true},
		{"a release", decimal.NewFromInt(1), nil, -1_000_000_000, false},
		{"a negative cost is a release", decimal.NewFromInt(1), &neg, -1_000_000_000, false},
		{"billed zero", decimal.NewFromInt(1), ptrDec(decimal.Zero), -1_000_000_000, true},
	}
	for _, c := range cases {
		delta, billed := PlanReconcile(c.reserved, c.real)
		if delta != c.wantDelta || billed != c.wantBilled {
			t.Errorf("%s: PlanReconcile = (%d, %v), want (%d, %v)", c.name, delta, billed, c.wantDelta, c.wantBilled)
		}
	}
}

func ptrDec(d decimal.Decimal) *decimal.Decimal { return &d }

func TestNoteAdjustFailureLogsTheFailure(t *testing.T) {
	var buf bytes.Buffer
	tr := NewRedisTracker(&fakeRedisBudgetBackend{}, slog.New(slog.NewTextHandler(&buf, nil)))
	tr.NoteAdjustFailure(context.Background(), "k", context.DeadlineExceeded)
	out := buf.String()
	if !strings.Contains(out, "budget_redis_backend_unavailable") || !strings.Contains(out, "op=reconcile") || !strings.Contains(out, "key_id=k") {
		t.Fatalf("log = %q, want budget_redis_backend_unavailable op=reconcile key_id=k", out)
	}
}
