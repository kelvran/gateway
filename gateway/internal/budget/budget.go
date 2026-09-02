// Package budget tracks each virtual key's cumulative USD spend against an
// optional cap, held in memory for the process lifetime.
//
// Per docs/rfcs/2026-09-02-virtual-keys-budgets.md, this is deliberately
// not persisted: a gateway restart resets every key's spend to zero. A
// real control-plane store (Postgres, per gateway/ARCHITECTURE.md's Tech
// Stack) doesn't exist yet — this is a documented Phase 2 gap, not a
// silently accepted one.
//
// Decimal arithmetic (github.com/shopspring/decimal), not float64, per
// docs/rfcs/2026-09-02-decimal-cost-accounting.md — repeated float64
// addition of small per-request cost fragments (exactly what Record does
// on every request) measurably drifts from the exact sum, which sits
// directly underneath the number Allow compares against a hard cap.
package budget

import (
	"sync"

	"github.com/shopspring/decimal"
)

// Tracker enforces a per-key cumulative USD spending cap. The zero value
// is not usable; construct with NewTracker. Safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	spent map[string]decimal.Decimal
}

// NewTracker constructs an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{spent: make(map[string]decimal.Decimal)}
}

// Allow reports whether keyID has remaining budget under capUSD, given its
// cumulative spend so far. capUSD <= 0 means unlimited (always true).
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

// Record adds costUSD to keyID's cumulative spend. Callers record once per
// request, after cost is known — including on requests that produced
// partial or zero usage, matching gateway/ARCHITECTURE.md's existing "cost/
// observability finalization always runs" principle. A negative costUSD is
// ignored: spend only ever accumulates upward, since a corrective credit
// mechanism isn't part of this pass's scope.
func (t *Tracker) Record(keyID string, costUSD decimal.Decimal) {
	if costUSD.Sign() < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent[keyID] = t.spent[keyID].Add(costUSD)
}
