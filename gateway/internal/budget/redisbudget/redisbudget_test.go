package redisbudget

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// One real Redis container shared by every test in this file — per
// docs/testing/TESTING.md §4's "real Redis via testcontainers, never
// mocked at this layer" commitment, the same convention
// internal/ratelimit/redislimiter's own test file already establishes.
// Each test uses its own unique key (see uniqueKey) so tests never
// interfere with each other despite sharing one container.
var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("redisbudget: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("redisbudget: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	m.Run()
}

var keyCounter atomic.Uint64

// uniqueKey returns a fresh virtual-key ID per call so concurrently or
// sequentially run tests never share Redis state.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), keyCounter.Add(1))
}

const usdScale = 1_000_000_000 // nano-USD per USD, mirrors budget.go's usdToNanoUSD

func TestReserveColdStartReservesFullHeadroomThenRejects(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale

	allowed, reserved, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() on an empty key = (%v, %v), want (true, %v) -- cold start reserves the FULL cap", allowed, reserved, capNano)
	}

	allowed, reserved, err = b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if allowed {
		t.Fatalf("Reserve() succeeded a second time after the full cap was already reserved, want rejected (reserved=%v)", reserved)
	}
}

func TestAdjustReconcilesReservationDownToRealCost(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale
	const realCostNano = 20 * usdScale

	_, reservedNano, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	delta := realCostNano - reservedNano // negative: real cost (20) well below the reserved estimate (100)
	if err := b.Adjust(ctx, key, delta); err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}

	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != realCostNano {
		t.Fatalf("SpentNanoUSD() after reconciling down to the real cost = %v, want %v", spent, realCostNano)
	}

	// The freed headroom (80) must now be available to a fresh reservation.
	allowed, reserved, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if !allowed || reserved != capNano-realCostNano {
		t.Fatalf("Reserve() after reconciliation = (%v, %v), want (true, %v)", allowed, reserved, capNano-realCostNano)
	}
}

// TestAdjustClampPreventsNegativeSpend is the regression proof for
// budgetAdjustLuaSrc's own "floor of 0" contract: releasing far more than
// was ever reserved (a real caller bug, or a release-only Reconcile after
// an unusually large cold-start reservation) must not drive spend
// negative, which would otherwise manufacture free extra headroom for
// the NEXT reservation.
func TestAdjustClampPreventsNegativeSpend(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale

	if _, _, err := b.Reserve(ctx, key, capNano, 0); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	// A wildly excessive release, far beyond the single $10 reservation
	// above.
	if err := b.Adjust(ctx, key, -1_000*usdScale); err != nil {
		t.Fatalf("Adjust() error = %v", err)
	}

	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != 0 {
		t.Fatalf("SpentNanoUSD() after an excessive release = %v, want 0 (clamped, never negative)", spent)
	}

	// Exactly the original cap's worth of headroom must be available —
	// not more, which would prove the clamp failed and manufactured extra
	// capacity from the excessive release.
	allowed, reserved, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() after the clamp = (%v, %v), want (true, %v)", allowed, reserved, capNano)
	}
}

// TestAdjustOnAnAlreadyExpiredKeyIsANoOp mirrors redislimiter's identical
// AdjustTPM contract: a Reconcile call arriving after its own window
// already rolled over via Redis's own key expiration (never an explicit
// epoch) finds nothing to correct, and must not error or fabricate a
// negative-spend key from scratch.
func TestAdjustOnAnAlreadyExpiredKeyIsANoOp(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	if err := b.Adjust(ctx, key, -50*usdScale); err != nil {
		t.Fatalf("Adjust() against a never-reserved key error = %v, want nil (silent no-op)", err)
	}

	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != 0 {
		t.Fatalf("SpentNanoUSD() after Adjust on a never-reserved key = %v, want 0", spent)
	}
}

func TestReserveFixedAdmitsExactDeltaThenRejectsOverflow(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale

	allowed, err := b.ReserveFixed(ctx, key, capNano, 6*usdScale, 0)
	if err != nil {
		t.Fatalf("ReserveFixed(6) error = %v", err)
	}
	if !allowed {
		t.Fatal("ReserveFixed(6) against an empty $10 cap = rejected, want allowed")
	}

	allowed, err = b.ReserveFixed(ctx, key, capNano, 5*usdScale, 0)
	if err != nil {
		t.Fatalf("ReserveFixed(5) error = %v", err)
	}
	if allowed {
		t.Fatal("ReserveFixed(5) on top of an existing 6 against a $10 cap (11 > 10) = allowed, want rejected")
	}

	// The rejected call above must not have mutated spend at all.
	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != 6*usdScale {
		t.Fatalf("SpentNanoUSD() after a rejected ReserveFixed = %v, want %v (unchanged)", spent, 6*usdScale)
	}
}

func TestMarkAlertBucketOnlyRecordsANewHighest(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	marked, err := b.MarkAlertBucket(ctx, key, 0.5, 0)
	if err != nil {
		t.Fatalf("MarkAlertBucket(0.5) error = %v", err)
	}
	if !marked {
		t.Fatal("MarkAlertBucket(0.5) against a never-alerted key = not marked, want marked")
	}

	marked, err = b.MarkAlertBucket(ctx, key, 0.4, 0)
	if err != nil {
		t.Fatalf("MarkAlertBucket(0.4) error = %v", err)
	}
	if marked {
		t.Fatal("MarkAlertBucket(0.4) after already marking 0.5 = marked, want not marked (0.4 does not exceed 0.5)")
	}

	marked, err = b.MarkAlertBucket(ctx, key, 0.75, 0)
	if err != nil {
		t.Fatalf("MarkAlertBucket(0.75) error = %v", err)
	}
	if !marked {
		t.Fatal("MarkAlertBucket(0.75) after 0.5 was marked = not marked, want marked (0.75 exceeds 0.5)")
	}
}

func TestDeletePurgesSpendAndAlertKeys(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale

	if _, _, err := b.Reserve(ctx, key, capNano, 0); err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if _, err := b.MarkAlertBucket(ctx, key, 0.5, 0); err != nil {
		t.Fatalf("MarkAlertBucket() error = %v", err)
	}

	if err := b.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() after Delete error = %v", err)
	}
	if spent != 0 {
		t.Fatalf("SpentNanoUSD() after Delete = %v, want 0", spent)
	}

	// A fresh Reserve after Delete must see an empty key again (the full
	// cap available), proving the spend key itself -- not just its
	// value -- was actually purged, not merely zeroed.
	allowed, reserved, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() after Delete error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() after Delete = (%v, %v), want (true, %v)", allowed, reserved, capNano)
	}

	if _, err := b.MarkAlertBucket(ctx, key, 0.5, 0); err != nil {
		t.Fatalf("MarkAlertBucket() error = %v", err)
	}
	marked, err := b.MarkAlertBucket(ctx, key, 0.5, 0)
	if err != nil {
		t.Fatalf("MarkAlertBucket() error = %v", err)
	}
	if marked {
		t.Fatal("MarkAlertBucket(0.5) a second time in a row after Delete = marked, want not marked (0.5 does not exceed itself) -- the alert key must have survived the intervening Reserve/Delete calls consistently, not silently reset")
	}
}

// TestReserveWindowExpiresViaRedisTTL is the load-bearing proof for this
// package's whole "no explicit epoch, Redis's own key expiration is the
// window-reset signal" design: a short resetIntervalMs window, once it
// elapses, must let a fresh Reserve see an empty key again -- exactly
// the in-memory Tracker's resetIfNeeded's own rolling-window behavior,
// just obtained here via TTL rather than an epoch counter.
func TestReserveWindowExpiresViaRedisTTL(t *testing.T) {
	b, err := Open(redisAddr)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale
	const windowMs = 200

	allowed, reserved, err := b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #1 error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() #1 = (%v, %v), want (true, %v)", allowed, reserved, capNano)
	}

	allowed, _, err = b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if allowed {
		t.Fatal("Reserve() #2 immediately after #1 exhausted the cap = allowed, want rejected")
	}

	time.Sleep(time.Duration(windowMs)*time.Millisecond*2 + 100*time.Millisecond)

	allowed, reserved, err = b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #3 (after the window expired) error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() #3 after the window expired = (%v, %v), want (true, %v) -- a fresh window", allowed, reserved, capNano)
	}
}

func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	b, err := Open("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Open() on an unreachable address returned an error = %v, want nil (dialing is lazy)", err)
	}
	defer func() { _ = b.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, err := b.Reserve(ctx, "any-key", usdScale, 0); err == nil {
		t.Fatal("Reserve() against an unreachable Redis address succeeded, want an error")
	}
}
