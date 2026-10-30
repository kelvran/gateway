// Package redisfused settles one finished request's three Redis writes -- the
// budget adjust, the token settlement and the spend-ledger record -- in a
// single EVAL (round-5 item 10 phase 2c-2: 5.0 to 3.0 Redis round trips per
// billable request).
//
// It exists for the case where the rate limiter, the budget and the ledger
// share one go-redis client (cmd/gateway's openSharedRedis); the three stores
// keep their standalone scripts for every other path (embeddings, mid-stream
// top-ups, alert marks, separate clients). The fused script is generated from
// the stores' own exported Lua constants, so it cannot drift from them, and
// each part's keys and arguments come from that store's own planner.
//
// Failure is per part: a part that raises is reported and stops nothing else
// (Redis does not roll back what it wrote before raising, exactly as when its
// standalone script raises). A transport or server-level error fails every
// part that was sent. Nothing here retries or falls back to the standalone
// scripts: an error cannot prove nothing was applied, so a fallback could
// settle twice. go-redis's own retry of a call whose reply was lost applies
// the whole settlement twice, as it already did per script
// (docs/operations/FAILURE-MODES.md).
package redisfused

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// Status is what happened to one part of a settlement.
type Status int8

const (
	// StatusSkipped: the part was not part of the request.
	StatusSkipped Status = iota
	// StatusApplied: the part ran to completion.
	StatusApplied
	// StatusFailed: the part raised, was refused before sending, or the whole
	// call failed.
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusSkipped:
		return "skipped"
	case StatusApplied:
		return "applied"
	case StatusFailed:
		return "failed"
	}
	return fmt.Sprintf("Status(%d)", int8(s))
}

// BudgetPart is the budget adjust: the arguments of redisbudget.Backend.Adjust.
type BudgetPart struct {
	KeyID        string
	DeltaNanoUSD int64
	Epoch        int64
	Billed       bool
}

// Request is one settlement. A nil Budget, an empty Tokens or a nil Ledger is a
// part that is not run; with all three absent nothing is sent.
type Request struct {
	Budget *BudgetPart
	// Tokens are the per-dimension settlements ratelimit.KeyLimiter.PlanSettle
	// produced, at most redislimiter.SettleMaxDims of them.
	Tokens []admission.Settlement
	Ledger *spendledger.Entry
}

// Part is the outcome of one part.
type Part struct {
	Status Status
	Err    error
}

// Result is the outcome of one Settle. It never carries a call-level error:
// every failure is on the parts that were attempted.
type Result struct {
	Budget, Tokens, Ledger Part
	// SpentAfterNanoUSD is the budget window's spend once the adjust ran; 0
	// unless Budget.Status is StatusApplied.
	SpentAfterNanoUSD int64
	// Sent reports whether a command was sent to Redis.
	Sent bool
}

// LedgerPlanner turns a ledger entry into the keys and arguments the ledger's
// record script takes; *redisledger.Store is one.
type LedgerPlanner interface {
	PlanRecord(e spendledger.Entry) (keys []string, args []any, err error)
}

// Settler runs fused settlements over one shared client.
type Settler struct {
	client  *redis.Client
	ledger  LedgerPlanner
	release func() error
}

// New returns a Settler over client. ledger plans the record part (the store
// that is the pipeline's own ledger, so rows land where the admin queries read
// them). release is called by Close; for a shared client it is the hold the
// settler took on it, so the client outlives every settlement. The settler
// never adds a hook: the client's own hook already counts the fused command.
func New(client *redis.Client, ledger LedgerPlanner, release func() error) *Settler {
	return &Settler{client: client, ledger: ledger, release: release}
}

// Close releases the settler's hold on the shared client.
func (s *Settler) Close() error {
	if s.release == nil {
		return nil
	}
	return s.release()
}

// call is a planned settlement: the fixed keys and arguments plus which parts
// they carry and which parts were refused before sending.
type call struct {
	keys                   []string
	args                   []any
	budget, tokens, ledger bool
	tokensErr, ledgerErr   error
	// key names the settlement in error text: the budget's key id, else the
	// ledger entry's, else the first token settlement's.
	key string
}

func zeros(n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = 0
	}
	return out
}

func flag(b bool) any {
	if b {
		return 1
	}
	return 0
}

// plan lays the request out in the fixed slots. It sends nothing.
func (s *Settler) plan(req Request) call {
	var c call

	bKeys, bArgs := []string{redisbudget.PlaceholderKey()}, zeros(redisbudget.AdjustArgs)
	if b := req.Budget; b != nil {
		bKeys, bArgs = redisbudget.PlanAdjust(b.KeyID, b.DeltaNanoUSD, b.Epoch, b.Billed)
		c.budget, c.key = true, b.KeyID
	}

	var tKeys []string
	tArgs := []any{0}
	switch n := len(req.Tokens); {
	case n == 0:
	case n > redislimiter.SettleMaxDims:
		c.tokensErr = fmt.Errorf("redisfused: %d token settlements, at most %d", n, redislimiter.SettleMaxDims)
	default:
		tKeys, tArgs = redislimiter.PlanSettleTokens(req.Tokens)
		c.tokens = true
		if c.key == "" {
			c.key = req.Tokens[0].Key
		}
	}
	tat, hist := redislimiter.PlaceholderSettleKeys()
	for len(tKeys) < tokenKeys {
		tKeys = append(tKeys, tat, hist)
	}
	for len(tArgs) < tokenArgs {
		tArgs = append(tArgs, 0)
	}

	lKeys, lArgs := redisledger.PlaceholderRecordKeys(), zeros(redisledger.RecordArgs)
	if e := req.Ledger; e != nil {
		if s.ledger == nil {
			c.ledgerErr = errors.New("redisfused: no ledger planner configured")
		} else if k, a, err := s.ledger.PlanRecord(*e); err != nil {
			c.ledgerErr = err
		} else {
			lKeys, lArgs = k, a
			c.ledger = true
			if c.key == "" {
				c.key = e.Key
			}
		}
	}

	c.keys = make([]string, 0, keyCount)
	c.keys = append(append(append(c.keys, bKeys...), tKeys...), lKeys...)
	c.args = make([]any, 0, argCount)
	c.args = append(c.args, flag(c.budget), flag(c.tokens), flag(c.ledger))
	c.args = append(append(append(c.args, bArgs...), tArgs...), lArgs...)
	return c
}

// Settle runs the request as one EVAL.
func (s *Settler) Settle(ctx context.Context, req Request) Result {
	c := s.plan(req)
	if !c.budget && !c.tokens && !c.ledger {
		return c.result(reply{})
	}
	raw, err := fusedScript.Run(ctx, s.client, c.keys, c.args...).Result()
	if err != nil {
		return wholeCallFailure(c, c.key, err)
	}
	r, err := parseReply(raw, c)
	if err != nil {
		return wholeCallFailure(c, c.key, err)
	}
	res := c.result(r)
	res.Sent = true
	return res
}

// reply is the parsed reply array.
type reply struct {
	budget, tokens, ledger Status
	spent                  int64
	budgetErr, tokensErr   string
	ledgerErr              string
}

// parseReply checks the reply against the call: seven elements, integers then
// strings, every status 0, 1 or 2 and nonzero exactly for the parts that were
// sent. A reply that does not match is a failure of the whole call, never a
// guess.
func parseReply(raw any, c call) (reply, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) != replyLen {
		return reply{}, fmt.Errorf("redisfused: unexpected fused result %T with %d elements, want %d", raw, len(arr), replyLen)
	}
	var ints [4]int64
	for i := range ints {
		v, ok := arr[i].(int64)
		if !ok {
			return reply{}, fmt.Errorf("redisfused: unexpected fused result element %d of type %T, want an integer", i, arr[i])
		}
		ints[i] = v
	}
	var strs [3]string
	for i := range strs {
		v, ok := arr[4+i].(string)
		if !ok {
			return reply{}, fmt.Errorf("redisfused: unexpected fused result element %d of type %T, want a string", 4+i, arr[4+i])
		}
		strs[i] = v
	}
	status := func(name string, v int64, sent bool) (Status, error) {
		if v < 0 || v > 2 {
			return 0, fmt.Errorf("redisfused: %s status %d out of range", name, v)
		}
		if sent == (v == 0) {
			return 0, fmt.Errorf("redisfused: %s status %d does not match the part being sent=%v", name, v, sent)
		}
		return Status(v), nil
	}
	var r reply
	var err error
	if r.budget, err = status("budget", ints[0], c.budget); err != nil {
		return reply{}, err
	}
	if r.tokens, err = status("tokens", ints[2], c.tokens); err != nil {
		return reply{}, err
	}
	if r.ledger, err = status("ledger", ints[3], c.ledger); err != nil {
		return reply{}, err
	}
	r.spent = ints[1]
	r.budgetErr, r.tokensErr, r.ledgerErr = strs[0], strs[1], strs[2]
	return r, nil
}

// result maps a parsed reply (the zero reply when nothing was sent) onto the
// parts, adding the errors of parts refused before sending.
func (c call) result(r reply) Result {
	var res Result
	res.Budget = Part{Status: r.budget}
	res.Tokens = Part{Status: r.tokens}
	res.Ledger = Part{Status: r.ledger}
	if r.budget == StatusApplied {
		res.SpentAfterNanoUSD = r.spent
	}
	if r.budget == StatusFailed {
		res.Budget.Err = fmt.Errorf("redisfused: budget part for key %q: %s", c.key, r.budgetErr)
	}
	if r.tokens == StatusFailed {
		res.Tokens.Err = fmt.Errorf("redisfused: token part for key %q: %s", c.key, r.tokensErr)
	}
	if r.ledger == StatusFailed {
		res.Ledger.Err = fmt.Errorf("redisfused: ledger part for key %q: %s", c.key, r.ledgerErr)
	}
	if c.tokensErr != nil {
		res.Tokens = Part{Status: StatusFailed, Err: c.tokensErr}
	}
	if c.ledgerErr != nil {
		res.Ledger = Part{Status: StatusFailed, Err: c.ledgerErr}
	}
	return res
}

// wholeCallFailure fails every part that was sent with err; a part refused
// before sending keeps its own error and a part not requested stays skipped.
func wholeCallFailure(c call, key string, err error) Result {
	cause := fmt.Errorf("redisfused: running fused settle for key %q: %w", key, err)
	res := c.result(reply{})
	res.Sent = true
	if c.budget {
		res.Budget = Part{Status: StatusFailed, Err: cause}
	}
	if c.tokens {
		res.Tokens = Part{Status: StatusFailed, Err: cause}
	}
	if c.ledger {
		res.Ledger = Part{Status: StatusFailed, Err: cause}
	}
	return res
}
