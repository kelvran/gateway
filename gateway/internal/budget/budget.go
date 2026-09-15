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
//
// Allow/Record are two independent, separately-locked operations — real
// request-handling code (gateway/internal/gateway/dataplane) must NOT
// call them directly, because the gap between an Allow check and its
// corresponding Record call (the real upstream provider call) is a
// genuine, 100%-reproducible TOCTOU race: concurrent requests can all
// pass Allow before any of them commits a Record. Reserve/Reconcile
// (below) is the concurrency-safe replacement, per
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md — Allow/
// Record themselves stay exactly as they were, both because they remain
// correct, self-contained primitives Reserve/Reconcile are built on top
// of, and because every existing test against them (TestAllow*/
// TestRecord*/TestConcurrentRecordNeverLosesAnUpdate) keeps proving
// exactly what it always proved.
package budget

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// State is one virtual key's full persisted budget state: cumulative
// spend plus enough rolling-window bookkeeping (PeriodStart/PeriodEpoch/
// BilledCount) that a process restart no longer resets a key's own
// reset-window clock to the restart moment — the real fix for the gap
// NewTrackerWithStore's own doc comment used to document as an accepted,
// "self-limiting" tradeoff. PeriodStart's zero value is the sentinel a
// Store implementation uses to signal "no rolling-window bookkeeping
// available for this key" (e.g. a legacy pre-migration entry) — see
// boltstore's own Load for the one real producer of that case.
type State struct {
	Spent       decimal.Decimal
	PeriodStart time.Time
	PeriodEpoch int64
	BilledCount int64
}

// Store persists budget spend durably across process restarts. Optional —
// a Tracker constructed via NewTracker (no store) is unchanged: pure
// in-memory. See internal/budget/boltstore for the real implementation.
type Store interface {
	Load(ctx context.Context) (map[string]State, error)
	Save(ctx context.Context, keyID string, state State) error
	// Delete purges keyID's persisted state entirely — for GDPR/CCPA
	// erasure-request handling, per
	// docs/upgrade-research/data-retention-right-to-erasure-2026-09-15.md.
	// A no-op, not an error, for a keyID with no persisted entry.
	Delete(ctx context.Context, keyID string) error
	Close() error
}

// Tracker enforces a per-key cumulative USD spending cap. The zero value
// is not usable; construct with NewTracker or NewTrackerWithStore. Safe
// for concurrent use.
type Tracker struct {
	mu          sync.Mutex
	spent       map[string]decimal.Decimal
	periodStart map[string]time.Time // rolling-window reset bookkeeping; see maybeResetLocked
	// periodEpoch is keyID's rolling-window generation counter, incremented
	// by resetIfNeeded every time it actually performs a reset (by
	// whichever caller's Allow/SpentUSD/Reserve/Record/Reconcile call
	// happens to observe the elapsed boundary first — see resetIfNeeded's
	// own doc comment). Reserve captures the current epoch alongside
	// reservedUSD; Reconcile compares its caller-supplied epoch against
	// the CURRENT epoch to detect whether a reset happened anywhere in
	// between — not just whether Reconcile's OWN resetIfNeeded call
	// happens to be the one that triggers it. Without this, a reservation
	// whose window rolled over via a DIFFERENT concurrent caller before
	// Reconcile ran would have its (now nonexistent) reservedUSD
	// subtracted from the freshly-reset ledger anyway, silently
	// undercounting real spend in the new window by exactly the leaked
	// reservation amount — a real, reproducible money-leak, see
	// TestReconcileDoesNotUndercountAcrossAConcurrentlyTriggeredReset.
	periodEpoch map[string]int64
	// billedCount is keyID's count of real (non-reservation) costs applied
	// via Reconcile — the denominator of Reserve's own historical-average
	// reservation estimate, per
	// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md. Zero for
	// every key until its first Reconcile call with a non-nil realCost;
	// reset to zero alongside spend whenever resetIfNeeded rolls a
	// rolling-window boundary, since a fresh window has no billing history
	// of its own yet either.
	billedCount map[string]int64
	// highestAlertedBucket/highestAlertedEpoch back
	// CheckAndMarkBudgetAlertBucket's dedup, per
	// docs/upgrade-research/cost-intelligence-finops-2026-09-14.md: a
	// fixed percent-of-cap threshold ladder needs to fire an alertable
	// event once per newly-crossed bucket, not on every request past it
	// the way checkBudgetWarnThreshold's own log line deliberately does.
	// Keyed by periodEpoch (see that field's own comment), not just
	// keyID, so a key correctly re-alerts every bucket again after its
	// rolling window resets, rather than staying permanently
	// "already alerted" against a window that no longer exists.
	highestAlertedBucket map[string]float64
	highestAlertedEpoch  map[string]int64
	store                Store // nil = pure in-memory, unchanged from before this RFC
	logger               *slog.Logger
	now                  func() time.Time // real clock in production; overridden directly by white-box tests
}

// NewTracker constructs an empty, pure in-memory Tracker.
func NewTracker() *Tracker {
	return &Tracker{spent: make(map[string]decimal.Decimal), periodStart: make(map[string]time.Time), periodEpoch: make(map[string]int64), billedCount: make(map[string]int64), highestAlertedBucket: make(map[string]float64), highestAlertedEpoch: make(map[string]int64), now: time.Now}
}

// NewTrackerWithStore constructs a Tracker backed by store: existing
// spend AND rolling-window bookkeeping (PeriodStart/PeriodEpoch/
// BilledCount) are loaded immediately, so a restart resumes exactly where
// it left off — including a key's reset-window clock, which no longer
// silently re-anchors to the restart moment — and every subsequent Record
// call persists synchronously before returning — no async-flush window,
// no data lost between a Record call and a crash. logger defaults to
// slog.Default() if nil; it is used only to report a Save failure (see
// Record's doc comment) — a persistence failure never fails the request
// itself.
//
// A key whose loaded State has a zero PeriodStart (a legacy
// pre-migration entry — see boltstore's own Load) is seeded with spend
// only, exactly matching this function's own pre-fix behavior for that
// one key: its window clock starts fresh at the first observation after
// this restart, per resetIfNeeded's own "first observation" rule, rather
// than fabricating a window boundary this Tracker was never actually
// told.
func NewTrackerWithStore(ctx context.Context, store Store, logger *slog.Logger) (*Tracker, error) {
	if logger == nil {
		logger = slog.Default()
	}
	states, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	spent := make(map[string]decimal.Decimal, len(states))
	periodStart := make(map[string]time.Time, len(states))
	periodEpoch := make(map[string]int64, len(states))
	billedCount := make(map[string]int64, len(states))
	for keyID, s := range states {
		spent[keyID] = s.Spent
		if !s.PeriodStart.IsZero() {
			periodStart[keyID] = s.PeriodStart
			periodEpoch[keyID] = s.PeriodEpoch
			billedCount[keyID] = s.BilledCount
		}
	}
	return &Tracker{spent: spent, periodStart: periodStart, periodEpoch: periodEpoch, billedCount: billedCount, highestAlertedBucket: make(map[string]float64), highestAlertedEpoch: make(map[string]int64), store: store, logger: logger, now: time.Now}, nil
}

// resetIfNeeded resets keyID's spend to zero and starts a fresh window,
// reporting justReset true, if resetInterval > 0 and at least that much
// time has elapsed since the window began. The very first observation of
// a key (no periodStart yet) starts its window at now() without
// resetting spend -- a freshly-loaded key from a Store may already have
// real, non-zero spend, and starting its clock is not the same as
// pretending that spend never happened. periodStart/periodEpoch are
// always returned as the CURRENT (post-call) values for keyID, so a
// caller can pass them straight to persistZeroIfStoreConfigured without
// a second lock acquisition, regardless of which branch was taken.
// Locks/unlocks t.mu itself; callers must not already hold it.
func (t *Tracker) resetIfNeeded(keyID string, resetInterval time.Duration) (justReset bool, periodStart time.Time, periodEpoch int64) {
	if resetInterval <= 0 {
		return false, time.Time{}, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	start, seen := t.periodStart[keyID]
	now := t.now()
	if !seen {
		t.periodStart[keyID] = now
		return false, now, t.periodEpoch[keyID]
	}
	if now.Sub(start) >= resetInterval {
		t.spent[keyID] = decimal.Zero
		// billedCount resets alongside spend: a fresh window has no
		// billing history of its own yet either, so Reserve's
		// historical-average estimate must not carry an average computed
		// against the OLD window's now-zeroed total, per
		// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md.
		t.billedCount[keyID] = 0
		t.periodStart[keyID] = now
		// periodEpoch bumps alongside spend/billedCount: any outstanding
		// reservation captured against the OLD window (by whichever
		// caller's Reserve happened before this reset, whether or not
		// it's the same caller doing the reset) must be recognizable as
		// stale by a later Reconcile call -- see periodEpoch's own field
		// comment.
		t.periodEpoch[keyID]++
		return true, now, t.periodEpoch[keyID]
	}
	return false, start, t.periodEpoch[keyID]
}

// persistZeroIfStoreConfigured durably records a just-occurred
// resetIfNeeded reset, for a caller (Allow/SpentUSD) that doesn't already
// persist its own result the way Record does. periodStart/periodEpoch
// must be the exact values resetIfNeeded returned alongside justReset —
// carried through so the persisted State reflects the fresh window's own
// boundary, not a stale one from before this reset. A no-op when no
// Store is configured. A persistence failure is logged, never fatal —
// mirrors Record's own established failure handling.
func (t *Tracker) persistZeroIfStoreConfigured(keyID string, periodStart time.Time, periodEpoch int64) {
	if t.store == nil {
		return
	}
	state := State{Spent: decimal.Zero, PeriodStart: periodStart, PeriodEpoch: periodEpoch, BilledCount: 0}
	if err := t.store.Save(context.Background(), keyID, state); err != nil {
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
	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
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
	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
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
// When a Store is configured, the new total — alongside the CURRENT
// periodStart/periodEpoch/billedCount, captured under the same lock
// acquisition that updates spend, so the persisted State is always
// internally consistent — is persisted synchronously before Record
// returns; this alone already durably reflects any reset that just
// happened, since it's computed from the post-reset bookkeeping, so
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
	state := State{Spent: newTotal, PeriodStart: t.periodStart[keyID], PeriodEpoch: t.periodEpoch[keyID], BilledCount: t.billedCount[keyID]}
	t.mu.Unlock()

	if t.store == nil {
		return
	}
	if err := t.store.Save(context.Background(), keyID, state); err != nil {
		t.logger.Warn("budget_persist_failed", "key_id", keyID, "error", err.Error())
	}
}

// Reserve is Allow's concurrency-safe replacement for real request-
// handling code, per
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md: it performs
// the exact same cap check Allow does, but — under the SAME lock
// acquisition, never released and re-acquired in between — immediately
// follows an "allowed" result with a provisional debit of a conservative
// reservation estimate, closing the TOCTOU window between Allow and
// Record that let concurrent requests all pass the check before any of
// them committed a debit. capUSD <= 0 (unlimited) behaves exactly like
// Allow: always allowed, reserved is false, reservedUSD is
// decimal.Zero — nothing is ever reserved against an uncapped key.
// allowed is false exactly when Allow would have returned false, in
// which case reserved is also false and reservedUSD is decimal.Zero:
// nothing was mutated, so there is nothing for a caller to release.
//
// Every true `reserved` return MUST be paired with exactly one Reconcile
// call for the same keyID/reservedUSD/reservationEpoch, even on an
// error/timeout path — see Reconcile's own doc comment for why a
// leaked, never-reconciled reservation permanently shrinks a key's
// remaining headroom. reservationEpoch (see the Tracker.periodEpoch
// field comment) must be threaded through to that same Reconcile call
// unchanged — never re-derived or refreshed by the caller — so Reconcile
// can detect whether a rolling-window reset happened for this key, by
// ANY caller, between this Reserve call and that Reconcile call.
func (t *Tracker) Reserve(keyID string, capUSD decimal.Decimal, resetInterval time.Duration) (allowed bool, reserved bool, reservedUSD decimal.Decimal, reservationEpoch int64) {
	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
	}
	if capUSD.Sign() <= 0 {
		return true, false, decimal.Zero, 0
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.spent[keyID].LessThan(capUSD) {
		return false, false, decimal.Zero, 0
	}
	reservedUSD = t.reservationAmountLocked(keyID, capUSD)
	t.spent[keyID] = t.spent[keyID].Add(reservedUSD)
	return true, true, reservedUSD, t.periodEpoch[keyID]
}

// reservationAmountLocked computes keyID's Reserve reservation amount:
// its own historical average real cost per Reconcile call with a
// non-nil realCost (spent so far divided by billedCount), once at least
// one such call has happened for this key — or, before any billing
// history exists at all (billedCount == 0: a brand-new key, or one whose
// rolling window/process just reset), the full remaining headroom under
// capUSD. The latter is deliberately maximally conservative given zero
// information — see this RFC's own "cold-start conservatism" section for
// why that's an accepted, self-resolving trade-off rather than a bug: it
// can never let a cold-start key's concurrent burst overshoot capUSD,
// and stops applying the moment ANY real cost is reconciled for the key.
// Callers must already hold t.mu and must already have confirmed
// spent[keyID] < capUSD (Reserve's own check), so capUSD.Sub(spent) here
// is guaranteed positive.
func (t *Tracker) reservationAmountLocked(keyID string, capUSD decimal.Decimal) decimal.Decimal {
	if count := t.billedCount[keyID]; count > 0 {
		return t.spent[keyID].Div(decimal.NewFromInt(count))
	}
	return capUSD.Sub(t.spent[keyID])
}

// Reconcile undoes a previous Reserve call's provisional debit (its
// reservedUSD return value, passed back here unchanged) and, if realCost
// is non-nil, applies the real cost in its place — atomically, under one
// lock acquisition, so no concurrent Reserve/Reconcile call for the same
// keyID can observe the intermediate "reservation removed but real cost
// not yet applied" state. Because Reconcile subtracts EXACTLY the
// reservedUSD value Reserve returned, the net effect on spend for a
// request that reserves then reconciles with no concurrent interleaving
// is byte-identical to the old Allow-then-Record pair, regardless of
// what the reservation estimate happened to be: spent_before + reserved
// − reserved + realCost == spent_before + realCost, exactly (decimal
// subtraction is an exact inverse of the identical decimal addition it
// undoes).
//
// realCost == nil (or negative, mirroring Record's own "negative cost
// ignored" rule) releases the reservation with no replacement — the
// correct call for a request that errored, timed out, or turned out
// non-billable (a cache hit or coalesced singleflight follower, per
// docs/rfcs/2026-09-05-gateway-cost-double-counting.md) before ever
// reaching a real cost. This release path is what prevents a permanent
// capacity leak: every Reserve that returns reserved == true MUST
// eventually reach a matching Reconcile call, on every return path
// (including error), or that reservation's amount is gone from the
// key's remaining headroom forever.
//
// Only a Reconcile call that actually applies a real cost persists to
// Store (matching Record's own persistence behavior exactly) — a
// release-only call changes nothing about the durable total (the
// reservation it undoes was never itself persisted), so it would be a
// wasted write to skip.
//
// reservationEpoch MUST be the exact value Reserve returned alongside
// reservedUSD (see Tracker.periodEpoch's own field comment and Reserve's
// doc comment) — it is how Reconcile detects a rolling-window reset that
// happened between the original Reserve call and this Reconcile call,
// including one triggered by a completely different concurrent caller
// (Allow/SpentUSD/Reserve/Record/Reconcile against the same key), not
// just one this specific Reconcile call's own resetIfNeeded happens to
// trigger. When t.periodEpoch[keyID] no longer matches reservationEpoch,
// the reservation being reconciled is against a window that no longer
// exists — its reservedUSD was already zeroed along with everything else
// in that window — so this skips the subtraction entirely (rather than
// incorrectly driving the fresh window's spend negative by exactly the
// leaked reservation amount, silently undercounting real cost) and
// applies realCost (if any) fresh into the new window instead. Before
// this epoch check existed, only the narrower "did MY OWN resetIfNeeded
// call just trigger the reset" case was handled, missing the
// cross-caller case entirely — see
// TestReconcileDoesNotUndercountAcrossAConcurrentlyTriggeredReset for a
// concrete, 100%-reproducible demonstration of the resulting money-leak.
func (t *Tracker) Reconcile(keyID string, reservedUSD decimal.Decimal, reservationEpoch int64, realCost *decimal.Decimal, resetInterval time.Duration) {
	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
	}

	t.mu.Lock()
	newTotal := t.spent[keyID]
	if t.periodEpoch[keyID] == reservationEpoch {
		newTotal = newTotal.Sub(reservedUSD)
	}
	billed := realCost != nil && realCost.Sign() >= 0
	if billed {
		newTotal = newTotal.Add(*realCost)
		t.billedCount[keyID]++
	}
	t.spent[keyID] = newTotal
	state := State{Spent: newTotal, PeriodStart: t.periodStart[keyID], PeriodEpoch: t.periodEpoch[keyID], BilledCount: t.billedCount[keyID]}
	t.mu.Unlock()

	if !billed || t.store == nil {
		return
	}
	if err := t.store.Save(context.Background(), keyID, state); err != nil {
		t.logger.Warn("budget_persist_failed", "key_id", keyID, "error", err.Error())
	}
}

// IncreaseReservation attempts to raise an existing outstanding
// reservation (previously granted by Reserve, or a prior
// IncreaseReservation call for the same keyID) from currentReservedUSD to
// newReservedUSD — the mid-stream top-up half of the reserve-then-
// reconcile design, per docs/upgrade-research/gateway-streaming-
// concurrent-sibling-reservation-gap-2026-09-09.md: a long-running
// streaming request's real cost can grow well past its own initial
// reservation (sized off cold-start full-headroom or historical-average
// estimates, neither of which anticipates an unusually long stream),
// silently understating its true claim on the key's headroom to any
// concurrent sibling for the FULL DURATION of the stream — not just one
// HTTP round-trip, the case Reserve/Reconcile's own reserve-then-
// reconcile design was originally built to bound. This closes that gap
// by letting the stream periodically true up its own reservation as
// real-time evidence (the caller's own output-length-based cost
// estimate) shows it growing.
//
// A no-op, always allowed (returns currentReservedUSD unchanged), when
// capUSD.Sign() <= 0 (unlimited key) or newReservedUSD is not actually
// larger than currentReservedUSD (nothing to top up — callers are
// expected to check this cheaply themselves before calling, to avoid
// acquiring t.mu on every one of a stream's many chunks, but this method
// stays correct even if a caller doesn't bother). Otherwise atomically
// checks whether the DELTA (newReservedUSD − currentReservedUSD) fits
// under the remaining headroom and, if so, applies it (spent[keyID] +=
// delta) and returns (true, newReservedUSD). If the delta does not fit,
// spent[keyID] is left COMPLETELY UNCHANGED (the existing, smaller
// reservation stays exactly as it was) and this returns (false,
// currentReservedUSD) — the caller must then treat this exactly like a
// runaway-completion-guard trip (streamrunaway.go): stop accepting
// further chunks from this request, but finish gracefully via the
// existing truncated-but-valid path, never as an error, since the
// request already legitimately passed its own initial admission check.
//
// The caller MUST use whichever of (newReservedUSD on success,
// currentReservedUSD unchanged on failure) this returns as the
// reservedUSD argument to its EVENTUAL Reconcile call — never the
// ORIGINAL pre-topup amount blindly, or Reconcile's own exact-inverse
// arithmetic (see its own doc comment) would undo the wrong amount.
//
// reservationEpoch/newReservationEpoch mirror Reserve/Reconcile's own
// epoch contract (see Tracker.periodEpoch's field comment): a long-
// running stream's original Reserve call and its later, possibly-
// repeated IncreaseReservation top-ups can straddle a rolling-window
// reset triggered by any OTHER concurrent caller. Without this, a
// top-up computed against a currentReservedUSD that no longer exists in
// the (already-reset) ledger would apply its delta on top of the
// freshly-zeroed spend — a phantom addition unrelated to any real spend
// in the new window, which can spuriously reject sibling requests
// against the same key until this stream's own eventual Reconcile call
// self-heals it. When the epoch has rolled over, this reserves
// newReservedUSD fresh against the new window's own remaining headroom
// instead of computing a delta at all. The caller MUST thread whichever
// epoch this returns into its eventual Reconcile call, mirroring
// Reserve's own contract — never the original pre-stream epoch.
func (t *Tracker) IncreaseReservation(keyID string, capUSD, currentReservedUSD, newReservedUSD decimal.Decimal, reservationEpoch int64, resetInterval time.Duration) (allowed bool, appliedUSD decimal.Decimal, newReservationEpoch int64) {
	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
	}
	if capUSD.Sign() <= 0 || !newReservedUSD.GreaterThan(currentReservedUSD) {
		return true, currentReservedUSD, reservationEpoch
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	currentEpoch := t.periodEpoch[keyID]
	if currentEpoch != reservationEpoch {
		if t.spent[keyID].Add(newReservedUSD).GreaterThan(capUSD) {
			// Nothing was reserved under the new epoch by this call —
			// the true outstanding reservation for Reconcile's purposes
			// is genuinely zero, never the stale currentReservedUSD
			// (which would otherwise make Reconcile subtract a phantom
			// amount a second time in this new window).
			return false, decimal.Zero, currentEpoch
		}
		t.spent[keyID] = t.spent[keyID].Add(newReservedUSD)
		return true, newReservedUSD, currentEpoch
	}

	delta := newReservedUSD.Sub(currentReservedUSD)
	if t.spent[keyID].Add(delta).GreaterThan(capUSD) {
		return false, currentReservedUSD, currentEpoch
	}
	t.spent[keyID] = t.spent[keyID].Add(delta)
	return true, newReservedUSD, currentEpoch
}

// BudgetAlertBuckets is the fixed percent-of-cap ladder
// CheckAndMarkBudgetAlertBucket checks against, per
// docs/upgrade-research/cost-intelligence-finops-2026-09-14.md's own
// finding that a static threshold ladder — not per-tenant-configurable,
// unlike VirtualKey.BudgetWarnPercent — is the only verified 2026
// production pattern for this; no evidence found that per-tenant
// customization is needed yet. Exported so a test (or a future admin-API
// read endpoint reporting "next bucket") can reference the same values
// this package checks against, rather than duplicating the literal slice.
var BudgetAlertBuckets = []float64{0.5, 0.75, 0.9, 1.0}

// CheckAndMarkBudgetAlertBucket reports the highest bucket in
// BudgetAlertBuckets that percentUsed has newly crossed for keyID, given
// whatever bucket (if any) was already alerted for keyID's CURRENT
// rolling-window epoch (see the periodEpoch field comment) — and records
// that new high-water mark so a later call in the same window with the
// same or a lower percentUsed does not re-report it. crossed is false,
// and bucket is meaningless, when percentUsed hasn't newly crossed any
// bucket beyond what's already been alerted this window.
//
// Deliberately reports only the single highest newly-crossed bucket, not
// every bucket skipped over by one large jump in spend — this is a
// "how close are we now" signal, not an audit trail of every threshold a
// request happened to leap past.
func (t *Tracker) CheckAndMarkBudgetAlertBucket(keyID string, percentUsed float64) (bucket float64, crossed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	currentEpoch := t.periodEpoch[keyID]
	alreadyAlerted := 0.0
	if t.highestAlertedEpoch[keyID] == currentEpoch {
		alreadyAlerted = t.highestAlertedBucket[keyID]
	}

	newHighest := alreadyAlerted
	for _, b := range BudgetAlertBuckets {
		if percentUsed >= b && b > newHighest {
			newHighest = b
		}
	}
	if newHighest <= alreadyAlerted {
		return 0, false
	}
	t.highestAlertedBucket[keyID] = newHighest
	t.highestAlertedEpoch[keyID] = currentEpoch
	return newHighest, true
}

// Delete purges keyID's spend/rolling-window/alert-bucket state, in
// memory and (if a Store is configured) durably — for GDPR/CCPA
// erasure-request handling, per
// docs/upgrade-research/data-retention-right-to-erasure-2026-09-15.md.
// Called by dataplane.Pipeline.DeleteVirtualKey alongside the identity
// store's own deletion, folded into that existing route rather than a
// new standalone one — a key's budget spend has no independent lawful
// purpose once the key itself is deleted. A no-op, not an error, for a
// keyID with no recorded state at all.
func (t *Tracker) Delete(keyID string) error {
	t.mu.Lock()
	delete(t.spent, keyID)
	delete(t.periodStart, keyID)
	delete(t.periodEpoch, keyID)
	delete(t.billedCount, keyID)
	delete(t.highestAlertedBucket, keyID)
	delete(t.highestAlertedEpoch, keyID)
	t.mu.Unlock()
	if t.store == nil {
		return nil
	}
	return t.store.Delete(context.Background(), keyID)
}

// Close releases the underlying store, if any. Safe to call even on a
// Tracker constructed via NewTracker (no store).
func (t *Tracker) Close() error {
	if t.store == nil {
		return nil
	}
	return t.store.Close()
}
