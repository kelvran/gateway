package redishook

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

var (
	readerOnce sync.Once
	reader     *sdkmetric.ManualReader
)

// metricsReader binds this test binary's global meter once (the otel
// delegate is process-lifetime; see telemetry's result_test.go).
func metricsReader() *sdkmetric.ManualReader {
	readerOnce.Do(func() {
		reader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	})
	return reader
}

func counts(t *testing.T) (roundTrips map[[2]string]int64, commands map[[2]string]int64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := metricsReader().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	roundTrips, commands = map[[2]string]int64{}, map[[2]string]int64{}
	attr := func(set attribute.Set, key string) string { v, _ := set.Value(attribute.Key(key)); return v.AsString() }
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				switch m.Name {
				case "kelvran.redis.round_trips":
					roundTrips[[2]string{attr(dp.Attributes, telemetry.AttrKelvranRedisStore), attr(dp.Attributes, telemetry.AttrKelvranRedisKind)}] += dp.Value
				case "kelvran.redis.commands":
					commands[[2]string{attr(dp.Attributes, telemetry.AttrKelvranRedisStore), attr(dp.Attributes, telemetry.AttrKelvranRedisCommand)}] += dp.Value
				}
			}
		}
	}
	return roundTrips, commands
}

// unreachableClient points at a closed port with retries off: every call
// fails fast, and the hook still sees each logical round trip.
func unreachableClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHookCountsOneRoundTripPerCommandAndOnePerPipeline(t *testing.T) {
	rtBefore, cmdBefore := counts(t)
	c := unreachableClient(t)
	c.AddHook(New("ratelimit"))
	ctx := telemetry.WithRedisRoundTripCounter(context.Background())
	_ = c.Ping(ctx).Err()
	pipe := c.Pipeline()
	pipe.Ping(ctx)
	pipe.Ping(ctx)
	pipe.Ping(ctx)
	_, _ = pipe.Exec(ctx)
	rt, cmds, ok := telemetry.RedisRoundTripsFromContext(ctx)
	if !ok || rt != 2 || cmds != 4 {
		t.Errorf("per-request counter = (%d, %d, %v); want 2 round trips (one command, one pipeline) and 4 commands", rt, cmds, ok)
	}
	rtAfter, cmdAfter := counts(t)
	if d := rtAfter[[2]string{"ratelimit", "command"}] - rtBefore[[2]string{"ratelimit", "command"}]; d != 1 {
		t.Errorf("round_trips{ratelimit,command} delta = %d, want 1", d)
	}
	if d := rtAfter[[2]string{"ratelimit", "pipeline"}] - rtBefore[[2]string{"ratelimit", "pipeline"}]; d != 1 {
		t.Errorf("round_trips{ratelimit,pipeline} delta = %d, want 1", d)
	}
	if d := cmdAfter[[2]string{"ratelimit", "ping"}] - cmdBefore[[2]string{"ratelimit", "ping"}]; d != 4 {
		t.Errorf("commands{ratelimit,ping} delta = %d, want 4 (one single, three in the pipeline)", d)
	}
}

func TestHookCountsWithoutARequestCounterOnTheContext(t *testing.T) {
	rtBefore, _ := counts(t)
	c := unreachableClient(t)
	c.AddHook(New("budget"))
	_ = c.Ping(context.Background()).Err() // no counter on the context: the process-wide counters still move
	rtAfter, _ := counts(t)
	if d := rtAfter[[2]string{"budget", "command"}] - rtBefore[[2]string{"budget", "command"}]; d != 1 {
		t.Errorf("round_trips{budget,command} delta = %d, want 1", d)
	}
}
