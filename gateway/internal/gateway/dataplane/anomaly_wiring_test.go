package dataplane

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/cache/inprocess"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/guardrail"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
)

// TestHandleChatCompletionLogsAnomalyOnFinishReasonSpike is the
// end-to-end wiring proof for observeAnomalySignals: a virtual key whose
// requests shift from an all-"stop" finish-reason baseline to a
// majority-"length" recent window must produce a real
// anomaly_detected_finish_reason_shift log line via the actual
// HandleChatCompletion path -- not just internal/anomaly's own
// lower-level unit tests.
func TestHandleChatCompletionLogsAnomalyOnFinishReasonSpike(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	keys := []identity.VirtualKey{
		{ID: "anomaly-key", KeyHash: testHashOf("anomaly-key-cred"), RateLimitBurst: 1000, RateLimitRefill: 1000},
	}
	deployments := []Deployment{
		{Name: "d1", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://unused"},
	}
	finishReason := "stop"
	verifier, err := identity.NewVerifier(keys)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	p, err := NewPipeline(Config{
		Verifier:       verifier,
		Limiter:        ratelimit.NewInMemoryKeyLimiter(keyConfigsFromVirtualKeys(keys)),
		Budget:         budget.NewTracker(),
		Cache:          inprocess.New(0),
		CacheL2:        inprocess.New(0),
		CacheL3:        inprocess.NewLexicalCache(0),
		Guardrails:     guardrail.NewEngine(guardrail.DefaultDetectors(), guardrail.DefaultPolicy(), "test", nil),
		Adapters:       adapter.Registry{"openai": openai.New()},
		Router:         testRouter(deployments),
		Deployments:    deployments,
		CostCalculator: costaccounting.NewCalculator(costaccounting.PriceTable{}),
		Upstream: func(ctx context.Context, dep Deployment, req any) (any, error) {
			resp := fakeOpenAIResponse("gpt-4o")
			resp.Choices[0].FinishReason = finishReason
			return resp, nil
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}

	// req varies its own content per call (via n, a distinct trailing
	// letter) so every call is a genuine cache MISS that actually reaches
	// the fake upstream above -- identical OR lexically-near-identical
	// requests (e.g. varying only a repeated substring's count) would
	// otherwise be served from an earlier call's own cached response via
	// L1 or L3's near-duplicate matching, never observing a later shift
	// in the fake upstream's own finishReason variable at all.
	req := func(n int) adapter.ChatRequest {
		content := "distinct request number " + string(rune('a'+n%26)) + string(rune('A'+(n/26)%26))
		return adapter.ChatRequest{Model: "gpt-4o", Messages: []adapter.Message{{Role: "user", Content: content}}}
	}

	// Baseline window: anomalyWindowSize (50) requests, all "stop".
	for i := 0; i < anomalyWindowSize; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer anomaly-key-cred", "", "", req(i), ""); err != nil {
			t.Fatalf("baseline request #%d: %v", i+1, err)
		}
	}
	if strings.Contains(logBuf.String(), "anomaly_detected_finish_reason_shift") {
		t.Fatal("anomaly flagged during the baseline window itself -- want no flag until a real shift in the SUBSEQUENT window")
	}

	// Recent window: shift to majority "length".
	finishReason = "length"
	for i := 0; i < anomalyWindowSize; i++ {
		if _, err := p.HandleChatCompletion(context.Background(), "Bearer anomaly-key-cred", "", "", req(anomalyWindowSize+i), ""); err != nil {
			t.Fatalf("recent request #%d: %v", i+1, err)
		}
	}

	if !strings.Contains(logBuf.String(), "anomaly_detected_finish_reason_shift") {
		t.Fatal("HandleChatCompletion never logged anomaly_detected_finish_reason_shift after a real all-stop -> all-length shift")
	}
}
