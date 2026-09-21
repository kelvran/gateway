package budget

// Tracker-level tests for the Redis-mode branches (NewRedisTracker) of
// Reserve/Reconcile/IncreaseReservation/SpentUSD/
// CheckAndMarkBudgetAlertBucket/Delete/Store/Close, against a fake
// RedisBackend — mirrors internal/ratelimit's own fakeBackend test
// pattern for the identical Redis-mode-vs-in-memory-mode split. Real
// Redis correctness for the Lua scripts themselves lives in
// internal/budget/redisbudget's own test file; this file proves
// budget.Tracker's OWN Redis-mode integration logic (USD<->nanoUSD
// conversion, fail-open contracts, epoch-elision) in isolation.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type fakeRedisBudgetBackend struct {
	reserveFunc         func(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (bool, int64, int64, error)
	reserveFixedFunc    func(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (bool, error)
	adjustFunc          func(ctx context.Context, keyID string, deltaNanoUSD, epoch int64) error
	spentNanoUSDFunc    func(ctx context.Context, keyID string) (int64, error)
	markAlertBucketFunc func(ctx context.Context, keyID string, newHighest float64, resetIntervalMs int64) (bool, error)
	deleteFunc          func(ctx context.Context, keyID string) error
	closeFunc           func() error

	reserveCalls         int
	lastReserveCapNano   int64
	lastReserveResetMs   int64
	reserveFixedCalls    int
	lastFixedDeltaNano   int64
	adjustCalls          int
	lastAdjustDeltaNano  int64
	lastAdjustEpoch      int64
	markAlertCalls       int
	lastMarkAlertHighest float64
	deleteCalls          int
	closeCalls           int
}

func (f *fakeRedisBudgetBackend) Reserve(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (bool, int64, int64, error) {
	f.reserveCalls++
	f.lastReserveCapNano = capNanoUSD
	f.lastReserveResetMs = resetIntervalMs
	if f.reserveFunc != nil {
		return f.reserveFunc(ctx, keyID, capNanoUSD, resetIntervalMs)
	}
	return true, capNanoUSD, 0, nil
}

func (f *fakeRedisBudgetBackend) ReserveFixed(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (bool, error) {
	f.reserveFixedCalls++
	f.lastFixedDeltaNano = deltaNanoUSD
	if f.reserveFixedFunc != nil {
		return f.reserveFixedFunc(ctx, keyID, capNanoUSD, deltaNanoUSD, resetIntervalMs)
	}
	return true, nil
}

func (f *fakeRedisBudgetBackend) Adjust(ctx context.Context, keyID string, deltaNanoUSD, epoch int64) error {
	f.adjustCalls++
	f.lastAdjustDeltaNano = deltaNanoUSD
	f.lastAdjustEpoch = epoch
	if f.adjustFunc != nil {
		return f.adjustFunc(ctx, keyID, deltaNanoUSD, epoch)
	}
	return nil
}

func (f *fakeRedisBudgetBackend) SpentNanoUSD(ctx context.Context, keyID string) (int64, error) {
	if f.spentNanoUSDFunc != nil {
		return f.spentNanoUSDFunc(ctx, keyID)
	}
	return 0, nil
}

func (f *fakeRedisBudgetBackend) MarkAlertBucket(ctx context.Context, keyID string, newHighest float64, resetIntervalMs int64) (bool, error) {
	f.markAlertCalls++
	f.lastMarkAlertHighest = newHighest
	if f.markAlertBucketFunc != nil {
		return f.markAlertBucketFunc(ctx, keyID, newHighest, resetIntervalMs)
	}
	return true, nil
}

func (f *fakeRedisBudgetBackend) Delete(ctx context.Context, keyID string) error {
	f.deleteCalls++
	if f.deleteFunc != nil {
		return f.deleteFunc(ctx, keyID)
	}
	return nil
}

func (f *fakeRedisBudgetBackend) Close() error {
	f.closeCalls++
	if f.closeFunc != nil {
		return f.closeFunc()
	}
	return nil
}

func TestUSDNanoUSDConversionRoundTripsExactly(t *testing.T) {
	cases := []string{"0", "0.000000001", "1", "10.5", "0.1", "0.2", "0.3", "9999999.999999999"}
	for _, c := range cases {
		usd := d(c)
		nano := usdToNanoUSD(usd)
		back := nanoUSDToUSD(nano)
		if !back.Equal(usd) {
			t.Errorf("usdToNanoUSD(%s) -> nanoUSDToUSD = %s, want %s (exact round trip)", c, back.String(), c)
		}
	}
}

// TestUSDNanoUSDConversionAvoidsFloat64RepeatedAdditionDrift is the
// direct regression proof for why this conversion goes through
// decimal.Decimal.Shift/Round/IntPart rather than a float64 USD*1e9
// cast: repeatedly adding 0.1 in float64 arithmetic is the textbook case
// that drifts from the exact sum (0.1 + 0.2 != 0.3 in float64) --
// converting through nano-USD integers must never reintroduce that.
func TestUSDNanoUSDConversionAvoidsFloat64RepeatedAdditionDrift(t *testing.T) {
	sum := decimal.Zero
	for i := 0; i < 10; i++ {
		sum = sum.Add(d("0.1"))
	}
	nano := usdToNanoUSD(sum)
	if nano != 1*usdScaleForTest {
		t.Errorf("usdToNanoUSD(0.1 summed 10 times) = %d, want %d (exactly $1, no drift)", nano, usdScaleForTest)
	}
}

const usdScaleForTest = 1_000_000_000

func TestReserveRedisModeConvertsUnitsCorrectly(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		reserveFunc: func(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (bool, int64, int64, error) {
			return true, capNanoUSD, 777, nil
		},
	}
	tr := NewRedisTracker(backend, nil)

	allowed, reserved, reservedUSD, epoch, err := tr.Reserve(context.Background(), "k", d("12.5"), 30*time.Second)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !allowed || !reserved {
		t.Fatalf("Reserve() = (%v, %v), want (true, true)", allowed, reserved)
	}
	if backend.lastReserveCapNano != 12_500_000_000 {
		t.Errorf("backend received capNanoUSD = %d, want %d (12.5 USD in nano-USD)", backend.lastReserveCapNano, 12_500_000_000)
	}
	if backend.lastReserveResetMs != 30_000 {
		t.Errorf("backend received resetIntervalMs = %d, want 30000 (30s in ms)", backend.lastReserveResetMs)
	}
	if !reservedUSD.Equal(d("12.5")) {
		t.Errorf("reservedUSD = %s, want 12.5 (nano-USD converted back exactly)", reservedUSD.String())
	}
	// epoch must be threaded straight through from the backend, unchanged
	// -- Reconcile's Redis-mode branch now genuinely depends on the exact
	// value RedisBackend.Reserve returned (see that method's own doc
	// comment for the cross-window corruption this closes).
	if epoch != 777 {
		t.Errorf("reservationEpoch = %d, want 777 (passed straight through from the backend)", epoch)
	}
}

func TestReserveRedisModeUnlimitedCapNeverCallsBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	allowed, reserved, reservedUSD, _, err := tr.Reserve(context.Background(), "k", d("0"), time.Hour)
	if err != nil || !allowed || reserved || !reservedUSD.IsZero() {
		t.Fatalf("Reserve(cap=0) = (%v, %v, %v, err=%v), want (true, false, 0, nil)", allowed, reserved, reservedUSD, err)
	}
	if backend.reserveCalls != 0 {
		t.Errorf("backend.Reserve called %d times for an unlimited (cap<=0) key, want 0", backend.reserveCalls)
	}
}

func TestReserveRedisModeRejectionAppliesNoReservation(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		reserveFunc: func(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (bool, int64, int64, error) {
			return false, 0, 0, nil
		},
	}
	tr := NewRedisTracker(backend, nil)

	allowed, reserved, reservedUSD, _, err := tr.Reserve(context.Background(), "k", d("10"), 0)
	if err != nil || allowed || reserved || !reservedUSD.IsZero() {
		t.Fatalf("Reserve() on a rejected backend call = (%v, %v, %v, err=%v), want (false, false, 0, nil)", allowed, reserved, reservedUSD, err)
	}
}

// TestReserveRedisModeBackendErrorReturnsAllowedFalse proves the exact
// contract callers (dataplane.go) rely on to implement fail-open
// themselves: a backend error surfaces as allowed=false plus a non-nil
// err -- a caller that forgets to check err would incorrectly treat this
// as a real budget-exceeded rejection, which is exactly why every real
// call site explicitly checks err first and overrides to fail-open, per
// internal/ratelimit's identical ReserveTPM contract.
func TestReserveRedisModeBackendErrorReturnsAllowedFalse(t *testing.T) {
	wantErr := errors.New("simulated redis outage")
	backend := &fakeRedisBudgetBackend{
		reserveFunc: func(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (bool, int64, int64, error) {
			return false, 0, 0, wantErr
		},
	}
	tr := NewRedisTracker(backend, nil)

	allowed, reserved, _, _, err := tr.Reserve(context.Background(), "k", d("10"), 0)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Reserve() err = %v, want %v", err, wantErr)
	}
	if allowed || reserved {
		t.Fatalf("Reserve() on backend error = (allowed=%v, reserved=%v), want (false, false) -- caller must fail open explicitly", allowed, reserved)
	}
}

func TestReconcileRedisModeBilledAppliesRealCostMinusReservedDelta(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	realCost := d("2")
	tr.Reconcile(context.Background(), "k", d("10"), 0, &realCost, 0)

	if backend.adjustCalls != 1 {
		t.Fatalf("backend.Adjust called %d times, want 1", backend.adjustCalls)
	}
	wantDelta := usdToNanoUSD(d("2").Sub(d("10"))) // realCost - reservedUSD = -8
	if backend.lastAdjustDeltaNano != wantDelta {
		t.Errorf("Adjust delta = %d, want %d (realCost=2 - reserved=10, in nano-USD)", backend.lastAdjustDeltaNano, wantDelta)
	}
}

func TestReconcileRedisModeReleaseOnlyAppliesNegativeReservedDelta(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	tr.Reconcile(context.Background(), "k", d("7"), 0, nil, 0)

	if backend.adjustCalls != 1 {
		t.Fatalf("backend.Adjust called %d times, want 1", backend.adjustCalls)
	}
	wantDelta := usdToNanoUSD(d("0").Sub(d("7")))
	if backend.lastAdjustDeltaNano != wantDelta {
		t.Errorf("Adjust delta on a release-only Reconcile = %d, want %d (-reservedUSD)", backend.lastAdjustDeltaNano, wantDelta)
	}
}

// TestReconcileRedisModePassesReservationEpochThroughToBackend is the
// direct regression proof for a real HIGH-severity finding from this
// session's own end-to-end audit: Reconcile's Redis-mode branch used to
// hardcode/ignore reservationEpoch entirely (Adjust had no epoch
// parameter at all) -- meaning a stale reservation that outlived its
// own window's TTL could have its delta applied to an unrelated LATER
// window that happened to reuse the same Redis key, since "key absent"
// alone can't distinguish "this window is gone" from "this window was
// REPLACED." Reconcile now threads reservationEpoch straight through to
// backend.Adjust unchanged -- proven here at the Tracker level (the
// actual epoch-mismatch enforcement itself is redisbudget's own Lua
// script, proven separately against real Redis in that package's test
// file). Break this by reverting Reconcile's own call site back to
// t.backend.Adjust(ctx, keyID, usdToNanoUSD(delta)) (dropping
// reservationEpoch): this test starts failing on lastAdjustEpoch.
func TestReconcileRedisModePassesReservationEpochThroughToBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	realCost := d("1")
	tr.Reconcile(context.Background(), "k", d("5"), 123_456, &realCost, 0)

	if backend.adjustCalls != 1 {
		t.Fatalf("backend.Adjust called %d times, want 1", backend.adjustCalls)
	}
	if backend.lastAdjustEpoch != 123_456 {
		t.Errorf("backend.Adjust received epoch = %d, want 123456 (reservationEpoch passed straight through unchanged)", backend.lastAdjustEpoch)
	}
}

func TestReconcileRedisModeLogsAndSwallowsBackendError(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		adjustFunc: func(ctx context.Context, keyID string, deltaNanoUSD, epoch int64) error {
			return errors.New("simulated redis outage")
		},
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	tr := NewRedisTracker(backend, logger)

	realCost := d("1")
	tr.Reconcile(context.Background(), "k", d("5"), 0, &realCost, 0) // must not panic

	if !bytes.Contains(logBuf.Bytes(), []byte("budget_redis_backend_unavailable")) {
		t.Errorf("log output = %q, want it to contain \"budget_redis_backend_unavailable\"", logBuf.String())
	}
}

func TestIncreaseReservationRedisModeUsesReserveFixedWithTheDelta(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		reserveFixedFunc: func(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (bool, error) {
			return true, nil
		},
	}
	tr := NewRedisTracker(backend, nil)

	allowed, applied, epoch, err := tr.IncreaseReservation(context.Background(), "k", d("20"), d("5"), d("8"), 42, 0)
	if err != nil || !allowed {
		t.Fatalf("IncreaseReservation() = (%v, err=%v), want (true, nil)", allowed, err)
	}
	if !applied.Equal(d("8")) {
		t.Errorf("appliedUSD = %s, want 8 (the new reservation)", applied.String())
	}
	if epoch != 42 {
		t.Errorf("newReservationEpoch = %d, want 42 (returned unchanged -- a top-up never creates a new window, so the same epoch must still reach the eventual Reconcile call)", epoch)
	}
	wantDelta := usdToNanoUSD(d("3")) // 8 - 5
	if backend.lastFixedDeltaNano != wantDelta {
		t.Errorf("ReserveFixed delta = %d, want %d (newReserved=8 - currentReserved=5)", backend.lastFixedDeltaNano, wantDelta)
	}
}

func TestIncreaseReservationRedisModeNoOpWhenNotGreaterNeverCallsBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	allowed, applied, epoch, err := tr.IncreaseReservation(context.Background(), "k", d("20"), d("8"), d("8"), 3, 0)
	if err != nil || !allowed || !applied.Equal(d("8")) || epoch != 3 {
		t.Fatalf("IncreaseReservation(new==current) = (%v, %v, %v, err=%v), want (true, 8, 3, nil)", allowed, applied, epoch, err)
	}
	if backend.reserveFixedCalls != 0 {
		t.Errorf("backend.ReserveFixed called %d times for a no-op top-up, want 0", backend.reserveFixedCalls)
	}
}

// TestIncreaseReservationRedisModeFailsOpenReturnsUnchangedOnError
// mirrors internal/ratelimit.KeyLimiter.IncreaseReservationTPM's own
// exact contract on a backend error: allowed=true and
// appliedUSD/newReservationEpoch UNCHANGED (never the failed new
// value) -- the caller (checkMidStreamReservationTopup) relies on this
// to keep a stream's prior reservation floor rather than pretending a
// real top-up happened.
func TestIncreaseReservationRedisModeFailsOpenReturnsUnchangedOnError(t *testing.T) {
	wantErr := errors.New("simulated redis outage")
	backend := &fakeRedisBudgetBackend{
		reserveFixedFunc: func(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (bool, error) {
			return false, wantErr
		},
	}
	tr := NewRedisTracker(backend, nil)

	allowed, applied, epoch, err := tr.IncreaseReservation(context.Background(), "k", d("20"), d("5"), d("8"), 7, 0)
	if !errors.Is(err, wantErr) {
		t.Fatalf("IncreaseReservation() err = %v, want %v", err, wantErr)
	}
	if !allowed {
		t.Error("IncreaseReservation() allowed = false on backend error, want true (fail open)")
	}
	if !applied.Equal(d("5")) || epoch != 7 {
		t.Errorf("IncreaseReservation() on backend error = (applied=%v, epoch=%v), want (5, 7) -- unchanged from currentReservedUSD/reservationEpoch", applied, epoch)
	}
}

func TestSpentUSDRedisModeConvertsUnitsAndFailsOpenOnError(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		spentNanoUSDFunc: func(ctx context.Context, keyID string) (int64, error) {
			return 3_500_000_000, nil
		},
	}
	tr := NewRedisTracker(backend, nil)

	spent := tr.SpentUSD(context.Background(), "k", 0)
	if !spent.Equal(d("3.5")) {
		t.Errorf("SpentUSD() = %s, want 3.5", spent.String())
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	errBackend := &fakeRedisBudgetBackend{
		spentNanoUSDFunc: func(ctx context.Context, keyID string) (int64, error) {
			return 0, errors.New("simulated redis outage")
		},
	}
	tr2 := NewRedisTracker(errBackend, logger)
	spent = tr2.SpentUSD(context.Background(), "k", 0)
	if !spent.IsZero() {
		t.Errorf("SpentUSD() on backend error = %s, want 0 (fail open, never blocks)", spent.String())
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("budget_redis_backend_unavailable")) {
		t.Errorf("log output = %q, want it to contain \"budget_redis_backend_unavailable\"", logBuf.String())
	}
}

func TestCheckAndMarkBudgetAlertBucketRedisModeComputesBucketBeforeCallingBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "k", 0.95, time.Hour)
	if !crossed || bucket != 0.9 {
		t.Fatalf("CheckAndMarkBudgetAlertBucket(0.95) = (%v, %v), want (0.9, true)", bucket, crossed)
	}
	if backend.markAlertCalls != 1 || backend.lastMarkAlertHighest != 0.9 {
		t.Errorf("backend.MarkAlertBucket called %d times with highest=%v, want 1 call with 0.9", backend.markAlertCalls, backend.lastMarkAlertHighest)
	}
}

func TestCheckAndMarkBudgetAlertBucketRedisModeBelowLowestNeverCallsBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "k", 0.2, 0)
	if crossed || bucket != 0 {
		t.Fatalf("CheckAndMarkBudgetAlertBucket(0.2) = (%v, %v), want (0, false)", bucket, crossed)
	}
	if backend.markAlertCalls != 0 {
		t.Errorf("backend.MarkAlertBucket called %d times below the lowest bucket, want 0", backend.markAlertCalls)
	}
}

func TestCheckAndMarkBudgetAlertBucketRedisModeFailsOpenOnBackendError(t *testing.T) {
	backend := &fakeRedisBudgetBackend{
		markAlertBucketFunc: func(ctx context.Context, keyID string, newHighest float64, resetIntervalMs int64) (bool, error) {
			return false, errors.New("simulated redis outage")
		},
	}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	tr := NewRedisTracker(backend, logger)

	bucket, crossed := tr.CheckAndMarkBudgetAlertBucket(context.Background(), "k", 0.9, 0)
	if crossed || bucket != 0 {
		t.Fatalf("CheckAndMarkBudgetAlertBucket() on backend error = (%v, %v), want (0, false) -- fail open, never alert on error", bucket, crossed)
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("budget_redis_backend_unavailable")) {
		t.Errorf("log output = %q, want it to contain \"budget_redis_backend_unavailable\"", logBuf.String())
	}
}

func TestDeleteRedisModeRoutesToBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	if err := tr.Delete("k"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if backend.deleteCalls != 1 {
		t.Errorf("backend.Delete called %d times, want 1", backend.deleteCalls)
	}
}

func TestStoreRedisModeReturnsNil(t *testing.T) {
	tr := NewRedisTracker(&fakeRedisBudgetBackend{}, nil)
	if got := tr.Store(); got != nil {
		t.Errorf("Store() on a Redis-mode Tracker = %v, want nil (nothing bbolt-backed to back up)", got)
	}
}

func TestCloseRedisModeRoutesToBackend(t *testing.T) {
	backend := &fakeRedisBudgetBackend{}
	tr := NewRedisTracker(backend, nil)

	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if backend.closeCalls != 1 {
		t.Errorf("backend.Close called %d times, want 1", backend.closeCalls)
	}
}
