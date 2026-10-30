package redisfused

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

func exists(t *testing.T, ctx context.Context, c *redis.Client, keys ...string) int64 {
	t.Helper()
	n, err := c.Exists(ctx, keys...).Result()
	if err != nil {
		t.Fatalf("EXISTS: %v", err)
	}
	return n
}

// Each part touches only its own keys, and the placeholders a fused call
// declares for the parts it does not run are never created.
func TestFlagCombinationsTouchOnlyTheirPart(t *testing.T) {
	ctx := context.Background()
	placeholders := append(append([]string{redisbudget.PlaceholderKey()}, func() []string { a, b := redislimiter.PlaceholderSettleKeys(); return []string{a, b} }()...), redisledger.PlaceholderRecordKeys()...)
	for mask := 1; mask < 8; mask++ {
		hasB, hasT, hasL := mask&1 != 0, mask&2 != 0, mask&4 != 0
		t.Run(strings.Join([]string{b2s(hasB, "budget"), b2s(hasT, "tokens"), b2s(hasL, "ledger")}, "+"), func(t *testing.T) {
			r := newRig(t)
			id := uniqueID(t)
			// The window exists in every mask, so a budget write that should not
			// have happened (or one that did not) shows in its numbers.
			seedBudget(t, ctx, r.client, id, 1_000, 99, 0)
			var req Request
			if hasB {
				req.Budget = &BudgetPart{KeyID: id, DeltaNanoUSD: 10, Epoch: 99, Billed: true}
			}
			if hasT {
				req.Tokens = []admission.Settlement{settle(id, 5, f64(10))}
			}
			var entry spendledger.Entry
			if hasL {
				entry = entryAt(id, "s1", 5)
				req.Ledger = &entry
			}
			res := r.settler.Settle(ctx, req)
			if res.Budget.Err != nil || res.Tokens.Err != nil || res.Ledger.Err != nil {
				t.Fatalf("parts failed: %+v", res)
			}
			bk, _ := redisbudget.PlanAdjust(id, 0, 0, false)
			tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: id}})
			lk, _, _ := r.ledger.PlanRecord(entryAt(id, "s1", 1))
			// tokens-only still writes the history (a real count); budget needs a seeded window.
			wantSpent, wantBilled := "1000", "0"
			if hasB {
				wantSpent, wantBilled = "1010", "1"
			}
			if got := r.client.HGet(ctx, bk[0], "spent").Val(); got != wantSpent {
				t.Errorf("budget spent = %q, want %q", got, wantSpent)
			}
			if got := r.client.HGet(ctx, bk[0], "billed_count").Val(); got != wantBilled {
				t.Errorf("budget billed_count = %q, want %q", got, wantBilled)
			}
			if got := exists(t, ctx, r.client, tk[1]) == 1; got != hasT {
				t.Errorf("token history exists=%v, want %v", got, hasT)
			}
			if got := exists(t, ctx, r.client, lk...) > 0; got != hasL {
				t.Errorf("ledger keys exist=%v, want %v", got, hasL)
			}
			if n := exists(t, ctx, r.client, placeholders...); n != 0 {
				t.Errorf("%d placeholder keys were created, want none", n)
			}
		})
	}
}

func b2s(ok bool, s string) string {
	if ok {
		return s
	}
	return "no-" + s
}

// The reply is always seven elements, only integers and strings (a nil or false
// would truncate a Lua array, and a status reply would break the parser).
func TestReplyIsAlwaysSevenIntegersAndStrings(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	for mask := 1; mask < 8; mask++ {
		id := uniqueID(t)
		var req Request
		if mask&1 != 0 {
			req.Budget = &BudgetPart{KeyID: id, DeltaNanoUSD: 1, Epoch: 1}
		}
		if mask&2 != 0 {
			req.Tokens = []admission.Settlement{settle(id, 1, nil), settle(id+daySuffix, 1, f64(3))}
		}
		if mask&4 != 0 {
			e := entryAt(id, "s1", 1)
			req.Ledger = &e
		}
		c := r.settler.plan(req)
		raw, err := fusedScript.Run(ctx, r.client, c.keys, c.args...).Result()
		if err != nil {
			t.Fatalf("mask %d: %v", mask, err)
		}
		arr, ok := raw.([]any)
		if !ok || len(arr) != 7 {
			t.Fatalf("mask %d: reply %T %v, want 7 elements", mask, raw, raw)
		}
		for i, el := range arr {
			switch el.(type) {
			case int64:
				if i >= 4 {
					t.Errorf("mask %d: element %d is an integer, want a string", mask, i)
				}
			case string:
				if i < 4 {
					t.Errorf("mask %d: element %d is a string, want an integer", mask, i)
				}
			default:
				t.Errorf("mask %d: element %d is %T", mask, i, el)
			}
		}
		if _, err := parseReply(raw, c); err != nil {
			t.Errorf("mask %d: parseReply rejected a real reply: %v", mask, err)
		}
	}
}

// A part that raises is contained: the others still run, its own status says
// so, and the writes it made before raising stay (Redis does not roll back).
func TestBudgetPartRaiseDoesNotStopTokensAndLedger(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	id := uniqueID(t)
	bk, _ := redisbudget.PlanAdjust(id, 0, 0, false)
	if err := r.client.Set(ctx, bk[0], "not a hash", 0).Err(); err != nil {
		t.Fatal(err)
	}
	entry := entryAt(id, "s1", 9)
	res := r.settler.Settle(ctx, Request{
		Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 5, Epoch: 1, Billed: true},
		Tokens: []admission.Settlement{settle(id, 0, f64(7))}, Ledger: &entry,
	})
	if res.Budget.Status != StatusFailed || res.Budget.Err == nil || !strings.Contains(res.Budget.Err.Error(), "WRONGTYPE") {
		t.Fatalf("budget = %+v, want failed with WRONGTYPE", res.Budget)
	}
	if res.Tokens.Status != StatusApplied || res.Ledger.Status != StatusApplied {
		t.Fatalf("tokens=%v ledger=%v, want both applied", res.Tokens.Status, res.Ledger.Status)
	}
	tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: id}})
	if got := r.client.HGet(ctx, tk[1], "count").Val(); got != "1" {
		t.Errorf("token history count = %q, want 1", got)
	}
	lk, _, _ := r.ledger.PlanRecord(entry)
	if exists(t, ctx, r.client, lk[0]) != 1 {
		t.Error("the ledger row was not written")
	}
	if res.SpentAfterNanoUSD != 0 {
		t.Errorf("SpentAfterNanoUSD = %d for a failed budget part, want 0", res.SpentAfterNanoUSD)
	}
}

func TestTokenPartRaiseKeepsItsHistoryWriteAndTheOtherParts(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	id := uniqueID(t)
	seedBudget(t, ctx, r.client, id, 1000, 1, 0)
	tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: id}})
	if err := r.client.Set(ctx, tk[0], "abc", 0).Err(); err != nil { // a TAT that is not a number
		t.Fatal(err)
	}
	entry := entryAt(id, "s1", 9)
	res := r.settler.Settle(ctx, Request{
		Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 5, Epoch: 1, Billed: true},
		Tokens: []admission.Settlement{settle(id, 10, f64(7))}, Ledger: &entry,
	})
	if res.Tokens.Status != StatusFailed || res.Tokens.Err == nil {
		t.Fatalf("tokens = %+v, want failed", res.Tokens)
	}
	if res.Budget.Status != StatusApplied || res.SpentAfterNanoUSD != 1005 || res.Ledger.Status != StatusApplied {
		t.Fatalf("budget=%v spent=%d ledger=%v, want applied, 1005, applied", res.Budget.Status, res.SpentAfterNanoUSD, res.Ledger.Status)
	}
	if got := r.client.HGet(ctx, tk[1], "count").Val(); got != "1" {
		t.Errorf("history count = %q, want 1: the write before the raise stays, as in the standalone script", got)
	}
}

func TestLedgerPartRaiseKeepsTheBudgetAndTokenWrites(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	id := uniqueID(t)
	seedBudget(t, ctx, r.client, id, 1000, 1, 0)
	entry := entryAt(id, "s1", 9)
	lk, _, _ := r.ledger.PlanRecord(entry)
	if err := r.client.Set(ctx, lk[2], "abc", 0).Err(); err != nil { // a corrupt cell count
		t.Fatal(err)
	}
	res := r.settler.Settle(ctx, Request{
		Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 5, Epoch: 1, Billed: true},
		Tokens: []admission.Settlement{settle(id, 0, f64(7))}, Ledger: &entry,
	})
	if res.Ledger.Status != StatusFailed || res.Ledger.Err == nil {
		t.Fatalf("ledger = %+v, want failed", res.Ledger)
	}
	if res.Budget.Status != StatusApplied || res.SpentAfterNanoUSD != 1005 || res.Tokens.Status != StatusApplied {
		t.Fatalf("budget=%v spent=%d tokens=%v, want applied, 1005, applied", res.Budget.Status, res.SpentAfterNanoUSD, res.Tokens.Status)
	}
}

// A ledger error never names the session or the cell field: those are
// attribution data the ledger keeps out of logs.
func TestLedgerErrorTextNeverContainsTheSession(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	id := uniqueID(t)
	entry := entryAt(id, "sess-SECRET", 9)
	lk, _, _ := r.ledger.PlanRecord(entry)
	if err := r.client.Set(ctx, lk[2], "abc", 0).Err(); err != nil {
		t.Fatal(err)
	}
	res := r.settler.Settle(ctx, Request{Ledger: &entry})
	if res.Ledger.Err == nil {
		t.Fatal("ledger part did not fail")
	}
	if strings.Contains(res.Ledger.Err.Error(), "SECRET") {
		t.Errorf("ledger error leaks the session: %v", res.Ledger.Err)
	}
}

// A server-level failure (here a read-only replica) is the whole call's, not a
// part's: it must surface as a call error exactly as a standalone script would,
// never as a swallowed part status. Each part is tried alone, so a wrapper that
// swallows it in one part cannot hide behind another part re-raising it.
func TestServerLevelErrorsAreNotSwallowedByThePartWrappers(t *testing.T) {
	ctx := context.Background()
	c := newClient(t, replicaAddr(t))
	led := redisledger.NewOn(c, ledgerOpts, func() error { return nil })
	s := New(c, led, func() error { return nil })
	id := uniqueID(t)
	entry := entryAt(id, "s1", 1)
	// The budget adjust writes only to a window that exists, so give the replica
	// one: promote it for a moment, seed, and demote it again.
	if err := c.Do(ctx, "REPLICAOF", "NO", "ONE").Err(); err != nil {
		t.Fatal(err)
	}
	seedBudget(t, ctx, c, id, 1000, 1, 0)
	if err := c.Do(ctx, "REPLICAOF", "127.0.0.1", "1").Err(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		req  Request
		part func(Result) Part
	}{
		"budget": {Request{Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 1, Epoch: 1}}, func(r Result) Part { return r.Budget }},
		"tokens": {Request{Tokens: []admission.Settlement{settle(id, 1, f64(2))}}, func(r Result) Part { return r.Tokens }},
		"ledger": {Request{Ledger: &entry}, func(r Result) Part { return r.Ledger }},
	} {
		t.Run(name, func(t *testing.T) {
			res := s.Settle(ctx, tc.req)
			p := tc.part(res)
			if p.Status != StatusFailed || p.Err == nil {
				t.Fatalf("%s = %+v, want failed", name, p)
			}
			if !strings.Contains(p.Err.Error(), "READONLY") || !strings.Contains(p.Err.Error(), "running fused settle") {
				t.Errorf("%s error = %v, want the call's own READONLY error, not a part status", name, p.Err)
			}
			if !res.Sent {
				t.Error("a call that was attempted and failed must still report Sent=true")
			}
		})
	}
}

// One fused settlement is one command; the first run after SCRIPT FLUSH costs
// the EVALSHA that misses plus the EVAL that loads the script, then one again.
func TestSettleIsOneCommandAndAScriptFlushCostsOneMore(t *testing.T) {
	ctx := context.Background()
	h := &countHook{}
	r := newRig(t, h)
	id := uniqueID(t)
	entry := entryAt(id, "s1", 5)
	req := Request{
		Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 1, Epoch: 1},
		Tokens: []admission.Settlement{settle(id, 1, f64(2))}, Ledger: &entry,
	}
	if err := r.client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	if err := r.client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	h.reset()
	r.settler.Settle(ctx, req)
	if got := h.names(); strings.Join(got, ",") != "evalsha,eval" {
		t.Errorf("first settle after SCRIPT FLUSH crossed %v, want [evalsha eval]", got)
	}
	h.reset()
	r.settler.Settle(ctx, req)
	if got := h.names(); strings.Join(got, ",") != "evalsha" {
		t.Errorf("warm settle crossed %v, want [evalsha]", got)
	}
}

// lossyProxy forwards to upstream and, once armed, drops the connection after
// the upstream has run the next EVALSHA but before its reply reaches the client:
// the lost-reply case in which go-redis retries.
type lossyProxy struct {
	ln    net.Listener
	armed atomic.Bool
	wg    sync.WaitGroup
}

func newLossyProxy(t *testing.T, upstream string) *lossyProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &lossyProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close(); p.wg.Wait() })
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			cl, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = cl.Close()
				continue
			}
			p.wg.Add(1)
			go p.pipe(cl, up)
		}
	}()
	return p
}

func (p *lossyProxy) pipe(cl, up net.Conn) {
	defer p.wg.Done()
	var drop atomic.Bool
	p.wg.Add(1)
	go func() { // upstream -> client
		defer p.wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := up.Read(buf)
			if n > 0 {
				if drop.Load() {
					_ = cl.Close()
					_ = up.Close()
					return
				}
				if _, werr := cl.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = cl.Close()
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for { // client -> upstream
		n, err := cl.Read(buf)
		if n > 0 {
			if bytes.Contains(bytes.ToLower(buf[:n]), []byte("evalsha")) && p.armed.CompareAndSwap(true, false) {
				drop.Store(true)
			}
			if _, werr := up.Write(buf[:n]); werr != nil {
				_ = up.Close()
				_ = cl.Close()
				return
			}
		}
		if err != nil {
			// A clean client close included: closing the upstream ends the
			// reader goroutine, so no connection outlives the subtest.
			_ = up.Close()
			_ = cl.Close()
			return
		}
	}
}

// Characterization of a documented hazard (FAILURE-MODES: a single EVAL is
// retried as a whole exactly as a single command was): go-redis retries a call
// whose reply was lost after the write, so the fused settlement is applied
// twice, all three parts together. Setting MaxRetries to -1 trades that for a
// lost settlement. The test pins the behaviour so a change in either library or
// design is noticed, not silently inherited.
func TestARetriedEvalAfterALostReplyAppliesEveryPartTwice(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		maxRetries int
		wantTimes  int64
		wantStatus Status
	}{
		{"default retries", 3, 2, StatusApplied},
		{"retries off", -1, 1, StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			direct := newClient(t, redisAddr(t))
			proxy := newLossyProxy(t, redisAddr(t))
			c := redis.NewClient(&redis.Options{Addr: proxy.ln.Addr().String(), MaxRetries: tc.maxRetries})
			t.Cleanup(func() { _ = c.Close() })
			led := redisledger.NewOn(c, ledgerOpts, func() error { return nil })
			s := New(c, led, func() error { return nil })
			id := uniqueID(t)
			seedBudget(t, ctx, direct, id, 1000, 1, 0)
			entry := entryAt(id, "s1", 5)
			if err := fusedScript.Load(ctx, direct).Err(); err != nil { // so the first EVALSHA executes
				t.Fatal(err)
			}
			if err := c.Ping(ctx).Err(); err != nil {
				t.Fatal(err)
			}
			proxy.armed.Store(true)
			res := s.Settle(ctx, Request{
				Budget: &BudgetPart{KeyID: id, DeltaNanoUSD: 10, Epoch: 1, Billed: true},
				Tokens: []admission.Settlement{settle(id, 0, f64(4))}, Ledger: &entry,
			})
			if res.Budget.Status != tc.wantStatus {
				t.Errorf("budget status = %v, want %v (err %v)", res.Budget.Status, tc.wantStatus, res.Budget.Err)
			}
			bk, _ := redisbudget.PlanAdjust(id, 0, 0, false)
			tk, _ := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: id}})
			lk, _, _ := led.PlanRecord(entry)
			if got := direct.HGet(ctx, bk[0], "billed_count").Val(); got != itoa(tc.wantTimes) {
				t.Errorf("billed_count = %s, want %d applications", got, tc.wantTimes)
			}
			if got := direct.HGet(ctx, tk[1], "count").Val(); got != itoa(tc.wantTimes) {
				t.Errorf("token history count = %s, want %d", got, tc.wantTimes)
			}
			if got := direct.HGet(ctx, lk[0], firstField(t, ctx, direct, lk[0])+"#n").Val(); got != itoa(tc.wantTimes) {
				t.Errorf("ledger requests = %s, want %d", got, tc.wantTimes)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// firstField is the spend field of the only cell in a one-row hour hash.
func firstField(t *testing.T, ctx context.Context, c *redis.Client, hashKey string) string {
	t.Helper()
	for f := range c.HGetAll(ctx, hashKey).Val() {
		if !strings.HasSuffix(f, "#n") {
			return f
		}
	}
	t.Fatalf("hash %s has no spend field", hashKey)
	return ""
}
