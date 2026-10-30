package redisfused

import (
	"context"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/budget/redisbudget"
	"github.com/kelvran/gateway/gateway/internal/ratelimit/redislimiter"
	"github.com/kelvran/gateway/gateway/internal/spendledger/redisledger"
)

// execTemplate runs the template around synthetic part bodies with every flag
// on, so the tests exercise the template's own handling of what a body does
// (raise, return the wrong thing) rather than the stores' scripts. It returns
// the call's error instead of failing, for the bodies that make the call raise.
func execTemplate(t *testing.T, budgetBody, tokensBody, ledgerBody string) ([]any, error) {
	t.Helper()
	c := newClient(t, redisAddr(t))
	keys := make([]string, keyCount)
	for i := range keys {
		keys[i] = uniqueID(t)
	}
	args := make([]any, argCount)
	for i := range args {
		args[i] = 0
	}
	args[0], args[1], args[2] = 1, 1, 1
	raw, err := redis.NewScript(compose(budgetBody, tokensBody, ledgerBody)).Run(context.Background(), c, keys, args...).Result()
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != replyLen {
		t.Fatalf("reply %T %v, want %d elements", raw, raw, replyLen)
	}
	return arr, nil
}

func runTemplate(t *testing.T, budgetBody, tokensBody, ledgerBody string) []any {
	t.Helper()
	arr, err := execTemplate(t, budgetBody, tokensBody, ledgerBody)
	if err != nil {
		t.Fatalf("running the composed script: %v", err)
	}
	return arr
}

// Every server-level prefix the template re-raises, in every part, comes back
// as the call's own error, as a standalone script would raise it; a part-level
// error (a wrong-type key, a command error, a plain Lua raise) is contained.
func TestTemplateReRaisesEveryServerLevelErrorPrefix(t *testing.T) {
	for _, prefix := range []string{"READONLY", "MISCONF", "NOREPLICAS", "OOM", "LOADING", "MASTERDOWN", "CLUSTERDOWN", "TRYAGAIN"} {
		for part, name := range []string{"budget", "tokens", "ledger"} {
			bodies := [3]string{"return 1", "return 1", "return 1"}
			bodies[part] = "error({err='" + prefix + " simulated'})"
			_, err := execTemplate(t, bodies[0], bodies[1], bodies[2])
			if err == nil || !strings.Contains(err.Error(), prefix) {
				t.Errorf("%s raised in the %s part: call error = %v, want it to carry %s", prefix, name, err, prefix)
			}
		}
	}
	for _, body := range []string{"error({err='WRONGTYPE Operation against a key holding the wrong kind of value'})", "error({err='ERR value is not an integer or out of range'})", "error('boom')"} {
		arr, err := execTemplate(t, body, "return 1", "return 1")
		if err != nil || arr[0] != int64(2) {
			t.Errorf("part-level error %q: call error %v, budget status %v, want contained (status 2)", body, err, arr)
		}
	}
}

// An error text is cut to 200 characters, so a part that raises with a huge
// message cannot flood the reply or the log line made from it.
func TestTemplateCutsAnErrorTextTo200Characters(t *testing.T) {
	arr := runTemplate(t, "error(string.rep('x', 500))", "return 1", "return 1")
	text, _ := arr[4].(string)
	if arr[0] != int64(2) || len(text) != 200 {
		t.Fatalf("budget status %v with a %d character error, want status 2 and exactly 200 characters", arr[0], len(text))
	}
	if arr[2] != int64(1) || arr[3] != int64(1) {
		t.Errorf("a raising budget body stopped the others: token status %v, ledger status %v", arr[2], arr[3])
	}
}

// The budget part reports its spend as a bare number; anything else is a
// failure of that part, named for what it returned.
func TestTemplateTreatsANonNumericBudgetResultAsAFailure(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"return 'abc'", "adjust returned string"},
		{"return", "adjust returned nil"},
		{"return {1}", "adjust returned table"},
	} {
		arr := runTemplate(t, tc.body, "return 1", "return 1")
		if arr[0] != int64(2) || arr[4] != tc.want {
			t.Errorf("body %q: status %v error %q, want status 2 and %q", tc.body, arr[0], arr[4], tc.want)
		}
		if arr[1] != int64(0) {
			t.Errorf("body %q: spend %v, want 0 for a failed part", tc.body, arr[1])
		}
	}
}

// A body's return value other than the budget's is not read: the token body
// ends in a status reply and the ledger body in a number, both discarded.
func TestTemplateIgnoresTheTokenAndLedgerReturnValues(t *testing.T) {
	arr := runTemplate(t, "return 41", "return redis.status_reply('OK')", "return 1")
	want := []any{int64(1), int64(41), int64(1), int64(1), "", "", ""}
	for i := range want {
		if arr[i] != want[i] {
			t.Errorf("element %d = %#v, want %#v", i, arr[i], want[i])
		}
	}
}

// A part that raises a plain Lua error is reported on its own and stops neither
// of the others; only the part that raised is status 2.
func TestTemplateContainsAPlainRaiseInEachPart(t *testing.T) {
	for i, bodies := range [][3]string{
		{"error('boom')", "return 1", "return 1"},
		{"return 1", "error('boom')", "return 1"},
		{"return 1", "return 1", "error('boom')"},
	} {
		arr := runTemplate(t, bodies[0], bodies[1], bodies[2])
		statuses := []any{arr[0], arr[2], arr[3]}
		for j, st := range statuses {
			want := int64(1)
			if j == i {
				want = 2
			}
			if st != want {
				t.Errorf("raise in part %d: statuses %v, want part %d failed and the others applied", i, statuses, i)
				break
			}
		}
		if !strings.Contains(arr[4+i].(string), "boom") {
			t.Errorf("raise in part %d: error text %q does not carry the message", i, arr[4+i])
		}
	}
}

// compose with the stores' own constants is what production runs.
func TestFusedSourceIsComposeOfTheStoresConstants(t *testing.T) {
	if fusedSrc != compose(redisbudget.AdjustLuaSrc, redislimiter.SettlePairLuaSrc, redisledger.RecordLuaSrc) {
		t.Error("fusedSrc is not the template composed around the three stores' own constants")
	}
}
