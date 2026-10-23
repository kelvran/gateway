package redislimiter

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Round-5 item 10 phase 2c-1: a Limiter can run over a client somebody else
// owns (the shared one); its Close then releases its hold and never closes
// the client itself.
func TestNewOnRunsOverASharedClientAndCloseOnlyReleases(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	released := 0
	l := NewOn(c, func() error { released++; return nil })
	ctx := context.Background()
	if ok, err := l.Allow(ctx, uniqueKey(t), 5, 1); err != nil || !ok {
		t.Fatalf("Allow over the shared client = %v, %v", ok, err)
	}
	if err := l.Close(); err != nil || released != 1 {
		t.Fatalf("Close = %v with %d releases, want nil and exactly 1", err, released)
	}
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("the shared client must outlive the limiter's Close: %v", err)
	}
}

func TestOpenStillOwnsAndClosesItsClient(t *testing.T) {
	l, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.client.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("after Close the owned client = %v, want ErrClosed", err)
	}
}
