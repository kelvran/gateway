// Package redishook counts the Redis round trips and commands the
// gateway's Redis-backed stores issue, as a go-redis Hook: one round trip
// per single command, one per pipeline however many commands it carries
// (client retries inside go-redis are not counted; a logical call is one
// round trip). Every count goes to telemetry's two Redis counters,
// attributed with the store's name, and to the per-request counter the
// pipeline installs on the request context, which finalize stamps on the
// chat span. Round-5 Tier C item 10 (2026-10-16): the measurement that must
// exist before the per-request Redis calls are pipelined -- the pass
// criterion is "at most 2 round trips per billable request", and only a
// client-side count can show that once pipelining lands (the server's
// INFO commandstats keeps counting commands).
//
// cmd/gateway attaches one Hook per store right after Open; the stores
// themselves know nothing about telemetry.
package redishook

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// Hook implements redis.Hook for one named store.
type Hook struct {
	store string
}

var _ redis.Hook = (*Hook)(nil)

// New returns a Hook attributing its counts to store ("ratelimit",
// "budget", "ledger").
func New(store string) *Hook { return &Hook{store: store} }

// DialHook passes through: the dial itself is not a command. go-redis does
// send its connection handshake through the hooks on the caller's context:
// HELLO through ProcessHook, by default CLIENT MAINT_NOTIFICATIONS the same
// way until the server rejects it, SELECT and CLIENT SETNAME (when the
// options ask for them) as one pipeline through ProcessPipelineHook, and
// AUTH only when HELLO failed -- so the request that opens a pooled
// connection is charged those round trips, real wire round trips, nil once
// the pool is warm. Likewise a script's first run after a Redis restart
// counts two (EVALSHA, then EVAL on NOSCRIPT), and a transaction pipeline
// counts its MULTI and EXEC.
func (h *Hook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook counts one round trip and one command per single call.
func (h *Hook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		telemetry.RecordRedisRoundTrip(ctx, h.store, telemetry.RedisKindCommand)
		telemetry.RecordRedisCommands(ctx, h.store, cmd.Name(), 1)
		telemetry.AddRedisRoundTrips(ctx, 1, 1)
		return next(ctx, cmd)
	}
}

// ProcessPipelineHook counts one round trip and len(cmds) commands per
// pipeline, by command name.
func (h *Hook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		telemetry.RecordRedisRoundTrip(ctx, h.store, telemetry.RedisKindPipeline)
		for _, cmd := range cmds {
			telemetry.RecordRedisCommands(ctx, h.store, cmd.Name(), 1)
		}
		telemetry.AddRedisRoundTrips(ctx, 1, len(cmds))
		return next(ctx, cmds)
	}
}
