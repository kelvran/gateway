package redisfused

import (
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// The fixed layout of one fused settlement. Redis numbers KEYS and ARGV from
// 1; every call carries all eight keys and twenty-six arguments, whichever
// parts it runs, so no index ever shifts. A part it does not run gets the
// store's own placeholder keys and zeros, and its flag in ARGV[1..3] is 0.
//
//	KEYS  1 budget spend key | 2,3 dimension 1 TAT, history | 4,5 dimension 2
//	      | 6,7,8 ledger hour hash, sessions set, cells count
//	ARGV  1 budget flag, 2 tokens flag, 3 ledger flag
//	      | 4..6 delta, expected epoch, billed
//	      | 7 dimension count, 8..11 dimension 1, 12..15 dimension 2
//	      | 16..26 the ledger entry's eleven arguments
const (
	tokenKeys = 2 * redislimiter.SettleMaxDims   // a TAT and a history per dimension
	tokenArgs = 1 + 4*redislimiter.SettleMaxDims // the count, then four per dimension

	kBudget = 1
	kTokens = kBudget + redisbudget.AdjustKeys
	kLedger = kTokens + tokenKeys

	aBudget = 4 // ARGV 1..3 are the part flags
	aTokens = aBudget + redisbudget.AdjustArgs
	aLedger = aTokens + tokenArgs

	keyCount = kLedger + redisledger.RecordKeys - 1
	argCount = aLedger + redisledger.RecordArgs - 1

	// replyLen is the length of the reply array, see luaTemplate.
	replyLen = 7
)

// The layout is a single sequence of slots; these fail the build if the
// numbers above were edited out of step with the counts the stores export.
const (
	_ = uint(keyCount - 8)
	_ = uint(8 - keyCount)
	_ = uint(argCount - 26)
	_ = uint(26 - argCount)
)

func indices(name string, from, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s[%d]", name, from+i)
	}
	return strings.Join(parts, ", ")
}

// fusedSrc is the template with each standalone body and each part's slot
// lists expanded.
var fusedSrc = compose(redisbudget.AdjustLuaSrc, redislimiter.SettlePairLuaSrc, redisledger.RecordLuaSrc)

// compose expands the template around the three part bodies. NewReplacer
// expands in one pass, so text inside a body that looks like a marker is never
// expanded again. Production passes the stores' own constants; the tests pass
// synthetic bodies to exercise the template's own behaviour.
func compose(budgetBody, tokensBody, ledgerBody string) string {
	return strings.NewReplacer(
		"<<BUDGET>>", budgetBody,
		"<<TOKENS>>", tokensBody,
		"<<LEDGER>>", ledgerBody,
		"<<KB>>", indices("KEYS", kBudget, redisbudget.AdjustKeys),
		"<<AB>>", indices("ARGV", aBudget, redisbudget.AdjustArgs),
		"<<KT>>", indices("KEYS", kTokens, tokenKeys),
		"<<AT>>", indices("ARGV", aTokens, tokenArgs),
		"<<KL>>", indices("KEYS", kLedger, redisledger.RecordKeys),
		"<<AL>>", indices("ARGV", aLedger, redisledger.RecordArgs),
	).Replace(luaTemplate)
}

// fusedScript is run with EVALSHA (go-redis), one command that takes effect;
// after a script-cache flush or a restart the first call costs one more round
// trip, for the EVALSHA that misses before the EVAL that loads and runs it.
var fusedScript = redis.NewScript(fusedSrc)

// luaTemplate runs the budget adjust, the token settlement and the ledger
// record, each standalone body verbatim as the body of its own function (so
// KEYS and ARGV inside it are that part's sub-tables, built from the original
// argument strings) under pcall: a part that raises is reported and stops
// nothing else. The ledger runs last. Server-level failures are not a part's
// and are re-raised, so the caller sees the error a standalone script would
// have raised.
//
// The reply is {budget status, budget spend after, tokens status, ledger
// status, budget error, tokens error, ledger error}. A status is 0 (not run),
// 1 (applied) or 2 (raised); the spend is a bare integer (a Lua tostring would
// turn it into 1e+14 notation from 1e14, which is 15 digits); the error texts are cut to 200
// characters and carry no argument values. Never nil or false: a Lua array
// ends at the first nil. The token body's status reply is discarded.
const luaTemplate = `
local function errtext(e)
  if type(e) == 'table' and e.err ~= nil then e = e.err end
  return string.sub(tostring(e), 1, 200)
end
local function fatal(e)
  local m = errtext(e)
  return string.find(m, '^READONLY') or string.find(m, '^MISCONF') or string.find(m, '^NOREPLICAS')
    or string.find(m, '^OOM') or string.find(m, '^LOADING') or string.find(m, '^MASTERDOWN')
    or string.find(m, '^CLUSTERDOWN') or string.find(m, '^TRYAGAIN')
end
local b_status, b_spent, b_err = 0, 0, ''
local t_status, t_err = 0, ''
local l_status, l_err = 0, ''

if tonumber(ARGV[1]) == 1 then
  local ok, r = pcall(function(KEYS, ARGV)
<<BUDGET>>
end, {<<KB>>}, {<<AB>>})
  if ok and type(r) == 'number' then
    b_status, b_spent = 1, r
  elseif ok then
    b_status, b_err = 2, 'adjust returned ' .. type(r)
  else
    if fatal(r) then error(r, 0) end
    b_status, b_err = 2, errtext(r)
  end
end

if tonumber(ARGV[2]) == 1 then
  local ok, r = pcall(function(KEYS, ARGV)
<<TOKENS>>
end, {<<KT>>}, {<<AT>>})
  if ok then
    t_status = 1
  else
    if fatal(r) then error(r, 0) end
    t_status, t_err = 2, errtext(r)
  end
end

if tonumber(ARGV[3]) == 1 then
  local ok, r = pcall(function(KEYS, ARGV)
<<LEDGER>>
end, {<<KL>>}, {<<AL>>})
  if ok then
    l_status = 1
  else
    if fatal(r) then error(r, 0) end
    l_status, l_err = 2, errtext(r)
  end
end

return {b_status, b_spent, t_status, l_status, b_err, t_err, l_err}
`
