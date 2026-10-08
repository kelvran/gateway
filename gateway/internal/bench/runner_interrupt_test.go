package bench

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/benchupstream"
)

// An interrupted run (SIGINT → context cancelled) cuts off its in-flight
// requests itself; those are the harness's doing, not the gateway's, and
// must not surface as errors in the partial result.
func TestInterruptedRunKeepsHarnessCancelledRequestsOutOfTheErrorCounts(t *testing.T) {
	srv := httptest.NewServer(benchupstream.New(benchupstream.Config{Latency: 2 * time.Second}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	res, err := Run(ctx, Config{Scenario: "int", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 30, Duration: 2 * time.Second, Seed: 11})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if res.Cancelled == 0 {
		t.Fatalf("cancelled = 0, want the in-flight requests the cancellation cut off")
	}
	if res.Errors != 0 || res.TransportErrors != 0 || res.Requests != 0 {
		t.Errorf("requests/errors/transport = %d/%d/%d, want 0/0/0: nothing completed and nothing failed on the gateway's account", res.Requests, res.Errors, res.TransportErrors)
	}
	if res.ScheduleLateness.Count != 0 {
		t.Errorf("lateness samples = %d, want none for cancelled requests", res.ScheduleLateness.Count)
	}
}
