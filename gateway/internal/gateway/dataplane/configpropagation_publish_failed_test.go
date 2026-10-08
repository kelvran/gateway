package dataplane

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/configpropagation"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/router"
)

// failingPublisher is a configpropagation.Publisher whose Redis is down:
// every Publish returns the same error, the way go-redis surfaces a
// refused connection or a timed-out PUBLISH.
type failingPublisher struct{ err error }

func (f failingPublisher) Publish(context.Context, configpropagation.MutationEvent) error {
	return f.err
}

// publishFailedDeltas collects the kelvran.configpropagation.publish_failed
// counter before and after fn runs and returns the per-event-type delta.
func publishFailedDeltas(t *testing.T, fn func()) map[string]int64 {
	t.Helper()
	reader := dataplaneTelemetryMetricsReaderForTest()
	var before metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &before); err != nil {
		t.Fatalf("reader.Collect (before): %v", err)
	}
	beforeSnap := snapshotDataplaneTelemetry(t, before)

	fn()

	var after metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &after); err != nil {
		t.Fatalf("reader.Collect (after): %v", err)
	}
	afterSnap := snapshotDataplaneTelemetry(t, after)

	deltas := map[string]int64{}
	for typ, v := range afterSnap.configPropagationPublishFailedByType {
		deltas[typ] = v - beforeSnap.configPropagationPublishFailedByType[typ]
	}
	return deltas
}

var publishFailedEventTypes = []string{
	configpropagation.TypeVirtualKeyUpsert,
	configpropagation.TypeVirtualKeyDelete,
	configpropagation.TypeDeploymentWeight,
}

// TestConfigPropagationPublishFailureIncrementsMetricCounterAndKeepsTheLocalMutation
// pins both halves of the publish-failure contract docs/operations/
// FAILURE-MODES.md documents: the local mutation is applied and the admin
// caller gets its success (the other replicas are the ones left stale), and
// every failed publish is counted once under its event type -- the log line
// alone (configpropagation_publish_failed) was invisible to any dashboard.
func TestConfigPropagationPublishFailureIncrementsMetricCounterAndKeepsTheLocalMutation(t *testing.T) {
	deployments := []router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 99},
		{Name: "canary", Model: "gpt-4o", Weight: 1},
	}
	p := newConfigPropagationTestPipeline(t, deployments, failingPublisher{err: errors.New("redis: connection refused")})

	deltas := publishFailedDeltas(t, func() {
		if err := p.UpsertVirtualKey(
			identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("brand-new-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
			ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100},
		); err != nil {
			t.Fatalf("UpsertVirtualKey must succeed locally when only the publish fails, got %v", err)
		}
		if !canAuthenticate(p, "brand-new-secret") {
			t.Fatal("the upserted key does not authenticate locally after a failed publish")
		}
		if err := p.DeleteVirtualKey("team-gamma"); err != nil {
			t.Fatalf("DeleteVirtualKey must succeed locally when only the publish fails, got %v", err)
		}
		if canAuthenticate(p, "brand-new-secret") {
			t.Fatal("the deleted key still authenticates locally after a failed publish")
		}
		if err := p.UpdateDeploymentWeight(context.Background(), "canary", 50); err != nil {
			t.Fatalf("UpdateDeploymentWeight must succeed locally when only the publish fails, got %v", err)
		}
		const samples = 1000
		if got := countStableVsCanary(p, "gpt-4o", "canary", samples); got < 250 || got > 750 {
			t.Fatalf("canary share after a local weight change = %d/%d, want roughly half: the local mutation must apply even though the publish failed", got, samples)
		}
	})

	for _, typ := range publishFailedEventTypes {
		if deltas[typ] != 1 {
			t.Errorf("kelvran.configpropagation.publish_failed{event_type=%q} delta = %d, want 1", typ, deltas[typ])
		}
	}
}

// TestConfigPropagationPublishSuccessDoesNotIncrementTheFailureCounter is
// the control: the same three mutations through a publisher that works
// leave the counter untouched.
func TestConfigPropagationPublishSuccessDoesNotIncrementTheFailureCounter(t *testing.T) {
	deployments := []router.Deployment{
		{Name: "stable", Model: "gpt-4o", Weight: 99},
		{Name: "canary", Model: "gpt-4o", Weight: 1},
	}
	p := newConfigPropagationTestPipeline(t, deployments, &capturingPublisher{})

	deltas := publishFailedDeltas(t, func() {
		if err := p.UpsertVirtualKey(
			identity.VirtualKey{ID: "team-gamma", KeyHash: testHashOf("brand-new-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
			ratelimit.KeyConfig{ID: "team-gamma", Capacity: 100, RefillPerSecond: 100},
		); err != nil {
			t.Fatalf("UpsertVirtualKey: %v", err)
		}
		if err := p.DeleteVirtualKey("team-gamma"); err != nil {
			t.Fatalf("DeleteVirtualKey: %v", err)
		}
		if err := p.UpdateDeploymentWeight(context.Background(), "canary", 50); err != nil {
			t.Fatalf("UpdateDeploymentWeight: %v", err)
		}
	})

	for _, typ := range publishFailedEventTypes {
		if deltas[typ] != 0 {
			t.Errorf("kelvran.configpropagation.publish_failed{event_type=%q} delta = %d after successful publishes, want 0", typ, deltas[typ])
		}
	}
}
