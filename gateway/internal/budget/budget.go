// Package budget tracks each virtual key's cumulative USD spend against an
// optional cap, held in memory for the process lifetime, with optional
// restart-durable persistence per docs/rfcs/2026-09-03-budget-persistence.md
// (NewTrackerWithStore) — NewTracker (no store) stays pure in-memory,
// resetting on restart, exactly as before that RFC.
//
// A key's cap is lifetime-of-the-process by default (resetInterval == 0
// everywhere below), or a real rolling window (e.g. "monthly") when a
// positive resetInterval is passed to Allow/SpentUSD/Record — see
// resetIfNeeded, added per docs/rfcs/2026-09-02-virtual-keys-budgets.md's
// own named future work ("no rolling-window budget reset... no scheduler
// and no persistence to reset against yet"). Deliberately lazy
// (checked on access, not a background ticker), mirroring
// internal/ratelimit.TokenBucket's own lazy-refill design.
//
// Decimal arithmetic (github.com/shopspring/decimal), not float64, per
// docs/rfcs/2026-09-02-decimal-cost-accounting.md — repeated float64
// addition of small per-request cost fragments (exactly what Record does
// on every request) measurably drifts from the exact sum, which sits
// directly underneath the number Allow compares against a hard cap.
package budget

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// Store persists budget spend durably across process restarts. Optional —
// a Tracker constructed via NewTracker (no store) is unchanged: pure
// in-memory. See internal/budget/boltstore for the real implementation.
type Store interface {
	Load(ctx context.Context) (map[string]decimal.Decimal, error)
	Save(ctx context.Context, keyID string, spent decimal.Decimal) error
	Close() error
}

// Tracker enforces a per-key cumulative USD spending cap. The zero value
// is not usable; construct with NewTracker or NewTrackerWithStore. Safe
// for concurrent use.
type Tracker struct {
	mu          sync.Mutex
	spent       map[string]decimal.Decimal
	periodStart map[string]time.Time // rolling-window reset bookkeeping; see maybeResetLocked
	store       Store                // nil = pure in-memory, unchanged from before this RFC
	logger      *slog.Logger
	now         func() time.Time // real clock in production; overridden directly by white-box tests
}

// NewTracker constructs an empty, pure in-memory Tracker.
func NewTracker() *Tracker {
	return &Tracker{spent: make(map[string]decimal.Decimal), periodStart: make(map[string]time.Time), now: time.Now}
}

// NewTrackerWithStore constructs a Tracker backed by store: existing
// spend is loaded immediately, so a restart resumes exactly where it left
// off, and every subsequent Record call persists synchronously before
// returning — no async-flush window, no data lost between a Record call
// and a crash. logger defaults to slog.Default() if nil; it is used only
// to report a Save failure (see Record's doc comment) — a persistence
// failure never fails the request itself.
//
// A key's rolling-window reset boundary (periodStart, see
// maybeResetLocked) is NOT itself persisted — only cumulative spend is,
// matching the real scope docs/rfcs/2026-09-03-budget-persistence.md
// shipped. A restart therefore resets a key's own reset-window clock to
// the restart moment (not its spend), extending that one window by at
// most resetInterval — a narrow, self-limiting, and pre-existing class
// of behavior (a store-less Tracker already loses all budget state on
// every restart today), not a new bypass this feature introduces.
func NewTrackerWithStore(ctx context.Context, store Store, logger *slog.Logger) (*Tracker, error) {
	if logger == nil {
		logger = slog.Default()
	}
	spent, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if spent == nil {
		spent = make(map[string]decimal.Decimal)
	}
	return &Tracker{spent: spent, periodStart: make(map[string]time.Time), store: store, logger: logger, now: time.Now}, nil
}

// resetIfNeeded resets keyID's spend to zero and starts a fresh window,
// returning true, if resetInterval > 0 and at least that much time has
// elapsed since the window began. The very first observation of a key
// (no periodStart yet) starts its window at now() without resetting
// spend -- a freshly-loaded key from a Store may already have real,
// non-zero spend, and starting its clock is not the same as pretending
// that spend never happened. Locks/unlocks t.mu itself; callers must not
// already hold it.
func (t *Tracker) resetIfNeeded(keyID string, resetInterval time.Duration) bool {
	if resetInterval <= 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	start, seen := t.periodStart[keyID]
	now := t.now()
	if !seen {
		t.periodStart[keyID] = now
		return false
	}
	if now.Sub(start) >= resetInterval {
		t.spent[keyID] = decimal.Zero
		t.periodStart[keyID] = now
		return true
	}
	return false
}

// persistZeroIfStoreConfigured durably records a just-occurred
// resetIfNeeded reset, for a caller (Allow/SpentUSD) that doesn't already
// persist its own result the way Record does. A no-op when no Store is
// configured. A persistence failure is logged, never fatal — mirrors
// Record's own established failure handling.
func (t *Tracker) persistZeroIfStoreConfigured(keyID string) {
	if t.store == nil {
		return
	}
	if err := t.store.Save(context.Background(), keyID, decimal.Zero); err != nil {
		t.logger.Warn("budget_persist_failed", "key_id", keyID, "error", err.Error())
	}
}

// Allow reports whether keyID has remaining budget under capUSD, given its
// cumulative spend so far. capUSD <= 0 means unlimited (always true).
// resetInterval > 0 enables per-key rolling-window budget resets — see
// resetIfNeeded; 0 preserves the original lifetime-of-the-process cap
// behavior exactly. Otherwise never touches the store — the read path
// stays exactly as fast as before persistence existed, except in the
// narrow case a reset boundary was just crossed, which durably persists
// the reset so a restart doesn't silently reload the stale pre-reset
// total.
func (t *Tracker) Allow(keyID string, capUSD decimal.Decimal, resetInterval time.Duration) bool {
	if t.resetIfNeeded(keyID, resetInterval) {
		t.persistZeroIfStoreConfigured(keyID)
	}
	if capUSD.Sign() <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// A never-recorded key's zero-value decimal.Decimal correctly compares
	// as 0 without any explicit initialization — the same implicit-zero
	// behavior the old float64 map had (verified by
	// TestAllowWithNoRecordedSpendUnderPositiveCap, not assumed).
	return t.spent[keyID].LessThan(capUSD)
}

// SpentUSD returns keyID's cumulative recorded spend so far, resetting
// first if resetInterval's window just elapsed (see Allow's own doc
// comment — same reasoning applies here) — callers pass the same
// resetInterval given to Allow/Record for the same key, so an
// observability read never reports a stale, pre-reset total the very
// next Allow call would already treat as reset. Otherwise never touches
// the store — like Allow, a pure in-memory read the rest of the time. A
// never-recorded key correctly returns the zero-value decimal.Decimal{}
// (renders as "0" via String()). Not synchronized with Allow as a single
// atomic read — under concurrent requests for the same key, a Record call
// could land between this and a subsequent Allow call. Acceptable here:
// this is an observability read (per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md's
// budget-spend-at-decision-time field), never part of the enforcement
// decision itself, which Allow alone still makes correctly.
func (t *Tracker) SpentUSD(keyID string, resetInterval time.Duration) decimal.Decimal {
	if t.resetIfNeeded(keyID, resetInterval) {
		t.persistZeroIfStoreConfigured(keyID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent[keyID]
}

// Record adds costUSD to keyID's cumulative spend. Callers record once per
// request, after cost is known — including on requests that produced
// partial or zero usage, matching gateway/ARCHITECTURE.md's existing "cost/
// observability finalization always runs" principle. A negative costUSD is
// ignored: spend only ever accumulates upward, since a corrective credit
// mechanism isn't part of this pass's scope. resetInterval > 0 resets
// keyID's spend to zero first if its rolling window just elapsed (see
// resetIfNeeded) — costUSD then accumulates on top of that fresh zero,
// not the stale pre-reset total.
//
// When a Store is configured, the new total is persisted synchronously
// before Record returns — this alone already durably reflects any reset
// that just happened, since it's computed from the post-reset spend, so
// Record never needs persistZeroIfStoreConfigured's separate call the way
// Allow/SpentUSD do. A persistence failure is logged
// ("budget_persist_failed") and Record still returns normally — the
// in-memory total is already correct for enforcement purposes; only this
// one update's restart-durability is at risk, a real but non-fatal
// degradation per docs/rfcs/2026-09-03-budget-persistence.md.
func (t *Tracker) Record(keyID string, costUSD decimal.Decimal, resetInterval time.Duration) {
	if costUSD.Sign() < 0 {
		return
	}
	t.resetIfNeeded(keyID, resetInterval)

	t.mu.Lock()
	newTotal := t.spent[keyID].Add(costUSD)
	t.spent[keyID] = newTotal
	t.mu.Unlock()

	if t.store == nil {
		return
	}
	if err := t.store.Save(context.Background(), keyID, newTotal); err != nil {
		t.logger.Warn("budget_persist_failed", "key_id", keyID, "error", err.Error())
	}
}

// Close releases the underlying store, if any. Safe to call even on a
// Tracker constructed via NewTracker (no store).
func (t *Tracker) Close() error {
	if t.store == nil {
		return nil
	}
	return t.store.Close()
}
