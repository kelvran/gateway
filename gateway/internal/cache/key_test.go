package cache

import "testing"

func TestKeyIsDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	k1 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1")
	k2 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1")
	if k1 != k2 {
		t.Errorf("Key is not deterministic: %q != %q", k1, k2)
	}
}

func TestKeyDiffersOnAnyField(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	base := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1")

	otherTenant := Key("team-beta", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1")
	otherModel := Key("team-alpha", "gpt-4o-mini", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1")
	otherMessages := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"bye"}]`, &temp, &maxTokens, "v1")
	otherTemp := 0.9
	otherTempKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &otherTemp, &maxTokens, "v1")
	otherMaxTokens := 200
	otherMaxTokensKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &otherMaxTokens, "v1")
	nilTemp := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, nil, &maxTokens, "v1")

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

	alphaKey := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1")
	betaKey := Key("team-beta", "gpt-4o", messages, &temp, &maxTokens, "v1")
	if alphaKey == betaKey {
		t.Fatalf("two different tenants asking an identical question produced the same cache key %q — cross-tenant cache leakage", alphaKey)
	}

	// Same tenant, same everything else: still deterministic.
	alphaKeyAgain := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1")
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

	alphaKey := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1")
	betaKey := NormalizedKey("team-beta", "gpt-4o", normalized, &temp, &maxTokens, "v1")
	if alphaKey == betaKey {
		t.Fatalf("two different tenants with identical normalized content produced the same L2 key %q — cross-tenant cache leakage", alphaKey)
	}

	alphaKeyAgain := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1")
	if alphaKey != alphaKeyAgain {
		t.Errorf("NormalizedKey is not deterministic: %q != %q", alphaKey, alphaKeyAgain)
	}
}

// TestKeyDiffersOnGuardrailPolicyVersion and its NormalizedKey sibling
// are the load-bearing proof for
// docs/rfcs/2026-09-03-guardrails-pii-regex-classifier.md's cache-hit
// safety mechanism: a guardrail policy/detector change must invalidate
// existing L1/L2 entries wholesale, by producing a different key for
// otherwise-identical requests — never a silent, unchecked serve of a
// hit that predates the policy change.
func TestKeyDiffersOnGuardrailPolicyVersion(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	v1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1")
	v2 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v2")
	if v1 == v2 {
		t.Errorf("Key did not change when guardrailPolicyVersion changed: %q == %q", v1, v2)
	}
}

func TestNormalizedKeyDiffersOnGuardrailPolicyVersion(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	normalized := "user: hi"

	v1 := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1")
	v2 := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v2")
	if v1 == v2 {
		t.Errorf("NormalizedKey did not change when guardrailPolicyVersion changed: %q == %q", v1, v2)
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

	l1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1")
	l2 := NormalizedKey("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1")
	if l1 == l2 {
		t.Errorf("Key and NormalizedKey produced the same hash %q for identical remaining inputs — their leading layer tag should make this impossible", l1)
	}
}
