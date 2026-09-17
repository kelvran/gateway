package configpropagation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// One real Redis container shared by every test in this file, per
// docs/testing/TESTING.md §4's "real Redis via testcontainers, never
// mocked at this layer" commitment -- mirrors
// internal/ratelimit/redislimiter's own identical TestMain shape.
var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("configpropagation: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("configpropagation: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	m.Run()
}

var testCounter atomic.Uint64

// waitForEvent polls got (a func returning the most recently received
// event, or nil) until it becomes non-nil or timeout elapses -- pub/sub
// delivery is asynchronous, so a test must not assert immediately after
// Publish returns.
func waitForEvent(t *testing.T, got func() *MutationEvent, timeout time.Duration) *MutationEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e := got(); e != nil {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

// TestPublishDeploymentWeightEventOverRealRedis proves a published
// MutationEvent is genuinely delivered, over a real Redis connection, to
// a real Subscribe call -- not just that Publish/Subscribe each succeed
// in isolation.
func TestPublishDeploymentWeightEventOverRealRedis(t *testing.T) {
	pub := Open(redisAddr)
	defer func() { _ = pub.Close() }()
	sub := Open(redisAddr)
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var received atomic.Pointer[MutationEvent]
	go func() { _ = sub.Subscribe(ctx, func(e MutationEvent) { received.Store(&e) }) }()

	// Give the subscription a moment to actually register with Redis
	// before publishing -- go-redis's Subscribe call returns once the
	// SUBSCRIBE command is sent, but there's no synchronous "subscription
	// confirmed" signal this test can wait on more precisely.
	time.Sleep(100 * time.Millisecond)

	payload, err := json.Marshal(DeploymentWeightPayload{Model: "gpt-4o", DeploymentName: "d1", Weight: 5})
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	want := MutationEvent{Type: TypeDeploymentWeight, OriginInstanceID: fmt.Sprintf("test-instance-%d", testCounter.Add(1)), Payload: payload}
	if err := pub.Publish(ctx, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := waitForEvent(t, received.Load, 5*time.Second)
	if got == nil {
		t.Fatal("subscriber never received the published event within 5s")
	}
	if got.Type != want.Type || got.OriginInstanceID != want.OriginInstanceID {
		t.Errorf("received = %+v, want Type=%q OriginInstanceID=%q", got, want.Type, want.OriginInstanceID)
	}
	var gotPayload DeploymentWeightPayload
	if err := json.Unmarshal(got.Payload, &gotPayload); err != nil {
		t.Fatalf("unmarshaling received payload: %v", err)
	}
	if gotPayload.Model != "gpt-4o" || gotPayload.DeploymentName != "d1" || gotPayload.Weight != 5 {
		t.Errorf("received payload = %+v, want {gpt-4o d1 5}", gotPayload)
	}
}

// TestSubscribeReturnsWhenContextCanceled proves Subscribe's own
// blocking loop actually exits (rather than leaking a goroutine forever)
// once its ctx is canceled -- the same shutdown contract
// RunHealthProbeLoop's own goroutine relies on.
func TestSubscribeReturnsWhenContextCanceled(t *testing.T) {
	sub := Open(redisAddr)
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(ctx, func(MutationEvent) {}) }()

	time.Sleep(50 * time.Millisecond) // let Subscribe actually start.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("Subscribe returned nil error on context cancellation, want context.Canceled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return within 5s of its context being canceled")
	}
}

// TestOpenNeverFailsOnUnreachableAddr mirrors
// internal/ratelimit/redislimiter's own identical proof: go-redis dials
// lazily, so Open itself must never fail just because addr is currently
// unreachable -- this property is what the fail-open design in
// dataplane.Pipeline.UpdateDeploymentWeight/cmd/gateway's subscriber
// wiring depends on.
func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	// Open has no error return at all (unlike redislimiter.Open, which
	// keeps one only for interface-shape symmetry) -- this test proves
	// the stronger claim directly: it doesn't even panic or block.
	pub := Open("127.0.0.1:1")
	defer func() { _ = pub.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := pub.Publish(ctx, MutationEvent{Type: TypeDeploymentWeight})
	if err == nil {
		t.Fatal("Publish against an unreachable address returned nil error, want a real connection error")
	}
}
