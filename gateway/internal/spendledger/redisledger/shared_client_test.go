package redisledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

func TestNewOnRunsOverASharedClientAndCloseOnlyReleases(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	released := 0
	s := NewOn(c, spendledger.Options{}, func() error { released++; return nil })
	e := spendledger.Entry{Key: "shared-client-k", Model: "m", At: time.Now(), SpendNanoUSD: 5, Requests: 1}
	if err := s.Record(context.Background(), e); err != nil {
		t.Fatalf("Record over the shared client = %v", err)
	}
	if err := s.Close(); err != nil || released != 1 {
		t.Fatalf("Close = %v with %d releases, want nil and exactly 1", err, released)
	}
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("the shared client must outlive the store's Close: %v", err)
	}
}

func TestOpenStillOwnsAndClosesItsClient(t *testing.T) {
	s, err := Open(redis.Options{Addr: redisAddr}, spendledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.client.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("after Close the owned client = %v, want ErrClosed", err)
	}
}

// PlanRecord is the keys and arguments Record hands the script.
func TestPlanRecordIsTheScriptInputRecordSends(t *testing.T) {
	s := NewOn(redis.NewClient(&redis.Options{Addr: redisAddr}), spendledger.Options{}, func() error { return nil })
	at := time.Date(2026, 10, 23, 10, 30, 0, 0, time.UTC)
	e := spendledger.Entry{Key: "k", Model: "m", Tool: "t", Session: "s1", At: at, SpendNanoUSD: 7, Requests: 1}
	keys, args, err := s.PlanRecord(e)
	if err != nil {
		t.Fatal(err)
	}
	hour := spendledger.HourBucket(at)
	if len(keys) != 3 || keys[0] != hashKey(hour, "k") || keys[1] != sessionsKey("k") || keys[2] != cellsKey("k") {
		t.Fatalf("keys = %v, want the hour hash, the sessions set and the cell counter", keys)
	}
	if len(args) != 11 {
		t.Fatalf("args = %d, want the script's 11", len(args))
	}
	if _, _, err := s.PlanRecord(spendledger.Entry{}); err == nil {
		t.Fatal("an invalid entry must fail before any script input is built")
	}
}
