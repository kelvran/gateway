package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// TestRotateVirtualKeyBothHashesWorkDuringGracePeriod proves the
// load-bearing claim of docs/upgrade-research/admin-operator-experience-2026-09-14.md
// Finding 1: rotating a key keeps the OLD secret usable for the requested
// grace period AND makes the NEW secret usable immediately -- no window
// where an in-flight caller using the old secret is broken.
func TestRotateVirtualKeyBothHashesWorkDuringGracePeriod(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-v1"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	if err := p.RotateVirtualKey("team-alpha", testHashOf("alpha-v2"), time.Hour); err != nil {
		t.Fatalf("RotateVirtualKey: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v1", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the OLD secret, still within grace period: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v2", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the NEW secret: %v", err)
	}
}

// TestRotateVirtualKeyOldHashRejectedAfterGracePeriodElapses proves the
// old secret stops working once its grace period has passed -- a
// zero/negative gracePeriod is the simplest way to construct an
// already-elapsed grace period deterministically, without an injectable
// clock.
func TestRotateVirtualKeyOldHashRejectedAfterGracePeriodElapses(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-v1"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	if err := p.RotateVirtualKey("team-alpha", testHashOf("alpha-v2"), -time.Hour); err != nil {
		t.Fatalf("RotateVirtualKey: %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v1", adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
		t.Error("HandleChatCompletion with the OLD secret succeeded past its (zero/negative) grace period, want rejection")
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v2", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the NEW secret: %v", err)
	}
}

// TestRotateVirtualKeyASecondTimeDropsTheDoublyOldHash proves the
// deliberate single-generation-back limit: rotating twice makes only the
// SECOND-to-last secret usable during its own grace period -- the very
// first secret (now two rotations old) stops working immediately, per
// RotateVirtualKey's own doc comment.
func TestRotateVirtualKeyASecondTimeDropsTheDoublyOldHash(t *testing.T) {
	keys := []identity.VirtualKey{
		{ID: "team-alpha", KeyHash: testHashOf("alpha-v1"), RateLimitBurst: 100, RateLimitRefill: 100},
	}
	p := newTestPipelineWithKeys(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		return fakeOpenAIResponse(dep.UpstreamModel), nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}}, keys)

	if err := p.RotateVirtualKey("team-alpha", testHashOf("alpha-v2"), time.Hour); err != nil {
		t.Fatalf("RotateVirtualKey (1st): %v", err)
	}
	if err := p.RotateVirtualKey("team-alpha", testHashOf("alpha-v3"), time.Hour); err != nil {
		t.Fatalf("RotateVirtualKey (2nd): %v", err)
	}

	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v1", adapter.ChatRequest{Model: "gpt-4o"}, ""); err == nil {
		t.Error("HandleChatCompletion with the doubly-old (v1) secret succeeded after a second rotation, want rejection")
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v2", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the singly-old (v2) secret, still within its own grace period: %v", err)
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer alpha-v3", adapter.ChatRequest{Model: "gpt-4o"}, ""); err != nil {
		t.Errorf("HandleChatCompletion with the current (v3) secret: %v", err)
	}
}

func TestRotateVirtualKeyUnknownNameReturnsNotFound(t *testing.T) {
	p := newTestPipeline(t, func(ctx context.Context, dep Deployment, req any) (any, error) {
		t.Fatal("upstream should never be called")
		return nil, nil
	}, []Deployment{{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"}})

	err := p.RotateVirtualKey("never-configured", testHashOf("irrelevant"), time.Hour)
	if !errors.Is(err, ErrVirtualKeyNotFound) {
		t.Fatalf("RotateVirtualKey(unknown) = %v, want ErrVirtualKeyNotFound", err)
	}
}
