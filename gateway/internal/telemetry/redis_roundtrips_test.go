package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRedisRoundTripCounterRidesOnTheContext(t *testing.T) {
	if _, _, ok := RedisRoundTripsFromContext(context.Background()); ok {
		t.Fatal("a bare context carries no counter")
	}
	ctx := WithRedisRoundTripCounter(context.Background())
	AddRedisRoundTrips(ctx, 1, 1)
	// finalize settles under WithoutCancel(ctx): values survive, so the
	// post-call round trips land on the same counter.
	AddRedisRoundTrips(context.WithoutCancel(ctx), 1, 3)
	AddRedisRoundTrips(context.Background(), 5, 5) // no counter: dropped, never a panic
	rt, cmds, ok := RedisRoundTripsFromContext(ctx)
	if !ok || rt != 2 || cmds != 4 {
		t.Errorf("counter = (%d, %d, %v); want 2 round trips, 4 commands, present", rt, cmds, ok)
	}
}

// redisTelemetrySnapshot sums the two Redis counters by their attribute
// pairs out of one collection; callers diff two snapshots (see
// cacheL3TelemetryMetricsReaderForTest's doc comment for why).
type redisTelemetrySnapshot struct {
	roundTrips map[[2]string]int64 // {store, kind}
	commands   map[[2]string]int64 // {store, command}
}

func snapshotRedisTelemetry(t *testing.T, reader *sdkmetric.ManualReader) redisTelemetrySnapshot {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	snap := redisTelemetrySnapshot{roundTrips: map[[2]string]int64{}, commands: map[[2]string]int64{}}
	attrOf := func(set attribute.Set, key string) string {
		v, _ := set.Value(attribute.Key(key))
		return v.AsString()
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				switch m.Name {
				case "kelvran.redis.round_trips":
					snap.roundTrips[[2]string{attrOf(dp.Attributes, AttrKelvranRedisStore), attrOf(dp.Attributes, AttrKelvranRedisKind)}] += dp.Value
				case "kelvran.redis.commands":
					snap.commands[[2]string{attrOf(dp.Attributes, AttrKelvranRedisStore), attrOf(dp.Attributes, AttrKelvranRedisCommand)}] += dp.Value
				}
			}
		}
	}
	return snap
}

func TestRecordRedisRoundTripCountsByStoreKindAndCommand(t *testing.T) {
	reader := cacheL3TelemetryMetricsReaderForTest()
	before := snapshotRedisTelemetry(t, reader)
	ctx := context.Background()
	RecordRedisRoundTrip(ctx, "ratelimit", RedisKindCommand)
	RecordRedisRoundTrip(ctx, "ratelimit", RedisKindPipeline)
	RecordRedisCommands(ctx, "ratelimit", "evalsha", 3)
	RecordRedisCommands(ctx, "budget", "hget", 1)
	after := snapshotRedisTelemetry(t, reader)
	delta := func(m, b map[[2]string]int64, k [2]string) int64 { return m[k] - b[k] }
	if d := delta(after.roundTrips, before.roundTrips, [2]string{"ratelimit", "command"}); d != 1 {
		t.Errorf("round_trips{ratelimit,command} delta = %d, want 1", d)
	}
	if d := delta(after.roundTrips, before.roundTrips, [2]string{"ratelimit", "pipeline"}); d != 1 {
		t.Errorf("round_trips{ratelimit,pipeline} delta = %d, want 1", d)
	}
	if d := delta(after.commands, before.commands, [2]string{"ratelimit", "evalsha"}); d != 3 {
		t.Errorf("commands{ratelimit,evalsha} delta = %d, want 3", d)
	}
	if d := delta(after.commands, before.commands, [2]string{"budget", "hget"}); d != 1 {
		t.Errorf("commands{budget,hget} delta = %d, want 1", d)
	}
}

func TestRecordRedisRoundTripsOnSpanOnlyWhenACounterIsPresent(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	ctx := WithRedisRoundTripCounter(context.Background())
	AddRedisRoundTrips(ctx, 11, 13)
	_, span := tp.Tracer("t").Start(ctx, "chat")
	RecordRedisRoundTripsOnSpan(ctx, span)
	span.End()
	_, bare := tp.Tracer("t").Start(context.Background(), "chat")
	RecordRedisRoundTripsOnSpan(context.Background(), bare)
	bare.End()
	ended := sr.Ended()
	if len(ended) != 2 {
		t.Fatalf("spans = %d, want 2", len(ended))
	}
	got := map[string]int64{}
	for _, kv := range ended[0].Attributes() {
		got[string(kv.Key)] = kv.Value.AsInt64()
	}
	if got[AttrKelvranRedisRoundTrips] != 11 || got[AttrKelvranRedisCommands] != 13 {
		t.Errorf("attributes = %v; want kelvran.redis.round_trips 11 and kelvran.redis.commands 13", got)
	}
	for _, kv := range ended[1].Attributes() {
		if string(kv.Key) == AttrKelvranRedisRoundTrips || string(kv.Key) == AttrKelvranRedisCommands {
			t.Errorf("a request without a counter must not claim a count: %v", kv)
		}
	}
}
