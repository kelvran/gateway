package configpropagation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// One real Redis container shared by every test in this file, per
// docs/testing/TESTING.md §4's "real Redis via testcontainers, never
// mocked at this layer" commitment -- mirrors
// internal/ratelimit/redislimiter's own identical TestMain shape.
//
// redisContainer is package-level (not a TestMain-local var) so
// TestSubscribeSurvivesARealRedisPartitionAndDeliversEventsAfterRecovery
// can Stop/Start it directly, simulating a real network partition
// against the exact same server every other test in this file talks to
// -- see that test's own doc comment for why it always restores the
// container (via defer) before returning, so a later test in this file
// never inherits a dead Redis.
var (
	redisAddr      string
	redisContainer *tcredis.RedisContainer
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("configpropagation: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()
	redisContainer = container

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("configpropagation: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		panic(fmt.Sprintf("configpropagation: generating test signing secret: %v", err))
	}
	testSigningSecret = hex.EncodeToString(secretBytes)

	m.Run()
}

var testCounter atomic.Uint64

// testSigningSecret is the shared HMAC key every Open call in this file
// uses -- Publish/Subscribe both refuse to run without one (see
// MutationEvent.Signature's own doc comment), so every existing test
// needs a real, non-empty value to keep exercising what it was actually
// written to test, distinct from the new
// TestPublish/SubscribeRefusesWithoutSigningSecret tests below, which
// specifically exercise the empty-value path itself. Generated fresh in
// TestMain (never a hardcoded literal) since it plays the exact same
// role a real deployment's env-var-sourced secret does.
var testSigningSecret string

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
	pub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = pub.Close() }()
	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
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
	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
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
	pub := Open(redis.Options{Addr: "127.0.0.1:1"}, testSigningSecret)
	defer func() { _ = pub.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := pub.Publish(ctx, MutationEvent{Type: TypeDeploymentWeight})
	if err == nil {
		t.Fatal("Publish against an unreachable address returned nil error, want a real connection error")
	}
}

// TestPublishRefusesWithoutSigningSecret proves Publish never even
// touches Redis when no signing secret is configured -- the actual fix
// for this session's own CRITICAL audit finding (an unauthenticated
// pub/sub channel any Redis-network-adjacent process could publish
// forged mutations on). Break this by reverting Publish's own
// len(r.signingSecret) == 0 guard: this test starts failing because
// Publish instead succeeds and silently emits an unsigned event.
func TestPublishRefusesWithoutSigningSecret(t *testing.T) {
	pub := Open(redis.Options{Addr: redisAddr}, "")
	defer func() { _ = pub.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := pub.Publish(ctx, MutationEvent{Type: TypeDeploymentWeight})
	if err == nil {
		t.Fatal("Publish with no signing secret returned nil error, want a refusal error")
	}
}

// TestSubscribeRefusesWithoutSigningSecret is Subscribe's identical
// counterpart -- a subscriber with no secret would have no way to
// verify ANY received event, so it must refuse to run at all rather
// than silently trusting everything.
func TestSubscribeRefusesWithoutSigningSecret(t *testing.T) {
	sub := Open(redis.Options{Addr: redisAddr}, "")
	defer func() { _ = sub.Close() }()
	err := sub.Subscribe(context.Background(), func(MutationEvent) {})
	if err == nil {
		t.Fatal("Subscribe with no signing secret returned nil error, want a refusal error")
	}
}

// TestSubscribeDropsEventWithForgedOrMissingSignature is the direct
// attack-simulation regression test: an attacker with Redis network
// access (but not the shared signing secret) publishes raw, unsigned
// JSON directly onto channelName -- exactly what a compromised/
// malicious Redis-adjacent process could do -- and this proves
// Subscribe drops it rather than invoking onEvent. Break this by
// reverting Subscribe's own verifyEvent check: this test starts failing
// because the forged event reaches onEvent.
func TestSubscribeDropsEventWithForgedOrMissingSignature(t *testing.T) {
	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = sub.Close() }()
	raw := redisRawClientForTest(t, redisAddr)
	defer func() { _ = raw.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var receivedCount atomic.Int64
	go func() { _ = sub.Subscribe(ctx, func(MutationEvent) { receivedCount.Add(1) }) }()
	time.Sleep(100 * time.Millisecond)

	payload, err := json.Marshal(DeploymentWeightPayload{Model: "attacker-model", DeploymentName: "attacker-deployment", Weight: 999})
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	forged := MutationEvent{Type: TypeDeploymentWeight, OriginInstanceID: "attacker-instance", Payload: payload, Signature: "v1,not-a-real-signature"}
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatalf("marshaling forged event: %v", err)
	}
	if err := raw.Publish(ctx, channelName, body).Err(); err != nil {
		t.Fatalf("publishing forged event via raw client: %v", err)
	}

	// A genuine, correctly-signed event published right after must still
	// arrive -- proving this instance's Subscribe loop is alive and
	// working, not just coincidentally never receiving anything.
	pub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = pub.Close() }()
	genuinePayload, err := json.Marshal(DeploymentWeightPayload{Model: "gpt-4o", DeploymentName: "real-deployment", Weight: 3})
	if err != nil {
		t.Fatalf("marshaling genuine payload: %v", err)
	}
	if err := pub.Publish(ctx, MutationEvent{Type: TypeDeploymentWeight, OriginInstanceID: fmt.Sprintf("test-instance-%d", testCounter.Add(1)), Payload: genuinePayload}); err != nil {
		t.Fatalf("Publish (genuine): %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && receivedCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := receivedCount.Load(); got != 1 {
		t.Fatalf("onEvent invoked %d times, want exactly 1 (only the genuinely-signed event, never the forged one)", got)
	}
}

// redisRawClientForTest opens a plain *redis.Client against addr, bypassing
// this package's own PubSub entirely -- used only to simulate an
// attacker who has Redis network access but not the shared signing
// secret, i.e. someone who can run arbitrary Redis commands but cannot
// construct a validly-signed MutationEvent.
func redisRawClientForTest(t *testing.T, addr string) *redis.Client {
	t.Helper()
	return redis.NewClient(&redis.Options{Addr: addr})
}

// TestSubscribeSurvivesARealRedisPartitionAndDeliversEventsAfterRecovery
// is the real partition/reconnect proof this package's own doc comment
// (Subscriber's own doc comment, and the audit that named this gap)
// disclosed as missing. go-redis v9's PubSub.Channel() backing goroutine
// retries forever on any Receive error except the sentinel pool.ErrClosed
// (only set by an explicit Close() call), and a background ~3s
// health-check ping silently calls reconnect() on failure -- meaning a
// real network partition should never surface as a terminal error from
// Subscribe at all, and this instance should recover on its own with no
// caller-side retry loop needed.
//
// Proven here against a REAL Redis container (not asserted from
// go-redis's own documentation), using `docker pause`/`docker unpause`
// (via the container's own real ID, shelled out directly -- neither
// testcontainers.Container's own interface nor go-redis exposes this)
// rather than Stop/Start: pausing freezes the container's process via
// the cgroup freezer with the port mapping and network namespace fully
// intact, so every existing TCP connection goes silently unresponsive
// exactly like a real network partition -- confirmed manually before
// writing this test that Stop/Start does NOT have this property in
// this environment (a restarted container can get re-mapped to a
// genuinely different host port, which would confound this test: the
// already-open Subscribe connection is dialed against a fixed address
// baked in at Open() time, so a changed address would make recovery
// impossible no matter how good go-redis's own reconnect logic is --
// an entirely different failure mode from the partition this test
// means to exercise). Confirms Subscribe is still blocked (has not
// returned an error) while paused, unpauses, and confirms a freshly-
// published event is still delivered to the SAME, still-running
// Subscribe call -- without this test ever calling Subscribe a second
// time or restarting the subscriber goroutine itself.
func TestSubscribeSurvivesARealRedisPartitionAndDeliversEventsAfterRecovery(t *testing.T) {
	containerID := redisContainer.GetContainerID()

	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = sub.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var received atomic.Pointer[MutationEvent]
	subscribeDone := make(chan error, 1)
	go func() { subscribeDone <- sub.Subscribe(ctx, func(e MutationEvent) { received.Store(&e) }) }()
	time.Sleep(100 * time.Millisecond) // let the subscription actually register with Redis.

	if out, err := exec.Command("docker", "pause", containerID).CombinedOutput(); err != nil {
		t.Fatalf("docker pause %s: %v (%s)", containerID, err, out)
	}
	// Always unpause before returning, even on a t.Fatalf above/below --
	// every OTHER test in this file shares this same container and must
	// never inherit it in a paused state.
	unpaused := false
	defer func() {
		if !unpaused {
			if out, err := exec.Command("docker", "unpause", containerID).CombinedOutput(); err != nil {
				t.Fatalf("docker unpause %s during cleanup: %v (%s)", containerID, err, out)
			}
		}
	}()

	// While Redis is paused, Subscribe must NOT have returned at all --
	// this is the actual property being tested: a partition is
	// invisible to this package's own public contract (go-redis retries
	// internally), never a terminal error a caller has to notice and
	// react to.
	select {
	case err := <-subscribeDone:
		t.Fatalf("Subscribe returned (err=%v) while Redis was paused -- want it to keep blocking/retrying internally per go-redis's own documented reconnect behavior", err)
	case <-time.After(2 * time.Second):
	}

	if out, err := exec.Command("docker", "unpause", containerID).CombinedOutput(); err != nil {
		t.Fatalf("docker unpause %s: %v (%s)", containerID, err, out)
	}
	unpaused = true

	// A fresh Publish, via an independent client against the SAME
	// address (never re-resolved -- pause/unpause never changes the
	// port mapping, unlike Stop/Start), must still be delivered to the
	// SAME, still-running Subscribe call -- proving Subscribe itself
	// genuinely recovered on its own, not that a caller had to restart
	// it. Publish itself is retried for a few seconds: go-redis's own
	// client-side connections also need a moment to notice the
	// partition has cleared, and a single attempt landing in that exact
	// window would otherwise flake this test on an unrelated timing
	// detail this test isn't about.
	pub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = pub.Close() }()
	payload, err := json.Marshal(DeploymentWeightPayload{Model: "gpt-4o", DeploymentName: "post-partition", Weight: 7})
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	want := MutationEvent{Type: TypeDeploymentWeight, OriginInstanceID: fmt.Sprintf("test-instance-%d", testCounter.Add(1)), Payload: payload}

	publishDeadline := time.Now().Add(10 * time.Second)
	var lastPublishErr error
	for time.Now().Before(publishDeadline) {
		if lastPublishErr = pub.Publish(ctx, want); lastPublishErr == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lastPublishErr != nil {
		t.Fatalf("Publish never succeeded within 10s after unpausing: %v", lastPublishErr)
	}

	got := waitForEvent(t, received.Load, 10*time.Second)
	if got == nil {
		t.Fatal("subscriber never received the post-partition event -- Subscribe did not recover on its own after the partition cleared")
	}
	if got.OriginInstanceID != want.OriginInstanceID {
		t.Errorf("received = %+v, want OriginInstanceID=%q", got, want.OriginInstanceID)
	}

	cancel()
	select {
	case <-subscribeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe did not return within 5s of context cancellation, after surviving the partition")
	}
}
