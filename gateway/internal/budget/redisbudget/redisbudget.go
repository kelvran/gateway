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
// The rolling window is implemented via Redis's OWN key expiration, not
// a separate epoch counter: a TTL is set only ONCE, when the key is
// first created (a fresh window) — every subsequent write within that
// same window uses KEEPTTL, preserving the original expiry rather than
// perpetually extending it on every request. When the key expires,
// Redis itself deletes it; the next Reserve/ReserveFixed/Adjust call
// naturally observes "key absent" and starts a fresh window — no
// distinct epoch value is ever threaded through, unlike the in-memory
// Tracker's periodEpoch (which exists specifically to protect against a
// LOCAL map being wholesale reset while a reservation is in flight; Redis
// mode has no such object-identity concern, since AdjustTPM-style
// delta-based updates are safe under arbitrary concurrent interleaving —
// see budget.go's own doc comment on why this mirrors
// internal/ratelimit's identical Redis-mode epoch-elision).
const budgetReserveLuaSrc = `
local cap = tonumber(ARGV[1])
local reset_interval_ms = tonumber(ARGV[2])

local spent = redis.call('GET', KEYS[1])
local is_new_window = false
if spent == false then
  spent = 0
  is_new_window = true
else
  spent = tonumber(spent)
end

if spent >= cap then
  return {0, '0'}
end

local reserved = cap - spent
local new_spent = spent + reserved

if is_new_window and reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_spent, 'PX', reset_interval_ms)
elseif reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_spent, 'KEEPTTL')
else
  redis.call('SET', KEYS[1], new_spent)
end

return {1, tostring(reserved)}
`

// budgetReserveFixedLuaSrc admits exactly delta (not "everything
// remaining") — the mid-stream reservation top-up's own shape
// (IncreaseReservation): a conditional admission check, rejecting
// outright if delta would push spent past cap, rather than
// budgetReserveLuaSrc's unconditional "take it all" cold-start
// behavior. Same window/TTL handling as budgetReserveLuaSrc.
const budgetReserveFixedLuaSrc = `
local cap = tonumber(ARGV[1])
local delta = tonumber(ARGV[2])
local reset_interval_ms = tonumber(ARGV[3])

local spent = redis.call('GET', KEYS[1])
local is_new_window = false
if spent == false then
  spent = 0
  is_new_window = true
else
  spent = tonumber(spent)
end

if spent + delta > cap then
  return {0}
end

local new_spent = spent + delta
if is_new_window and reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_spent, 'PX', reset_interval_ms)
elseif reset_interval_ms > 0 then
  redis.call('SET', KEYS[1], new_spent, 'KEEPTTL')
else
  redis.call('SET', KEYS[1], new_spent)
end

return {1}
`

// budgetAdjustLuaSrc reconciles a prior reservation with the real cost,
// once known — adds deltaNanoUSD (signed: realCost - reservedUSD) onto
// the stored spend, clamped at a floor of 0 (an over-release larger than
// what was ever reserved must not drive spend negative, which would
// otherwise manufacture free extra headroom on the NEXT reservation). A
// missing key (the window already rolled over via expiration since the
// reservation was taken) is a silent no-op — mirroring the in-memory
// epoch-guard's identical "the reservation's own window no longer
// exists, nothing to correct" behavior, obtained here for free via
// Redis's own expiration rather than an explicit epoch comparison.
const budgetAdjustLuaSrc = `
local delta = tonumber(ARGV[1])

local spent = redis.call('GET', KEYS[1])
if spent == false then
  return redis.status_reply('OK')
end
spent = tonumber(spent)

local new_spent = math.max(spent + delta, 0)
redis.call('SET', KEYS[1], new_spent, 'KEEPTTL')
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

func spendKey(keyID string) string { return "budget:" + keyID }
func alertKey(keyID string) string { return "budget:alert:" + keyID }

// Reserve implements budget.RedisBackend.
func (b *Backend) Reserve(ctx context.Context, keyID string, capNanoUSD, resetIntervalMs int64) (allowed bool, reservedNanoUSD int64, err error) {
	val, err := b.reserveScript.Run(ctx, b.client, []string{spendKey(keyID)}, capNanoUSD, resetIntervalMs).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redisbudget: running reserve script for key %q: %w", keyID, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 2 {
		return false, 0, fmt.Errorf("redisbudget: unexpected reserve script result shape %v for key %q", val, keyID)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return false, 0, fmt.Errorf("redisbudget: unexpected reserve script result[0] type %T for key %q", results[0], keyID)
	}
	reservedStr, ok := results[1].(string)
	if !ok {
		return false, 0, fmt.Errorf("redisbudget: unexpected reserve script result[1] type %T for key %q", results[1], keyID)
	}
	reservedNanoUSD, err = strconv.ParseInt(reservedStr, 10, 64)
	if err != nil {
		return false, 0, fmt.Errorf("redisbudget: parsing reserved amount %q for key %q: %w", reservedStr, keyID, err)
	}
	return allowedInt == 1, reservedNanoUSD, nil
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

// Adjust implements budget.RedisBackend.
func (b *Backend) Adjust(ctx context.Context, keyID string, deltaNanoUSD int64) error {
	if _, err := b.adjustScript.Run(ctx, b.client, []string{spendKey(keyID)}, deltaNanoUSD).Result(); err != nil {
		return fmt.Errorf("redisbudget: running adjust script for key %q: %w", keyID, err)
	}
	return nil
}

// SpentNanoUSD implements budget.RedisBackend. A plain GET, not a Lua
// script — a read-only query has no atomicity requirement of its own.
func (b *Backend) SpentNanoUSD(ctx context.Context, keyID string) (int64, error) {
	val, err := b.client.Get(ctx, spendKey(keyID)).Result()
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
