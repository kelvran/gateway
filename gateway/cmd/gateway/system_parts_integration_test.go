package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// A role:"system" message whose `parts` carry a non-text part, on a
// deployment whose system prompt holds text only (anthropic here; bedrock
// and gemini share the rule), is a request-shape fault: 400
// system_parts_unsupported with param "messages", decided in the adapter
// before any upstream call -- never a 502 for something the client sent, and
// never a silent drop of the part. Only the OpenAI-shaped route can build such
// a message (its `parts` member); the Anthropic ingress records a non-text
// system block as untranslatable instead.
func TestIntegrationChatCompletionsSystemMessageNonTextPartIs400(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "the adapter must refuse the request before any upstream call", http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("KELVRAN_SYSTEM_PARTS_TEST_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "all", KeyHash: testKeyHash("all-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "claude-primary", Model: "claude-sys", Provider: "anthropic", UpstreamModel: "claude-sys", BaseURL: upstream.URL, APIKeyEnv: "KELVRAN_SYSTEM_PARTS_TEST_KEY"},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"claude-sys": {PromptPerToken: decimal.RequireFromString("0.000003"), CompletionPerToken: decimal.RequireFromString("0.000015")},
		},
	}
	pipeline, err := buildPipeline(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	gw := httptest.NewServer(newDataPlaneMux(pipeline))
	t.Cleanup(gw.Close)

	// A real 1x1 PNG, so the inline-part MIME check passes and the adapter is
	// what refuses the request.
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	body := `{"model":"claude-sys","messages":[{"role":"system","content":"Be terse.","parts":[{"type":"image","media_type":"image/png","data":"` + png + `"}]},{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer all-secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, raw)
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("body is not an error envelope: %v\n%s", err, raw)
	}
	if env.Error.Type != "invalid_request_error" || env.Error.Code != "system_parts_unsupported" || env.Error.Param != "messages" {
		t.Errorf("error = %+v, want invalid_request_error / system_parts_unsupported / messages", env.Error)
	}
	if !strings.Contains(env.Error.Message, "system message") || strings.Contains(env.Error.Message, png[:16]) {
		t.Errorf("message = %q, want it to name the system message's part and never echo the part's data", env.Error.Message)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls = %d, want 0: the refusal is decided before any upstream call", calls.Load())
	}
}
