// Package budget tracks each virtual key's cumulative USD spend against an
// optional cap, held in memory for the process lifetime, with optional
// restart-durable persistence per docs/rfcs/2026-09-03-budget-persistence.md
// (NewTrackerWithStore) — NewTracker (no store) stays pure in-memory,
// resetting on restart, exactly as before that RFC.
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
	mu     sync.Mutex
	spent  map[string]decimal.Decimal
	store  Store // nil = pure in-memory, unchanged from before this RFC
	logger *slog.Logger
}

// NewTracker constructs an empty, pure in-memory Tracker.
func NewTracker() *Tracker {
	return &Tracker{spent: make(map[string]decimal.Decimal)}
}

// NewTrackerWithStore constructs a Tracker backed by store: existing
// spend is loaded immediately, so a restart resumes exactly where it left
// off, and every subsequent Record call persists synchronously before
// returning — no async-flush window, no data lost between a Record call
// and a crash. logger defaults to slog.Default() if nil; it is used only
// to report a Save failure (see Record's doc comment) — a persistence
// failure never fails the request itself.
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
	return &Tracker{spent: spent, store: store, logger: logger}, nil
}

// Allow reports whether keyID has remaining budget under capUSD, given its
// cumulative spend so far. capUSD <= 0 means unlimited (always true).
// Never touches the store — the read path stays exactly as fast as before
// persistence existed.
func (t *Tracker) Allow(keyID string, capUSD decimal.Decimal) bool {
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

// SpentUSD returns keyID's cumulative recorded spend so far. Never touches
// the store — like Allow, a pure in-memory read. A never-recorded key
// correctly returns the zero-value decimal.Decimal{} (renders as "0" via
// String()). Not synchronized with Allow as a single atomic read — under
// concurrent requests for the same key, a Record call could land between
// this and a subsequent Allow call. Acceptable here: this is an
// observability read (per
// docs/rfcs/2026-09-03-gatewayevents-decision-enrichment.md's
// budget-spend-at-decision-time field), never part of the enforcement
// decision itself, which Allow alone still makes correctly.
func (t *Tracker) SpentUSD(keyID string) decimal.Decimal {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent[keyID]
}

// Record adds costUSD to keyID's cumulative spend. Callers record once per
// request, after cost is known — including on requests that produced
// partial or zero usage, matching gateway/ARCHITECTURE.md's existing "cost/
// observability finalization always runs" principle. A negative costUSD is
// ignored: spend only ever accumulates upward, since a corrective credit
// mechanism isn't part of this pass's scope.
//
// When a Store is configured, the new total is persisted synchronously
// before Record returns. A persistence failure is logged
// ("budget_persist_failed") and Record still returns normally — the
// in-memory total is already correct for enforcement purposes; only this
// one update's restart-durability is at risk, a real but non-fatal
// degradation per docs/rfcs/2026-09-03-budget-persistence.md.
func (t *Tracker) Record(keyID string, costUSD decimal.Decimal) {
	if costUSD.Sign() < 0 {
		return
	}
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
