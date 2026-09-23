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

	"github.com/kelvran/gateway/gateway/internal/telemetry"
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
//
// Store alone is NOT sufficient for correct multi-replica enforcement:
// Load runs once, at construction, and every decision afterward
// (Reserve/Reconcile/IncreaseReservation/SpentUSD/
// CheckAndMarkBudgetAlertBucket) is made against this Tracker's own LOCAL
// in-memory maps, never re-consulting Store — two replicas each backed by
// the same Store never see each other's spend in real time. RedisBackend
// below (via NewRedisTracker) is the actual cross-replica fix: the
// admission decision itself runs inside Redis, atomically, per
// internal/budget/redisbudget's own doc comment.
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

// RedisBackend is the cross-replica-consistent alternative to Store,
// implemented by internal/budget/redisbudget.Backend — a Tracker
// constructed via NewRedisTracker uses this exclusively (see each
// Tracker method's own Redis-mode branch) instead of the local in-memory
// maps NewTracker/NewTrackerWithStore populate. Per the same
// interface-lives-in-the-consumer idiom internal/ratelimit's own
// RedisBackend interface establishes, this package deliberately does not
// import internal/budget/redisbudget — *redisbudget.Backend satisfies
// this interface structurally.
//
// Every method takes capNanoUSD/deltaNanoUSD/newHighest amounts as
// integer NANO-USD (see usdToNanoUSD/nanoUSDToUSD) or a raw float64
// (MarkAlertBucket's newHighest, a percentage, never a currency amount) —
// never a decimal.Decimal directly, since that type cannot cross the Lua
// boundary redisbudget's scripts run inside.
type RedisBackend interface {
	// Reserve atomically reserves the FULL remaining headroom under
	// capNanoUSD for keyID (Redis mode's unconditional cold-start
	// reservation strategy — see redisbudget's own doc comment for why),
	// starting a fresh resetIntervalMs-TTL'd window if keyID has no
	// existing entry. resetIntervalMs <= 0 means no expiry (a
	// lifetime-of-the-key cap, mirroring the in-memory Tracker's own
	// resetInterval<=0 convention). epoch identifies the WINDOW this
	// reservation was taken against — the exact value that must be
	// threaded back through the matching Adjust call, unchanged, the
	// same way the in-memory branch's reservationEpoch already must be
	// (see Reserve's own doc comment below and
	// redisbudget.budgetReserveLuaSrc's doc comment for why this is a
	// real correctness requirement in Redis mode too, not merely an
	// in-memory-mode concept).
	Reserve(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (allowed bool, reservedNanoUSD int64, epoch int64, err error)
	// ReserveFixed atomically admits exactly deltaNanoUSD (not "everything
	// remaining") against capNanoUSD — IncreaseReservation's mid-stream
	// top-up shape. Never takes/needs an epoch: unlike Reserve+Adjust's
	// deferred reconciliation, each ReserveFixed call is immediate and
	// self-correcting (it re-reads current spend fresh every time).
	ReserveFixed(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (allowed bool, err error)
	// Adjust reconciles a prior Reserve reservation with a signed
	// deltaNanoUSD (realCost − reservedUSD, in nano-USD) — a silent no-op
	// if keyID's window already expired, OR if a DIFFERENT, newer window
	// has since started under this same keyID (epoch no longer matches
	// what this reservation was taken against) — see
	// redisbudget.budgetAdjustLuaSrc's own doc comment for why both
	// cases matter, not just the first. epoch must be exactly the value
	// the matching Reserve call returned.
	Adjust(ctx context.Context, keyID string, deltaNanoUSD, epoch int64) error
	// SpentNanoUSD reads keyID's current cumulative spend — a plain read,
	// no atomicity requirement of its own (mirrors SpentUSD's identical
	// in-memory read-only contract).
	SpentNanoUSD(ctx context.Context, keyID string) (int64, error)
	// MarkAlertBucket is CheckAndMarkBudgetAlertBucket's Redis-mode
	// equivalent: newHighest (a percentage, e.g. 0.75) is recorded only if
	// it exceeds whatever bucket is already marked for keyID.
	MarkAlertBucket(ctx context.Context, keyID string, newHighest float64, resetIntervalMs int64) (marked bool, err error)
	// MarkWarnAlerted is CheckAndMarkBudgetWarnAlerted's Redis-mode
	// equivalent: a plain "set exactly once per window" primitive backing
	// checkBudgetWarnThreshold's own webhook-delivery dedup, in a
	// COMPLETELY SEPARATE Redis key namespace from MarkAlertBucket's own
	// (see redisbudget.warnAlertKey's own doc comment) — the two
	// mechanisms never share dedup state, even though both key off the
	// same keyID/rolling-window shape. Unlike MarkAlertBucket, there is no
	// ladder/highest-value comparison here: marked is true exactly once
	// per resetIntervalMs-TTL'd window per keyID, false on every
	// subsequent call until that window rolls over.
	MarkWarnAlerted(ctx context.Context, keyID string, resetIntervalMs int64) (marked bool, err error)
	// Delete purges keyID's spend and alert-bucket state entirely — GDPR/
	// CCPA erasure, mirroring Store.Delete's identical contract.
	Delete(ctx context.Context, keyID string) error
	// Close closes the underlying Redis client.
	Close() error
}

// nanoUSDPerUSD is the fixed-point scale usdToNanoUSD/nanoUSDToUSD convert
// through: 9 decimal digits of USD precision, comfortably inside the
// range Lua/Redis's float64-based number type represents every integer
// exactly (up to 2^53, i.e. USD amounts up to roughly 9,000,000) — see
// internal/budget/redisbudget's own package doc comment for why crossing
// the Lua boundary as a float64 USD amount directly would silently
// reintroduce the exact drift decimal.Decimal was chosen to prevent.
const nanoUSDPerUSD = 9

// usdToNanoUSD converts a USD decimal.Decimal amount to an integer
// nano-USD scalar for a RedisBackend call. Rounds to the nearest whole
// nano-USD (sub-nano-USD precision does not exist anywhere else in this
// package either) — Shift(9) then Round(0) then IntPart() is an EXACT
// integer conversion at that point, never a lossy float64 round-trip.
func usdToNanoUSD(usd decimal.Decimal) int64 {
	return usd.Shift(nanoUSDPerUSD).Round(0).IntPart()
}

// nanoUSDToUSD converts a RedisBackend integer nano-USD scalar back to a
// USD decimal.Decimal — the exact inverse of usdToNanoUSD.
func nanoUSDToUSD(nano int64) decimal.Decimal {
	return decimal.New(nano, -nanoUSDPerUSD)
}

// resetIntervalToMs converts resetInterval to the millisecond TTL a
// RedisBackend call expects — 0 preserves resetInterval<=0's existing
// "no expiry" meaning exactly (time.Duration(0).Milliseconds() == 0).
func resetIntervalToMs(resetInterval time.Duration) int64 {
	return resetInterval.Milliseconds()
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
	// warnAlertedEpoch backs CheckAndMarkBudgetWarnAlerted's own dedup —
	// checkBudgetWarnThreshold's brand-new webhook-delivery capability,
	// firing at most once per rolling-window epoch per virtual key.
	// Deliberately a SEPARATE map (own key namespace, own Redis backend
	// method) from highestAlertedBucket/highestAlertedEpoch above, even
	// though both track "already alerted this epoch" shapes — the two
	// are independent mechanisms (a single warn-percent crossing vs. a
	// four-rung bucket ladder) that must never share dedup state with
	// each other. The map value is the epoch the key was last warned in;
	// callers must use the two-value map form to distinguish "never
	// warned" from "warned at epoch 0" (a lifetime-cap key, or any key's
	// very first window, never advances past epoch 0) — see
	// CheckAndMarkBudgetWarnAlerted's own doc comment.
	warnAlertedEpoch map[string]int64
	store            Store // nil = pure in-memory, unchanged from before this RFC
	// backend, when non-nil, routes every enforcement-relevant method
	// (Reserve/Reconcile/IncreaseReservation/SpentUSD/
	// CheckAndMarkBudgetAlertBucket/Delete/Store/Close) to Redis instead
	// of the local in-memory maps/store above — see NewRedisTracker and
	// each method's own Redis-mode branch. Mutually exclusive with store:
	// a Tracker constructed via NewRedisTracker never populates spent/
	// periodStart/periodEpoch/billedCount/store at all, since Redis's own
	// key expiration replaces resetIfNeeded's epoch-based window tracking
	// entirely (see RedisBackend's own doc comment).
	backend RedisBackend
	logger  *slog.Logger
	now     func() time.Time // real clock in production; overridden directly by white-box tests
}

// NewTracker constructs an empty, pure in-memory Tracker.
func NewTracker() *Tracker {
	return &Tracker{spent: make(map[string]decimal.Decimal), periodStart: make(map[string]time.Time), periodEpoch: make(map[string]int64), billedCount: make(map[string]int64), highestAlertedBucket: make(map[string]float64), highestAlertedEpoch: make(map[string]int64), warnAlertedEpoch: make(map[string]int64), now: time.Now}
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
	return &Tracker{spent: spent, periodStart: periodStart, periodEpoch: periodEpoch, billedCount: billedCount, highestAlertedBucket: make(map[string]float64), highestAlertedEpoch: make(map[string]int64), warnAlertedEpoch: make(map[string]int64), store: store, logger: logger, now: time.Now}, nil
}

// NewRedisTracker constructs a Tracker whose enforcement decisions run
// atomically inside Redis via backend, giving correct cap-check-and-debit
// behavior across any number of gateway replicas sharing one Redis
// instance — unlike NewTrackerWithStore, which only ever provides
// single-process restart-durability (see Store's own doc comment).
// logger defaults to slog.Default() if nil, mirroring
// NewTrackerWithStore's identical convention — used only to report a
// Redis backend error on a fail-open (never enforcement-fatal) path; see
// each method's own Redis-mode branch.
func NewRedisTracker(backend RedisBackend, logger *slog.Logger) *Tracker {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tracker{backend: backend, logger: logger, now: time.Now}
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
		telemetry.RecordPersistenceFailed(context.Background(), "budget", keyID)
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
//
// In Redis mode (t.backend != nil), reads straight through to
// backend.SpentNanoUSD — a backend error fails open exactly like
// Reserve's own fail-open contract, logged and reported as zero spend,
// since this is an observability read that must never itself block a
// request. ctx is only ever consulted in Redis mode.
func (t *Tracker) SpentUSD(ctx context.Context, keyID string, resetInterval time.Duration) decimal.Decimal {
	if t.backend != nil {
		nano, err := t.backend.SpentNanoUSD(ctx, keyID)
		if err != nil {
			t.logger.Warn("budget_redis_backend_unavailable", "key_id", keyID, "op", "spent_usd", "error", err.Error())
			return decimal.Zero
		}
		return nanoUSDToUSD(nano)
	}
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
		telemetry.RecordPersistenceFailed(context.Background(), "budget", keyID)
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
//
// In Redis mode (t.backend != nil), ctx is used for the real Redis call
// and err is non-nil exactly when that call itself failed (network error,
// timeout, script error) — callers must fail OPEN on a non-nil err
// (allowed=true, treated as if reserved=false, nothing to Reconcile),
// mirroring internal/ratelimit's identical "fail-open, not fail-closed"
// policy; err is always nil in in-memory mode. reservationEpoch in Redis
// mode is whatever RedisBackend.Reserve returned — a real per-window
// value (see that method's own doc comment), not the always-0
// placeholder this used to be before this session's own end-to-end
// audit found the cross-window corruption that elided value allowed —
// it MUST still be threaded through to Reconcile unchanged, exactly
// like the in-memory branch's value, since Reconcile's Redis-mode
// branch now genuinely consults it too.
func (t *Tracker) Reserve(ctx context.Context, keyID string, capUSD decimal.Decimal, resetInterval time.Duration) (allowed bool, reserved bool, reservedUSD decimal.Decimal, reservationEpoch int64, err error) {
	if t.backend != nil {
		if capUSD.Sign() <= 0 {
			return true, false, decimal.Zero, 0, nil
		}
		allowedRedis, reservedNano, epoch, redisErr := t.backend.Reserve(ctx, keyID, usdToNanoUSD(capUSD), resetIntervalToMs(resetInterval))
		if redisErr != nil {
			return false, false, decimal.Zero, 0, redisErr
		}
		if !allowedRedis {
			return false, false, decimal.Zero, 0, nil
		}
		return true, true, nanoUSDToUSD(reservedNano), epoch, nil
	}

	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
	}
	if capUSD.Sign() <= 0 {
		return true, false, decimal.Zero, 0, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.spent[keyID].LessThan(capUSD) {
		return false, false, decimal.Zero, 0, nil
	}
	reservedUSD = t.reservationAmountLocked(keyID, capUSD)
	t.spent[keyID] = t.spent[keyID].Add(reservedUSD)
	return true, true, reservedUSD, t.periodEpoch[keyID], nil
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
//
// In Redis mode (t.backend != nil), reservationEpoch IS consulted —
// passed straight through to RedisBackend.Adjust, which no-ops both
// when keyID's window already expired (key absent) AND when a
// DIFFERENT, newer window has since started under this same keyID
// (epoch field no longer matches), covering the exact cross-window
// corruption case the in-memory epoch check above exists for. This
// used to be elided entirely (reservationEpoch was always hardcoded 0
// in Redis mode, and Adjust had no epoch concept at all) — a real
// HIGH-severity gap this session's own end-to-end audit found: a
// reservation that outlived its own window's TTL could have its stale
// delta applied to an unrelated LATER window that happened to reuse the
// same Redis key, since "key absent" alone cannot distinguish "this
// window is gone" from "this window was REPLACED by a newer one." The
// delta applied is realCost−reservedUSD when billed, or −reservedUSD on
// a release-only call — the same net effect the in-memory branch's
// subtract-then-add achieves via two decimal operations. A Redis backend
// error here is logged and swallowed, never propagated: Reconcile is a
// cleanup call with no caller left to fail open FOR — the in-memory
// spend/enforcement path has already run its course by the time this is
// called, exactly as a Store.Save persistence failure is already
// non-fatal here today.
func (t *Tracker) Reconcile(ctx context.Context, keyID string, reservedUSD decimal.Decimal, reservationEpoch int64, realCost *decimal.Decimal, resetInterval time.Duration) {
	if t.backend != nil {
		billed := realCost != nil && realCost.Sign() >= 0
		delta := decimal.Zero.Sub(reservedUSD)
		if billed {
			delta = realCost.Sub(reservedUSD)
		}
		if err := t.backend.Adjust(ctx, keyID, usdToNanoUSD(delta), reservationEpoch); err != nil {
			t.logger.Warn("budget_redis_backend_unavailable", "key_id", keyID, "op", "reconcile", "error", err.Error())
		}
		return
	}

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
		telemetry.RecordPersistenceFailed(context.Background(), "budget", keyID)
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
//
// In Redis mode (t.backend != nil), reservationEpoch is accepted but
// never consulted here — unlike Reconcile's own Redis-mode branch
// (which now genuinely checks it, see that method's own doc comment),
// ReserveFixed's admission decision is immediate and self-correcting on
// every call, so there is no deferred delta here that could land in the
// wrong window. newReservationEpoch is always returned unchanged, since
// a top-up never creates a new window — the SAME epoch value must still
// reach the eventual Reconcile call this reservation is paired with. A
// backend error fails
// OPEN exactly like checkMidStreamReservationTopup's own established
// policy for the TPM dimension's IncreaseReservationTPM: err is non-nil,
// allowed is true, and appliedUSD/newReservationEpoch are returned
// UNCHANGED (currentReservedUSD/reservationEpoch) — the caller must
// treat a non-nil err as "no top-up happened, but don't cut off the
// stream for a Redis-reachability problem."
func (t *Tracker) IncreaseReservation(ctx context.Context, keyID string, capUSD, currentReservedUSD, newReservedUSD decimal.Decimal, reservationEpoch int64, resetInterval time.Duration) (allowed bool, appliedUSD decimal.Decimal, newReservationEpoch int64, err error) {
	if t.backend != nil {
		if capUSD.Sign() <= 0 || !newReservedUSD.GreaterThan(currentReservedUSD) {
			return true, currentReservedUSD, reservationEpoch, nil
		}
		delta := newReservedUSD.Sub(currentReservedUSD)
		allowedRedis, redisErr := t.backend.ReserveFixed(ctx, keyID, usdToNanoUSD(capUSD), usdToNanoUSD(delta), resetIntervalToMs(resetInterval))
		if redisErr != nil {
			return true, currentReservedUSD, reservationEpoch, redisErr
		}
		if !allowedRedis {
			return false, currentReservedUSD, reservationEpoch, nil
		}
		return true, newReservedUSD, reservationEpoch, nil
	}

	if justReset, ps, pe := t.resetIfNeeded(keyID, resetInterval); justReset {
		t.persistZeroIfStoreConfigured(keyID, ps, pe)
	}
	if capUSD.Sign() <= 0 || !newReservedUSD.GreaterThan(currentReservedUSD) {
		return true, currentReservedUSD, reservationEpoch, nil
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
			return false, decimal.Zero, currentEpoch, nil
		}
		t.spent[keyID] = t.spent[keyID].Add(newReservedUSD)
		return true, newReservedUSD, currentEpoch, nil
	}

	delta := newReservedUSD.Sub(currentReservedUSD)
	if t.spent[keyID].Add(delta).GreaterThan(capUSD) {
		return false, currentReservedUSD, currentEpoch, nil
	}
	t.spent[keyID] = t.spent[keyID].Add(delta)
	return true, newReservedUSD, currentEpoch, nil
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
//
// In Redis mode (t.backend != nil), the dedup lives in
// backend.MarkAlertBucket (a compare-and-set against its own
// resetInterval-TTL'd key) rather than highestAlertedBucket/
// highestAlertedEpoch — resetInterval is only ever consulted in this
// mode, to size that key's TTL the same way Reserve sizes the spend
// key's own. A backend error is logged and treated as "not crossed" —
// this is an alert-dedup signal, never an enforcement decision, so
// failing open (never alerting, in the worst case) is the correct
// default, not failing the request.
func (t *Tracker) CheckAndMarkBudgetAlertBucket(ctx context.Context, keyID string, percentUsed float64, resetInterval time.Duration) (bucket float64, crossed bool) {
	newHighest := 0.0
	for _, b := range BudgetAlertBuckets {
		if percentUsed >= b && b > newHighest {
			newHighest = b
		}
	}
	if newHighest <= 0 {
		return 0, false
	}

	if t.backend != nil {
		marked, err := t.backend.MarkAlertBucket(ctx, keyID, newHighest, resetIntervalToMs(resetInterval))
		if err != nil {
			t.logger.Warn("budget_redis_backend_unavailable", "key_id", keyID, "op", "check_alert_bucket", "error", err.Error())
			return 0, false
		}
		if !marked {
			return 0, false
		}
		return newHighest, true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	currentEpoch := t.periodEpoch[keyID]
	alreadyAlerted := 0.0
	if t.highestAlertedEpoch[keyID] == currentEpoch {
		alreadyAlerted = t.highestAlertedBucket[keyID]
	}
	if newHighest <= alreadyAlerted {
		return 0, false
	}
	t.highestAlertedBucket[keyID] = newHighest
	t.highestAlertedEpoch[keyID] = currentEpoch
	return newHighest, true
}

// CheckAndMarkBudgetWarnAlerted reports whether this is the first call for
// keyID within its CURRENT rolling-window epoch (see the periodEpoch field
// comment) since checkBudgetWarnThreshold's own warn-percent threshold was
// crossed — the dedup gate for that function's webhook-delivery
// capability, added alongside (never replacing) its existing,
// deliberately-unchanged re-logs-every-request slog line; see that
// function's own doc comment for why the two are now two independent
// mechanisms. Unlike CheckAndMarkBudgetAlertBucket, there is no ladder
// here: the caller has already determined the single warn-percent
// threshold was crossed, so this method's only job is "has keyID already
// been marked warned this window" — newlyCrossed is false on every
// subsequent call within the same epoch, and true again once the epoch
// advances (a fresh rolling window has no warn history of its own yet
// either, mirroring CheckAndMarkBudgetAlertBucket's identical per-epoch
// reasoning).
//
// Deliberately does NOT call resetIfNeeded itself — exactly like
// CheckAndMarkBudgetAlertBucket, it relies on the caller (finalize) having
// already reconciled the real cost for this request BEFORE calling this,
// which is what actually advances periodEpoch for a just-elapsed window;
// see that method's own doc comment for the same reasoning.
//
// Uses the two-value map form (t.warnAlertedEpoch[keyID]) rather than a
// bare index expression: a lifetime-cap key (resetInterval <= 0, whose
// periodEpoch never advances past 0) — or, for that matter, ANY key's
// very first rolling window — has currentEpoch == 0, which is also
// map[string]int64's own zero value. Without distinguishing "never
// warned" (ok == false) from "warned at epoch 0" (ok == true, epoch ==
// 0), the very first crossing for such a key would be silently treated
// as already-warned and never fire its webhook at all.
//
// In Redis mode (t.backend != nil), the dedup lives in
// backend.MarkWarnAlerted (a "set exactly once per window" compare-and-set
// against its own resetInterval-TTL'd key, in a completely separate Redis
// key namespace from MarkAlertBucket's own — see redisbudget.warnAlertKey's
// own doc comment for why they must never collide) rather than
// warnAlertedEpoch. A backend error is logged and treated as "not
// crossed" — this is an alert-dedup signal for a webhook-delivery
// capability, never an enforcement decision, so failing open (never
// alerting, in the worst case) is the correct default, exactly mirroring
// CheckAndMarkBudgetAlertBucket's own identical failure handling.
func (t *Tracker) CheckAndMarkBudgetWarnAlerted(ctx context.Context, keyID string, resetInterval time.Duration) (newlyCrossed bool) {
	if t.backend != nil {
		marked, err := t.backend.MarkWarnAlerted(ctx, keyID, resetIntervalToMs(resetInterval))
		if err != nil {
			t.logger.Warn("budget_redis_backend_unavailable", "key_id", keyID, "op", "check_warn_alerted", "error", err.Error())
			return false
		}
		return marked
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	currentEpoch := t.periodEpoch[keyID]
	if alertedEpoch, alreadyWarned := t.warnAlertedEpoch[keyID]; alreadyWarned && alertedEpoch == currentEpoch {
		return false
	}
	t.warnAlertedEpoch[keyID] = currentEpoch
	return true
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
//
// In Redis mode (t.backend != nil), routes straight to backend.Delete
// with a background context — mirrors Store.Delete's own
// context.Background() convention for this same non-hot-path,
// admin-mutation call (UpsertVirtualKey/DeleteVirtualKey neither thread
// a request-scoped ctx of their own).
func (t *Tracker) Delete(keyID string) error {
	if t.backend != nil {
		return t.backend.Delete(context.Background(), keyID)
	}
	t.mu.Lock()
	delete(t.spent, keyID)
	delete(t.periodStart, keyID)
	delete(t.periodEpoch, keyID)
	delete(t.billedCount, keyID)
	delete(t.highestAlertedBucket, keyID)
	delete(t.highestAlertedEpoch, keyID)
	delete(t.warnAlertedEpoch, keyID)
	t.mu.Unlock()
	if t.store == nil {
		return nil
	}
	return t.store.Delete(context.Background(), keyID)
}

// Store returns the underlying durable Store, or nil if this Tracker was
// constructed via NewTracker (no store) or NewRedisTracker (a Redis-mode
// Tracker has nothing bbolt-backed to back up at all — Redis handles its
// own durability/replication) — an escape hatch for a caller that needs
// to reach the concrete implementation (e.g.
// dataplane.Pipeline.BackupStores type-asserting for a bbolt backup
// primitive), since Tracker itself has no backup-shaped method of its
// own; it only ever calls Load/Save/Delete/Close on this value.
func (t *Tracker) Store() Store {
	return t.store
}

// Close releases the underlying store or Redis backend, if any. Safe to
// call even on a Tracker constructed via NewTracker (no store, no
// backend).
func (t *Tracker) Close() error {
	if t.backend != nil {
		return t.backend.Close()
	}
	if t.store == nil {
		return nil
	}
	return t.store.Close()
}
