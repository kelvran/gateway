package redisstore

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/kelvran/gateway/gateway/internal/identity"
)

// One real Redis container shared by every test in this file — per
// docs/testing/TESTING.md §4's "real Redis via testcontainers, never
// mocked at this layer" commitment, mirroring
// internal/budget/redisbudget's own test file. Each test flushes the
// shared Hash first (see freshStore) so tests never see leftover state
// from a prior one, despite sharing one container — this package has
// no per-test unique-key discipline the way redisbudget/redislimiter do,
// since every virtual key lives in ONE shared Hash (hashKey), not a
// per-test key.
var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("redisstore: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("redisstore: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	m.Run()
}

// freshStore opens a Store against the shared test container, first
// deleting hashKey outright so this test starts from a genuinely empty
// Hash regardless of what any previous test left behind.
func freshStore(t *testing.T) *Store {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := client.Del(context.Background(), hashKey).Err(); err != nil {
		t.Fatalf("flushing %q before test: %v", hashKey, err)
	}
	_ = client.Close()

	s, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestLoadOnFreshHashReturnsEmptyMap(t *testing.T) {
	s := freshStore(t)
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load on a fresh Hash = %v, want empty map", got)
	}
}

func TestSaveThenLoadRoundTripsExactly(t *testing.T) {
	s := freshStore(t)
	want := identity.VirtualKey{
		ID:                  "team-alpha",
		KeyHash:             "aa11",
		BudgetUSD:           decimal.RequireFromString("12.345"),
		BudgetResetInterval: 3600_000_000_000, // 1h, as a raw time.Duration int64
		RateLimitBurst:      50,
		RateLimitRefill:     10,
		AllowedModels:       map[string]struct{}{"gpt-4o": {}},
	}
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gotVK, ok := got["team-alpha"]
	if !ok {
		t.Fatal("Load did not return \"team-alpha\" at all")
	}
	if gotVK.ID != want.ID || gotVK.KeyHash != want.KeyHash || !gotVK.BudgetUSD.Equal(want.BudgetUSD) || gotVK.RateLimitBurst != want.RateLimitBurst {
		t.Errorf("round-tripped VirtualKey = %+v, want %+v", gotVK, want)
	}
	if _, ok := gotVK.AllowedModels["gpt-4o"]; !ok {
		t.Errorf("round-tripped AllowedModels = %v, want a \"gpt-4o\" entry", gotVK.AllowedModels)
	}
}

func TestSaveUpsertsRatherThanDuplicating(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, identity.VirtualKey{ID: "team-alpha", KeyHash: "v1"}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(ctx, identity.VirtualKey{ID: "team-alpha", KeyHash: "v2"}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Load()) = %d, want 1 (upsert, not a duplicate entry)", len(got))
	}
	if got["team-alpha"].KeyHash != "v2" {
		t.Errorf("team-alpha.KeyHash = %q, want %q (the latest Save, not the first)", got["team-alpha"].KeyHash, "v2")
	}
}

func TestDeleteRemovesTheEntry(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, identity.VirtualKey{ID: "team-alpha", KeyHash: "v1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete(ctx, "team-alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load after Delete = %v, want empty map", got)
	}
}

func TestDeleteOfNeverPersistedIDIsANoOpNotAnError(t *testing.T) {
	s := freshStore(t)
	if err := s.Delete(context.Background(), "never-persisted"); err != nil {
		t.Errorf("Delete of a never-persisted ID: %v, want nil error", err)
	}
}

// TestPersistsAcrossReopen is the load-bearing test for this whole
// package's own reason to exist: data saved by one *Store instance must
// be readable by a brand-new *Store instance opened later against the
// SAME Redis address — simulating a process restart AND, unlike
// boltstore's identical test, a genuinely different SECOND PROCESS
// (two independent *redis.Client connections) sharing the exact same
// underlying data, which a bbolt file's exclusive lock could never
// support at all.
func TestPersistsAcrossReopen(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := client.Del(context.Background(), hashKey).Err(); err != nil {
		t.Fatalf("flushing %q before test: %v", hashKey, err)
	}
	_ = client.Close()

	ctx := context.Background()

	s1, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := s1.Save(ctx, identity.VirtualKey{ID: "team-alpha", KeyHash: "alpha-hash"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Save(ctx, identity.VirtualKey{ID: "team-beta", KeyHash: "beta-hash"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// A brand-new Store, opened later, against the same Redis address --
	// simulating the gateway process restarting (or a second replica
	// starting up for the first time).
	s2, err := Open(redis.Options{Addr: redisAddr})
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Load(ctx)
	if err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(Load()) after reopen = %d, want 2", len(got))
	}
	if got["team-alpha"].KeyHash != "alpha-hash" {
		t.Errorf("team-alpha after reopen = %+v, want KeyHash %q", got["team-alpha"], "alpha-hash")
	}
	if got["team-beta"].KeyHash != "beta-hash" {
		t.Errorf("team-beta after reopen = %+v, want KeyHash %q", got["team-beta"], "beta-hash")
	}
}

// TestLoadRejectsCorruptValue proves a corrupted stored value (written
// directly, bypassing Save) surfaces as a clear error from Load, never a
// silent zero-value VirtualKey or a panic — mirrors boltstore's identical
// test.
func TestLoadRejectsCorruptValue(t *testing.T) {
	s := freshStore(t)

	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = client.Close() }()
	if err := client.HSet(context.Background(), hashKey, "team-corrupt", "not-json-at-all").Err(); err != nil {
		t.Fatalf("writing a corrupt value directly: %v", err)
	}

	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("Load with a corrupt stored value returned nil error, want an error")
	}
}

func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	s, err := Open(redis.Options{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Open() on an unreachable address returned an error = %v, want nil (dialing is lazy)", err)
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := s.Load(ctx); err == nil {
		t.Fatal("Load() against an unreachable Redis address succeeded, want an error")
	}
}
