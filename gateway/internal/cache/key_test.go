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

// TestNormalizedKeyIsolatesTenantsOnOtherwiseIdenticalRequests is
// NormalizedKey's own version of the load-bearing proof above —
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md explicitly requires
// L2 to fold tenantID into the hash exactly as L1's Key already does, not
// as an afterthought.
func TestNormalizedKeyIsolatesTenantsOnOtherwiseIdenticalRequests(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	normalized := "user: identical question"

	alphaKey := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens)
	betaKey := NormalizedKey("team-beta", "gpt-4o", normalized, &temp, &maxTokens)
	if alphaKey == betaKey {
		t.Fatalf("two different tenants with identical normalized content produced the same L2 key %q — cross-tenant cache leakage", alphaKey)
	}

	alphaKeyAgain := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens)
	if alphaKey != alphaKeyAgain {
		t.Errorf("NormalizedKey is not deterministic: %q != %q", alphaKey, alphaKeyAgain)
	}
}

// TestKeyAndNormalizedKeyNeverCollide proves L1 and L2 keys live in
// disjoint spaces even given the same logical inputs — Key and
// NormalizedKey use the same hash construction, so an L1 key must never
// be mistakable for an L2 key (they're stored in separate cache.Cache
// instances per the RFC, but this is cheap insurance against a future
// refactor accidentally sharing one map).
func TestKeyAndNormalizedKeyNeverCollide(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	l1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens)
	l2 := NormalizedKey("team-alpha", "gpt-4o", messages, &temp, &maxTokens)
	if l1 == l2 {
		t.Errorf("Key and NormalizedKey produced the same hash %q for identical remaining inputs — their leading layer tag should make this impossible", l1)
	}
}
