package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// newModelsIntegrationServer serves /v1/models and /v1/chat/completions over
// a real pipeline with three canonical models (two chat, one embedding) and
// the given keys. No upstream is ever reached: listing models is a local read.
func newModelsIntegrationServer(t *testing.T, keys []controlplane.VirtualKeyConfig, models map[string]controlplane.ModelMetadataConfig) *httptest.Server {
	t.Helper()
	return newModelsIntegrationServerWithLogger(t, keys, models, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newModelsIntegrationServerCapturingLogs is newModelsIntegrationServer with
// the gateway's JSON log captured, for tests that assert on (or on the
// absence of) log content.
func newModelsIntegrationServerCapturingLogs(t *testing.T, keys []controlplane.VirtualKeyConfig, models map[string]controlplane.ModelMetadataConfig) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return newModelsIntegrationServerWithLogger(t, keys, models, slog.New(slog.NewJSONHandler(&logs, nil))), &logs
}

func newModelsIntegrationServerWithLogger(t *testing.T, keys []controlplane.VirtualKeyConfig, models map[string]controlplane.ModelMetadataConfig, logger *slog.Logger) *httptest.Server {
	t.Helper()
	t.Setenv("KELVRAN_MODELS_INTEGRATION_TEST_KEY", "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr:  ":0",
		VirtualKeys: keys,
		Deployments: []controlplane.DeploymentConfig{
			{Name: "gpt4o-a", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: "http://127.0.0.1:9/unused", APIKeyEnv: "KELVRAN_MODELS_INTEGRATION_TEST_KEY"},
			{Name: "gpt4o-b", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o-2024-08-06", BaseURL: "http://127.0.0.1:9/unused", APIKeyEnv: "KELVRAN_MODELS_INTEGRATION_TEST_KEY"},
			{Name: "mini", Model: "gpt-4o-mini", Provider: "openai", UpstreamModel: "gpt-4o-mini", BaseURL: "http://127.0.0.1:9/unused", APIKeyEnv: "KELVRAN_MODELS_INTEGRATION_TEST_KEY"},
			{Name: "embed", Model: "text-embedding-3-small", Provider: "openai", UpstreamModel: "text-embedding-3-small", BaseURL: "http://127.0.0.1:9/unused", APIKeyEnv: "KELVRAN_MODELS_INTEGRATION_TEST_KEY", Kind: "embedding"},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o":                 {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
			"gpt-4o-mini":            {PromptPerToken: decimal.RequireFromString("0.00000015"), CompletionPerToken: decimal.RequireFromString("0.0000006")},
			"text-embedding-3-small": {PromptPerToken: decimal.RequireFromString("0.00000002"), CompletionPerToken: decimal.Zero},
		},
		Models: models,
	}
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	mux := newDataPlaneMux(pipeline)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func modelsTestKeys() []controlplane.VirtualKeyConfig {
	return []controlplane.VirtualKeyConfig{
		{Name: "all", KeyHash: testKeyHash("all-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		{Name: "mini-only", KeyHash: testKeyHash("mini-secret"), RateLimitBurst: 100, RateLimitRefill: 100, AllowedModels: []string{"gpt-4o-mini"}},
	}
}

// getModels performs GET /v1/models<query> with the given bearer ("" sends
// no Authorization header) and returns status, headers and body.
func getModels(t *testing.T, gwURL, bearer, query string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, gwURL+"/v1/models"+query, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// openAIModelList is what the OpenAI SDK reads.
type openAIModelList struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// anthropicModelList is what the Anthropic SDK (and Claude Code) read.
type anthropicModelList struct {
	Data []struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
		Kind        string `json:"kind"`
	} `json:"data"`
	FirstID *string `json:"first_id"`
	LastID  *string `json:"last_id"`
	HasMore bool    `json:"has_more"`
}

func TestIntegrationModelsRequiresAuth(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	for _, bearer := range []string{"", "not-a-key"} {
		status, hdr, body := getModels(t, gw.URL, bearer, "")
		if status != http.StatusUnauthorized {
			t.Errorf("bearer %q: status = %d, want 401; body %s", bearer, status, body)
		}
		if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("bearer %q: Content-Type = %q, want the JSON error envelope", bearer, ct)
		}
		var env struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Type != "authentication_error" {
			t.Errorf("bearer %q: envelope = %s (err %v), want type authentication_error", bearer, body, err)
		}
	}
}

func TestIntegrationModelsSupersetDecodesForBothSDKs(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), map[string]controlplane.ModelMetadataConfig{
		"gpt-4o": {DisplayName: "GPT-4o", Description: "General-purpose chat."},
	})
	before := time.Now().Add(-2 * time.Second)
	status, hdr, body := getModels(t, gw.URL, "all-secret", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var asOpenAI openAIModelList
	if err := json.Unmarshal(body, &asOpenAI); err != nil {
		t.Fatalf("OpenAI shape: %v\n%s", err, body)
	}
	if asOpenAI.Object != "list" || len(asOpenAI.Data) != 3 {
		t.Fatalf("OpenAI shape: object=%q, %d models; want list with 3\n%s", asOpenAI.Object, len(asOpenAI.Data), body)
	}
	wantIDs := []string{"gpt-4o", "gpt-4o-mini", "text-embedding-3-small"}
	for i, d := range asOpenAI.Data {
		if d.ID != wantIDs[i] || d.Object != "model" || d.OwnedBy != "openai" || d.Created < before.Unix() {
			t.Errorf("OpenAI shape data[%d] = %+v, want id %q, object model, owned_by openai, recent created", i, d, wantIDs[i])
		}
	}

	var asAnthropic anthropicModelList
	if err := json.Unmarshal(body, &asAnthropic); err != nil {
		t.Fatalf("Anthropic shape: %v\n%s", err, body)
	}
	if asAnthropic.HasMore || asAnthropic.FirstID == nil || asAnthropic.LastID == nil || *asAnthropic.FirstID != "gpt-4o" || *asAnthropic.LastID != "text-embedding-3-small" {
		t.Errorf("Anthropic shape pagination = has_more %v first %v last %v, want false/gpt-4o/text-embedding-3-small", asAnthropic.HasMore, asAnthropic.FirstID, asAnthropic.LastID)
	}
	for i, d := range asAnthropic.Data {
		if d.Type != "model" {
			t.Errorf("data[%d].type = %q, want model", i, d.Type)
		}
		if _, err := time.Parse(time.RFC3339, d.CreatedAt); err != nil {
			t.Errorf("data[%d].created_at = %q is not RFC 3339: %v", i, d.CreatedAt, err)
		}
	}
	if g := asAnthropic.Data[0]; g.DisplayName != "GPT-4o" || g.Description != "General-purpose chat." || g.Kind != "chat" {
		t.Errorf("gpt-4o entry = %+v, want operator display_name/description and kind chat", g)
	}
	if m := asAnthropic.Data[1]; m.DisplayName != "gpt-4o-mini" || m.Description != "" {
		t.Errorf("gpt-4o-mini entry = %+v, want display_name falling back to the id and an empty description", m)
	}
	if e := asAnthropic.Data[2]; e.Kind != "embedding" {
		t.Errorf("text-embedding-3-small kind = %q, want embedding", e.Kind)
	}
}

func TestIntegrationModelsPagination(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	page := func(query string) anthropicModelList {
		t.Helper()
		status, _, body := getModels(t, gw.URL, "all-secret", query)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d; body %s", query, status, body)
		}
		var l anthropicModelList
		if err := json.Unmarshal(body, &l); err != nil {
			t.Fatalf("%s: %v\n%s", query, err, body)
		}
		return l
	}
	ids := func(l anthropicModelList) []string {
		out := make([]string, 0, len(l.Data))
		for _, d := range l.Data {
			out = append(out, d.ID)
		}
		return out
	}

	p1 := page("?limit=1")
	if got := ids(p1); len(got) != 1 || got[0] != "gpt-4o" || !p1.HasMore || *p1.FirstID != "gpt-4o" || *p1.LastID != "gpt-4o" {
		t.Errorf("limit=1 -> %v has_more=%v first=%v last=%v, want [gpt-4o] true gpt-4o gpt-4o", got, p1.HasMore, p1.FirstID, p1.LastID)
	}
	p2 := page("?limit=1&after_id=" + *p1.LastID)
	if got := ids(p2); len(got) != 1 || got[0] != "gpt-4o-mini" || !p2.HasMore {
		t.Errorf("after_id=gpt-4o -> %v has_more=%v, want [gpt-4o-mini] true", got, p2.HasMore)
	}
	p3 := page("?limit=1&after_id=" + *p2.LastID)
	if got := ids(p3); len(got) != 1 || got[0] != "text-embedding-3-small" || p3.HasMore {
		t.Errorf("after_id=gpt-4o-mini -> %v has_more=%v, want [text-embedding-3-small] false (auto-pagination must terminate)", got, p3.HasMore)
	}
	p4 := page("?limit=1&after_id=text-embedding-3-small")
	if len(p4.Data) != 0 || p4.HasMore || p4.FirstID != nil || p4.LastID != nil {
		t.Errorf("after the last id -> %+v, want an empty page with null cursors and has_more false", p4)
	}
	p5 := page("?limit=1&before_id=text-embedding-3-small")
	if got := ids(p5); len(got) != 1 || got[0] != "gpt-4o-mini" || !p5.HasMore {
		t.Errorf("before_id=text-embedding-3-small -> %v has_more=%v, want [gpt-4o-mini] true (gpt-4o still precedes it)", got, p5.HasMore)
	}
	all := page("?limit=5000")
	if got := ids(all); len(got) != 3 || all.HasMore {
		t.Errorf("limit=5000 (clamped) -> %v has_more=%v, want all 3 and false", got, all.HasMore)
	}
}

func TestIntegrationModelsBadQueryIs400Envelope(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	cases := map[string]string{
		"?limit=0":                               "limit",
		"?limit=-3":                              "limit",
		"?limit=abc":                             "limit",
		"?after_id=no-such-model":                "after_id",
		"?before_id=no-such-model":               "before_id",
		"?after_id=gpt-4o&before_id=gpt-4o-mini": "after_id",
	}
	for query, wantParam := range cases {
		status, _, body := getModels(t, gw.URL, "all-secret", query)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body %s", query, status, body)
			continue
		}
		var env struct {
			Error struct {
				Type  string  `json:"type"`
				Param *string `json:"param"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Type != "invalid_request_error" || env.Error.Param == nil || *env.Error.Param != wantParam {
			t.Errorf("%s: envelope = %s (err %v), want invalid_request_error naming %q", query, body, err, wantParam)
		}
	}
}

// TestIntegrationModelsNoRedirectOnTrailingSlash: Claude Code treats any
// redirect from this route as a failed provider, so the pattern must be the
// exact path -- "/v1/models/" is a 404, never a ServeMux 301.
func TestIntegrationModelsNoRedirectOnTrailingSlash(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect is a failure for this route")
	}}
	for path, want := range map[string]int{"/v1/models?limit=1000": http.StatusOK, "/v1/models/": http.StatusNotFound} {
		req, _ := http.NewRequest(http.MethodGet, gw.URL+path, nil)
		req.Header.Set("Authorization", "Bearer all-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v (a redirect would surface here)", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: status = %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestIntegrationModelsFiltersPerKey(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	status, _, body := getModels(t, gw.URL, "mini-secret", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	var l openAIModelList
	if err := json.Unmarshal(body, &l); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(l.Data) != 1 || l.Data[0].ID != "gpt-4o-mini" {
		t.Errorf("scoped key sees %+v, want only gpt-4o-mini", l.Data)
	}
}

func TestIntegrationModelsMethodNotAllowed(t *testing.T) {
	gw := newModelsIntegrationServer(t, modelsTestKeys(), nil)
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer all-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodGet {
		t.Errorf("POST /v1/models = %d Allow %q, want 405 Allow GET", resp.StatusCode, resp.Header.Get("Allow"))
	}
}
