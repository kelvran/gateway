package redisbudget

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Round-5 item 10 phase 2a: the reserve script reports the window's spend
// before this reservation, so the dataplane's separate HGET for the
// decision event's budget_spent_usd disappears (one round trip fewer on
// every key).
func TestReserveReportsTheSpendBeforeTheReservation(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale

	allowed, reserved, epoch, before, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil || !allowed || reserved != capNano || before != 0 {
		t.Fatalf("first Reserve = (%v, %v, %v, before %v, %v), want (true, cap, _, 0, nil): a new window has spent nothing", allowed, reserved, epoch, before, err)
	}
	if _, err := b.Adjust(ctx, key, 3*usdScale-reserved, epoch, true); err != nil {
		t.Fatal(err)
	}
	allowed, reserved, _, before, err = b.Reserve(ctx, key, capNano, 0)
	if err != nil || !allowed || before != 3*usdScale {
		t.Fatalf("Reserve after a 3 USD settlement = (%v, before %v, %v), want before = 3 USD", allowed, before, err)
	}
	if reserved != 3*usdScale {
		t.Fatalf("reserved = %v, want the 3 USD mean: the reported spend is the one the mean was taken over, before this reservation", reserved)
	}

	// The refused branch reports the spend too (it is the cap).
	full := uniqueKey(t)
	if _, _, _, _, err := b.Reserve(ctx, full, 10*usdScale, 0); err != nil {
		t.Fatal(err)
	}
	allowed, reserved, _, before, err = b.Reserve(ctx, full, 10*usdScale, 0)
	if err != nil || allowed || reserved != 0 || before != 10*usdScale {
		t.Fatalf("refused Reserve = (%v, %v, before %v, %v), want (false, 0, 10 USD, nil)", allowed, reserved, before, err)
	}

	// An uncapped key reports its tracked spend and still reserves nothing.
	free := uniqueKey(t)
	allowed, reserved, epoch, before, err = b.Reserve(ctx, free, 0, 0)
	if err != nil || !allowed || reserved != 0 || before != 0 {
		t.Fatalf("uncapped first Reserve = (%v, %v, before %v, %v), want (true, 0, 0, nil)", allowed, reserved, before, err)
	}
	if _, err := b.Adjust(ctx, free, 5*usdScale, epoch, true); err != nil {
		t.Fatal(err)
	}
	if _, _, _, before, err = b.Reserve(ctx, free, 0, 0); err != nil || before != 5*usdScale {
		t.Fatalf("uncapped Reserve after a 5 USD settlement reports before = %v (%v), want 5 USD", before, err)
	}
}

// The adjust script reports the window's spend after the adjustment, so
// finalize's warn-threshold and alert-ladder checks no longer re-read it
// (two round trips fewer on a capped key).
func TestAdjustReportsTheSpendAfterTheAdjustment(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	ctx := context.Background()
	key := uniqueKey(t)
	const capNano = 100 * usdScale

	_, reserved, epoch, _, err := b.Reserve(ctx, key, capNano, 0)
	if err != nil {
		t.Fatal(err)
	}
	after, err := b.Adjust(ctx, key, 5*usdScale-reserved, epoch, true)
	if err != nil || after != 5*usdScale {
		t.Fatalf("Adjust reports %v (%v), want the settled 5 USD", after, err)
	}
	if spent, _ := b.SpentNanoUSD(ctx, key); spent != after {
		t.Fatalf("SpentNanoUSD = %v, Adjust reported %v: the report must be the stored value", spent, after)
	}

	// A settlement against a replaced window writes nothing and reports the
	// current window's spend untouched -- what the folded read used to see.
	stale := uniqueKey(t)
	_, _, staleEpoch, _, err := b.Reserve(ctx, stale, 10*usdScale, 200)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	// Window #2 gets a long TTL: only window #1 had to expire during the sleep.
	_, _, freshEpoch, _, err := b.Reserve(ctx, stale, 10*usdScale, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if freshEpoch == staleEpoch {
		t.Fatal("setup: the window did not turn")
	}
	after, err = b.Adjust(ctx, stale, -5*usdScale, staleEpoch, true)
	if err != nil || after != 10*usdScale {
		t.Fatalf("stale-epoch Adjust reports %v (%v), want window #2's untouched 10 USD", after, err)
	}
	if spent, _ := b.SpentNanoUSD(ctx, stale); spent != 10*usdScale {
		t.Fatalf("the stale-epoch Adjust changed window #2's spend to %v", spent)
	}

	// No window at all: nothing is written and the report is 0.
	gone := uniqueKey(t)
	after, err = b.Adjust(ctx, gone, 1*usdScale, 1, true)
	if err != nil || after != 0 {
		t.Fatalf("Adjust on a missing window reports %v (%v), want 0", after, err)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	if n, _ := c.Exists(ctx, spendKey(gone)).Result(); n != 0 {
		t.Fatal("a reporting Adjust must not create a window hash")
	}
}
