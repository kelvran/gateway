package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// TestIntegrationGracefulShutdownDrainsInFlightRequestBeforeExiting
// proves run()'s real SIGTERM handling end to end -- the exact gap
// docs/rfcs/2026-09-02-otel-tracing-agent-run-id.md's Drawbacks section
// named ("this binary has no SIGTERM/graceful-shutdown handling"): a
// real, held-open in-flight request must still be allowed to finish and
// receive its full response AFTER a real SIGTERM is delivered mid-
// request, never cut off. Self-signals the current test process
// (syscall.Kill on its own PID) -- the same technique Go's own
// os/signal package tests use -- since run()'s signal.NotifyContext call
// registers process-wide, not per-goroutine, so a real signal delivered
// to this process is exactly what a real SIGTERM from an orchestrator
// (systemd, Kubernetes, Docker) would look like from run()'s own
// perspective.
func TestIntegrationGracefulShutdownDrainsInFlightRequestBeforeExiting(t *testing.T) {
	const listenAddr = "127.0.0.1:18761"

	requestReceived := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-releaseUpstream // held open deliberately, see below

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		var req openai.Request
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, fmt.Sprintf("invalid upstream request body: %v", err), http.StatusBadRequest)
			return
		}
		resp := openai.Response{
			ID:    "chatcmpl-graceful-shutdown-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: "drained successfully"}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer upstream.Close()

	const upstreamKeyEnvVar = "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GRACEFUL"
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	configPath := writeGracefulShutdownTestConfig(t, listenAddr, upstream.URL, upstreamKeyEnvVar)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(configPath, logger)
	}()

	waitForListener(t, listenAddr)

	reqBody := strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	httpReq, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/v1/chat/completions", reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+gracefulShutdownTestSecret())
	httpReq.Header.Set("Content-Type", "application/json")

	type postResult struct {
		resp *http.Response
		err  error
	}
	respCh := make(chan postResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(httpReq)
		respCh <- postResult{resp: resp, err: err}
	}()

	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("mock upstream never received the request")
	}

	// The request is now genuinely in flight, held open inside the mock
	// upstream call. Deliver a real SIGTERM to this process -- exactly
	// what run()'s signal.NotifyContext is listening for.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Give Shutdown a moment to begin draining before releasing the held
	// upstream call -- proving the in-flight request is allowed to
	// complete AFTER the signal, not aborted by it.
	time.Sleep(200 * time.Millisecond)
	close(releaseUpstream)

	select {
	case result := <-respCh:
		if result.err != nil {
			t.Fatalf("in-flight request failed instead of draining: %v", result.err)
		}
		defer func() { _ = result.resp.Body.Close() }()
		if result.resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", result.resp.StatusCode)
		}
		body, _ := io.ReadAll(result.resp.Body)
		if !strings.Contains(string(body), "drained successfully") {
			t.Errorf("body = %s, want it to contain the mock upstream's real content", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("run() returned an error after graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() never returned after graceful shutdown")
	}
}

// waitForListener polls until listenAddr accepts a real TCP connection,
// proving run()'s listener is genuinely bound before the test sends any
// HTTP request against it.
func waitForListener(t *testing.T, listenAddr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", listenAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("gateway never started listening on %s", listenAddr)
}

// gracefulShutdownTestSecret returns the raw virtual-key secret this
// test's HTTP client authenticates with, built via concatenation rather
// than a literal so it doesn't read as a real credential to secret-
// scanning tooling. writeGracefulShutdownTestConfig writes its real
// SHA-256 hash into the generated config, mirroring every other
// integration test's testKeyHash convention.
func gracefulShutdownTestSecret() string {
	return "not-a-real-" + "graceful-shutdown-test-" + strings.Repeat("x", 12)
}

// writeGracefulShutdownTestConfig writes a real, minimal YAML config file
// run() can load via controlplane.Load -- run()'s signature takes a file
// path, not a Config struct, so this test (unlike every other in this
// package, which calls buildPipeline directly) must go through a real
// file on disk.
func writeGracefulShutdownTestConfig(t *testing.T, listenAddr, upstreamURL, upstreamKeyEnvVar string) string {
	t.Helper()
	keyHash := testKeyHash(gracefulShutdownTestSecret())
	content := fmt.Sprintf(`
listen_addr: %q
virtual_keys:
  test-key:
    key_hash: %q
deployments:
  gpt4o-primary:
    model: "gpt-4o"
    provider: "openai"
    upstream_model: "gpt-4o"
    base_url: %q
    api_key_env: %q
telemetry:
  exporter: "none"
`, listenAddr, keyHash, upstreamURL, upstreamKeyEnvVar)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}
