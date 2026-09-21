// Package redisbudget implements internal/budget.RedisBackend over
// Redis, giving budget.Tracker's cap-check-and-debit a genuinely atomic,
// cross-replica-consistent implementation — unlike internal/budget.Store
// (Load/Save/Delete/Close), which only ever provides single-process
// restart-durability: Store.Load runs once at construction and
// Tracker's own Reserve/Reconcile/IncreaseReservation always decide
// against LOCAL in-memory maps afterward, so two replicas each backed by
// the same Store never actually see each other's spend in real time.
// This package closes that gap the same way internal/ratelimit/
// redislimiter does for the rate-limit dimension: the admission decision
// itself runs inside Redis, atomically, via Lua.
//
// Per the same interface-lives-in-the-consumer idiom
// internal/ratelimit/redislimiter's own doc comment establishes, this
// package deliberately does not import internal/budget — *Backend
// satisfies that package's RedisBackend interface structurally.
//
// USD amounts cross the Lua boundary as integer NANO-USD (decimal.Decimal
// shifted by 1e9, per budget.go's own usdToNanoUSD/nanoUSDToUSD helpers)
// — never as a float string parsed by Lua's own number type, which is a
// float64 double. Doing arithmetic on a float64 representation of a
// currency amount would silently reintroduce the exact repeated-addition
// drift decimal.Decimal was chosen to prevent in the first place (per
// budget.go's own package doc comment). Nano-USD gives 9 decimal digits
// of USD precision while every value handled here stays comfortably
// inside the range Lua/Redis's float64-based number type represents
// exactly (integers up to 2^53, i.e. USD amounts up to roughly
// 9,000,000 — far beyond any realistic per-key budget cap).
package redisbudget

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// budgetReserveLuaSrc reserves the FULL remaining headroom under cap in
// one atomic step — the Redis-mode analog of
// budget.Tracker.reservationAmountLocked's own "no billing history yet"
// cold-start-conservative fallback, used unconditionally in Redis mode
// (no historical-average tracking exists here — a disclosed, deliberate
// simplification: this affects concurrent-request THROUGHPUT under a
// near-exhausted cap, since a single in-flight reservation can
// temporarily consume all remaining headroom, but never CORRECTNESS —
// spend can never exceed cap, and a later Reconcile's real-cost delta
// always corrects the ledger exactly).
//
// The rolling window is implemented via Redis's OWN key expiration: a
// TTL is set only ONCE, when the key is first created (a fresh window),
// via PEXPIRE right after HSET (HSET on an existing hash never touches
// an already-set TTL, so no KEEPTTL-equivalent call is needed on
// subsequent writes within the same window). When the key expires,
// Redis itself deletes it; the next Reserve/ReserveFixed/Adjust call
// naturally observes "key absent" and starts a fresh window.
//
// Every window ALSO gets its own epoch value (the 'epoch' hash field,
// generated from redis.call('TIME') at window-creation, never a
// client-supplied timestamp — the same clock-skew-safety reason
// tpmLuaSrc's own doc comment gives for internal/ratelimit/redislimiter)
// — added 2026-09-21 closing a real HIGH-severity finding from this
// session's own end-to-end audit: without it, a Reserve/Reconcile pair
// spanning a window boundary could have its STALE delta applied to a
// completely different, unrelated LATER window that happened to reuse
// the same Redis key after the original window expired and a new
// Reserve started a fresh one — Adjust's own "key absent -> no-op"
// check (the ONLY correctness check that existed here before this fix)
// only catches the window being GONE, never the window having been
// REPLACED by a newer one by the time a late Adjust call finally runs.
// budget.go's in-memory Tracker.periodEpoch has always guarded against
// exactly this cross-window case (see that field's own comment); this
// epoch value is Redis mode's genuine equivalent, not an elided
// simplification the way earlier doc comments here claimed.
const budgetReserveLuaSrc = `
local cap = tonumber(ARGV[1])
local reset_interval_ms = tonumber(ARGV[2])

local state = redis.call('HMGET', KEYS[1], 'spent', 'epoch')
local spent = tonumber(state[1])
local epoch = tonumber(state[2])
local is_new_window = false
if spent == nil then
  spent = 0
  is_new_window = true
  local time = redis.call('TIME')
  epoch = tonumber(time[1]) * 1000000 + tonumber(time[2])
end

if spent >= cap then
  return {0, 0, epoch}
end

local reserved = cap - spent
local new_spent = spent + reserved

redis.call('HSET', KEYS[1], 'spent', new_spent, 'epoch', epoch)
if is_new_window and reset_interval_ms > 0 then
  redis.call('PEXPIRE', KEYS[1], reset_interval_ms)
end

-- reserved/epoch are returned as bare Lua numbers, NEVER tostring()'d --
-- Lua 5.1's tostring on a number uses the C "%.14g" format, which
-- switches to scientific notation ("1e+14") once the value's exponent
-- reaches 14 significant digits; strconv.ParseInt on the Go side then
-- fails on that string, making Reserve silently behave as "denied" for
-- any cap >= $100,000 (100,000 USD * 1e9 nano-USD/USD == 1e14). Redis's
-- own Lua-to-RESP conversion for a bare number is a (long long) cast --
-- exact for any integer-valued float64 up to 2^53 (~9,007,199 nano-USD
-- WHOLE-USD units, i.e. ~$9,007,199 -- matches this package's own doc
-- comment on the real precision ceiling), and both values are always
-- integer-valued, so this cast never loses precision in practice. The
-- denied branch above returns a bare 0 (not '0') for the identical
-- reason -- Go's Reserve must be able to type-assert every branch's
-- elements the same way.
return {1, reserved, epoch}
`

// budgetReserveFixedLuaSrc admits exactly delta (not "everything
// remaining") — the mid-stream reservation top-up's own shape
// (IncreaseReservation): a conditional admission check, rejecting
// outright if delta would push spent past cap, rather than
// budgetReserveLuaSrc's unconditional "take it all" cold-start
// behavior. Same Hash/window/TTL handling as budgetReserveLuaSrc (same
// KEYS[1], so it must speak the identical Hash shape) — but never
// touches the 'epoch' field itself: unlike Reserve+Adjust's own
// deferred, potentially-cross-window reconciliation, ReserveFixed's
// admission decision is immediate and self-correcting on every call
// (it re-reads current spend fresh each time), so it never needs an
// epoch to detect a stale delta landing in the wrong window — there is
// no deferred delta here at all. On a genuinely new window, epoch is
// still generated and stored (identically to budgetReserveLuaSrc) so a
// LATER Reserve/Adjust pair against this same window has a real epoch
// to check against, rather than a hash missing that field entirely.
const budgetReserveFixedLuaSrc = `
local cap = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local reset_interval_ms = tonumber(ARGV[3])

local state = redis.call('HMGET', KEYS[1], 'spent', 'epoch')
local spent = tonumber(state[1])
local epoch = tonumber(state[2])
local is_new_window = false
if spent == nil then
  spent = 0
  is_new_window = true
  local time = redis.call('TIME')
  epoch = tonumber(time[1]) * 1000000 + tonumber(time[2])
end

if spent + delta > cap then
  return {0}
end

local new_spent = spent + delta
redis.call('HSET', KEYS[1], 'spent', new_spent, 'epoch', epoch)
if is_new_window and reset_interval_ms > 0 then
  redis.call('PEXPIRE', KEYS[1], reset_interval_ms)
end

return {1}
`

// budgetAdjustLuaSrc reconciles a prior reservation with the real cost,
// once known — adds deltaNanoUSD (signed: realCost - reservedUSD) onto
// the stored spend, clamped at a floor of 0 (an over-release larger than
// what was ever reserved must not drive spend negative, which would
// otherwise manufacture free extra headroom on the NEXT reservation). A
// missing key (the window already rolled over via expiration since the
// reservation was taken, and no NEWER window has started yet either) is
// a silent no-op.
//
// expectedEpoch (ARGV[2], the epoch value Reserve returned alongside
// this same reservation — see budgetReserveLuaSrc's own doc comment) is
// checked against the key's CURRENT epoch field before applying delta —
// a mismatch means a newer window has since started under this same
// keyID, and this reservation's delta belongs to the OLD window that no
// longer exists, not the one currently stored here; applying it would
// silently corrupt an entirely unrelated window's ledger. This is the
// actual fix for a real HIGH-severity finding from this session's own
// end-to-end audit — before it existed, only "key absent" was checked,
// which misses exactly this cross-window case (the key isn't absent,
// it's just a DIFFERENT window now).
const budgetAdjustLuaSrc = `
local delta = tonumber(ARGV[1])
local expected_epoch = tonumber(ARGV[2])

local state = redis.call('HMGET', KEYS[1], 'spent', 'epoch')
local spent = tonumber(state[1])
local epoch = tonumber(state[2])
if spent == nil or epoch ~= expected_epoch then
  return redis.status_reply('OK')
end

local new_spent = math.max(spent + delta, 0)
redis.call('HSET', KEYS[1], 'spent', new_spent)
return redis.status_reply('OK')
`

// budgetAlertLuaSrc is a compare-and-set primitive for the budget-alert
// ladder's own dedup (budget.Tracker.CheckAndMarkBudgetAlertBucket's
// Redis-mode equivalent): newHighest is written only if it exceeds
// whatever bucket is already recorded, reported via the returned
// `marked` bool. Manages its own window-tied TTL independently of the
// spend key (same first-write-only/KEEPTTL convention) rather than
// reading the spend key's own remaining TTL — a small, disclosed
// simplification: the alert key's own window boundary can drift
// slightly from the spend key's if the two are first touched at
// different times, but this only affects WHEN a stale alert-bucket
// dedup clears (cosmetic), never actual budget enforcement, which never
// reads this key at all.
const budgetAlertLuaSrc = `
local new_highest = tonumber(ARGV[1])
local reset_interval_ms = tonumber(ARGV[2])

local current = redis.call('GET', KEYS[1])
local is_new_window = false
if current == false then
  current = 0
  is_new_window = true
else
  current = tonumber(current)
end

if new_highest <= current then
  return {0, tostring(current)}
end

if is_new_window and reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_highest, 'PX', reset_interval_ms)
elseif reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_highest, 'KEEPTTL')
else
  redis.call('SET', KEYS[1], new_highest)
end
return {1, tostring(new_highest)}
`

// Backend is a Redis-backed implementation of internal/budget.RedisBackend.
// The zero value is not usable — construct with Open.
type Backend struct {
	client        *redis.Client
	reserveScript *redis.Script
	fixedScript   *redis.Script
	adjustScript  *redis.Script
	alertScript   *redis.Script
}

// Open constructs a Backend against the Redis server at addr
// ("host:port"). go-redis dials lazily, mirroring
// internal/ratelimit/redislimiter.Open's identical "never fails on an
// unreachable addr" contract — the gateway itself must still be able to
// start even when Redis is briefly unreachable.
func Open(addr string) (*Backend, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	return &Backend{
		client:        client,
		reserveScript: redis.NewScript(budgetReserveLuaSrc),
		fixedScript:   redis.NewScript(budgetReserveFixedLuaSrc),
		adjustScript:  redis.NewScript(budgetAdjustLuaSrc),
		alertScript:   redis.NewScript(budgetAlertLuaSrc),
	}, nil
}

// spendKey/alertKey percent-encode keyID (via url.QueryEscape) before
// concatenating, rather than embedding it raw — closing a real
// namespace-collision finding from this session's own end-to-end audit:
// a virtual-key ID containing a literal "alert:" (e.g. "alert:foo") made
// spendKey("alert:foo") == "budget:alert:foo" collide with
// alertKey("foo") == "budget:alert:foo", since neither function's own
// fixed prefix is distinguishable from user-controlled content once
// concatenated raw. url.QueryEscape encodes every ':' as "%3A" (not in
// its unreserved charset), so an escaped keyID can never contain a raw
// ':' — meaning spendKey's own output can never coincidentally start
// with "budget:alert:" unless keyID itself, unescaped, literally begins
// with an already-escaped "alert%3A", which is a genuinely different
// string once escaped and therefore never equal to alertKey's own
// literal "alert:" infix. Virtual-key IDs have no charset restriction
// today (any string an admin call accepts), so this must hold for
// arbitrary input, not just the common case.
func spendKey(keyID string) string { return "budget:" + url.QueryEscape(keyID) }
func alertKey(keyID string) string { return "budget:alert:" + url.QueryEscape(keyID) }

// Reserve implements budget.RedisBackend.
func (b *Backend) Reserve(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (allowed bool, reservedNanoUSD int64, epoch int64, err error) {
	val, err := b.reserveScript.Run(ctx, b.client, []string{spendKey(keyID)}, capNanoUSD, resetIntervalMs).Result()
	if err != nil {
		return false, 0, 0, fmt.Errorf("redisbudget: running reserve script for key %q: %w", keyID, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 3 {
		return false, 0, 0, fmt.Errorf("redisbudget: unexpected reserve script result shape %v for key %q", val, keyID)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("redisbudget: unexpected reserve script result[0] type %T for key %q", results[0], keyID)
	}
	// reserved/epoch are bare Lua numbers (Redis's own Lua-to-RESP
	// conversion delivers them as Integer replies, parsed by go-redis as
	// int64) -- NEVER tostring()'d strings, which would silently
	// truncate to scientific notation ("1e+14") for any cap >=
	// $100,000 and fail this exact type assertion. See
	// budgetReserveLuaSrc's own doc comment for the full explanation.
	reservedNanoUSD, ok = results[1].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("redisbudget: unexpected reserve script result[1] type %T for key %q", results[1], keyID)
	}
	epoch, ok = results[2].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("redisbudget: unexpected reserve script result[2] type %T for key %q", results[2], keyID)
	}
	return allowedInt == 1, reservedNanoUSD, epoch, nil
}

// ReserveFixed implements budget.RedisBackend.
func (b *Backend) ReserveFixed(ctx context.Context, keyID string, capNanoUSD, deltaNanoUSD, resetIntervalMs int64) (allowed bool, err error) {
	val, err := b.fixedScript.Run(ctx, b.client, []string{spendKey(keyID)}, capNanoUSD, deltaNanoUSD, resetIntervalMs).Result()
	if err != nil {
		return false, fmt.Errorf("redisbudget: running reserve-fixed script for key %q: %w", keyID, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 1 {
		return false, fmt.Errorf("redisbudget: unexpected reserve-fixed script result shape %v for key %q", val, keyID)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return false, fmt.Errorf("redisbudget: unexpected reserve-fixed script result[0] type %T for key %q", results[0], keyID)
	}
	return allowedInt == 1, nil
}

// Adjust implements budget.RedisBackend. epoch must be exactly the
// value Reserve returned alongside the reservation being reconciled —
// see budgetAdjustLuaSrc's own doc comment for why a mismatch (a newer
// window has since started under this same keyID) makes this a silent
// no-op rather than corrupting that newer window's spend.
func (b *Backend) Adjust(ctx context.Context, keyID string, deltaNanoUSD, epoch int64) error {
	if _, err := b.adjustScript.Run(ctx, b.client, []string{spendKey(keyID)}, deltaNanoUSD, epoch).Result(); err != nil {
		return fmt.Errorf("redisbudget: running adjust script for key %q: %w", keyID, err)
	}
	return nil
}

// SpentNanoUSD implements budget.RedisBackend. A plain HGET (spendKey
// is a Hash — see budgetReserveLuaSrc's own doc comment for why),
// not a Lua script — a read-only query has no atomicity requirement of
// its own.
func (b *Backend) SpentNanoUSD(ctx context.Context, keyID string) (int64, error) {
	val, err := b.client.HGet(ctx, spendKey(keyID), "spent").Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redisbudget: getting spend for key %q: %w", keyID, err)
	}
	spent, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("redisbudget: parsing spend %q for key %q: %w", val, keyID, err)
	}
	return spent, nil
}

// MarkAlertBucket implements budget.RedisBackend.
func (b *Backend) MarkAlertBucket(ctx context.Context, keyID string, newHighest float64, resetIntervalMs int64) (marked bool, err error) {
	val, err := b.alertScript.Run(ctx, b.client, []string{alertKey(keyID)}, newHighest, resetIntervalMs).Result()
	if err != nil {
		return false, fmt.Errorf("redisbudget: running alert script for key %q: %w", keyID, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 2 {
		return false, fmt.Errorf("redisbudget: unexpected alert script result shape %v for key %q", val, keyID)
	}
	markedInt, ok := results[0].(int64)
	if !ok {
		return false, fmt.Errorf("redisbudget: unexpected alert script result[0] type %T for key %q", results[0], keyID)
	}
	return markedInt == 1, nil
}

// Delete implements budget.RedisBackend — purges both the spend and
// alert-bucket keys for keyID, mirroring Tracker.Delete's in-memory
// equivalent (GDPR/CCPA erasure). A no-op, not an error, for a keyID
// with no recorded state at all (DEL on a missing key is always a
// harmless 0).
func (b *Backend) Delete(ctx context.Context, keyID string) error {
	if err := b.client.Del(ctx, spendKey(keyID), alertKey(keyID)).Err(); err != nil {
		return fmt.Errorf("redisbudget: deleting key %q: %w", keyID, err)
	}
	return nil
}

// Close closes the underlying Redis client.
func (b *Backend) Close() error {
	return b.client.Close()
}
