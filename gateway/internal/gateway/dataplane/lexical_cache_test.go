package dataplane

// Cache L3-lite's own load-bearing full-pipeline proofs, per
// docs/rfcs/2026-09-03-cache-l3-lite-lexical-hard-gated.md: a lexical
// near-duplicate is a real hit, an entity-mismatched near-duplicate is a
// real miss, a volatile query never hits L3 at all, and an L3 backend
// error fails closed to a real upstream call rather than crashing or
// silently serving.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestHandleChatCompletionLexicalNearDuplicateHitsL3 proves a request whose
// only difference from a prior one is internal whitespace — untouched by
// L2's narrow 3-operation allowlist, so both L1 and L2 genuinely miss — is
// nonetheless a real L3 hit. Shingles' strings.Fields-based word splitting
// collapses whitespace runs, so the two texts produce byte-identical
// MinHash signatures (a deterministic Jaccard estimate of 1.0, not a
// probabilistic near-miss); neither message carries any entity/number/date,
// so the hard gate's fingerprint check trivially passes too.
func TestHandleChatCompletionLexicalNearDuplicateHitsL3(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search works in a sorted array"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "Explain how binary search   works in a sorted array"}}}

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
		t.Fatalf("upstreamCalls = %d, want 1 (the second, lexically-near-duplicate request should be an L3 cache hit, not a real upstream call)", upstreamCalls)
	}
}

// TestHandleChatCompletionL3NeverServesAcrossDifferentResponseFormat is a
// round-4 backlog-audit finding: before this fix, L3-lite had no gate on
// ResponseFormat at all -- two byte-identical-messages requests differing
// ONLY in whether one demands schema-conforming JSON would collide on
// the exact same L3 entry, serving a plain-text-intent cached response to
// a caller that explicitly asked for structured output (or vice versa).
// Reuses TestHandleChatCompletionLexicalNearDuplicateHitsL3's own clean,
// non-volatile, entity-free content -- deliberately NOT the
// "today's weather"-style volatile text prompt_test.go's own fingerprint
// tests use to dodge L3 entirely, since this test's whole point is to
// exercise L3, not bypass it.
func TestHandleChatCompletionL3NeverServesAcrossDifferentResponseFormat(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	const content = "Explain how binary search works in a sorted array"
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}}
	structured := adapter.ChatRequest{
		Model:    "gpt-4o",
		Messages: []adapter.Message{{Role: "user", Content: content}},
		ResponseFormat: &adapter.ResponseFormat{
			Type:       "json_schema",
			JSONSchema: &adapter.JSONSchema{Name: "answer", Schema: []byte(`{"type":"object"}`)},
		},
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", plain); err != nil {
		t.Fatalf("first HandleChatCompletion (plain): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("after first request: upstreamCalls = %d, want 1", upstreamCalls)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", structured); err != nil {
		t.Fatalf("second HandleChatCompletion (structured): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("after a byte-identical-messages request that ALSO sets ResponseFormat: upstreamCalls = %d, want 2 (L3 must never serve a plain-text-cached entry to a request demanding structured output)", upstreamCalls)
	}
}

// TestCheckLexicalCacheNeverServesAcrossDifferentReasoningBlocksFingerprint
// closes docs/rfcs/2026-09-12-gateway-reasoning-content-canonical-schema.md's
// own cache-key decision: two requests differing ONLY in accumulated
// ReasoningBlocks history must never collide on an L3 similarity hit,
// since replayed reasoning content is causally read by the model and can
// change output, per AGENTS.md's "never weaken the cache hard-gate" rule.
//
// A direct unit test of checkLexicalCache — rather than a full
// HandleChatCompletion round trip, unlike this file's other gate tests —
// is necessary here specifically because, unlike ResponseFormat/
// PromptFingerprint (separate ChatRequest fields entirely outside
// Messages), ReasoningBlocks lives ON adapter.Message itself: varying it
// also perturbs normalizeMessages' own JSON marshal and therefore the
// real MinHash signature a full pipeline call would compute, entangling
// this gate's effect with the pre-existing similarity gate. Passing an
// identical, hand-fixed signature to both the write and the query
// isolates the ReasoningBlocksFingerprint gate as the ONLY variable.
func TestCheckLexicalCacheNeverServesAcrossDifferentReasoningBlocksFingerprint(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	ctx := context.Background()
	vk := &identity.VirtualKey{ID: "test-key"}
	fixedSignature := []uint64{1, 2, 3, 4}

	writtenMessages := []adapter.Message{
		{Role: "assistant", ReasoningBlocks: []adapter.ReasoningBlock{{Sequence: 0, Text: "original reasoning"}}},
	}
	writtenResp := []byte(`{"id":"cached-resp"}`)
	if err := p.cacheL3.Put(ctx, vk.ID, fixedSignature, writtenResp, nil, "gpt-4o", p.guardrails.Version(), "", "", nil, reasoningBlocksFingerprint(writtenMessages), time.Hour); err != nil {
		t.Fatalf("cacheL3.Put: %v", err)
	}

	// Byte-identical role/everything-else via the forced-identical
	// signature above, but genuinely different ReasoningBlocks history —
	// exactly the scenario this gate exists to close.
	mismatched := adapter.ChatRequest{
		Model: "gpt-4o",
		Messages: []adapter.Message{
			{Role: "assistant", ReasoningBlocks: []adapter.ReasoningBlock{{Sequence: 0, Text: "a completely different chain of thought"}}},
		},
	}
	if _, _, _, hit := p.checkLexicalCache(ctx, vk, mismatched, "irrelevant-l1-key", fixedSignature, ""); hit {
		t.Error("checkLexicalCache returned a hit for a query whose ReasoningBlocks differ from the written entry's fingerprint — the hard gate must reject this")
	}

	// Sanity check: the SAME setup but with ReasoningBlocks matching what
	// was written IS a real hit — proving the gate rejects on a genuine
	// mismatch, not unconditionally.
	matching := adapter.ChatRequest{Model: "gpt-4o", Messages: writtenMessages}
	cached, _, _, hit := p.checkLexicalCache(ctx, vk, matching, "irrelevant-l1-key", fixedSignature, "")
	if !hit {
		t.Fatal("checkLexicalCache returned a miss for a query whose ReasoningBlocks exactly match the written entry — the gate must not reject a genuine match")
	}
	if string(cached) != string(writtenResp) {
		t.Errorf("cached = %q, want %q", cached, writtenResp)
	}
}

// TestHandleChatCompletionEntityMismatchIsNotAnL3Hit proves the hard gate's
// whole reason for existing actually holds end-to-end: a query about $92
// must never be served from an entry cached for a query about $93, even
// though the two requests are otherwise near-identical.
func TestHandleChatCompletionEntityMismatchIsNotAnL3Hit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is 15% of $92"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is 15% of $93"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — a different dollar amount must never be served from a cached entry for a different amount, per the entity/number hard gate", upstreamCalls)
	}
}

// TestHandleChatCompletionNegationParticleInsertionIsNotAnL3Hit is the
// new, narrow gate's own full-pipeline proof, per DECISIONS.md's
// [2026-09-12] entry -- a negation particle swapped into an otherwise
// near-duplicate query (identical entities, no numbers/dates) must
// never be served from the un-negated version's cached entry. Distinct
// from, and does NOT reopen, the antonym-verb-flip case DECISIONS.md's
// [2026-09-08] entry already investigated and rejected fixing here --
// see NegationFingerprint's own doc comment and
// TestNegationFingerprintDoesNotCatchAntonymVerbFlip (entities_test.go)
// for that documented non-goal.
//
// This deliberately long fixture pair (a single "can"->"cannot" word
// swap inside an otherwise byte-identical ~70-word passage) is NOT
// arbitrary padding: a single-word swap corrupts a small, roughly-fixed
// number of 3-word shingles regardless of sentence length, so a SHORT
// near-duplicate pair (e.g. the entity-mismatch test's own short
// sentences) would fall well below l3MinSimilarity=0.9 on this swap
// alone and never even reach the freshness-risk-model's similarity
// check this test needs to isolate the new gate from — confirmed
// empirically (not assumed) against this exact pair via a throwaway
// scratch computation using cache.MinHashSignature/JaccardEstimate
// directly: similarity=0.90625, comfortably clearing the floor, with
// both entity fingerprints empty (matching) and negation fingerprints
// genuinely differing (map[] vs map["cannot"]) -- isolating the new
// gate as the ONLY thing that can reject this candidate.
func TestHandleChatCompletionNegationParticleInsertionIsNotAnL3Hit(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	const passageTemplate = "The attending physician reviewed the patient's full chart and medication history carefully before making any final decision about the ongoing clinical trial protocol for this particular case, and after consulting at length with the pharmacy team and the patient's own family members regarding all of the risks and benefits involved, ultimately decided that the nursing staff %s administer the study drug to the patient during the scheduled evening medication round as documented in the approved protocol"
	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: fmt.Sprintf(passageTemplate, "can")}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: fmt.Sprintf(passageTemplate, "cannot")}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — a negation particle swapped into an otherwise near-duplicate (similarity=0.90625, well above the L3 floor) query must never be served from a cached entry for the un-negated version, per the new negation-mismatch hard gate", upstreamCalls)
	}
}

// TestHandleChatCompletionVolatileQueryNeverHitsL3 proves the volatility
// bypass takes priority over an otherwise-valid L3 hit: the second request
// here is a byte-for-byte lexical near-duplicate of the first (same
// whitespace-collapse guarantee as the near-duplicate-hit test above) with
// an identical entity fingerprint ("Paris"), yet must never be served from
// L3 because it matches the volatility keyword list.
func TestHandleChatCompletionVolatileQueryNeverHitsL3(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is the weather in Paris right now"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "What is the weather in Paris   right now"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 — a volatile (weather) query must bypass L3 entirely, even though the second request is otherwise a lexical near-duplicate with an identical fingerprint", upstreamCalls)
	}
}

// TestHandleChatCompletionVolatileKeywordInSystemPromptDoesNotBypassL3
// proves the fix for isVolatileQuery's role-scoping bug (docs/upgrade-
// research/gateway-cache-volatility-scope-2026-09-09.md): a STATIC
// system prompt containing a volatility keyword must never disable L3
// for the whole deployment — only the user's own message content should
// drive the bypass decision. Both requests share the identical system
// prompt (containing "current"); the user message is a lexical
// near-duplicate with no volatility content of its own, so this must be
// a real L3 hit, not a bypass.
func TestHandleChatCompletionVolatileKeywordInSystemPromptDoesNotBypassL3(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	systemPrompt := adapter.Message{Role: "system", Content: "You may discuss current stock prices with the customer."}
	first := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{systemPrompt, {Role: "user", Content: "Explain how binary search works in a sorted array"}}}
	second := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{systemPrompt, {Role: "user", Content: "Explain how binary search   works in a sorted array"}}}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", first); err != nil {
		t.Fatalf("first HandleChatCompletion: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", second); err != nil {
		t.Fatalf("second HandleChatCompletion: %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 — a volatility keyword in a STATIC system prompt must never bypass L3; only the user's own message content should drive the bypass decision", upstreamCalls)
	}
}

// failingLexicalCache is a cache.LexicalCache whose Search always errors —
// simulating a real L3 backend outage (e.g. a future networked
// implementation), never exercised by inprocess.LexicalCache itself, which
// cannot fail.
type failingLexicalCache struct{}

func (failingLexicalCache) Search(_ context.Context, _ string, _ []uint64, _ int) ([]cache.LexicalCandidate, error) {
	return nil, errors.New("simulated L3 backend failure")
}

func (failingLexicalCache) Put(_ context.Context, _ string, _ []uint64, _ []byte, _ map[string]struct{}, _ string, _ string, _ string, _ string, _ map[string]struct{}, _ string, _ time.Duration) error {
	return nil
}

// TestHandleChatCompletionLexicalCacheSearchErrorFailsClosedToUpstream
// proves checkLexicalCache's fail-closed discipline end-to-end: an L3
// backend that errors on every Search call must never crash or silently
// serve an unchecked response — the request must still succeed via a real
// upstream call, exactly as if L3 had simply reported a miss.
func TestHandleChatCompletionLexicalCacheSearchErrorFailsClosedToUpstream(t *testing.T) {
	var upstreamCalls int
	p := newTestPipelineWithCacheL3(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, failingLexicalCache{})

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hello there"}}}
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if err != nil {
		t.Fatalf("HandleChatCompletion with a failing L3 backend: %v", err)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("response model = %q, want %q", resp.Model, "gpt-4o")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 — an L3 search error must fail closed to a real upstream call, never crash or silently serve", upstreamCalls)
	}
}

// newTestPipelineWithCacheL3 is this file's own local helper, needed
// because every other test pipeline helper hardcodes a real
// inprocess.NewLexicalCache(0) — this is the one test that specifically
// needs to inject a misbehaving L3 implementation instead.
func newTestPipelineWithCacheL3(t *testing.T, upstream UpstreamCaller, l3 cache.LexicalCache) *Pipeline {
	t.Helper()
	keys := defaultTestVirtualKeys()
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p, err := NewPipeline(Config{
		Verifier:   verifier,
		Limiter:    ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:     budget.NewTracker(),
		Cache:      inprocess.New(0),
		CacheL2:    inprocess.New(0),
		CacheL3:    l3,
		Guardrails: guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters: adapter.Registry{
			"openai": openai.New(),
		},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream:       upstream,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}
