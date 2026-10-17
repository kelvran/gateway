package redislimiter

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
)

// admitLuaSrc is the one-round-trip rate-limit gate (round-5 item 10 phase
// 2b, 2026-10-17): the RPM token bucket of luaSrc, then the TPM and the
// tokens-per-day GCRA reservations of tpmReserveLuaSrc, evaluated in that
// order, RESERVING NOTHING UNLESS ALL THREE ADMIT -- the RPM state is
// persisted on every call, the token taken only then. Until then the three ran as
// three scripts, each conditional on the previous one, so a TPM or
// tokens-per-day refusal had already consumed an RPM token and a
// tokens-per-day refusal had to release the TPM reservation with a fourth
// round trip; now a refused request consumes nothing in any dimension.
//
// KEYS: [1] the RPM hash, [2] the TPM TAT, [3] the TPM history hash, [4] the
// day TAT, [5] the day history hash (the last four are derived even when the
// dimension is off and are then never touched). ARGV: rpm capacity, rpm
// refill per second, the client's now in ms (the RPM part keeps the client
// clock, as luaSrc did, so it stays deterministic under a fake clock), then
// per GCRA dimension burst, rate, period, cold start (the day dimension's is
// the cap; the script lowers it to the minute reservation when that is
// smaller, ratelimit.dayColdStartCap). Burst <= 0 turns a dimension off.
//
// The RPM bucket's refreshed state is persisted on every call, as luaSrc
// persisted it, so a hammered key never lets its hash expire; only the token
// is taken on a full admit. Returns {allowed, refused_by, retry_after_sec,
// tpm_cost, tpd_cost, tpd_cold} -- the floats tostring()'d as the single
// scripts return theirs (see tpmReserveLuaSrc's note), parsed back by
// parseScriptFloat.
const admitLuaSrc = `
local rpm_capacity = tonumber(ARGV[1])
local rpm_refill = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local tpm_burst = tonumber(ARGV[4])
local tpm_rate = tonumber(ARGV[5])
local tpm_period = tonumber(ARGV[6])
local tpm_cold = tonumber(ARGV[7])
local tpd_burst = tonumber(ARGV[8])
local tpd_rate = tonumber(ARGV[9])
local tpd_period = tonumber(ARGV[10])
local tpd_cold_cap = tonumber(ARGV[11])

local state = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill_ms')
local tokens = tonumber(state[1])
local last_refill_ms = tonumber(state[2])
if tokens == nil then
  tokens = rpm_capacity
  last_refill_ms = now
end
tokens = math.min(rpm_capacity, tokens + math.max(0, (now - last_refill_ms) / 1000.0) * rpm_refill)
local rpm_ttl = math.max(60, math.ceil(2 * rpm_capacity / math.max(rpm_refill, 0.001)))
local function persist_rpm(t)
  redis.call('HSET', KEYS[1], 'tokens', tostring(t), 'last_refill_ms', now)
  redis.call('EXPIRE', KEYS[1], rpm_ttl)
end
if tokens < 1 then
  persist_rpm(tokens)
  return {0, 'rpm', '0', '0', '0', '0'}
end

local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)

-- One GCRA decision, computed without writing: the settled mean when the
-- history has one, the cold start otherwise, clamped to the burst.
local function decide(tat_key, hist_key, burst, rate, period, cold)
  local cost = cold
  local hist = redis.call('HMGET', hist_key, 'sum', 'count')
  if hist[1] and hist[2] then
    local count = tonumber(hist[2])
    if count and count > 0 then
      cost = tonumber(hist[1]) / count
      if cost < 0 then cost = 0 end
      if cost > burst then cost = burst end
    end
  end
  local emission = (period * 1000) / rate
  local tat = redis.call('GET', tat_key)
  if tat == false then
    tat = now_ms
  else
    tat = tonumber(tat)
  end
  tat = math.max(tat, now_ms)
  local new_tat = tat + emission * cost
  local allow_at = new_tat - emission * burst
  if allow_at > now_ms then
    return false, (allow_at - now_ms) / 1000.0, cost, new_tat, 0
  end
  return true, 0, cost, new_tat, math.ceil(new_tat - now_ms) + 1000
end

local tpm_cost, tpm_new_tat, tpm_ttl = 0, 0, 0
if tpm_burst > 0 then
  local ok, retry, cost, new_tat, ttl = decide(KEYS[2], KEYS[3], tpm_burst, tpm_rate, tpm_period, tpm_cold)
  if not ok then
    persist_rpm(tokens)
    return {0, 'tpm', tostring(retry), tostring(cost), '0', '0'}
  end
  tpm_cost, tpm_new_tat, tpm_ttl = cost, new_tat, ttl
end

local tpd_cost, tpd_cold, tpd_new_tat, tpd_ttl = 0, 0, 0, 0
if tpd_burst > 0 then
  tpd_cold = tpd_cold_cap
  if tpm_burst > 0 and tpm_cost > 0 and tpm_cost < tpd_cold then
    tpd_cold = tpm_cost
  end
  local ok, retry, cost, new_tat, ttl = decide(KEYS[4], KEYS[5], tpd_burst, tpd_rate, tpd_period, tpd_cold)
  if not ok then
    persist_rpm(tokens)
    return {0, 'tpd', tostring(retry), tostring(tpm_cost), tostring(cost), tostring(tpd_cold)}
  end
  tpd_cost, tpd_new_tat, tpd_ttl = cost, new_tat, ttl
end

persist_rpm(tokens - 1)
if tpm_burst > 0 then
  redis.call('SET', KEYS[2], tpm_new_tat, 'PX', tpm_ttl)
end
if tpd_burst > 0 then
  redis.call('SET', KEYS[4], tpd_new_tat, 'PX', tpd_ttl)
end
return {1, '', '0', tostring(tpm_cost), tostring(tpd_cost), tostring(tpd_cold)}
`

// settlePairLuaSrc settles up to two GCRA dimensions in one round trip:
// tpmSettleLuaSrc's body once per dimension (the history learns the real
// count when one is given; the TAT moves by the delta, never into the past),
// in one atomic EVAL rather than a pipeline of two EVALSHAs -- go-redis's
// NOSCRIPT fallback is inert inside a pipeline, and a retried pipeline would
// settle twice. ARGV[1] is the dimension count n; KEYS[2i-1]/KEYS[2i] are
// dimension i's TAT and history, ARGV[2+4(i-1) ..] its delta increment,
// has_real, real tokens and history TTL in ms.
const settlePairLuaSrc = `
local n = tonumber(ARGV[1])
local time = redis.call('TIME')
local now_ms = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
for i = 1, n do
  local tat_key = KEYS[2 * i - 1]
  local hist_key = KEYS[2 * i]
  local base = 2 + 4 * (i - 1)
  local delta_increment = tonumber(ARGV[base])
  if tonumber(ARGV[base + 1]) == 1 then
    redis.call('HINCRBYFLOAT', hist_key, 'sum', ARGV[base + 2])
    redis.call('HINCRBY', hist_key, 'count', 1)
    redis.call('PEXPIRE', hist_key, ARGV[base + 3])
  end
  local tat = redis.call('GET', tat_key)
  if tat ~= false then
    local new_tat = math.max(tonumber(tat) + delta_increment, now_ms)
    redis.call('SET', tat_key, new_tat, 'PX', math.ceil(new_tat - now_ms) + 1000)
  end
end
return redis.status_reply('OK')
`

var (
	admitScript      = redis.NewScript(admitLuaSrc)
	settlePairScript = redis.NewScript(settlePairLuaSrc)
)

func tatKey(key string) string { return "ratelimit:tpm:" + url.QueryEscape(key) }

// Admit implements ratelimit.RedisBackend: the three-dimension gate in one
// round trip, reserving only on a full admit. A key whose TPM or
// tokens-per-day dimension is off (Burst <= 0) still gets its RPM decision.
func (l *Limiter) Admit(ctx context.Context, req admission.Request) (admission.Result, error) {
	keys := []string{
		"ratelimit:" + url.QueryEscape(req.RPMKey),
		tatKey(req.TPM.Key), tpmHistoryKey(req.TPM.Key),
		tatKey(req.TPD.Key), tpmHistoryKey(req.TPD.Key),
	}
	val, err := admitScript.Run(ctx, l.client, keys,
		req.RPMCapacity, req.RPMRefillPerSecond, time.Now().UnixMilli(),
		req.TPM.Burst, req.TPM.Rate, 1.0, req.TPM.ColdStart,
		req.TPD.Burst, req.TPD.Rate, 1.0, req.TPD.ColdStart,
	).Result()
	if err != nil {
		return admission.Result{}, fmt.Errorf("redislimiter: running admit script for key %q: %w", req.RPMKey, err)
	}
	results, ok := val.([]interface{})
	if !ok || len(results) != 6 {
		return admission.Result{}, fmt.Errorf("redislimiter: unexpected admit script result shape %v for key %q", val, req.RPMKey)
	}
	allowedInt, ok := results[0].(int64)
	if !ok {
		return admission.Result{}, fmt.Errorf("redislimiter: unexpected admit script result[0] type %T for key %q", results[0], req.RPMKey)
	}
	refusedBy, ok := results[1].(string)
	if !ok {
		return admission.Result{}, fmt.Errorf("redislimiter: unexpected admit script result[1] type %T for key %q", results[1], req.RPMKey)
	}
	var r admission.Result
	r.Allowed = allowedInt == 1
	r.RefusedBy = refusedBy
	for i, dst := range []*float64{&r.RetryAfterSec, &r.TPMCost, &r.TPDCost, &r.TPDColdStart} {
		f, perr := parseScriptFloat(results[2+i], req.RPMKey, fmt.Sprintf("admit[%d]", 2+i))
		if perr != nil {
			return admission.Result{}, perr
		}
		*dst = f
	}
	return r, nil
}

// SettleTokens implements ratelimit.RedisBackend: every settlement in one
// atomic round trip; none means no call at all.
func (l *Limiter) SettleTokens(ctx context.Context, settlements []admission.Settlement) error {
	if len(settlements) == 0 {
		return nil
	}
	keys := make([]string, 0, 2*len(settlements))
	args := make([]interface{}, 0, 1+4*len(settlements))
	args = append(args, len(settlements))
	for _, s := range settlements {
		keys = append(keys, tatKey(s.Key), tpmHistoryKey(s.Key))
		hasReal, real := 0, 0.0
		if s.RealTokens != nil {
			hasReal, real = 1, *s.RealTokens
		}
		args = append(args, s.DeltaIncrement, hasReal, real, tpmHistoryTTL.Milliseconds())
	}
	if _, err := settlePairScript.Run(ctx, l.client, keys, args...).Result(); err != nil {
		return fmt.Errorf("redislimiter: running settle-tokens script for key %q: %w", settlements[0].Key, err)
	}
	return nil
}
