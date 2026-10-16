package redisbudget

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestReserveOpensAWindowForAnUncappedKey pins the 2026-10-12 branch of
// budgetReserveLuaSrc: a non-positive cap opens the key's window (spent 0,
// a fresh epoch, the reset-interval TTL exactly as for a capped key),
// reserves nothing and answers allowed with the window's epoch, so the
// settlement's Adjust has a window to land the real cost on. Before, the
// Go tracker never reached Redis for such a key and its spend read 0.
func TestReserveOpensAWindowForAnUncappedKey(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = b.Close() }()
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = client.Close() }()
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		cap  int64
	}{{"cap 0", 0}, {"negative cap", -5}} {
		t.Run(tc.name, func(t *testing.T) {
			key := uniqueKey(t)
			allowed, reserved, epoch, _, err := b.Reserve(ctx, key, tc.cap, 0)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			if !allowed || reserved != 0 || epoch == 0 {
				t.Fatalf("Reserve(cap %d) = (%v, %d, %d), want (true, 0, a real epoch)", tc.cap, allowed, reserved, epoch)
			}
			vals, err := client.HMGet(ctx, spendKey(key), "spent", "epoch").Result()
			if err != nil {
				t.Fatal(err)
			}
			if vals[0] != "0" || vals[1] != strconv.FormatInt(epoch, 10) {
				t.Fatalf("window = %v, want spent 0 and epoch %d", vals, epoch)
			}
			if ttl := client.PTTL(ctx, spendKey(key)).Val(); ttl != -1 {
				t.Fatalf("PTTL = %v, want -1 (a lifetime window when the reset interval is 0)", ttl)
			}
			if _, _, again, _, err := b.Reserve(ctx, key, tc.cap, 0); err != nil || again != epoch {
				t.Fatalf("second Reserve epoch = %d (err %v), want the same window %d", again, err, epoch)
			}
			if _, err := b.Adjust(ctx, key, 500, epoch, true); err != nil {
				t.Fatal(err)
			}
			if spent, _ := b.SpentNanoUSD(ctx, key); spent != 500 {
				t.Fatalf("spent after Adjust(+500, epoch) = %d, want 500", spent)
			}
			if _, err := b.Adjust(ctx, key, 500, epoch+1, true); err != nil {
				t.Fatal(err)
			}
			if spent, _ := b.SpentNanoUSD(ctx, key); spent != 500 {
				t.Fatalf("spent after a stale-epoch Adjust = %d, want 500 unchanged", spent)
			}
		})
	}

	t.Run("a reset interval sets the window's TTL", func(t *testing.T) {
		key := uniqueKey(t)
		if _, _, _, _, err := b.Reserve(ctx, key, 0, 60_000); err != nil {
			t.Fatal(err)
		}
		if ttl := client.PTTL(ctx, spendKey(key)).Val(); ttl <= 0 || ttl > 60*time.Second {
			t.Fatalf("PTTL = %v, want within (0, 60s]", ttl)
		}
	})
}
