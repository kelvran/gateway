package dataplane

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// TestDeploymentTPMTokens pins the quota-weighting formula against the
// worked example in controlplane.DeploymentConfig.TPMOutputTokenMultiplier's
// doc comment: inclusive input 100 (20 fresh + 60 cache-read + 20
// cache-write), output 20.
func TestDeploymentTPMTokens(t *testing.T) {
	u := adapter.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CacheReadTokens: 60, CacheCreationTokens: 20}
	tests := []struct {
		name string
		dep  Deployment
		want float64
	}{
		{"raw tokens (both fields unset)", Deployment{}, 120},
		{"multiplier 1 explicit is raw", Deployment{TPMOutputTokenMultiplier: 1}, 120},
		{"exclude cache reads only", Deployment{TPMExcludeCacheReadTokens: true}, 60},
		{"x10 output only", Deployment{TPMOutputTokenMultiplier: 10}, 300},
		{"Bedrock Sonnet 5 shape: x10, cache reads free (= Bedrock's 20 + 20 + 200)", Deployment{TPMOutputTokenMultiplier: 10, TPMExcludeCacheReadTokens: true}, 240},
		{"Bedrock Claude 4.8 shape: x15, cache reads free", Deployment{TPMOutputTokenMultiplier: 15, TPMExcludeCacheReadTokens: true}, 340},
		{"Bedrock Claude <= 4.7 shape: x5, cache reads free", Deployment{TPMOutputTokenMultiplier: 5, TPMExcludeCacheReadTokens: true}, 140},
		{"sub-1 multiplier is treated as 1 (the parser rejects it; defensive)", Deployment{TPMOutputTokenMultiplier: 0.5}, 120},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deploymentTPMTokens(tt.dep, u); got != tt.want {
				t.Errorf("deploymentTPMTokens = %v, want %v", got, tt.want)
			}
		})
	}
	if got := deploymentTPMTokens(Deployment{TPMOutputTokenMultiplier: 10, TPMExcludeCacheReadTokens: true}, adapter.Usage{}); got != 0 {
		t.Errorf("deploymentTPMTokens(zero usage) = %v, want 0", got)
	}
	// A misbehaving upstream reporting more cache reads than prompt tokens
	// must not produce a negative debit (which would CREDIT the bucket).
	if got := deploymentTPMTokens(Deployment{TPMExcludeCacheReadTokens: true}, adapter.Usage{PromptTokens: 5, CacheReadTokens: 50, CompletionTokens: 1}); got != 1 {
		t.Errorf("deploymentTPMTokens(cache reads > prompt) = %v, want 1 (prompt clamped to 0)", got)
	}
}

// TestDeploymentTPMWeightingExhaustsTheBucketEarlierThanRawCounting is
// the differential proof that the weighting is really applied to the
// deployment bucket: two deployments with identical 100-token buckets and
// an identical upstream (prompt 20 of which 10 cached, completion 5 —
// 25 raw tokens, 20 - 10 + 5*10 = 60 weighted). Raw counting admits a
// third call (100 -> 75 -> 50 -> 25); x10-with-cache-reads-excluded
// admits two (cold start reserves 100, reconciles to 40; the second
// reserves the 60 running mean, reconciles to -20) and rejects the third
// with Reason "tpm". Had deploymentTPMTokens ignored the two fields, the
// weighted deployment would have admitted the third call too.
func TestDeploymentTPMWeightingExhaustsTheBucketEarlierThanRawCounting(t *testing.T) {
	newPipeline := func(dep Deployment) *Pipeline {
		return &Pipeline{
			deploymentLimiter: deploymentTPMLimiter(dep.Name, 100, 0),
			logger:            discardLogger(),
			adapters:          adapter.Registry{"openai": openai.New()},
			upstream: func(ctx context.Context, d Deployment, req any) (any, error) {
				return &openai.Response{
					ID:    "chatcmpl-weighted",
					Model: d.UpstreamModel,
					Choices: []openai.Choice{
						{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello"`)}, FinishReason: "stop"},
					},
					Usage: openai.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25, PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: 10}},
				}, nil
			},
		}
	}
	req := adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}

	raw := Deployment{Name: "raw", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o"}
	rawPipeline := newPipeline(raw)
	for i := 1; i <= 3; i++ {
		if _, err := rawPipeline.callDeploymentWithCapacityCheck(context.Background(), raw, req); err != nil {
			t.Fatalf("raw deployment call %d: %v, want admitted (25 raw tokens per call against 100)", i, err)
		}
	}

	weighted := raw
	weighted.Name = "weighted"
	weighted.TPMOutputTokenMultiplier = 10
	weighted.TPMExcludeCacheReadTokens = true
	weightedPipeline := newPipeline(weighted)
	for i := 1; i <= 2; i++ {
		if _, err := weightedPipeline.callDeploymentWithCapacityCheck(context.Background(), weighted, req); err != nil {
			t.Fatalf("weighted deployment call %d: %v, want admitted", i, err)
		}
	}
	_, err := weightedPipeline.callDeploymentWithCapacityCheck(context.Background(), weighted, req)
	assertDeploymentCapacityTPM(t, err, "weighted")
}
