package main

import (
	"context"
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
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"drained successfully"`)}, FinishReason: "stop"},
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

// TestIntegrationGracefulShutdownRejectsNewRequestAfterSIGTERM proves
// the other real gap this package's suite was missing: run()'s own
// http.Server.Shutdown call closes the listener as soon as shutdown
// begins (per net/http's own documented Shutdown behavior: "Shutdown
// works by first closing all open listeners..."), so a genuinely NEW
// connection attempt shortly after SIGTERM is delivered must be
// refused, never served and never left hanging open indefinitely --
// distinct from TestIntegrationGracefulShutdownDrainsInFlightRequestBeforeExiting
// above, which proves an ALREADY-in-flight request is still allowed to
// finish.
func TestIntegrationGracefulShutdownRejectsNewRequestAfterSIGTERM(t *testing.T) {
	const listenAddr = "127.0.0.1:18763"

	requestReceived := make(chan struct{})
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-releaseUpstream
	}))
	defer upstream.Close()

	const upstreamKeyEnvVar = "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GRACEFUL_NEW_REQUEST"
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	configPath := writeGracefulShutdownTestConfig(t, listenAddr, upstream.URL, upstreamKeyEnvVar)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() {
		runErr <- run(configPath, logger)
	}()

	waitForListener(t, listenAddr)

	// Get one request genuinely in flight first, purely so run() has a
	// real reason to still be inside its own drain/shutdown sequence
	// when this test sends its second, NEW request below (an idle
	// server with zero in-flight requests would otherwise complete
	// Shutdown near-instantly, making the timing below unreliable).
	firstReqBody := strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	firstReq, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/v1/chat/completions", firstReqBody)
	if err != nil {
		t.Fatalf("NewRequest (first): %v", err)
	}
	firstReq.Header.Set("Authorization", "Bearer "+gracefulShutdownTestSecret())
	firstReq.Header.Set("Content-Type", "application/json")
	go func() { _, _ = http.DefaultClient.Do(firstReq) }() //nolint:bodyclose -- fire-and-forget; this test never inspects its response.

	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("mock upstream never received the first request")
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// A brief, deliberate wait for Shutdown to have genuinely begun
	// (and, per its own documented behavior, already closed the
	// listener) before attempting a brand-new connection -- mirroring
	// the existing successful-drain test's identical "give Shutdown a
	// moment" pattern, just used here to prove the OPPOSITE case.
	time.Sleep(200 * time.Millisecond)

	secondReqBody := strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	secondReq, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/v1/chat/completions", secondReqBody)
	if err != nil {
		t.Fatalf("NewRequest (second): %v", err)
	}
	secondReq.Header.Set("Authorization", "Bearer "+gracefulShutdownTestSecret())
	secondReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(secondReq)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a brand-new request after SIGTERM succeeded with status %d, want a connection error — the listener should already be closed", resp.StatusCode)
	}

	close(releaseUpstream) // let the first, held-open request finish so run() can exit cleanly.
	select {
	case <-runErr:
	case <-time.After(5 * time.Second):
		t.Fatal("run() never returned after graceful shutdown")
	}
}

// TestIntegrationGracefulShutdownForceExitsWhenRequestOutlivesDrainGrace
// is the regression proof this package's own suite was missing: the
// force-exit path (a request that outlives BOTH gracefulShutdownTimeout
// AND postShutdownDrainGrace) was previously only unit-tested on the
// bare drainInFlight helper in isolation, never exercised through run()'s
// real end-to-end SIGTERM handling the way
// TestIntegrationGracefulShutdownDrainsInFlightRequestBeforeExiting above
// proves the successful-drain path. Overrides both timing vars to a
// short duration for this one test only (restored via t.Cleanup) so it
// runs in well under a second rather than the real 45s worst case.
func TestIntegrationGracefulShutdownForceExitsWhenRequestOutlivesDrainGrace(t *testing.T) {
	origTimeout, origGrace := gracefulShutdownTimeout, postShutdownDrainGrace
	gracefulShutdownTimeout = 100 * time.Millisecond
	postShutdownDrainGrace = 100 * time.Millisecond
	t.Cleanup(func() {
		gracefulShutdownTimeout, postShutdownDrainGrace = origTimeout, origGrace
	})

	const listenAddr = "127.0.0.1:18762"

	requestReceived := make(chan struct{})
	// Deliberately never released — the mock upstream call blocks
	// forever, holding the in-flight request open past both overridden
	// windows above.
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-releaseUpstream
	}))
	defer upstream.Close()

	const upstreamKeyEnvVar = "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_GRACEFUL_FORCE_EXIT"
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	configPath := writeGracefulShutdownTestConfig(t, listenAddr, upstream.URL, upstreamKeyEnvVar)

	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
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

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// run() must return a real, non-nil "graceful shutdown" error --
	// Shutdown's own gracefulShutdownTimeout wait gives up on the
	// never-finishing in-flight request.
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("run() returned nil error, want a real graceful-shutdown error for a request that outlives the drain grace")
		}
		if !strings.Contains(err.Error(), "graceful shutdown") {
			t.Errorf("run() error = %v, want it to mention \"graceful shutdown\"", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() never returned — the force-exit path did not bound the wait as expected")
	}

	// The client's in-flight request must never receive a completed
	// response -- the connection is dropped, not gracefully finished,
	// since the handler goroutine was still blocked on the upstream call
	// when the process gave up waiting on it.
	select {
	case result := <-respCh:
		if result.err == nil {
			_ = result.resp.Body.Close()
			t.Fatal("in-flight request completed successfully, want it to fail/hang since the upstream call never released")
		}
	case <-time.After(1 * time.Second):
		// Also acceptable: the client is still blocked because the
		// underlying TCP connection was never actually torn down by the
		// (still-running) handler goroutine itself -- what matters is
		// that no successful response body was ever delivered, which the
		// respCh-received branch above already checks when it fires.
	}

	if !strings.Contains(logBuf.String(), "gateway_shutdown_forced_with_requests_still_in_flight") {
		t.Errorf("log output does not contain the forced-shutdown warning line; full output:\n%s", logBuf.String())
	}

	close(releaseUpstream) // let the leaked mock-upstream handler goroutine exit, avoiding a test-process leak.
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

// TestShutdownServersConcurrentlyClosesBothListenersImmediately is the
// direct regression proof for a real bug an audit found: shutdownBoth
// previously called server.Shutdown(shutdownCtx) and
// adminServer.Shutdown(shutdownCtx) SEQUENTIALLY. http.Server.Shutdown's
// own first action, before it ever polls for idle connections, is
// closing its own listener(s) — so with sequential calls, the SECOND
// server's listener stayed open and kept ACCEPTING BRAND-NEW connections
// for the entire time the FIRST server's Shutdown call was still
// draining (up to the full gracefulShutdownTimeout), even though the
// process was already mid-shutdown. Proves the fix directly against
// shutdownServersConcurrently, using two real *http.Server instances:
// server A's one in-flight request blocks (deliberately) for the whole
// test, so its own Shutdown call never returns on its own — the exact
// "slow client-facing drain" shape the audit's own scenario named.
// Server B (standing in for the admin server) has no in-flight request
// at all; this test only checks whether B's LISTENER itself closes
// promptly, independent of any request activity.
func TestShutdownServersConcurrentlyClosesBothListenersImmediately(t *testing.T) {
	aReleased := make(chan struct{})
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (A): %v", err)
	}
	srvA := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-aReleased
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srvA.Serve(lnA) }()

	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (B): %v", err)
	}
	srvB := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srvB.Serve(lnB) }()

	// A's one in-flight request, held open by aReleased above, for the
	// whole duration of this test.
	aReqDone := make(chan struct{})
	go func() {
		defer close(aReqDone)
		resp, reqErr := http.Get("http://" + lnA.Addr().String() + "/")
		if reqErr == nil {
			_ = resp.Body.Close()
		}
	}()

	time.Sleep(20 * time.Millisecond) // let A's request genuinely reach its handler first.

	const shutdownTimeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- shutdownServersConcurrently(ctx,
			namedServer{name: "A", srv: srvA},
			namedServer{name: "B", srv: srvB},
		)
	}()

	// The load-bearing assertion: B's listener must reject a brand-new
	// connection attempt SHORTLY after shutdown starts, even though A's
	// own Shutdown call is still blocked and won't return for the
	// remainder of shutdownTimeout. With the pre-fix sequential code,
	// adminServer.Shutdown (B here) was never even CALLED yet at this
	// point — its own listener, and every route behind it, would still
	// be accepting new connections.
	time.Sleep(100 * time.Millisecond)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	_, connErr := client.Get("http://" + lnB.Addr().String() + "/")
	if connErr == nil {
		t.Error("a brand-new connection to B succeeded 100ms after shutdown started, want it rejected — B's listener should already be closed, concurrently with A's still-draining Shutdown call")
	}

	close(aReleased)
	<-aReqDone
	select {
	case err := <-shutdownDone:
		// The load-bearing assertion an audit found missing: both
		// servers drained cleanly well within shutdownTimeout (A's
		// request was released immediately above), so the returned
		// error must be nil — a regression that made
		// shutdownServersConcurrently spuriously return a non-nil
		// error on an otherwise-clean concurrent shutdown would
		// previously have gone undetected by this test, since it only
		// checked for receipt before a timeout, never the value itself.
		if err != nil {
			t.Errorf("shutdownServersConcurrently returned %v, want nil for a clean shutdown of both servers", err)
		}
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("shutdownServersConcurrently never returned")
	}
}

// TestShutdownServersConcurrentlyAttributesEachTimeoutToItsOwnServer is
// the regression proof for a second real gap the same audit found:
// net/http's own Server.Shutdown returns only the bare context error
// (context.DeadlineExceeded, a fixed, unattributed value) with no
// listener identity baked in — a plain errors.Join over both servers'
// raw errors used to read as two textually IDENTICAL "context deadline
// exceeded" lines, giving an operator no way to tell which server(s)
// actually failed to drain. Both servers here hold a request open past
// a short shutdown deadline, so both Shutdown calls genuinely time out —
// the returned error must name BOTH servers, distinguishably.
func TestShutdownServersConcurrentlyAttributesEachTimeoutToItsOwnServer(t *testing.T) {
	blockForever := make(chan struct{})
	defer close(blockForever)

	newBlockingServer := func(t *testing.T) (*http.Server, net.Listener) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("net.Listen: %v", err)
		}
		srv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				<-blockForever
			}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() { _ = srv.Serve(ln) }()
		return srv, ln
	}

	srvA, lnA := newBlockingServer(t)
	srvB, lnB := newBlockingServer(t)

	for _, addr := range []string{lnA.Addr().String(), lnB.Addr().String()} {
		go func(addr string) {
			resp, err := http.Get("http://" + addr + "/")
			if err == nil {
				_ = resp.Body.Close()
			}
		}(addr)
	}
	time.Sleep(20 * time.Millisecond) // let both requests genuinely reach their handlers first.

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := shutdownServersConcurrently(ctx,
		namedServer{name: "server-A", srv: srvA},
		namedServer{name: "server-B", srv: srvB},
	)
	if err == nil {
		t.Fatal("shutdownServersConcurrently returned nil, want a timeout error from both servers")
	}
	if !strings.Contains(err.Error(), "server-A") {
		t.Errorf("error %q does not name server-A", err.Error())
	}
	if !strings.Contains(err.Error(), "server-B") {
		t.Errorf("error %q does not name server-B", err.Error())
	}
}
