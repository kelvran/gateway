package budget

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
)

// recordingBackend is a RedisBackend with Redis's observable semantics for
// one test: Reserve opens the key's window and reserves the headroom (or
// nothing for a non-positive cap), Adjust applies a delta only when the
// epoch matches the window's, SpentNanoUSD reads the window.
type recordingBackend struct {
	mu       sync.Mutex
	epoch    int64
	spent    map[string]int64
	reserves []int64 // the capNanoUSD of every Reserve call, in order
	adjusts  [][2]int64
}

func newRecordingBackend(epoch int64) *recordingBackend {
	return &recordingBackend{epoch: epoch, spent: map[string]int64{}}
}

func (b *recordingBackend) Reserve(_ context.Context, keyID string, capNanoUSD, _ int64) (bool, int64, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reserves = append(b.reserves, capNanoUSD)
	spent, open := b.spent[keyID]
	if !open {
		b.spent[keyID] = 0
	}
	if capNanoUSD <= 0 {
		return true, 0, b.epoch, nil
	}
	if spent >= capNanoUSD {
		return false, 0, b.epoch, nil
	}
	b.spent[keyID] = capNanoUSD
	return true, capNanoUSD - spent, b.epoch, nil
}

func (b *recordingBackend) ReserveFixed(context.Context, string, int64, int64, int64) (bool, error) {
	return true, nil
}

func (b *recordingBackend) Adjust(_ context.Context, keyID string, deltaNanoUSD, epoch int64, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adjusts = append(b.adjusts, [2]int64{deltaNanoUSD, epoch})
	if _, open := b.spent[keyID]; !open || epoch != b.epoch {
		return nil // Redis's own no-op: no window, or a window this epoch does not name
	}
	b.spent[keyID] = max(b.spent[keyID]+deltaNanoUSD, 0)
	return nil
}

func (b *recordingBackend) SpentNanoUSD(_ context.Context, keyID string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent[keyID], nil
}

func (b *recordingBackend) MarkAlertBucket(context.Context, string, float64, int64) (bool, error) {
	return false, nil
}
func (b *recordingBackend) MarkWarnAlerted(context.Context, string, int64) (bool, error) {
	return false, nil
}
func (b *recordingBackend) Delete(_ context.Context, keyID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.spent, keyID)
	return nil
}
func (b *recordingBackend) Close() error { return nil }

// TestRedisTrackerOpensAWindowForAnUncappedKey: in Redis mode an uncapped
// key used to be short-circuited before the backend, so no window existed
// and the settlement's Adjust found nothing to land on -- its spend read 0
// everywhere. Since 2026-10-12 Reserve reaches the backend for a
// non-positive cap too: the window opens, nothing is reserved, and the
// window's epoch comes back so Reconcile lands the real cost exactly as it
// does for a capped key.
func TestRedisTrackerOpensAWindowForAnUncappedKey(t *testing.T) {
	fb := newRecordingBackend(777)
	tr := NewRedisTracker(fb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	allowed, reserved, reservedUSD, epoch, err := tr.Reserve(ctx, "k", decimal.Zero, 0)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if !allowed || reserved || !reservedUSD.IsZero() {
		t.Fatalf("Reserve(uncapped) = allowed %v reserved %v reservedUSD %s, want allowed, not reserved, zero", allowed, reserved, reservedUSD)
	}
	if epoch != 777 {
		t.Fatalf("Reserve(uncapped) epoch = %d, want the backend's window epoch 777 (the value Reconcile needs)", epoch)
	}
	if len(fb.reserves) != 1 || fb.reserves[0] != 0 {
		t.Fatalf("backend Reserve calls = %v, want exactly one with cap 0 (the window must be opened)", fb.reserves)
	}

	cost := decimal.RequireFromString("0.25")
	tr.Reconcile(ctx, "k", reservedUSD, epoch, &cost, 0)
	if got := tr.SpentUSD(ctx, "k", 0); !got.Equal(cost) {
		t.Fatalf("SpentUSD after settling an uncapped key = %s, want %s (adjusts %v)", got, cost, fb.adjusts)
	}
	if len(fb.adjusts) != 1 || fb.adjusts[0] != [2]int64{250_000_000, 777} {
		t.Fatalf("Adjust calls = %v, want one of (+250000000, epoch 777)", fb.adjusts)
	}
}

// TestRedisTrackerUncappedKeyStillFailsOpenOnABackendError: the posture a
// capped key has is the uncapped key's too -- a Redis error at Reserve is
// returned for the caller to fail open, never a denial.
func TestRedisTrackerUncappedKeyStillFailsOpenOnABackendError(t *testing.T) {
	tr := NewRedisTracker(erroringBackend{newRecordingBackend(1)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, reserved, _, _, err := tr.Reserve(context.Background(), "k", decimal.Zero, 0)
	if err == nil || reserved {
		t.Fatalf("Reserve(uncapped) with a failing backend = reserved %v, err %v; want the error surfaced and nothing reserved", reserved, err)
	}
}

type erroringBackend struct{ *recordingBackend }

func (erroringBackend) Reserve(context.Context, string, int64, int64) (bool, int64, int64, error) {
	return false, 0, 0, context.DeadlineExceeded
}
