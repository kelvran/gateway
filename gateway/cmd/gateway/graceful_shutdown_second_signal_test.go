package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/adapter/openai"
)

// TestSecondSIGTERMForceKillsChildProcessDuringGracefulDrain proves the
// real risk this test file exists to close: run()'s signal.NotifyContext
// call (main.go) registers SIGTERM, and its ctx.Done() branch calls stop()
// SYNCHRONOUSLY on the very first signal — un-registering the handler and
// restoring Go's default (terminate) disposition for anything received
// after, by design (see that call site's own doc comment). No existing
// test proves this: the 3 tests in graceful_shutdown_integration_test.go
// all run() IN-PROCESS and each send exactly one real SIGTERM to the TEST
// BINARY's own PID (os.Getpid()) — sending a SECOND real SIGTERM the same
// way would kill the whole `go test` process, not cleanly fail one
// assertion. This test instead builds and runs the REAL gateway binary as
// a genuine child process, signaling its PID, never the test binary's own.
func TestSecondSIGTERMForceKillsChildProcessDuringGracefulDrain(t *testing.T) {
	binaryPath := buildGatewayBinaryForTest(t)

	const listenAddr = "127.0.0.1:18762"

	requestReceived := make(chan struct{})
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestReceived)
		<-releaseUpstream // held open deliberately, mirroring the in-process test's own convention.

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
			ID:    "chatcmpl-second-sigterm-test",
			Model: req.Model,
			Choices: []openai.Choice{
				{Index: 0, Message: openai.Message{Role: "assistant", Content: json.RawMessage(`"should never be seen"`)}, FinishReason: "stop"},
			},
			Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	// Both registered via t.Cleanup (never a plain defer) and in this
	// exact order deliberately: t.Cleanup callbacks run LIFO, and a plain
	// defer upstream.Close() would otherwise run BEFORE any t.Cleanup
	// callback fires (defers unwind as part of the test function's own
	// return/Goexit, strictly before t.Cleanup's post-test phase) --
	// hanging forever on a t.Fatal from this point on, since
	// upstream.Close() waits for the still-held-open connection this
	// test deliberately never released yet. Registering the release
	// SECOND makes it run FIRST at cleanup time, unblocking the handler
	// before Close() ever waits on it.
	t.Cleanup(func() { upstream.Close() })
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseUpstream) }) })

	const upstreamKeyEnvVar = "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_SECOND_SIGTERM"
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")

	configPath := writeGracefulShutdownTestConfig(t, listenAddr, upstream.URL, upstreamKeyEnvVar)

	cmd := exec.Command(binaryPath, "-config", configPath)
	// exec.Command inherits the current process's environment by
	// default (cmd.Env is left nil) -- t.Setenv above already reached
	// the real OS environment, so upstreamKeyEnvVar is visible to the
	// child exactly as it would be to run() called in-process.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	logLines := newSyncLineBuffer()
	go logLines.consume(stdout)

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting gateway-under-test: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort: if the test's own assertions already forced the
		// child to exit, Kill/Wait are both harmless no-ops-with-error,
		// which this discards deliberately -- this is cleanup, not a new
		// assertion.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	waitForListener(t, listenAddr)

	reqBody := strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	httpReq, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/v1/chat/completions", reqBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+gracefulShutdownTestSecret())
	httpReq.Header.Set("Content-Type", "application/json")
	go func() {
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil {
			_ = resp.Body.Close()
		}
		// Deliberately no assertion on this response here: the whole
		// point of this test is that the child dies mid-request, so
		// whatever this client observes (a connection reset, an EOF, or
		// nothing if the test process itself is torn down first) is
		// expected, not a signal of pass/fail.
	}()

	select {
	case <-requestReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("mock upstream never received the request")
	}

	// First SIGTERM: real graceful shutdown must begin, but the process
	// must NOT exit yet -- the in-flight request above is still held
	// open, well within gracefulShutdownTimeout/postShutdownDrainGrace's
	// combined window.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending first SIGTERM: %v", err)
	}

	if !logLines.waitForSubstring(t, "gateway shutting down", 5*time.Second) {
		t.Fatalf("child process never logged \"gateway shutting down\" after the first SIGTERM; captured log lines: %v", logLines.snapshot())
	}

	// Signal 0 delivers nothing but reports (via the returned error)
	// whether the process still exists -- the standard Unix liveness
	// probe, used here (not cmd.Wait, which would consume the process's
	// exit and race with the second SIGTERM below) specifically to avoid
	// disturbing the process's own signal-delivery state.
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child process already exited after only ONE SIGTERM (want it still draining): %v", err)
	}

	// Second SIGTERM: per this file's own doc comment, stop() already
	// unregistered the handler on the first signal above -- Go's default
	// SIGTERM disposition (terminate) must now apply.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending second SIGTERM: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case err := <-waitErr:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Wait() error = %v (%T), want an *exec.ExitError reporting a signal-terminated exit", err, err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok {
			t.Fatalf("ExitError.Sys() = %T, want syscall.WaitStatus", exitErr.Sys())
		}
		if !status.Signaled() {
			t.Errorf("child process exited cleanly (code %d) rather than being terminated by a signal -- the second SIGTERM should have force-killed it via Go's default disposition, not let it run to a normal exit", status.ExitStatus())
		} else if sig := status.Signal(); sig != syscall.SIGTERM {
			t.Errorf("child process was terminated by signal %v, want SIGTERM", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not exit promptly after a second SIGTERM -- the graceful-drain path appears to have absorbed it instead of force-killing")
	}

	releaseOnce.Do(func() { close(releaseUpstream) })
}

// buildGatewayBinaryForTest compiles the real gateway binary (this
// package, ".") once per test into a temp directory, returning its path.
// Deliberately a plain (non-race-instrumented) build: this test exercises
// OS-signal timing, not data-race detection, and a race build's much
// heavier instrumentation would only slow down an already real
// process-spawn-and-wait test for no relevant benefit.
func buildGatewayBinaryForTest(t *testing.T) string {
	t.Helper()
	outputPath := filepath.Join(t.TempDir(), "gateway-under-test")
	cmd := exec.Command("go", "build", "-o", outputPath, ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building gateway binary for test: %v\n%s", err, output)
	}
	return outputPath
}

// syncLineBuffer captures a child process's stdout line by line, safe for
// concurrent read (waitForSubstring/snapshot) while a background goroutine
// is still appending -- os/exec's own docs require StdoutPipe's reader be
// fully drained promptly, and a plain bytes.Buffer written from one
// goroutine and read from another is a data race without this lock.
type syncLineBuffer struct {
	mu    sync.Mutex
	lines []string
}

func newSyncLineBuffer() *syncLineBuffer {
	return &syncLineBuffer{}
}

func (b *syncLineBuffer) consume(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		b.mu.Lock()
		b.lines = append(b.lines, scanner.Text())
		b.mu.Unlock()
	}
}

func (b *syncLineBuffer) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

func (b *syncLineBuffer) waitForSubstring(t *testing.T, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, line := range b.snapshot() {
			if strings.Contains(line, substr) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// TestSecondSIGTERMHelperBinaryProvesTheUnderlyingStdlibMechanism is a
// cheap, fast companion to
// TestSecondSIGTERMForceKillsChildProcessDuringGracefulDrain: it proves
// the same underlying signal.NotifyContext/stop() mechanism main.go
// relies on, via a tiny throwaway helper program (not the full gateway
// binary) that is far cheaper to build -- useful as a fast regression
// guard on the raw mechanism if the heavier, real-binary test above is
// ever judged too slow/flaky for a given CI run and skipped. Still a real
// subprocess, never the test binary's own PID.
func TestSecondSIGTERMHelperBinaryProvesTheUnderlyingStdlibMechanism(t *testing.T) {
	helperDir := t.TempDir()
	helperSrc := filepath.Join(helperDir, "main.go")
	const helperProgram = `package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	fmt.Println("ready")
	<-ctx.Done()
	stop()
	fmt.Println("first-signal-handled")
	select {}
}
`
	if err := os.WriteFile(helperSrc, []byte(helperProgram), 0o600); err != nil {
		t.Fatalf("writing helper source: %v", err)
	}
	helperBin := filepath.Join(helperDir, "sigtermhelper")
	buildCmd := exec.Command("go", "build", "-o", helperBin, helperSrc)
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("building helper binary: %v\n%s", err, output)
	}

	cmd := exec.Command(helperBin)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	logLines := newSyncLineBuffer()
	go logLines.consume(stdout)

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !logLines.waitForSubstring(t, "ready", 5*time.Second) {
		t.Fatalf("helper never printed \"ready\"; captured: %v", logLines.snapshot())
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending first SIGTERM: %v", err)
	}
	if !logLines.waitForSubstring(t, "first-signal-handled", 5*time.Second) {
		t.Fatalf("helper never printed \"first-signal-handled\" after the first SIGTERM; captured: %v", logLines.snapshot())
	}

	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("helper already exited after only ONE SIGTERM: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sending second SIGTERM: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("Wait() error = %v (%T), want an *exec.ExitError", err, err)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Errorf("helper did not terminate via a second SIGTERM as expected: status=%+v", exitErr.Sys())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit promptly after a second SIGTERM")
	}
}
