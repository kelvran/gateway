package cache

import "testing"

func TestKeyIsDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	k1 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1", "", "")
	k2 := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1", "", "")
	if k1 != k2 {
		t.Errorf("Key is not deterministic: %q != %q", k1, k2)
	}
}

func TestKeyDiffersOnAnyField(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	base := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1", "", "")

	otherTenant := Key("team-beta", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1", "", "")
	otherModel := Key("team-alpha", "gpt-4o-mini", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens, "v1", "", "")
	otherMessages := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"bye"}]`, &temp, &maxTokens, "v1", "", "")
	otherTemp := 0.9
	otherTempKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &otherTemp, &maxTokens, "v1", "", "")
	otherMaxTokens := 200
	otherMaxTokensKey := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &otherMaxTokens, "v1", "", "")
	nilTemp := Key("team-alpha", "gpt-4o", `[{"role":"user","content":"hi"}]`, nil, &maxTokens, "v1", "", "")

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

	alphaKey := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	betaKey := Key("team-beta", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	if alphaKey == betaKey {
		t.Fatalf("two different tenants asking an identical question produced the same cache key %q — cross-tenant cache leakage", alphaKey)
	}

	// Same tenant, same everything else: still deterministic.
	alphaKeyAgain := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
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

	alphaKey := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
	betaKey := NormalizedKey("team-beta", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
	if alphaKey == betaKey {
		t.Fatalf("two different tenants with identical normalized content produced the same L2 key %q — cross-tenant cache leakage", alphaKey)
	}

	alphaKeyAgain := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
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

	v1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	v2 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v2", "", "")
	if v1 == v2 {
		t.Errorf("Key did not change when guardrailPolicyVersion changed: %q == %q", v1, v2)
	}
}

func TestNormalizedKeyDiffersOnGuardrailPolicyVersion(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	normalized := "user: hi"

	v1 := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
	v2 := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v2", "", "")
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

	l1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	l2 := NormalizedKey("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	if l1 == l2 {
		t.Errorf("Key and NormalizedKey produced the same hash %q for identical remaining inputs — their leading layer tag should make this impossible", l1)
	}
}

// TestKeyDiffersOnResponseFormatFingerprint and its NormalizedKey sibling
// are the load-bearing proof that two requests differing ONLY in their
// requested response_format/JSON-schema must never collide on the same
// cache entry — serving a schema-conforming JSON response to a caller
// that asked for unstructured text (or vice versa) is exactly the kind of
// correctness-breaking collision this cache's own "gated on correctness,
// not just similarity" design exists to prevent.
func TestKeyDiffersOnResponseFormatFingerprint(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	noFormat := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	withFormat := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", `{"type":"json_schema","json_schema":{"name":"x","schema":{}}}`, "")
	if noFormat == withFormat {
		t.Errorf("Key did not change when responseFormatFingerprint changed: %q == %q", noFormat, withFormat)
	}
}

func TestNormalizedKeyDiffersOnResponseFormatFingerprint(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	normalized := "user: hi"

	noFormat := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
	withFormat := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", `{"type":"json_schema","json_schema":{"name":"x","schema":{}}}`, "")
	if noFormat == withFormat {
		t.Errorf("NormalizedKey did not change when responseFormatFingerprint changed: %q == %q", noFormat, withFormat)
	}
}

// TestKeyEmptyResponseFormatFingerprintIsStillDeterministic replaces a
// prior byte-for-byte backward-compatibility assertion against the
// pre-2026-09-17 hash scheme. That guarantee was deliberately dropped as
// part of fixing a real ambiguous-delimiter collision in writeField (see
// key.go's doc comment) — the fix changes Key's output for every input,
// including this one, so asserting a specific historical hash constant
// would just re-pin the old (broken) scheme's byte layout. What still
// matters, and what this asserts instead, is that an empty
// responseFormatFingerprint remains fully deterministic and distinct from
// a non-empty one (covered separately by TestKeyDiffersOnResponseFormatFingerprint).
func TestKeyEmptyResponseFormatFingerprintIsStillDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	k1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	k2 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	if k1 != k2 {
		t.Errorf("Key with empty responseFormatFingerprint is not deterministic: %q != %q", k1, k2)
	}
}

// TestKeyDiffersOnPromptFingerprint and its NormalizedKey sibling are the
// load-bearing proof for internal/prompt's own cache-key fold: two
// requests that happen to resolve to the same message content (e.g. two
// different prompt_id/prompt_version combinations that coincidentally
// produce identical text) must still be distinguishable at the cache-key
// level, mirroring responseFormatFingerprint's own identical rationale.
func TestKeyDiffersOnPromptFingerprint(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	noPrompt := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	withPrompt := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "greeting:v1:abc123")
	if noPrompt == withPrompt {
		t.Errorf("Key did not change when promptFingerprint changed: %q == %q", noPrompt, withPrompt)
	}

	otherPrompt := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "greeting:v2:def456")
	if withPrompt == otherPrompt {
		t.Errorf("Key did not change when promptFingerprint changed between two non-empty values: %q == %q", withPrompt, otherPrompt)
	}
}

func TestNormalizedKeyDiffersOnPromptFingerprint(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	normalized := "user: hi"

	noPrompt := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "")
	withPrompt := NormalizedKey("team-alpha", "gpt-4o", normalized, &temp, &maxTokens, "v1", "", "greeting:v1:abc123")
	if noPrompt == withPrompt {
		t.Errorf("NormalizedKey did not change when promptFingerprint changed: %q == %q", noPrompt, withPrompt)
	}
}

// TestKeyEmptyPromptFingerprintIsStillDeterministic is
// promptFingerprint's own version of
// TestKeyEmptyResponseFormatFingerprintIsStillDeterministic above — see
// that test's doc comment for why this no longer pins a historical hash
// constant.
func TestKeyEmptyPromptFingerprintIsStillDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	messages := `[{"role":"user","content":"hi"}]`

	k1 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	k2 := Key("team-alpha", "gpt-4o", messages, &temp, &maxTokens, "v1", "", "")
	if k1 != k2 {
		t.Errorf("Key with empty promptFingerprint is not deterministic: %q != %q", k1, k2)
	}
}

// TestKeyAmbiguousDelimiterCollisionIsFixed is the load-bearing regression
// proof for the real bug fixed in key.go's writeField: two DIFFERENT
// (model, messages) pairs, crafted so the first's model field contains a
// literal NUL byte immediately followed by what looks like the second
// pair's own "messages" tag, must NOT collide. Under the pre-fix bare
// "\x00tag=value" scheme they did collide by construction — this exact
// (model, messages) pair combination was hand-verified to produce
// byte-identical hash input under that scheme before this fix.
func TestKeyAmbiguousDelimiterCollisionIsFixed(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	// model absorbs a fake "\x00messages=b" tag+value; messages is empty
	// on this side of the pair.
	collidingModel := Key("team-alpha", "a\x00messages=b", "c", &temp, &maxTokens, "v1", "", "")
	// The "real" split of the exact same total bytes: model="a",
	// messages="b\x00messages=c".
	collidingMessages := Key("team-alpha", "a", "b\x00messages=c", &temp, &maxTokens, "v1", "", "")

	if collidingModel == collidingMessages {
		t.Fatalf("Key(model=%q, messages=%q) collided with Key(model=%q, messages=%q): both produced %q — ambiguous-delimiter collision is NOT fixed",
			"a\x00messages=b", "c", "a", "b\x00messages=c", collidingModel)
	}
}

// TestNormalizedKeyAmbiguousDelimiterCollisionIsFixed is NormalizedKey's
// own version of TestKeyAmbiguousDelimiterCollisionIsFixed above.
func TestNormalizedKeyAmbiguousDelimiterCollisionIsFixed(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	collidingModel := NormalizedKey("team-alpha", "a\x00messages=b", "c", &temp, &maxTokens, "v1", "", "")
	collidingMessages := NormalizedKey("team-alpha", "a", "b\x00messages=c", &temp, &maxTokens, "v1", "", "")

	if collidingModel == collidingMessages {
		t.Fatalf("NormalizedKey(model=%q, messages=%q) collided with NormalizedKey(model=%q, messages=%q): both produced %q — ambiguous-delimiter collision is NOT fixed",
			"a\x00messages=b", "c", "a", "b\x00messages=c", collidingModel)
	}
}
