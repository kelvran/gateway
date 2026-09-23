package configpropagation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestInCanaryCohortDefaultsToEveryoneWhenUnsetOrFull proves the two
// "no restriction" cases: CanaryPercent <= 0 (unset, every event
// published before this field existed) and >= 100 both always return
// true, regardless of instanceID.
func TestInCanaryCohortDefaultsToEveryoneWhenUnsetOrFull(t *testing.T) {
	for _, cp := range []int{0, -1, 100, 150} {
		for _, id := range []string{"instance-a", "instance-b", "anything"} {
			if !inCanaryCohort(id, cp) {
				t.Errorf("inCanaryCohort(%q, %d) = false, want true", id, cp)
			}
		}
	}
}

// TestInCanaryCohortDistributesApproximatelyToThePercentage is the
// load-bearing statistical proof: across a large population of distinct
// instance IDs, a 10% canary must select approximately 10% of them --
// not exactly 10% (this is a deterministic hash, not a true random
// sample), but within a generous tolerance band that would still fail
// loudly if the comparison were inverted or badly broken.
func TestInCanaryCohortDistributesApproximatelyToThePercentage(t *testing.T) {
	const population = 1000
	const canaryPercent = 10

	var inCohort int
	for i := 0; i < population; i++ {
		if inCanaryCohort(fmt.Sprintf("instance-%d", i), canaryPercent) {
			inCohort++
		}
	}

	// A generous +/-5 percentage-point band (5%-15% of 1000 = 50-150) --
	// wide enough to tolerate FNV-1a's own real distribution variance,
	// tight enough that an inverted comparison (which would select ~90%,
	// i.e. ~900) fails hard.
	if inCohort < 50 || inCohort > 150 {
		t.Errorf("inCanaryCohort selected %d/%d instances for a %d%% canary, want approximately 100 (50-150 band)", inCohort, population, canaryPercent)
	}
}

// TestInCanaryCohortIsDeterministicPerInstance proves the same
// instanceID always gets the same cohort decision for a given
// percentage -- required for "wait for promotion" to make sense at all;
// a non-deterministic cohort would mean an out-of-cohort instance might
// randomly flip into the cohort on a LATER canaried event at the same
// percentage, defeating the whole point of a stable canary population.
func TestInCanaryCohortIsDeterministicPerInstance(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("instance-%d", i)
		first := inCanaryCohort(id, 30)
		for j := 0; j < 5; j++ {
			if inCanaryCohort(id, 30) != first {
				t.Fatalf("inCanaryCohort(%q, 30) returned different results across repeated calls", id)
			}
		}
	}
}

// TestSubscribeSkipsOutOfCohortCanaryEventThenDeliversOnPromotion is the
// real end-to-end proof for MutationEvent.CanaryPercent's whole
// contract: an instance NOT in a canary's cohort must never receive
// that event, but a follow-up event at the SAME PublishedAtUnixNano with
// CanaryPercent raised to 100 (a "promote to everyone" republish) must
// reach it.
func TestSubscribeSkipsOutOfCohortCanaryEventThenDeliversOnPromotion(t *testing.T) {
	// Pick two instance IDs cheaply verified to land on opposite sides of
	// a 10% cohort split for THIS specific test run, rather than trusting
	// two hardcoded literal strings to always do so (FNV-1a's mapping is
	// fixed but this keeps the test self-verifying against the real
	// function instead of a assumption about its output).
	var inCohortID, outOfCohortID string
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("canary-test-instance-%d", i)
		if inCanaryCohort(id, 10) {
			if inCohortID == "" {
				inCohortID = id
			}
		} else if outOfCohortID == "" {
			outOfCohortID = id
		}
		if inCohortID != "" && outOfCohortID != "" {
			break
		}
	}
	if inCohortID == "" || outOfCohortID == "" {
		t.Fatal("setup: could not find both an in-cohort and an out-of-cohort instance ID for a 10% canary within 1000 tries")
	}

	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = sub.Close() }()
	pub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = pub.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var receivedCount atomic.Int64
	go func() { _ = sub.Subscribe(ctx, outOfCohortID, func(MutationEvent) { receivedCount.Add(1) }) }()
	time.Sleep(100 * time.Millisecond)

	payload, err := json.Marshal(DeploymentWeightPayload{Model: "gpt-4o", DeploymentName: "canary-dep", Weight: 5})
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	publishedAt := time.Now().UnixNano()
	canaryEvent := MutationEvent{
		Type:                TypeDeploymentWeight,
		OriginInstanceID:    fmt.Sprintf("publisher-%d", testCounter.Add(1)),
		PublishedAtUnixNano: publishedAt,
		Payload:             payload,
		CanaryPercent:       10,
	}
	if err := pub.Publish(ctx, canaryEvent); err != nil {
		t.Fatalf("Publish (canary): %v", err)
	}

	// Give the out-of-cohort subscriber every chance to (wrongly) receive
	// it before asserting it didn't.
	time.Sleep(300 * time.Millisecond)
	if got := receivedCount.Load(); got != 0 {
		t.Fatalf("out-of-cohort instance received %d canaried events, want 0", got)
	}

	// Promote: same PublishedAtUnixNano, CanaryPercent now 100 (everyone).
	promoteEvent := MutationEvent{
		Type:                TypeDeploymentWeight,
		OriginInstanceID:    canaryEvent.OriginInstanceID,
		PublishedAtUnixNano: publishedAt,
		Payload:             payload,
		CanaryPercent:       100,
	}
	if err := pub.Publish(ctx, promoteEvent); err != nil {
		t.Fatalf("Publish (promote): %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && receivedCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := receivedCount.Load(); got != 1 {
		t.Fatalf("out-of-cohort instance received %d events after promotion, want exactly 1", got)
	}
	_ = inCohortID // documents the counterpart id found above; not separately asserted in this test.
}

// TestSubscribeDropsEventWithTamperedCanaryPercent is
// TestSubscribeDropsEventWithForgedOrMissingSignature's own sibling for
// CanaryPercent specifically: a validly-signed event whose
// CanaryPercent is altered AFTER signing (simulating a network-level
// tamper) must fail signature verification and never reach onEvent --
// proving CanaryPercent is genuinely covered by the HMAC, not a
// signature-exempt field an attacker could rewrite for free.
func TestSubscribeDropsEventWithTamperedCanaryPercent(t *testing.T) {
	sub := Open(redis.Options{Addr: redisAddr}, testSigningSecret)
	defer func() { _ = sub.Close() }()
	raw := redisRawClientForTest(t, redisAddr)
	defer func() { _ = raw.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var receivedCount atomic.Int64
	go func() { _ = sub.Subscribe(ctx, "test-instance", func(MutationEvent) { receivedCount.Add(1) }) }()
	time.Sleep(100 * time.Millisecond)

	payload, err := json.Marshal(DeploymentWeightPayload{Model: "gpt-4o", DeploymentName: "tamper-dep", Weight: 5})
	if err != nil {
		t.Fatalf("marshaling payload: %v", err)
	}
	// Sign a legitimate CanaryPercent: 10 event, then flip it to 100
	// AFTER signing -- exactly what a network-level tamper would do.
	genuine := MutationEvent{
		Type:                TypeDeploymentWeight,
		OriginInstanceID:    fmt.Sprintf("publisher-%d", testCounter.Add(1)),
		PublishedAtUnixNano: time.Now().UnixNano(),
		Payload:             payload,
		CanaryPercent:       10,
	}
	genuine.Signature = signEvent([]byte(testSigningSecret), genuine)
	genuine.CanaryPercent = 100 // tamper, post-signing

	body, err := json.Marshal(genuine)
	if err != nil {
		t.Fatalf("marshaling tampered event: %v", err)
	}
	if err := raw.Publish(ctx, channelName, body).Err(); err != nil {
		t.Fatalf("publishing tampered event via raw client: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if got := receivedCount.Load(); got != 0 {
		t.Fatalf("onEvent invoked %d times for a CanaryPercent-tampered event, want 0 (signature must no longer verify)", got)
	}
}
