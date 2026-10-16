package telemetry

import (
	"context"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Redis round-trip accounting (round-5 Tier C item 10, 2026-10-16): the
// measurement that has to exist before the per-request Redis calls are
// pipelined. internal/redishook feeds the two counters from a go-redis
// Hook on every Redis-backed store; the per-request counter rides on the
// request context so finalize can stamp the chat span with the request's
// own total. Nothing here is on the hot path beyond two counter Adds and
// two atomic increments per Redis call.
const (
	// RedisKindCommand is one single-command round trip; RedisKindPipeline
	// is one pipeline round trip however many commands it carried.
	RedisKindCommand  = "command"
	RedisKindPipeline = "pipeline"
)

// The instrument names and units, shared with the test that pins their
// Prometheus exposition names (the bench harness greps for those): a unit
// with a suffix would change the exposed series name.
const (
	redisRoundTripsName = "kelvran.redis.round_trips"
	redisRoundTripsUnit = "{round_trip}"
	redisCommandsName   = "kelvran.redis.commands"
	redisCommandsUnit   = "{command}"
)

var (
	redisRoundTripsCounter = mustInt64Counter(meter, redisRoundTripsName,
		metric.WithDescription("Redis round trips issued by the gateway's Redis-backed stores: one per single command, one per pipeline; client retries are not counted."),
		metric.WithUnit(redisRoundTripsUnit))
	redisCommandsCounter = mustInt64Counter(meter, redisCommandsName,
		metric.WithDescription("Redis commands issued by the gateway's Redis-backed stores, by command name; a pipeline of n commands counts n."),
		metric.WithUnit(redisCommandsUnit))
)

// RecordRedisRoundTrip counts one round trip for store (ratelimit, budget,
// ledger) of the given kind.
func RecordRedisRoundTrip(ctx context.Context, store, kind string) {
	redisRoundTripsCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrKelvranRedisStore, store),
		attribute.String(AttrKelvranRedisKind, kind)))
}

// RecordRedisCommands counts n commands of one name for store.
func RecordRedisCommands(ctx context.Context, store, command string, n int64) {
	redisCommandsCounter.Add(ctx, n, metric.WithAttributes(
		attribute.String(AttrKelvranRedisStore, store),
		attribute.String(AttrKelvranRedisCommand, command)))
}

type redisRoundTripCounter struct {
	roundTrips atomic.Int64
	commands   atomic.Int64
}

type redisCounterKey struct{}

// WithRedisRoundTripCounter installs a per-request counter on ctx. The
// pipeline installs one at the top of every chat completion; a context
// derived with WithoutCancel (the settlement context) keeps it, so the
// post-call calls land on the same counter.
func WithRedisRoundTripCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, redisCounterKey{}, &redisRoundTripCounter{})
}

// AddRedisRoundTrips adds to the request's counter when ctx carries one and
// does nothing otherwise, so a store used outside a request (startup
// hydration, admin routes) never panics.
func AddRedisRoundTrips(ctx context.Context, roundTrips, commands int) {
	c, ok := ctx.Value(redisCounterKey{}).(*redisRoundTripCounter)
	if !ok {
		return
	}
	c.roundTrips.Add(int64(roundTrips))
	c.commands.Add(int64(commands))
}

// RedisRoundTripsFromContext reads the request's counter; ok is false when
// ctx carries none.
func RedisRoundTripsFromContext(ctx context.Context) (roundTrips, commands int, ok bool) {
	c, ok := ctx.Value(redisCounterKey{}).(*redisRoundTripCounter)
	if !ok {
		return 0, 0, false
	}
	return int(c.roundTrips.Load()), int(c.commands.Load()), true
}

// RecordRedisRoundTripsOnSpan stamps the request's totals on span as
// kelvran.redis.round_trips and kelvran.redis.commands -- 0 and 0 for a
// request served entirely in memory -- and sets nothing when ctx carries no
// counter, so a span never claims a count that was not kept.
func RecordRedisRoundTripsOnSpan(ctx context.Context, span trace.Span) {
	roundTrips, commands, ok := RedisRoundTripsFromContext(ctx)
	if !ok {
		return
	}
	span.SetAttributes(
		attribute.Int(AttrKelvranRedisRoundTrips, roundTrips),
		attribute.Int(AttrKelvranRedisCommands, commands))
}
