package redisbudget

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// Round-5 item 10 phase 2c-1: a Backend can run over a client somebody else
// owns (the shared one); its Close then releases its hold and never closes
// the client itself.
func TestNewOnRunsOverASharedClientAndCloseOnlyReleases(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	released := 0
	b := NewOn(c, func() error { released++; return nil })
	ctx := context.Background()
	if ok, _, _, _, err := b.Reserve(ctx, uniqueKey(t), 5, 0); err != nil || !ok {
		t.Fatalf("Reserve over the shared client = %v, %v", ok, err)
	}
	if err := b.Close(); err != nil || released != 1 {
		t.Fatalf("Close = %v with %d releases, want nil and exactly 1", err, released)
	}
	if err := c.Ping(ctx).Err(); err != nil {
		t.Fatalf("the shared client must outlive the backend's Close: %v", err)
	}
}

func TestOpenStillOwnsAndClosesItsClient(t *testing.T) {
	b, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.client.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("after Close the owned client = %v, want ErrClosed", err)
	}
}
