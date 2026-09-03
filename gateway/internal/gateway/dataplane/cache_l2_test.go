package dataplane

import (
	"context"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

func TestNormalizeMessagesTrimsOuterWhitespace(t *testing.T) {
	got := normalizeMessages([]adapter.Message{{Role: "user", Content: "  hello world  "}})
	want := normalizeMessages([]adapter.Message{{Role: "user", Content: "hello world"}})
	if got != want {
		t.Errorf("normalizeMessages did not normalize outer whitespace: %q != %q", got, want)
	}
}

func TestNormalizeMessagesStripsTrailingTerminalPunctuationOnLastMessageOnly(t *testing.T) {
	withQuestionMark := normalizeMessages([]adapter.Message{
		{Role: "user", Content: "what is 2+2"},
		{Role: "assistant", Content: "4"},
		{Role: "user", Content: "are you sure?"},
	})
	withoutQuestionMark := normalizeMessages([]adapter.Message{
		{Role: "user", Content: "what is 2+2"},
		{Role: "assistant", Content: "4"},
		{Role: "user", Content: "are you sure"},
	})
	if withQuestionMark != withoutQuestionMark {
		t.Errorf("trailing '?' on the last message was not stripped: %q != %q", withQuestionMark, withoutQuestionMark)
	}

	// The SAME punctuation mark on a NON-last message must NOT be
	// stripped — the allowlist is "last message only," not "anywhere."
	midMessageMark := normalizeMessages([]adapter.Message{
		{Role: "user", Content: "are you sure?"},
		{Role: "assistant", Content: "yes"},
		{Role: "user", Content: "ok thanks"},
	})
	midMessageMarkStripped := normalizeMessages([]adapter.Message{
		{Role: "user", Content: "are you sure"},
		{Role: "assistant", Content: "yes"},
		{Role: "user", Content: "ok thanks"},
	})
	if midMessageMark == midMessageMarkStripped {
		t.Error("trailing punctuation on a non-last message was incorrectly stripped")
	}
}

func TestNormalizeMessagesUnicodeNFC(t *testing.T) {
	// "é" as a single precomposed codepoint (U+00E9) vs. "e" + combining
	// acute accent (U+0065 U+0301) — canonically identical under NFC,
	// visually and semantically identical, byte-different without it.
	precomposed := normalizeMessages([]adapter.Message{{Role: "user", Content: "café"}})
	decomposed := normalizeMessages([]adapter.Message{{Role: "user", Content: "café"}})
	if precomposed != decomposed {
		t.Errorf("NFC-equivalent Unicode forms produced different normalized output: %q != %q", precomposed, decomposed)
	}
}

// TestNormalizeMessagesNeverCollapsesCodeIndentation is the single most
// important test in this feature, per
// docs/rfcs/2026-09-03-cache-l2-normalized-match.md's own framing: the
// allowlist was deliberately narrowed (no internal whitespace collapse,
// no case-folding) specifically because Kelvran serves agent traffic that
// plausibly includes pasted code, where indentation is genuinely
// meaningful. Two otherwise-identical questions about Python snippets
// differing ONLY in indentation depth must produce DIFFERENT normalized
// output — if this test ever fails, the allowlist has regressed back
// toward the collision risk this RFC exists to avoid.
func TestNormalizeMessagesNeverCollapsesCodeIndentation(t *testing.T) {
	twoSpace := normalizeMessages([]adapter.Message{{
		Role:    "user",
		Content: "why does this fail?\ndef f():\n  return 1",
	}})
	fourSpace := normalizeMessages([]adapter.Message{{
		Role:    "user",
		Content: "why does this fail?\ndef f():\n    return 1",
	}})
	if twoSpace == fourSpace {
		t.Fatal("normalizeMessages collapsed internal whitespace, making two Python snippets with different (semantically meaningful) indentation depth collide — this is exactly the collision risk the RFC's allowlist was narrowed to prevent")
	}
}

// TestNormalizeMessagesNeverFoldsCase is the case-folding analogue of the
// indentation test above: a capitalized identifier and a lowercase
// reserved word are different questions, and must never collide.
func TestNormalizeMessagesNeverFoldsCase(t *testing.T) {
	capitalized := normalizeMessages([]adapter.Message{{Role: "user", Content: "Is 'Select' a valid variable name in Python"}})
	lowercase := normalizeMessages([]adapter.Message{{Role: "user", Content: "Is 'select' a valid variable name in Python"}})
	if capitalized == lowercase {
		t.Fatal("normalizeMessages folded case, making a capitalized identifier and a lowercase reserved word collide — a real, different question")
	}
}

func TestNormalizeMessagesLeavesRoleAndToolCallsUnchanged(t *testing.T) {
	msgs := []adapter.Message{
		{Role: "assistant", Content: "  calling a tool  ", ToolCalls: []adapter.ToolCall{{ID: "call-1", Name: "get_weather"}}},
	}
	got := normalizeMessages(msgs)
	if got == "" {
		t.Fatal("normalizeMessages returned empty output")
	}
	// A message with different ToolCalls must NOT normalize to the same
	// output — tool calls are real request content, never cosmetic.
	otherToolCall := []adapter.Message{
		{Role: "assistant", Content: "  calling a tool  ", ToolCalls: []adapter.ToolCall{{ID: "call-2", Name: "get_weather"}}},
	}
	if got == normalizeMessages(otherToolCall) {
		t.Error("normalizeMessages produced identical output for messages with different ToolCalls")
	}
}

// TestL2HitPromotesIntoL1 is the load-bearing proof for checkCache's
// promotion behavior: an L1 miss + L2 hit must both return the cached
// response AND leave L1 populated, so the next byte-identical repeat
// becomes an L1 hit without consulting L2 again.
func TestL2HitPromotesIntoL1(t *testing.T) {
	l1 := inprocess.New(0)
	l2 := inprocess.New(0)
	ctx := context.Background()

	l2Key := "some-l2-key"
	l1Key := "some-l1-key"
	value := []byte(`{"id":"resp-1"}`)
	if err := l2.Put(ctx, l2Key, value, time.Hour); err != nil {
		t.Fatalf("l2.Put: %v", err)
	}

	p := &Pipeline{cache: l1, cacheL2: l2, cacheTTL: time.Hour}

	cached, hit := p.checkCache(ctx, l1Key, l2Key)
	if !hit {
		t.Fatal("checkCache did not report a hit for a value present in L2")
	}
	if string(cached) != string(value) {
		t.Errorf("checkCache returned %q, want %q", cached, value)
	}

	// L1 must now be populated under l1Key — the promotion.
	promoted, ok, err := l1.Get(ctx, l1Key)
	if err != nil || !ok {
		t.Fatalf("L1 was not populated after an L2 hit: ok=%v err=%v", ok, err)
	}
	if string(promoted) != string(value) {
		t.Errorf("promoted L1 value = %q, want %q", promoted, value)
	}
}

// TestWriteCacheWritesBothLayers proves a genuine miss populates BOTH L1
// and L2 eagerly, per gateway/ARCHITECTURE.md's "cache write-back (all
// layers)" line.
func TestWriteCacheWritesBothLayers(t *testing.T) {
	l1 := inprocess.New(0)
	l2 := inprocess.New(0)
	l3 := inprocess.NewLexicalCache(0)
	ctx := context.Background()
	p := &Pipeline{cache: l1, cacheL2: l2, cacheL3: l3, cacheTTL: time.Hour, cacheL2TTL: time.Hour, cacheL3TTL: time.Hour}

	value := []byte(`{"id":"resp-1"}`)
	p.writeCache(ctx, "team-alpha", "l1key", "l2key", []uint64{1, 2, 3}, nil, "gpt-4o", value)

	if _, ok, _ := l1.Get(ctx, "l1key"); !ok {
		t.Error("writeCache did not populate L1")
	}
	if _, ok, _ := l2.Get(ctx, "l2key"); !ok {
		t.Error("writeCache did not populate L2")
	}
	if candidates, _ := l3.Search(ctx, "team-alpha", []uint64{1, 2, 3}, 5); len(candidates) == 0 {
		t.Error("writeCache did not populate L3")
	}
}

// TestHandleChatCompletionL2CacheHitOnNormalizedButNotExactRepeat is the
// full-pipeline proof: a request that's byte-different from a prior one
// (added trailing whitespace and a trailing "?") but normalization-
// equivalent must hit L2 and never reach the upstream a second time.
func TestHandleChatCompletionL2CacheHitOnNormalizedButNotExactRepeat(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "what is the weather in paris"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "  what is the weather in paris?  "}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second)
	if err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("second response model = %q, want %q", resp.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (the second, normalization-equivalent request should be an L2 cache hit, not a real upstream call)", upstreamCalls)
	}
}

// TestHandleChatCompletionL2CacheIsolatedAcrossVirtualKeys is L2's own
// version of dataplane_test.go's load-bearing L1 cross-tenant proof: two
// different virtual keys sending normalization-equivalent (not just
// byte-identical) requests must never share a cache entry.
func TestHandleChatCompletionL2CacheIsolatedAcrossVirtualKeys(t *testing.T) {
	var upstreamCalls int

	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, defaultTwoTenantVirtualKeys(t))

	reqAlpha := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "identical question?"}}}
	reqBeta := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "identical question"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-secret", reqAlpha); err != nil {
		t.Fatalf("alpha request: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer beta-secret", reqBeta); err != nil {
		t.Fatalf("beta request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — two different tenants with normalization-equivalent requests must NOT share an L2 cache entry", upstreamCalls)
	}
}

// defaultTwoTenantVirtualKeys is a small local helper avoiding a
// dependency on any other test file's specific two-tenant fixture.
func defaultTwoTenantVirtualKeys(t *testing.T) []identity.VirtualKey {
	t.Helper()
	return []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{ID: "team-beta", KeyHash: testHashOf("beta-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
}
