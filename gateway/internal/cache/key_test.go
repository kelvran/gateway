package cache

import "testing"

func TestKeyIsDeterministic(t *testing.T) {
	temp := 0.5
	maxTokens := 100

	k1 := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	k2 := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	if k1 != k2 {
		t.Errorf("Key is not deterministic: %q != %q", k1, k2)
	}
}

func TestKeyDiffersOnAnyField(t *testing.T) {
	temp := 0.5
	maxTokens := 100
	base := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)

	otherModel := Key("gpt-4o-mini", `[{"role":"user","content":"hi"}]`, &temp, &maxTokens)
	otherMessages := Key("gpt-4o", `[{"role":"user","content":"bye"}]`, &temp, &maxTokens)
	otherTemp := 0.9
	otherTempKey := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, &otherTemp, &maxTokens)
	otherMaxTokens := 200
	otherMaxTokensKey := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, &temp, &otherMaxTokens)
	nilTemp := Key("gpt-4o", `[{"role":"user","content":"hi"}]`, nil, &maxTokens)

	for name, k := range map[string]string{
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
