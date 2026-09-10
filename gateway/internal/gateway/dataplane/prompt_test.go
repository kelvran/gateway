package dataplane

// Integration tests for the server-side prompt/template management
// feature (see internal/prompt): a request naming PromptID resolves
// through the full pipeline into real Messages content before routing,
// folds a fingerprint into the cache key, and rejects a request that
// sets both PromptID and Messages.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// providerMessageContents decodes an *openai.Request's own Messages into
// plain strings, in order -- providerReq (the Upstream closure's own
// "req any" parameter) is always *openai.Request for the "openai"
// provider every test pipeline in this package uses.
func providerMessageContents(t *testing.T, providerReq any) []string {
	t.Helper()
	req, ok := providerReq.(*openai.Request)
	if !ok {
		t.Fatalf("providerReq is %T, want *openai.Request", providerReq)
	}
	contents := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			t.Fatalf("decoding message %d content %s: %v", i, m.Content, err)
		}
		contents[i] = s
	}
	return contents
}

// TestPromptIDResolvesToRealMessageContentThroughTheFullPipeline is the
// end-to-end proof: a request naming PromptID (with no Messages of its
// own) reaches the upstream caller carrying the RESOLVED, substituted
// content -- not the raw "{{name}}" template, and not an empty message
// list.
func TestPromptIDResolvesToRealMessageContentThroughTheFullPipeline(t *testing.T) {
	var gotContents []string
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		gotContents = providerMessageContents(t, providerReq)
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments)

	if _, err := p.UpsertPrompt("greeting", []adapter.Message{
		{Role: "system", Content: "You are {{persona}}."},
		{Role: "user", Content: "Say hi to {{name}}."},
	}); err != nil {
		t.Fatalf("UpsertPrompt: %v", err)
	}

	req := adapter.ChatRequest{
		Model:           "gpt-4o",
		PromptID:        "greeting",
		PromptVariables: map[string]string{"persona": "a pirate", "name": "Ada"},
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req); err != nil {
		t.Fatalf("HandleChatCompletion: %v", err)
	}

	want := []string{"You are a pirate.", "Say hi to Ada."}
	if len(gotContents) != len(want) {
		t.Fatalf("upstream received %d messages, want %d: %v", len(gotContents), len(want), gotContents)
	}
	for i := range want {
		if gotContents[i] != want[i] {
			t.Errorf("message %d content = %q, want %q", i, gotContents[i], want[i])
		}
	}
}

// TestPromptIDAndMessagesBothSetIsRejected proves the "replace, never
// merge" design: a request that sets BOTH PromptID and a non-empty
// Messages must fail with ErrPromptAndMessagesBothSet (mapped to a 400
// by cmd/gateway/main.go's writeErrorResponse) and must never reach the
// upstream caller at all.
func TestPromptIDAndMessagesBothSetIsRejected(t *testing.T) {
	var upstreamCalls int
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments)

	if _, err := p.UpsertPrompt("greeting", []adapter.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("UpsertPrompt: %v", err)
	}

	req := adapter.ChatRequest{
		Model:    "gpt-4o",
		PromptID: "greeting",
		Messages: []adapter.Message{{Role: "user", Content: "this should never be allowed alongside prompt_id"}},
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if !errors.Is(err, ErrPromptAndMessagesBothSet) {
		t.Errorf("err = %v, want ErrPromptAndMessagesBothSet", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0 -- a rejected request must never reach the upstream", upstreamCalls)
	}
}

// TestUnknownPromptIDFailsWithErrPromptResolutionFailed proves an
// unresolvable prompt_id/prompt_version is a real, typed failure --
// never a silent empty-messages call to the upstream.
func TestUnknownPromptIDFailsWithErrPromptResolutionFailed(t *testing.T) {
	var upstreamCalls int
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments)

	req := adapter.ChatRequest{Model: "gpt-4o", PromptID: "does-not-exist"}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", req)
	if !errors.Is(err, ErrPromptResolutionFailed) {
		t.Errorf("err = %v, want ErrPromptResolutionFailed", err)
	}
	if upstreamCalls != 0 {
		t.Errorf("upstreamCalls = %d, want 0", upstreamCalls)
	}
}

// TestPromptFingerprintBustsACacheHitEvenWithByteIdenticalResolvedContent
// is the load-bearing proof for the cache-key fold itself (cache.Key's
// new promptFingerprint parameter): two DIFFERENT prompt IDs whose
// stored content happens to resolve to byte-identical text must still
// be treated as distinct cache entries. If dataplane forgot to fold
// promptFP into cache.Key/NormalizedKey, the second request's identical
// resolved message content would collide with the first's L1 entry and
// wrongly serve a cache hit instead of a genuine, distinguishable
// resolution of a different template.
//
// The message content deliberately contains a volatility keyword
// ("today"/"weather", per volatileQueryPattern) so Cache L3-lite's own
// lexical near-duplicate matching bypasses entirely (isVolatileQuery) --
// L3 has no promptFingerprint/responseFormatFingerprint concept of its
// own at all (a pre-existing scope boundary this feature doesn't touch,
// matching Phase 1's identical scoping of responseFormatFingerprint to
// L1/L2 only), so without the bypass L3 would happily serve a
// content-similarity hit across two different prompt_ids and mask
// exactly the L1/L2 behavior this test exists to isolate.
func TestPromptFingerprintBustsACacheHitEvenWithByteIdenticalResolvedContent(t *testing.T) {
	var upstreamCalls int
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments)

	const content = "what's today's weather"
	if _, err := p.UpsertPrompt("greeting-a", []adapter.Message{{Role: "user", Content: content}}); err != nil {
		t.Fatalf("UpsertPrompt greeting-a: %v", err)
	}
	if _, err := p.UpsertPrompt("greeting-b", []adapter.Message{{Role: "user", Content: content}}); err != nil {
		t.Fatalf("UpsertPrompt greeting-b: %v", err)
	}

	ctx := context.Background()
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", PromptID: "greeting-a"}); err != nil {
		t.Fatalf("first HandleChatCompletion (greeting-a): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("after first request: upstreamCalls = %d, want 1", upstreamCalls)
	}

	// Sanity check: repeating the EXACT same request (same prompt_id)
	// really does hit the cache -- proves the harness itself still
	// caches normally, so the next assertion's "2" isn't just cache
	// being broken/disabled entirely.
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", PromptID: "greeting-a"}); err != nil {
		t.Fatalf("repeat HandleChatCompletion (greeting-a): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("after repeating the identical request: upstreamCalls = %d, want 1 (should have been a cache hit)", upstreamCalls)
	}

	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", PromptID: "greeting-b"}); err != nil {
		t.Fatalf("second HandleChatCompletion (greeting-b): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("after a different prompt_id resolving to byte-identical content: upstreamCalls = %d, want 2 (the promptFingerprint cache-key fold must bust the would-be collision)", upstreamCalls)
	}
}

// TestPromptVersionChangeBustsACacheHit proves the same fold from the
// angle the spec calls out explicitly: pinning a DIFFERENT
// PromptVersion of the SAME prompt_id must not reuse the prior
// version's cache entry, even when (as constructed here) the two
// versions' stored content is byte-identical. See the sibling test
// above for why the content includes a volatility keyword (bypasses
// Cache L3-lite, which has no promptFingerprint concept of its own).
func TestPromptVersionChangeBustsACacheHit(t *testing.T) {
	var upstreamCalls int
	deployments := []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, providerReq any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments)

	const content = "what's today's weather"
	if _, err := p.UpsertPrompt("greeting", []adapter.Message{{Role: "user", Content: content}}); err != nil {
		t.Fatalf("UpsertPrompt v1: %v", err)
	}
	if _, err := p.UpsertPrompt("greeting", []adapter.Message{{Role: "user", Content: content}}); err != nil {
		t.Fatalf("UpsertPrompt v2: %v", err)
	}

	ctx := context.Background()
	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", PromptID: "greeting", PromptVersion: 1}); err != nil {
		t.Fatalf("HandleChatCompletion (version 1): %v", err)
	}
	if upstreamCalls != 1 {
		t.Fatalf("after version 1: upstreamCalls = %d, want 1", upstreamCalls)
	}

	if _, err := p.HandleChatCompletion(ctx, "Bearer test-key", adapter.ChatRequest{Model: "gpt-4o", PromptID: "greeting", PromptVersion: 2}); err != nil {
		t.Fatalf("HandleChatCompletion (version 2): %v", err)
	}
	if upstreamCalls != 2 {
		t.Errorf("after pinning a different version with byte-identical content: upstreamCalls = %d, want 2 (the promptFingerprint cache-key fold must bust the would-be collision)", upstreamCalls)
	}
}
