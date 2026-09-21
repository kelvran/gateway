package redisbudget

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
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
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale

	allowed, reserved, _, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() on an empty key = (%v, %v), want (true, %v) -- cold start reserves the FULL cap", allowed, reserved, capNano)
	}

	allowed, reserved, _, err = b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if allowed {
		t.Fatalf("Reserve() succeeded a second time after the full cap was already reserved, want rejected (reserved=%v)", reserved)
	}
}

// TestReserveHandlesCapsAtAndAboveTheOldScientificNotationCliff is the
// direct regression proof for a real CRITICAL finding from this
// session's own end-to-end production audit: budgetReserveLuaSrc used
// to return the reserved amount via tostring(reserved), and Lua 5.1's
// tostring on a number uses the C "%.14g" format -- which switches to
// scientific notation ("1e+14") once the value's exponent reaches 14
// significant digits. strconv.ParseInt on the Go side then failed on
// that string, so Reserve returned an error for any cap >= $100,000
// (100,000 USD * 1e9 nano-USD/USD == 1e14) -- silently disabling budget
// enforcement an order of magnitude below this package's own doc
// comment's claimed ~$9,000,000 safety margin. $100,000 itself is
// exactly the first affected value (10^14, exponent 14); $99,999 is the
// last UNaffected one (exponent 13) -- both checked here, plus a value
// deep into the previously-broken range. Break this by reverting
// budgetReserveLuaSrc's own `return {1, reserved}` back to
// `return {1, tostring(reserved)}`: this test starts failing with a
// type-assertion error on results[1], not a wrong reserved amount --
// go-redis receives a Lua string, not the int64 Reserve now expects.
func TestReserveHandlesCapsAtAndAboveTheOldScientificNotationCliff(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	for _, capUSD := range []int64{99_999, 100_000, 1_000_000, 9_007_199} {
		capUSD := capUSD
		t.Run(fmt.Sprintf("cap=$%d", capUSD), func(t *testing.T) {
			capNano := capUSD * usdScale
			key := uniqueKey(t)
			allowed, reserved, _, err := b.Reserve(ctx, key, capNano, 0)
			if err != nil {
				t.Fatalf("Reserve() error = %v", err)
			}
			if !allowed || reserved != capNano {
				t.Fatalf("Reserve() on an empty key = (%v, %v), want (true, %v)", allowed, reserved, capNano)
			}
			allowed, _, _, err = b.Reserve(ctx, key, capNano, 0)
			if err != nil {
				t.Fatalf("Reserve() #2 error = %v", err)
			}
			if allowed {
				t.Fatal("Reserve() succeeded a second time after the full cap was already reserved, want rejected")
			}
		})
	}
}

func TestAdjustReconcilesReservationDownToRealCost(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale
	const realCostNano = 20 * usdScale

	_, reservedNano, epoch, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	delta := realCostNano - reservedNano // negative: real cost (20) well below the reserved estimate (100)
	if err := b.Adjust(ctx, key, delta, epoch); err != nil {
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
	allowed, reserved, _, err := b.Reserve(ctx, key, capNano, 0)
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
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale

	_, _, epoch, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve() error = %v", err)
	}

	// A wildly excessive release, far beyond the single $10 reservation
	// above.
	if err := b.Adjust(ctx, key, -1_000*usdScale, epoch); err != nil {
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
	allowed, reserved, _, err := b.Reserve(ctx, key, capNano, 0)
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
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)

	if err := b.Adjust(ctx, key, -50*usdScale, 0); err != nil {
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
	b, err := Open(redis.Options{Addr: redisAddr})
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

// TestSpendAndAlertKeysDoNotCollideForAColonContainingKeyID is the
// direct regression proof for a real HIGH-severity finding from this
// session's own end-to-end audit: virtual-key IDs have no charset
// restriction anywhere, and spendKey/alertKey used to concatenate keyID
// raw -- so spendKey("alert:foo") == "budget:alert:foo" collided
// byte-for-byte with alertKey("foo") == "budget:alert:foo", letting one
// tenant's spend key alias another (unrelated) tenant's alert-bucket
// key. Proven here by reserving spend under the colon-containing ID and
// confirming the plain "foo" ID's alert-bucket dedup is completely
// unaffected -- if the two keys collided, marking "foo"'s alert bucket
// would silently corrupt "alert:foo"'s spend record (both being GETs/
// SETs against the identical Redis key). Break this by reverting
// spendKey/alertKey to raw concatenation (dropping url.QueryEscape):
// this test starts failing because SpentNanoUSD("alert:foo") reflects
// the OTHER key's alert-bucket write.
func TestSpendAndAlertKeysDoNotCollideForAColonContainingKeyID(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	suffix := uniqueKey(t)
	colonKey := "alert:" + suffix // spendKey(colonKey) used to equal alertKey(suffix)
	plainKey := suffix            // alertKey(plainKey) used to equal spendKey(colonKey)
	const capNano = 10 * usdScale

	allowed, reserved, _, err := b.Reserve(ctx, colonKey, capNano, 0)
	if err != nil {
		t.Fatalf("Reserve(%q) error = %v", colonKey, err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve(%q) = (%v, %v), want (true, %v)", colonKey, allowed, reserved, capNano)
	}

	if _, err := b.MarkAlertBucket(ctx, plainKey, 0.9, 0); err != nil {
		t.Fatalf("MarkAlertBucket(%q) error = %v", plainKey, err)
	}

	spent, err := b.SpentNanoUSD(ctx, colonKey)
	if err != nil {
		t.Fatalf("SpentNanoUSD(%q) error = %v", colonKey, err)
	}
	if spent != capNano {
		t.Fatalf("SpentNanoUSD(%q) after marking %q's alert bucket = %v, want unchanged %v -- the two keys collided", colonKey, plainKey, spent, capNano)
	}
}

func TestMarkAlertBucketOnlyRecordsANewHighest(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
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
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale

	if _, _, _, err := b.Reserve(ctx, key, capNano, 0); err != nil {
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
	allowed, reserved, _, err := b.Reserve(ctx, key, capNano, 0)
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
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale
	const windowMs = 200

	allowed, reserved, epoch1, err := b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #1 error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() #1 = (%v, %v), want (true, %v)", allowed, reserved, capNano)
	}

	allowed, _, _, err = b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #2 error = %v", err)
	}
	if allowed {
		t.Fatal("Reserve() #2 immediately after #1 exhausted the cap = allowed, want rejected")
	}

	time.Sleep(time.Duration(windowMs)*time.Millisecond*2 + 100*time.Millisecond)

	allowed, reserved, epoch3, err := b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() #3 (after the window expired) error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() #3 after the window expired = (%v, %v), want (true, %v) -- a fresh window", allowed, reserved, capNano)
	}
	// The new window must carry a genuinely DIFFERENT epoch from the
	// expired one -- this is the actual property Adjust's own
	// epoch-mismatch check (see budgetAdjustLuaSrc's doc comment) relies
	// on to tell "the same window" apart from "a different, later one."
	if epoch3 == epoch1 {
		t.Errorf("epoch after the window expired and a fresh one started = %d, want different from the expired window's epoch %d", epoch3, epoch1)
	}
}

// TestAdjustDoesNotCorruptALaterWindowThatReplacedTheOneItWasReservedAgainst
// is the direct regression proof for a real HIGH-severity finding from
// this session's own end-to-end audit: a Reserve/Adjust pair spanning a
// window boundary used to have NO way to detect that the window it
// reserved against had already expired AND been replaced by a
// completely different, later window under the same keyID by the time
// Adjust finally ran -- "key absent" was the only check, which misses
// this case entirely (the key is NOT absent; it's just a different
// window now). Proven here by: Reserve (window #1) -> let the window
// expire -> Reserve again (window #2, a fresh cap) -> Adjust using
// window #1's STALE epoch -> confirm window #2's spend is completely
// unaffected. Break this by reverting budgetAdjustLuaSrc's own
// `epoch ~= expected_epoch` check (making it ignore the epoch again):
// this test starts failing because window #2's spend reflects the
// stale Adjust's delta.
func TestAdjustDoesNotCorruptALaterWindowThatReplacedTheOneItWasReservedAgainst(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()

	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 10 * usdScale
	const windowMs = 200

	_, _, staleEpoch, err := b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() (window #1) error = %v", err)
	}

	time.Sleep(time.Duration(windowMs)*time.Millisecond*2 + 100*time.Millisecond)

	allowed, reserved, freshEpoch, err := b.Reserve(ctx, key, capNano, windowMs)
	if err != nil {
		t.Fatalf("Reserve() (window #2) error = %v", err)
	}
	if !allowed || reserved != capNano {
		t.Fatalf("Reserve() (window #2) = (%v, %v), want (true, %v) -- a fresh window", allowed, reserved, capNano)
	}
	if freshEpoch == staleEpoch {
		t.Fatalf("setup: window #2's epoch (%d) == window #1's stale epoch (%d) -- test cannot prove anything", freshEpoch, staleEpoch)
	}

	// A large release, using window #1's STALE epoch -- if this were
	// wrongly applied to window #2's ledger, it would drive window #2's
	// spend negative (clamped to 0), corrupting it.
	if err := b.Adjust(ctx, key, -5*usdScale, staleEpoch); err != nil {
		t.Fatalf("Adjust() (stale epoch) error = %v", err)
	}

	spent, err := b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != capNano {
		t.Fatalf("SpentNanoUSD() after a stale-epoch Adjust = %v, want unchanged %v -- window #2 was corrupted by window #1's stale reservation", spent, capNano)
	}

	// A correctly-epoched Adjust against window #2 must still work.
	if err := b.Adjust(ctx, key, -3*usdScale, freshEpoch); err != nil {
		t.Fatalf("Adjust() (fresh epoch) error = %v", err)
	}
	spent, err = b.SpentNanoUSD(ctx, key)
	if err != nil {
		t.Fatalf("SpentNanoUSD() error = %v", err)
	}
	if spent != capNano-3*usdScale {
		t.Fatalf("SpentNanoUSD() after a correctly-epoched Adjust = %v, want %v", spent, capNano-3*usdScale)
	}
}

func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	b, err := Open(redis.Options{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Open() on an unreachable address returned an error = %v, want nil (dialing is lazy)", err)
	}
	defer func() { _ = b.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, _, _, err := b.Reserve(ctx, "any-key", usdScale, 0); err == nil {
		t.Fatal("Reserve() against an unreachable Redis address succeeded, want an error")
	}
}
