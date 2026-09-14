// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter/anthropic"
	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// TestIntegrationVirtualModelAliasFansOutAcrossProviders is a real,
// direct, end-to-end proof of a genuine finding from
// docs/upgrade-research/competitor-feature-parity-2026-09-14.md Finding
// 1: Kelvran's WRR pool already supports "virtual model aliasing" --
// one client-facing name spanning genuinely different real providers --
// with ZERO new routing code, because controlplane.DeploymentConfig's
// own doc comment already states "multiple deployments may share the
// same Model" with no homogeneity requirement anywhere in config
// parsing, router construction, or dataplane's own routing path. This
// was previously undemonstrated by any test in this repo -- the closest
// prior integration coverage
// (TestBuildPipelineAcceptsEveryRealRegisteredProvider) proves every
// registered provider individually, never two DIFFERENT providers
// sharing one canonical Model name.
//
// This test deliberately does NOT prove per-alias price differentiation
// works -- it doesn't, and can't: costaccounting.PriceTable is keyed by
// the canonical Model name (realServingModel returns dep.Model, the
// shared alias name, not dep.UpstreamModel), so every deployment under
// one alias is priced identically regardless of which real provider
// actually served the request. That remains a real, disclosed
// limitation -- see gateway/ARCHITECTURE.md's own corrected entry.
func TestIntegrationVirtualModelAliasFansOutAcrossProviders(t *testing.T) {
	var mu sync.Mutex
	providerCalls := map[string]int{}

	openaiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		providerCalls["openai"]++
		mu.Unlock()
		resp := openai.Response{
			ID:    "chatcmpl-alias-test",
			Model: "gpt-4o",
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello from openai"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer openaiUpstream.Close()

	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		providerCalls["anthropic"]++
		mu.Unlock()
		resp := anthropic.Response{
			ID: "msg_alias_test", Model: "claude-opus-4-6", Role: "assistant",
			Content:    []anthropic.ContentBlock{{Type: "text", Text: "hello from anthropic"}},
			StopReason: "end_turn",
			Usage:      anthropic.Usage{InputTokens: 5, OutputTokens: 3},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer anthropicUpstream.Close()

	t.Setenv("KELVRAN_ALIAS_TEST_OPENAI_KEY", "fake-upstream-key-not-a-real-secret")
	t.Setenv("KELVRAN_ALIAS_TEST_ANTHROPIC_KEY", "fake-upstream-key-not-a-real-secret")

	const aliasName = "kelvran-smart"
	gatewayKey := "test-gateway-key-alias"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "alias-openai-leg",
				Model:         aliasName,
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       openaiUpstream.URL,
				APIKeyEnv:     "KELVRAN_ALIAS_TEST_OPENAI_KEY",
			},
			{
				Name:          "alias-anthropic-leg",
				Model:         aliasName,
				Provider:      "anthropic",
				UpstreamModel: "claude-opus-4-6",
				BaseURL:       anthropicUpstream.URL,
				APIKeyEnv:     "KELVRAN_ALIAS_TEST_ANTHROPIC_KEY",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			aliasName: {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	client := &http.Client{}
	const requestCount = 10
	for i := 0; i < requestCount; i++ {
		reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"alias-test-message-%d"}]}`, aliasName, i)
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("building request %d: %v", i, err)
		}
		req.Header.Set("Authorization", "Bearer "+gatewayKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, body: %s", i, resp.StatusCode, body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if providerCalls["openai"] == 0 {
		t.Error("openai leg of the alias was never called across 10 requests -- want the alias to genuinely fan out across both providers")
	}
	if providerCalls["anthropic"] == 0 {
		t.Error("anthropic leg of the alias was never called across 10 requests -- want the alias to genuinely fan out across both providers")
	}
	if providerCalls["openai"]+providerCalls["anthropic"] != requestCount {
		t.Errorf("total provider calls = %d, want %d", providerCalls["openai"]+providerCalls["anthropic"], requestCount)
	}
	t.Logf("alias %q fanned out: openai=%d anthropic=%d", aliasName, providerCalls["openai"], providerCalls["anthropic"])
}
