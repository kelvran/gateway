package cache

import "testing"

func TestKeyIsDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	k1 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	k2 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	if k1 != k2 {
		t.Errorf("Key is not deterministic: %q != %q", k1, k2)
	}
}

func TestKeyDiffersOnAnyField(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	base := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)

	otherTenant := Key("team-beta", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	otherModel := Key("team-alpha", "gpt-4o-mini", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	otherMessages := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"bye"}]`, &temp, &maxTokens)
	otherTemp := 0.9
	otherTempKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &otherTemp, &maxTokens)
	otherMaxTokens := 200
	otherMaxTokensKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &otherMaxTokens)
	nilTemp := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, nil, &maxTokens)

	for name, k := range map[string]string{
		"tenant":     otherTenant,
		"model":      otherModel,
		"messages":   otherMessages,
		"temp":       otherTempKey,
		"max_tokens": otherMaxTokensKey,
		"nil temp":   nilTemp,
	} {
		if k == base {
			t.Errorf("Key did not change when %s changed", name)
		}
	}
}

// TestKeyIsolatesTenantsOnOtherwiseIdenticalRequests is the load-bearing
// test for docs/rfcs/2026-09-02-virtual-keys-budgets.md's whole premise:
// two different tenants asking a byte-for-byte identical question must
// never collide on the same cache entry. TestKeyDiffersOnAnyField already
// covers this as one case among several; this test isolates it as its own
// named assertion so a future change can't accidentally weaken tenant
// isolation while still passing "differs on some field."
func TestKeyIsolatesTenantsOnOtherwiseIdenticalRequests(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"identical question"}]`

	alphaKey := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens)
	betaKey := Key("team-beta", "gpt-4o", messages, &temp, &maxTokens)
	if alphaKey == betaKey {
		t.Fatalf("two different tenants asking an identical question produced the same cache key %q — cross-tenant cache leakage", alphaKey)
	}

	// Same tenant, same everything else: still deterministic.
	alphaKeyAgain := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens)
	if alphaKey != alphaKeyAgain {
		t.Errorf("same tenant + identical request produced different keys: %q != %q", alphaKey, alphaKeyAgain)
	}
}
