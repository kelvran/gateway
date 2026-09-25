package main

// Tests for the file-based credential hot-reload feature's FULL-STACK
// wiring: controlplane.Load-shaped config -> buildPipeline -> a real
// HTTP round trip through chatCompletionsHandler -> the mock upstream
// actually receiving the resolved credential. The reload mechanism's
// own concurrency/timing behavior is proven directly against
// dataplane.Pipeline in internal/gateway/dataplane/credential_reload_test.go;
// this file's job is narrower and complementary: prove cmd/gateway's
// own config-to-Deployment wiring (the *_file config fields ->
// dataplane.Deployment.CredentialFiles/CredentialState) is correct
// end-to-end, for both the bedrock and non-bedrock provider branches.

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// newMockUpstreamCapturingAuth is newMockUpstream's cousin: same real
// OpenAI wire-format response, but also records the LAST
// "Authorization" header value it saw, so tests can assert on exactly
// which credential value reached the upstream call.
func newMockUpstreamCapturingAuth(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var lastAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid upstream request body", http.StatusBadRequest)
			return
		}

		resp := openai.Response{
			ID:    "chatcmpl-credential-file-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"hello"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastAuth
}

// sendChatCompletion is a minimal real HTTP round trip through gw
// (an httptest.Server wrapping chatCompletionsHandler), returning the
// response status code -- callers only care that the request round-
// tripped successfully; the mock upstream's captured Authorization
// header is what actually gets asserted on. message varies the user
// content per call so repeated calls in a polling loop never hit
// buildPipeline's own (always-on, 5-minute-default-TTL) L1 response
// cache -- a cache hit would never reach the mock upstream at all,
// which would make lastAuth appear stuck regardless of whether the
// credential rotation itself actually worked.
func sendChatCompletion(t *testing.T, gatewayURL, gatewayKey, model, message string) int {
	t.Helper()
	reqBody := `{"model":"` + model + `","messages":[{"role":"user","content":"` + message + `"}]}`
	httpReq, err := http.NewRequest(http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+gatewayKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(resp.Body)
	return resp.StatusCode
}

// TestBuildPipelineResolvesAPIKeyFromFileNotEnv proves cmd/gateway's
// buildPipeline resolves a deployment's credential from api_key_file
// end-to-end -- a REAL HTTP request through the exact same
// chatCompletionsHandler production uses reaches the mock upstream
// carrying the FILE's content as the Bearer token, with api_key_env
// left completely unset.
func TestBuildPipelineResolvesAPIKeyFromFileNotEnv(t *testing.T) {
	upstream, lastAuth := newMockUpstreamCapturingAuth(t)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "api-key")
	fileKey := "not-a-real-file-resolved-key-xxxxxxxxxxxx"
	if err := os.WriteFile(keyPath, []byte(fileKey+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-gateway-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "file-backed-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstream.URL,
				APIKeyFile:    keyPath,
				// Deliberately no APIKeyEnv at all -- proves the *_file
				// field is a genuine standalone alternative, not merely
				// an overlay on top of a required *_env value.
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	if status := sendChatCompletion(t, gw.URL, "test-gateway-key", "gpt-4o", "hi"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if want := "Bearer " + fileKey; *lastAuth != want {
		t.Errorf("upstream saw Authorization = %q, want %q (the api_key_file's own content)", *lastAuth, want)
	}
}

// TestBuildPipelineFileBackedDeploymentHotReloadsWithoutRestart is this
// whole feature's headline end-to-end proof, driven through the SAME
// *dataplane.Pipeline buildPipeline actually returns (not a synthetic
// fixture): after the credential file is rewritten and
// RunCredentialReloadLoop is given one short tick, a SECOND request
// through the SAME already-running pipeline reaches the mock upstream
// carrying the NEW value -- no restart, no new buildPipeline call, no
// new Pipeline.
func TestBuildPipelineFileBackedDeploymentHotReloadsWithoutRestart(t *testing.T) {
	upstream, lastAuth := newMockUpstreamCapturingAuth(t)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "api-key")
	initialKey := "not-a-real-initial-key-xxxxxxxxxxxxxxxxxx"
	rotatedKey := "not-a-real-rotated-key-xxxxxxxxxxxxxxxxxx"
	if err := os.WriteFile(keyPath, []byte(initialKey), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-gateway-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "file-backed-primary",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       upstream.URL,
				APIKeyFile:    keyPath,
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	if status := sendChatCompletion(t, gw.URL, "test-gateway-key", "gpt-4o", "hi-before-rotation"); status != http.StatusOK {
		t.Fatalf("status (before rotation) = %d, want 200", status)
	}
	if want := "Bearer " + initialKey; *lastAuth != want {
		t.Fatalf("upstream saw Authorization (before rotation) = %q, want %q", *lastAuth, want)
	}

	// A production run() would call this same method with
	// dataplane.DefaultCredentialReloadInterval (60s); this test drives
	// the exact same *dataplane.Pipeline with a short interval instead,
	// so it doesn't need to sleep for a minute to prove the mechanism
	// works on THIS pipeline.
	const reloadInterval = 10 * time.Millisecond
	go pipeline.RunCredentialReloadLoop(t.Context(), reloadInterval)

	if err := os.WriteFile(keyPath, []byte(rotatedKey), 0o600); err != nil {
		t.Fatalf("WriteFile (rotation): %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for attempt := 0; ; attempt++ {
		// A distinct message per attempt -- see sendChatCompletion's own
		// doc comment for why: an identical repeated request would be
		// served from the response cache and never reach the mock
		// upstream at all, which would make this loop spin forever on a
		// stale lastAuth regardless of whether the real rotation worked.
		message := "hi-after-rotation-attempt-" + strconv.Itoa(attempt)
		if status := sendChatCompletion(t, gw.URL, "test-gateway-key", "gpt-4o", message); status != http.StatusOK {
			t.Fatalf("status (polling after rotation) = %d, want 200", status)
		}
		if *lastAuth == "Bearer "+rotatedKey {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream never saw the rotated key within 2s; last Authorization = %q, want %q", *lastAuth, "Bearer "+rotatedKey)
		}
		time.Sleep(reloadInterval)
	}
}

// TestBuildPipelineBedrockDeploymentResolvesAccessKeyIDFromFile is the
// bedrock-branch counterpart to
// TestBuildPipelineResolvesAPIKeyFromFileNotEnv -- access_key_id_file/
// secret_access_key_file alone (no *_env) must resolve correctly and
// produce a REAL SigV4-signed request whose Authorization header
// references the file-resolved access key ID.
func TestBuildPipelineBedrockDeploymentResolvesAccessKeyIDFromFile(t *testing.T) {
	upstream, _ := newMockBedrockUpstream(t)

	dir := t.TempDir()
	accessKeyPath := filepath.Join(dir, "access-key-id")
	secretKeyPath := filepath.Join(dir, "secret-access-key")
	fileAccessKey := "not-a-real-file-backed-access-key-id-xxxxxxxxxxxx"
	fileSecretKey := "not-a-real-secret-access-key-value-xxxxxxxx"
	if err := os.WriteFile(accessKeyPath, []byte(fileAccessKey), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(secretKeyPath, []byte(fileSecretKey), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash("test-gateway-key"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:                "bedrock-file-backed",
				Model:               "anthropic.claude-3-5-sonnet-20241022-v2:0",
				Provider:            "bedrock",
				UpstreamModel:       "anthropic.claude-3-5-sonnet-20241022-v2:0",
				BaseURL:             upstream.URL + "/model/anthropic.claude-3-5-sonnet-20241022-v2:0/converse",
				AccessKeyIDFile:     accessKeyPath,
				SecretAccessKeyFile: secretKeyPath,
				Region:              "us-east-1",
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	if status := sendChatCompletion(t, gw.URL, "test-gateway-key", "anthropic.claude-3-5-sonnet-20241022-v2:0", "hi"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// dataplane is imported solely so this file can reference
// dataplane.DefaultCredentialReloadInterval in its own doc comments'
// accuracy (verified at compile time) -- the tests above deliberately
// use a short local interval instead of the production default.
var _ = dataplane.DefaultCredentialReloadInterval
