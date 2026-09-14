package boltstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/identity"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestLoadOnFreshDatabaseReturnsEmptyMap(t *testing.T) {
	s, _ := openTestStore(t)
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Load on a fresh database = %v, want empty map", got)
	}
}

func TestSaveThenLoadRoundTripsExactly(t *testing.T) {
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
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
	s, _ := openTestStore(t)
	if err := s.Delete(context.Background(), "never-persisted"); err != nil {
		t.Errorf("Delete of a never-persisted ID: %v, want nil error", err)
	}
}

// TestPersistsAcrossReopen is the load-bearing test for
// docs/upgrade-research/admin-operator-experience-2026-09-14.md Finding 2:
// data saved by one *Store instance must be readable by a brand-new
// *Store instance opened later against the same file path — the literal
// storage-layer proof of "survives a restart," mirroring
// budget/boltstore's own identical test.
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.db")
	ctx := context.Background()

	s1, err := Open(path)
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

	// A brand-new Store, opened later, against the same file — simulating
	// the gateway process restarting.
	s2, err := Open(path)
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
// silent zero-value VirtualKey or a panic.
func TestLoadRejectsCorruptValue(t *testing.T) {
	s, _ := openTestStore(t)

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte("team-corrupt"), []byte("not-json-at-all"))
	})
	if err != nil {
		t.Fatalf("writing a corrupt value directly: %v", err)
	}

	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("Load with a corrupt stored value returned nil error, want an error")
	}
}
