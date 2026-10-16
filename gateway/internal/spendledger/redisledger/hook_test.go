package redisledger

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

type recordingHook struct{ process, pipeline atomic.Int32 }

func (h *recordingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *recordingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { h.process.Add(1); return next(ctx, cmd) }
}
func (h *recordingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error { h.pipeline.Add(1); return next(ctx, cmds) }
}

func TestAddHookObservesTheStoresRoundTrips(t *testing.T) {
	s, err := Open(redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1}, spendledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	h := &recordingHook{}
	s.AddHook(h)
	_ = s.client.Ping(context.Background()).Err()
	if h.process.Load() != 1 {
		t.Errorf("hook saw %d round trips, want 1", h.process.Load())
	}
}
