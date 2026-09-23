package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestNon2xxUpstreamResponseIsNeverCached is the direct regression proof
// against a real, closed vllm-project/semantic-router GitHub issue
// (#2130, fixed by PR #2131): that gateway's semantic response cache
// unconditionally stored whatever the upstream returned -- including a
// transient or permanent ERROR response -- and replayed the cached error
// as an HTTP 200 to every subsequent semantically-similar request for
// the full TTL, even after the real upstream had recovered. Kelvran's Go
// adapters return a non-2xx upstream status as a Go error value
// (*UpstreamHTTPError), never as a populated adapter.ChatResponse, and
// the cache-write call sites only run once a real ChatResponse exists --
// structurally, an error return can never reach them. Proven here: a
// failing first request must not poison the cache for an identical
// second request that succeeds -- if it did, the second call would
// incorrectly short-circuit to a cached (nonexistent) response instead
// of actually reaching the upstream.
func TestNon2xxUpstreamResponseIsNeverCached(t *testing.T) {
	var upstreamCalls int
	shouldFail := true
	keys := []identity.VirtualKey{
		{ID: "non2xx-key", KeyHash: testHashOf("non2xx-key-cred"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		upstreamCalls++
		if shouldFail {
			return nil, &UpstreamHTTPError{StatusCode: 500, Body: "transient upstream failure"}
		}
		return fakeOpenAIResponse("gpt-4o"), nil
	}, deployments, keys)

	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	// First request: upstream fails with a real 500.
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer non2xx-key-cred", "", "", req, ""); err == nil {
		t.Fatal("HandleChatCompletion with a failing upstream: got nil error, want a real error")
	} else {
		var httpErr *UpstreamHTTPError
		if !errors.As(err, &httpErr) {
			t.Fatalf("HandleChatCompletion error = %v, want it to wrap *UpstreamHTTPError", err)
		}
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 after the first (failing) request", upstreamCalls)
	}

	// Second, IDENTICAL request: upstream now succeeds. It must actually
	// be attempted again, not short-circuited to a cached error entry
	// that (per the semantic-router bug) would never have existed in a
	// correctly-designed cache anyway -- but this assertion is the real
	// proof: if the first failure had been cached and replayed, this
	// call would return the SAME error again despite upstream now
	// succeeding, or it would skip calling upstream entirely.
	shouldFail = false
	resp, err := p.HandleChatCompletion(context.Background(), "Bearer non2xx-key-cred", "", "", req, "")
	if err != nil {
		t.Fatalf("HandleChatCompletion on the second (now-succeeding) request: %v", err)
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 -- the second request must actually reach upstream, never served from a poisoned cache entry left by the first failure", upstreamCalls)
	}
	if resp.Model != "gpt-4o" {
		t.Errorf("resp.Model = %q, want a real response from the now-succeeding upstream", resp.Model)
	}
}
