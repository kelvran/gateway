package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// newEmbeddingOnlyUpstream behaves like an OpenAI-compatible embedding
// model's endpoint. A chat-shaped body (one carrying "messages", which is
// what the kind-unaware probe used to send) is rejected first, whatever
// else it carries — exactly what Bedrock's Titan embedding model did to the
// old probe in the 2026-10-07/08 live verification (F3). An embeddings body
// (carrying "input") gets a one-vector 200 only if its wire model is the
// deployment's upstream_model; the canonical alias is a 404 "model not
// found", the way a real provider answers an unknown model id.
// Discrimination is by body shape, not path, so the pre-fix probe cannot
// pass by accident.
func newEmbeddingOnlyUpstream(t *testing.T, upstreamModel string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var wire struct {
			Model    string          `json:"model"`
			Input    json.RawMessage `json:"input"`
			Messages json.RawMessage `json:"messages"`
		}
		_ = json.Unmarshal(body, &wire)
		switch {
		case len(wire.Messages) > 0:
			http.Error(w, `{"error":{"message":"this model does not support chat completions"}}`, http.StatusBadRequest)
		case len(wire.Input) == 0:
			http.Error(w, `{"error":{"message":"input is required"}}`, http.StatusBadRequest)
		case wire.Model != upstreamModel:
			http.Error(w, `{"error":{"message":"model not found: `+strings.ReplaceAll(wire.Model, "`", "")+`"}}`, http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","model":"` + upstreamModel + `","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestIntegrationReadyzIsReadyWithAnEmbeddingDeployment is the end-to-end F3
// reproduction: one chat deployment and one Kind "embedding" deployment behind
// real HTTP upstreams, three probe passes, then /readyz must be 200 with BOTH
// models true. Before the fix the embedding deployment's chat-shaped probe got
// a 400 every time, so /readyz was 503 whenever an embedding deployment was
// configured with health probes on.
func TestIntegrationReadyzIsReadyWithAnEmbeddingDeployment(t *testing.T) {
	chatUpstream, _ := newHealthProbeCountingUpstream(true)
	defer chatUpstream.Close()
	embUpstream := newEmbeddingOnlyUpstream(t, "text-embedding-3-small")

	t.Setenv("KELVRAN_HEALTH_PROBE_EMB_TEST_CHAT_KEY", "fake-upstream-key-not-a-real-secret")
	t.Setenv("KELVRAN_HEALTH_PROBE_EMB_TEST_EMB_KEY", "fake-upstream-key-not-a-real-secret")

	gatewayKey := "test-gateway-key-health-probe-emb"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "chat", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: chatUpstream.URL, APIKeyEnv: "KELVRAN_HEALTH_PROBE_EMB_TEST_CHAT_KEY"},
			// Canonical alias deliberately differs from upstream_model: the
			// mock answers 404 to the alias, so this also proves the probe
			// (and HandleEmbeddings, which shares callEmbeddingDeployment)
			// sends upstream_model on the wire.
			{Name: "titan-embed", Model: "fast-embed", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: embUpstream.URL + "/v1/embeddings", APIKeyEnv: "KELVRAN_HEALTH_PROBE_EMB_TEST_EMB_KEY", Kind: "embedding"},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o":     {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
			"fast-embed": {PromptPerToken: decimal.RequireFromString("0.00000002"), CompletionPerToken: decimal.Zero},
		},
		HealthProbe: controlplane.HealthProbeConfig{UnhealthyThreshold: 3, HealthyThreshold: 2},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		pipeline.ProbeDeployments(ctx)
	}

	rec := httptest.NewRecorder()
	readyzHandler(pipeline)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200 (each deployment answers its own kind of probe); body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ready  bool            `json:"ready"`
		Models map[string]bool `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/readyz body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if !body.Ready || !body.Models["gpt-4o"] || !body.Models["fast-embed"] {
		t.Errorf("/readyz body = %+v, want ready with both models true", body)
	}
}
