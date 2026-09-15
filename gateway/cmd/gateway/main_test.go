package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestTrackInFlightIncrementsAndDecrementsAroundHandler proves the
// WaitGroup genuinely brackets the wrapped handler's execution -- non-zero
// while it's running, zero once it returns -- the exact property
// drainInFlight relies on to know when it's safe to stop waiting.
func TestTrackInFlightIncrementsAndDecrementsAroundHandler(t *testing.T) {
	var wg sync.WaitGroup
	release := make(chan struct{})
	inHandler := make(chan struct{})
	handler := trackInFlight(&wg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		<-release
	}))

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()

	select {
	case <-inHandler:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Mid-request: Wait must block, proving the count is genuinely non-zero.
	waitReturned := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitReturned)
	}()
	select {
	case <-waitReturned:
		t.Fatal("wg.Wait() returned while the handler was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never returned")
	}

	select {
	case <-waitReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("wg.Wait() never returned after the handler completed")
	}
}

// TestDrainInFlightWaitsForRequestsThatFinishWithinGrace proves
// drainInFlight does not force-exit early: a handler that finishes well
// inside its grace period must be allowed to do so before drainInFlight
// returns.
func TestDrainInFlightWaitsForRequestsThatFinishWithinGrace(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	finished := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		wg.Done()
		close(finished)
	}()

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	start := time.Now()
	drainInFlight(&wg, 5*time.Second, logger)
	elapsed := time.Since(start)

	select {
	case <-finished:
	default:
		t.Fatal("drainInFlight returned before the tracked work actually finished")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("drainInFlight took %v, want it to return promptly once the tracked work finished, not wait out the full grace", elapsed)
	}
}

// TestDrainInFlightGivesUpAfterGraceAndLogsAWarning proves drainInFlight
// never blocks forever: work that outlives its grace period must still
// cause a prompt return (plus a warning log), not an indefinite hang.
func TestDrainInFlightGivesUpAfterGraceAndLogsAWarning(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done() // let the tracked goroutine's Wait() eventually unblock so the test itself doesn't leak forever

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	start := time.Now()
	drainInFlight(&wg, 100*time.Millisecond, logger)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("drainInFlight took %v, want it to give up promptly after its grace period elapsed, not block indefinitely", elapsed)
	}
	if !bytes.Contains(logBuf.Bytes(), []byte("gateway_shutdown_forced_with_requests_still_in_flight")) {
		t.Errorf("log output = %q, want it to contain the forced-shutdown warning", logBuf.String())
	}
}
