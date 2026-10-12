package redislimiter

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// tpmHistoryTTL bounds how long a key's TPM usage history outlives its last
// settlement. The arrival-time key itself lives for seconds (tpmLuaSrc), but
// the history is what sizes the NEXT reservation: a key's usage profile is
// stable over a day, the hash is a few bytes per key and model, and a key
// idle for longer than this simply pays one cold-start reservation again.
const tpmHistoryTTL = 24 * time.Hour

// tpmHistoryKey is the Redis key holding key's running usage history, a
// hash with `sum` (real tokens settled) and `count` (settlements). It sits
// beside the arrival-time key under the same escaped suffix: url.QueryEscape
// never emits ':', so no escaped key can end in ":hist" and collide. A
// sibling key, not a richer value in the arrival-time key, because replicas
// on the previous release still read that key with tonumber -- a changed
// shape would break a rolling upgrade, a sibling does not.
func tpmHistoryKey(key string) string {
	return "ratelimit:tpm:" + url.QueryEscape(key) + ":hist"
}

// tpmReserveLuaSrc is tpmLuaSrc with the reservation sized from the key's
// own history: with `count` settlements recorded, the cost is the mean real
// usage clamped to the burst; with none, the cold-start cost the caller
// passes (the full burst, so a cold key's concurrent burst can never
// overshoot its cap -- docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-
// fix.md). KEYS[1] is the arrival-time key, KEYS[2] the history key; the
// third reply is the cost actually reserved, which the caller settles later.
// Before 2026-10-13 every Redis-mode reservation was the full burst, which
// admitted one request per key at a time (the 2026-10-08 live run's F2).
const tpmReserveLuaSrc = `
local burst = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local period = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])

local hist = redis.call('HMGET', KEYS[2], 'sum', 'count')
if hist[1] and hist[2] then
  local count = tonumber(hist[2])
  if count and count > 0 then
    cost = tonumber(hist[1]) / count
    if cost < 0 then cost = 0 end
    if cost > burst then cost = burst end
  end
end

local emission_interval = (period * 1000) / rate
local increment = emission_interval * cost
local burst_offset = emission_interval * burst

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

local tat = redis.call('GET', KEYS[1])
if tat == false then
  tat = now_ms
else
  tat = tonumber(tat)
end
tat = math.max(tat, now_ms)

local new_tat = tat + increment
local allow_at = new_tat - burst_offset

if allow_at > now_ms then
  local retry_after_sec = (allow_at - now_ms) / 1000.0
  return {0, tostring(retry_after_sec), tostring(cost)}
end

local ttl_ms = math.ceil(new_tat - now_ms) + 1000
redis.call('SET', KEYS[1], new_tat, 'PX', ttl_ms)
return {1, '0', tostring(cost)}
`

// tpmSettleLuaSrc is tpmAdjustLuaSrc plus the history: when the real count
// is known (ARGV[2] == 1) it is added to the history first, so a request
// whose arrival-time key already expired still teaches the next
// reservation; then the arrival time is adjusted exactly as AdjustTPM does.
// A release with no real count (ARGV[2] == 0) and a mid-stream top-up
// (AdjustTPM) never touch the history: they are reservation changes, not
// usage.
const tpmSettleLuaSrc = `
local delta_increment = tonumber(ARGV[1])

if tonumber(ARGV[2]) == 1 then
  redis.call('HINCRBYFLOAT', KEYS[2], 'sum', ARGV[3])
  redis.call('HINCRBY', KEYS[2], 'count', 1)
  redis.call('PEXPIRE', KEYS[2], ARGV[4])
end

local tat = redis.call('GET', KEYS[1])
if tat == false then
  return redis.status_reply('OK')
end
tat = tonumber(tat)

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

local new_tat = math.max(tat + delta_increment, now_ms)
local ttl_ms = math.ceil(new_tat - now_ms) + 1000
redis.call('SET', KEYS[1], new_tat, 'PX', ttl_ms)
return redis.status_reply('OK')
`

var (
	tpmReserveScript = redis.NewScript(tpmReserveLuaSrc)
	tpmSettleScript  = redis.NewScript(tpmSettleLuaSrc)
)

// ReserveTPM is AllowTPM with the reservation sized from key's own usage
// history (tpmReserveLuaSrc): coldStartCost is reserved only while the key
// has no settled history, the running mean of settled real tokens -- clamped
// to burst -- afterwards. reservedCost is what was actually reserved, the
// value the caller must hand back to SettleTPM's delta. A non-nil error means
// the Redis call itself failed; callers fail open as for AllowTPM.
func (l *Limiter) ReserveTPM(ctx context.Context, key string, burst, rate, periodSec, coldStartCost float64) (allowed bool, retryAfterSec, reservedCost float64, err error) {
	tatKey := "ratelimit:tpm:" + url.QueryEscape(key)
	val, err := tpmReserveScript.Run(ctx, l.client, []string{tatKey, tpmHistoryKey(key)}, burst, rate, periodSec, coldStartCost).Result()
	if err != nil {
		return false, 0, 0, fmt.Errorf("redislimiter: running TPM reserve script for key %q: %w", key, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 3 {
		return false, 0, 0, fmt.Errorf("redislimiter: unexpected TPM reserve script result shape %v for key %q", val, key)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return false, 0, 0, fmt.Errorf("redislimiter: unexpected TPM reserve script result[0] type %T for key %q", results[0], key)
	}
	retryAfterSec, err = parseScriptFloat(results[1], key, "retryAfterSec")
	if err != nil {
		return false, 0, 0, err
	}
	reservedCost, err = parseScriptFloat(results[2], key, "reservedCost")
	if err != nil {
		return false, 0, 0, err
	}
	return allowedInt == 1, retryAfterSec, reservedCost, nil
}

// SettleTPM reconciles a prior ReserveTPM reservation: deltaIncrement moves
// the arrival time exactly as AdjustTPM does (emission interval times real
// minus reserved; a release passes the negated reservation), and a non-nil
// realTokens is recorded in key's usage history so the next reservation is
// sized from it. A nil realTokens -- a request that failed, timed out or was
// not billable -- leaves the history alone.
func (l *Limiter) SettleTPM(ctx context.Context, key string, deltaIncrement float64, realTokens *float64) error {
	tatKey := "ratelimit:tpm:" + url.QueryEscape(key)
	hasReal, real := 0, 0.0
	if realTokens != nil {
		hasReal, real = 1, *realTokens
	}
	if _, err := tpmSettleScript.Run(ctx, l.client, []string{tatKey, tpmHistoryKey(key)}, deltaIncrement, hasReal, real, tpmHistoryTTL.Milliseconds()).Result(); err != nil {
		return fmt.Errorf("redislimiter: running TPM settle script for key %q: %w", key, err)
	}
	return nil
}

func parseScriptFloat(v interface{}, key, what string) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("redislimiter: unexpected TPM reserve script %s type %T for key %q", what, v, key)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("redislimiter: parsing TPM reserve script %s %q for key %q: %w", what, s, key, err)
	}
	return f, nil
}
