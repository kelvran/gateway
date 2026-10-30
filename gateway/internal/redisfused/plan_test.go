package redisfused

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/admission"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// fakePlanner stands in for the ledger store: fixed keys and arguments, so the
// layout tests need no Redis.
type fakePlanner struct{ err error }

func (f fakePlanner) PlanRecord(spendledger.Entry) ([]string, []any, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	args := make([]any, redisledger.RecordArgs)
	for i := range args {
		args[i] = "L" + strconv.Itoa(i+1)
	}
	return []string{"LK1", "LK2", "LK3"}, args, nil
}

func newPure(err error) *Settler { return &Settler{ledger: fakePlanner{err: err}} }

// The three standalone bodies are embedded byte for byte: the fused script is
// a pure function of the stores' own constants, so it cannot drift from them.
func TestSourceEmbedsEachStandaloneBodyVerbatim(t *testing.T) {
	for name, body := range map[string]string{
		"budget adjust": redisbudget.AdjustLuaSrc,
		"token settle":  redislimiter.SettlePairLuaSrc,
		"ledger record": redisledger.RecordLuaSrc,
	} {
		if strings.Count(fusedSrc, body) != 1 {
			t.Errorf("the fused script embeds the %s body %d times, want exactly once", name, strings.Count(fusedSrc, body))
		}
	}
}

// Every KEYS index 1..8 and every ARGV index 4..26 is handed to exactly one
// part's sub-table; the flags (ARGV 1..3) are read by the wrapper itself. A
// shifted index would make a part read another part's data.
func TestSubTablesCoverEveryKeyAndArgExactlyOnce(t *testing.T) {
	tables := regexp.MustCompile(`end, \{([^}]*)\}, \{([^}]*)\}\)`).FindAllStringSubmatch(fusedSrc, -1)
	if len(tables) != 3 {
		t.Fatalf("found %d part calls in the fused script, want 3 (budget, tokens, ledger)", len(tables))
	}
	idx := regexp.MustCompile(`(KEYS|ARGV)\[(\d+)\]`)
	seen := map[string]int{}
	for _, tb := range tables {
		for _, m := range idx.FindAllStringSubmatch(tb[1]+","+tb[2], -1) {
			seen[m[1]+m[2]]++
		}
	}
	var want []string
	for i := 1; i <= keyCount; i++ {
		want = append(want, fmt.Sprintf("KEYS%d", i))
	}
	for i := 4; i <= argCount; i++ {
		want = append(want, fmt.Sprintf("ARGV%d", i))
	}
	var got []string
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s is handed to %d sub-tables, want 1", k, n)
		}
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sub-tables cover %v, want %v", got, want)
	}
}

func sameValues(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if fmt.Sprint(a[i]) != fmt.Sprint(b[i]) {
			return false
		}
	}
	return true
}

// plan puts each part's own planner output in its slots, pads the rest with
// the stores' placeholders, and sets the flags to exactly the parts present.
func TestPlanFillsTheFixedLayout(t *testing.T) {
	budgetPart := &BudgetPart{KeyID: "k1", DeltaNanoUSD: -1234, Epoch: 1_700_000_000_000_000, Billed: true}
	entry := entryAt("k1", "s1", 77)
	dims := []admission.Settlement{settle("k1", 3.5, f64(120)), settle("k1\x00dim=day", -2, nil)}
	for _, hasBudget := range []bool{false, true} {
		for n := 0; n <= 2; n++ {
			for _, hasLedger := range []bool{false, true} {
				name := fmt.Sprintf("budget=%v/dims=%d/ledger=%v", hasBudget, n, hasLedger)
				t.Run(name, func(t *testing.T) {
					req := Request{Tokens: dims[:n]}
					if hasBudget {
						req.Budget = budgetPart
					}
					if hasLedger {
						req.Ledger = &entry
					}
					c := newPure(nil).plan(req)
					if len(c.keys) != keyCount || len(c.args) != argCount {
						t.Fatalf("layout is %d keys and %d args, want %d and %d", len(c.keys), len(c.args), keyCount, argCount)
					}
					flags := []any{b2i(hasBudget), b2i(n > 0), b2i(hasLedger)}
					if !sameValues(c.args[:3], flags) {
						t.Errorf("flags = %v, want %v", c.args[:3], flags)
					}
					// budget slot
					if hasBudget {
						bk, ba := redisbudget.PlanAdjust("k1", -1234, 1_700_000_000_000_000, true)
						if c.keys[0] != bk[0] || !sameValues(c.args[3:6], ba) {
							t.Errorf("budget slot = %v %v, want %v %v", c.keys[0], c.args[3:6], bk, ba)
						}
					} else if c.keys[0] != redisbudget.PlaceholderKey() {
						t.Errorf("absent budget declares %q, want the placeholder %q", c.keys[0], redisbudget.PlaceholderKey())
					}
					// token slots: n then four per dimension, padded to two dimensions
					tk, ta := redislimiter.PlanSettleTokens(dims[:n])
					tat, hist := redislimiter.PlaceholderSettleKeys()
					for len(tk) < 2*redislimiter.SettleMaxDims {
						tk = append(tk, tat, hist)
					}
					for len(ta) < 1+4*redislimiter.SettleMaxDims {
						ta = append(ta, 0)
					}
					if !reflect.DeepEqual(c.keys[1:5], tk) || !sameValues(c.args[6:15], ta) {
						t.Errorf("token slots = %v %v, want %v %v", c.keys[1:5], c.args[6:15], tk, ta)
					}
					// ledger slot
					if hasLedger {
						lk, la, _ := fakePlanner{}.PlanRecord(entry)
						if !reflect.DeepEqual(c.keys[5:8], lk) || !sameValues(c.args[15:26], la) {
							t.Errorf("ledger slot = %v %v, want %v %v", c.keys[5:8], c.args[15:26], lk, la)
						}
					} else if !reflect.DeepEqual(c.keys[5:8], redisledger.PlaceholderRecordKeys()) {
						t.Errorf("absent ledger declares %v, want the placeholders %v", c.keys[5:8], redisledger.PlaceholderRecordKeys())
					}
					if c.budget != hasBudget || c.tokens != (n > 0) || c.ledger != hasLedger {
						t.Errorf("call flags = %v %v %v", c.budget, c.tokens, c.ledger)
					}
				})
			}
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestPlanRefusesMoreThanTwoTokenSettlements(t *testing.T) {
	three := []admission.Settlement{settle("a", 1, nil), settle("b", 1, nil), settle("c", 1, nil)}
	c := newPure(nil).plan(Request{Tokens: three, Budget: &BudgetPart{KeyID: "k"}})
	if c.tokens || c.tokensErr == nil {
		t.Fatalf("three settlements were planned (tokens=%v, err=%v), want a refusal", c.tokens, c.tokensErr)
	}
	if !c.budget {
		t.Error("a refused token part must not stop the budget part")
	}
}

func TestPlanRecordErrorDisablesOnlyTheLedgerPart(t *testing.T) {
	boom := errors.New("invalid entry")
	entry := entryAt("k", "s", 1)
	c := newPure(boom).plan(Request{Budget: &BudgetPart{KeyID: "k"}, Tokens: []admission.Settlement{settle("k", 1, nil)}, Ledger: &entry})
	if c.ledger || !errors.Is(c.ledgerErr, boom) {
		t.Fatalf("ledger=%v err=%v, want the part off and the planner's error kept", c.ledger, c.ledgerErr)
	}
	if !c.budget || !c.tokens {
		t.Error("a ledger planning error must not stop the budget and token parts")
	}
	if c.args[2] != 0 {
		t.Errorf("ledger flag = %v, want 0", c.args[2])
	}
}

// With every part absent there is nothing to send: no command, and the client
// is never touched (a nil client would panic).
func TestSettleWithNothingToDoSendsNothing(t *testing.T) {
	res := newPure(nil).Settle(context.Background(), Request{})
	if res.Sent || res.Budget.Status != StatusSkipped || res.Tokens.Status != StatusSkipped || res.Ledger.Status != StatusSkipped {
		t.Fatalf("empty request = %+v, want nothing sent and every part skipped", res)
	}
}

func TestParseReplyRejectsMalformedReplies(t *testing.T) {
	all := call{budget: true, tokens: true, ledger: true}
	onlyBudget := call{budget: true}
	ok := []any{int64(1), int64(42), int64(1), int64(1), "", "", ""}
	cases := []struct {
		name string
		raw  any
		c    call
		want string // substring of the error; "" = accepted
	}{
		{"valid", ok, all, ""},
		{"not an array", "OK", all, "unexpected"},
		{"too short", ok[:6], all, "unexpected"},
		{"too long", append(append([]any{}, ok...), "x"), all, "unexpected"},
		{"status not an integer", []any{"1", int64(0), int64(1), int64(1), "", "", ""}, all, "unexpected"},
		{"error text not a string", []any{int64(1), int64(0), int64(1), int64(1), int64(0), "", ""}, all, "unexpected"},
		{"status out of range", []any{int64(3), int64(0), int64(1), int64(1), "", "", ""}, all, "status"},
		{"part ran though its flag was off", []any{int64(1), int64(0), int64(1), int64(1), "", "", ""}, call{tokens: true, ledger: true}, "status"},
		{"part skipped though its flag was on", []any{int64(0), int64(0), int64(1), int64(1), "", "", ""}, all, "status"},
		{"nil element", []any{int64(1), nil, int64(1), int64(1), "", "", ""}, all, "unexpected"},
		{"only budget, others zero", []any{int64(1), int64(5), int64(0), int64(0), "", "", ""}, onlyBudget, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReply(tc.raw, tc.c)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("parseReply = %v, want accepted", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("parseReply error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// A failed call (transport error, server-level error, malformed reply) fails
// every part that was attempted and leaves the others skipped; a part refused
// before sending keeps its own error.
func TestWholeCallFailureFailsEveryAttemptedPart(t *testing.T) {
	boom := errors.New("connection reset")
	c := call{budget: true, ledger: true, tokensErr: errors.New("three dimensions")}
	res := wholeCallFailure(c, "k", boom)
	if res.Budget.Status != StatusFailed || !errors.Is(res.Budget.Err, boom) {
		t.Errorf("budget = %+v, want failed with the cause", res.Budget)
	}
	if res.Ledger.Status != StatusFailed || !errors.Is(res.Ledger.Err, boom) {
		t.Errorf("ledger = %+v, want failed with the cause", res.Ledger)
	}
	if res.Tokens.Status != StatusFailed || res.Tokens.Err == nil || errors.Is(res.Tokens.Err, boom) {
		t.Errorf("tokens = %+v, want the pre-send refusal, not the transport error", res.Tokens)
	}
	if !res.Sent {
		t.Error("a call that was attempted is Sent even when it failed")
	}
}

// A part that did not apply reports no spend and names the key in its error,
// so a log line made from it says whose settlement was lost.
func TestResultNamesTheKeyAndReportsNoSpendForAFailedPart(t *testing.T) {
	c := call{budget: true, tokens: true, ledger: true, key: "key-7"}
	res := c.result(reply{budget: StatusFailed, tokens: StatusFailed, ledger: StatusFailed, spent: 99, budgetErr: "b", tokensErr: "t", ledgerErr: "l"})
	if res.SpentAfterNanoUSD != 0 {
		t.Errorf("SpentAfterNanoUSD = %d for a failed budget part, want 0", res.SpentAfterNanoUSD)
	}
	for name, p := range map[string]Part{"budget": res.Budget, "tokens": res.Tokens, "ledger": res.Ledger} {
		if p.Status != StatusFailed || p.Err == nil || !strings.Contains(p.Err.Error(), "key-7") {
			t.Errorf("%s = %+v, want a failure that names the key", name, p)
		}
	}
	ok := c.result(reply{budget: StatusApplied, tokens: StatusApplied, ledger: StatusApplied, spent: 99})
	if ok.SpentAfterNanoUSD != 99 || ok.Budget.Err != nil || ok.Tokens.Err != nil || ok.Ledger.Err != nil {
		t.Errorf("an applied settlement = %+v, want the spend 99 and no errors", ok)
	}
}

// The differential test runs both paths through the stores' planners, so a
// planner bug would be shared by both and cancel out. These pin what each
// planner emits against the script inputs their Lua bodies read.
func TestStorePlannersEmitTheScriptInputs(t *testing.T) {
	keys, args := redisbudget.PlanAdjust("k 1", -5, 7, true)
	if !reflect.DeepEqual(keys, []string{"budget:k+1"}) || !sameValues(args, []any{-5, 7, 1}) {
		t.Errorf("PlanAdjust billed = %v %v, want [budget:k+1] [-5 7 1]", keys, args)
	}
	if _, args := redisbudget.PlanAdjust("k", 5, 7, false); !sameValues(args, []any{5, 7, 0}) {
		t.Errorf("PlanAdjust unbilled args = %v, want [5 7 0]", args)
	}
	if len(args) != redisbudget.AdjustArgs || len(keys) != redisbudget.AdjustKeys {
		t.Errorf("PlanAdjust returned %d keys and %d args, want %d and %d", len(keys), len(args), redisbudget.AdjustKeys, redisbudget.AdjustArgs)
	}

	real := 10.0
	tk, ta := redislimiter.PlanSettleTokens([]admission.Settlement{{Key: "k", DeltaIncrement: 1.5, RealTokens: &real}, {Key: "k2", DeltaIncrement: -2}})
	if len(tk) != 4 || tk[0] != "ratelimit:tpm:k" || tk[2] != "ratelimit:tpm:k2" {
		t.Errorf("PlanSettleTokens keys = %v, want a TAT and a history per dimension, TATs under ratelimit:tpm:", tk)
	}
	want := []any{2, 1.5, 1, 10.0, int64(86_400_000), -2.0, 0, 0.0, int64(86_400_000)}
	if !sameValues(ta, want) {
		t.Errorf("PlanSettleTokens args = %v, want %v (count, then delta, has-real, real, history TTL ms per dimension)", ta, want)
	}
}

// A container that cannot start fails EVERY call that wants Redis and never
// hands out an empty address (go-redis would turn that into localhost:6379, the
// real Redis), and the starter runs once.
func TestRedisProviderFailsEveryCallWhenTheStarterFails(t *testing.T) {
	calls := 0
	p := &redisProvider{start: func(context.Context) (string, func(), error) {
		calls++
		return "", nil, errors.New("docker is down")
	}}
	for i := 0; i < 3; i++ {
		if addr, err := p.get(); err == nil || addr != "" {
			t.Fatalf("call %d = %q, %v, want an error and no address", i, addr, err)
		}
	}
	if calls != 1 {
		t.Errorf("the starter ran %d times, want once", calls)
	}
}

// The guard that keeps a test off the real Redis refuses every spelling of its
// port, and an empty address (which go-redis turns into localhost:6379).
func TestCheckRedisAddrRefusesTheRealRedisInEverySpelling(t *testing.T) {
	for _, bad := range []string{"", "127.0.0.1:6379", "localhost:6379", "[::1]:6379", "127.0.0.1:06379", "127.0.0.1:006379", "127.0.0.1:+6379", "127.0.0.1:6379 ", "127.0.0.1:redis", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:-1"} {
		if err := checkRedisAddr(bad); err == nil {
			t.Errorf("checkRedisAddr(%q) accepted an address that can reach the real Redis", bad)
		}
	}
	for _, good := range []string{"127.0.0.1:16379", "localhost:16380", "127.0.0.1:49153"} {
		if err := checkRedisAddr(good); err != nil {
			t.Errorf("checkRedisAddr(%q) = %v, want accepted", good, err)
		}
	}
}

// A ledger entry with no planner to plan it, or one the planner refuses, is a
// failed ledger part that sends nothing and keeps the cause.
func TestSettleReportsALedgerPlanningFailureOnTheLedgerPart(t *testing.T) {
	entry := entryAt("k", "s", 1)
	res := (&Settler{}).Settle(context.Background(), Request{Ledger: &entry})
	if res.Ledger.Status != StatusFailed || res.Ledger.Err == nil || !strings.Contains(res.Ledger.Err.Error(), "no ledger planner") || res.Sent {
		t.Errorf("no planner: %+v, want a failed ledger part, nothing sent", res)
	}
	boom := errors.New("invalid entry")
	res = newPure(boom).Settle(context.Background(), Request{Ledger: &entry})
	if res.Ledger.Status != StatusFailed || !errors.Is(res.Ledger.Err, boom) || res.Sent {
		t.Errorf("planner error: %+v, want a failed ledger part carrying the cause, nothing sent", res)
	}
	if res.Budget.Status != StatusSkipped || res.Tokens.Status != StatusSkipped {
		t.Errorf("the parts that were not requested must stay skipped: %+v", res)
	}
}
