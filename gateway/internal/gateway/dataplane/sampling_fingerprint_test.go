package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/idempotency"
	idempotencyinprocess "github.com/kelvran/gateway/gateway/internal/idempotency/inprocess"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestSamplingFingerprint pins the cache-key / L3-gate input item 11 slice
// S5 derives from the four sampling fields: "" when none is set (so every
// request built before the fields existed keeps folding the empty string),
// otherwise the canonical JSON of the set fields only, distinct per field
// and per value.
func TestSamplingFingerprint(t *testing.T) {
	if got := samplingFingerprint(adapter.ChatRequest{}); got != "" {
		t.Errorf("samplingFingerprint(no sampling) = %q, want \"\"", got)
	}
	topP := 0.9
	topK := 5
	cases := map[string]adapter.ChatRequest{
		`{"top_p":0.9}`:    {TopP: &topP},
		`{"top_k":5}`:      {TopK: &topK},
		`{"stop":["END"]}`: {StopSequences: adapter.StopSequences{"END"}},
		`{"effort":"low"}`: {Effort: "low"},
		`{"top_p":0.9,"top_k":5,"stop":["END"],"effort":"low"}`: {TopP: &topP, TopK: &topK, StopSequences: adapter.StopSequences{"END"}, Effort: "low"},
	}
	for want, req := range cases {
		if got := samplingFingerprint(req); got != want {
			t.Errorf("samplingFingerprint = %q, want %q", got, want)
		}
	}
	zero := 0.0
	if got := samplingFingerprint(adapter.ChatRequest{TopP: &zero}); got != `{"top_p":0}` {
		t.Errorf("samplingFingerprint(top_p 0) = %q, want {\"top_p\":0} (an explicit 0 is a value, not absence)", got)
	}
}

// TestHandleChatCompletionNeverServesAcrossDifferentSampling is the
// full-pipeline proof for slice S5's fold: byte-identical messages with a
// different top_p must not share a cache entry at any layer, and the same
// sampling repeated IS a hit.
func TestHandleChatCompletionNeverServesAcrossDifferentSampling(t *testing.T) {
	var upstreamCalls int
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	const content = "Explain how binary search works in a sorted array"
	topP := 0.9
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}}
	sampled := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}, TopP: &topP}
	for i, req := range []adapter.ChatRequest{plain, sampled, sampled} {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
			t.Fatalf("HandleChatCompletion #%d: %v", i+1, err)
		}
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 (plain and sampled are different requests; the repeated sampled request is a hit)", upstreamCalls)
	}
}

// TestCheckLexicalCacheNeverServesAcrossDifferentSamplingFingerprint is the
// L3 gate half, with a hand-fixed signature so the sampling gate is the
// ONLY variable.
func TestCheckLexicalCacheNeverServesAcrossDifferentSamplingFingerprint(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse("gpt-4o"), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	ctx := context.Background()
	vk := &identity.VirtualKey{ID: "test-key"}
	fixedSignature := []uint64{1, 2, 3, 4}
	messages := []adapter.Message{{Role: "user", Content: "hi"}}
	written := adapter.ChatRequest{Model: "gpt-4o", Messages: messages, StopSequences: adapter.StopSequences{"END"}}
	writtenResp := []byte(`{"id":"cached-resp"}`)
	if err := p.cacheL3.Put(ctx, vk.ID, fixedSignature, writtenResp, nil, "gpt-4o", p.guardrails.Version(), "", "", nil, reasoningBlocksFingerprint(messages), "", "", "", samplingFingerprint(written), "", time.Hour); err != nil {
		t.Fatalf("cacheL3.Put: %v", err)
	}
	if _, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, adapter.ChatRequest{Model: "gpt-4o", Messages: messages}, "irrelevant-l1-key", fixedSignature, ""); hit {
		t.Error("checkLexicalCache returned a hit for a query without stop sequences against an entry written with one -- the gate must reject this")
	}
	cached, _, _, hit := p.checkLexicalCache(ctx, vk, vk.ID, written, "irrelevant-l1-key", fixedSignature, "")
	if !hit || string(cached) != string(writtenResp) {
		t.Fatalf("checkLexicalCache miss (or wrong body %q) for a query whose sampling matches the written entry", cached)
	}
}

// TestIdempotencyFingerprintFoldsHiddenSamplingFields extends the S4 repair:
// top_k and effort are json:"-" like Thinking, so they too must be folded
// into the Idempotency-Key fingerprint explicitly (top_p and stop are on
// the wire and already change the marshaled body); a request without any
// hidden field still fingerprints byte-for-byte as sha256(body).
func TestIdempotencyFingerprintFoldsHiddenSamplingFields(t *testing.T) {
	topP := 0.9
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, TopP: &topP}
	body, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	base, err := idempotencyFingerprint(plain)
	if err != nil {
		t.Fatalf("idempotencyFingerprint: %v", err)
	}
	if want := sha256.Sum256(body); base != want {
		t.Errorf("fingerprint of a request with only wire fields changed: got %x, want sha256(json.Marshal(req)) %x", base, want)
	}
	topK := 5
	withTopK := plain
	withTopK.TopK = &topK
	fpTopK, err := idempotencyFingerprint(withTopK)
	if err != nil {
		t.Fatal(err)
	}
	withEffort := plain
	withEffort.Effort = "low"
	fpEffort, err := idempotencyFingerprint(withEffort)
	if err != nil {
		t.Fatal(err)
	}
	if fpTopK == base || fpEffort == base || fpTopK == fpEffort {
		t.Errorf("hidden sampling fields did not separate the fingerprints: base %x, top_k %x, effort %x", base, fpTopK, fpEffort)
	}
}

// TestHandleChatCompletionIdempotencyKeyReusedWithDifferentTopKIsAMismatch
// is the pipeline-level proof: two bodies identical on the wire but with a
// different hidden top_k under one Idempotency-Key are a 422 mismatch, not
// a replay.
func TestHandleChatCompletionIdempotencyKeyReusedWithDifferentTopKIsAMismatch(t *testing.T) {
	p := newTestPipelineWithIdempotency(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, defaultTestVirtualKeys(), idempotencyinprocess.New())

	topK := 5
	plain := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	withTopK := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}, TopK: &topK}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", plain, "reused-key"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", withTopK, "reused-key")
	if !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch for a reused key whose body differs only in the hidden top_k", err)
	}
}
