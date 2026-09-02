package boltstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
	bolt "go.etcd.io/bbolt"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "budget.db")
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
	want := decimal.RequireFromString("0.0000075") // many decimal places on purpose
	if err := s.Save(context.Background(), "team-alpha", want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	gotVal, ok := got["team-alpha"]
	if !ok {
		t.Fatal("Load did not return \"team-alpha\" at all")
	}
	if !gotVal.Equal(want) {
		t.Errorf("round-tripped value = %v, want exactly %v (byte-for-byte decimal precision, no numeric coercion)", gotVal, want)
	}
}

func TestSaveUpsertsRatherThanDuplicating(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, "team-alpha", decimal.RequireFromString("1")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(ctx, "team-alpha", decimal.RequireFromString("2")); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Load()) = %d, want 1 (upsert, not a duplicate entry)", len(got))
	}
	if !got["team-alpha"].Equal(decimal.RequireFromString("2")) {
		t.Errorf("team-alpha = %v, want 2 (the latest Save, not the first)", got["team-alpha"])
	}
}

// TestPersistsAcrossReopen is the load-bearing test for
// docs/rfcs/2026-09-03-budget-persistence.md's entire reason for
// existing: data saved by one *Store instance must be readable by a
// brand-new *Store instance opened later against the same file path —
// the literal storage-layer proof of "survives a restart."
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")
	ctx := context.Background()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := s1.Save(ctx, "team-alpha", decimal.RequireFromString("12.345")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Save(ctx, "team-beta", decimal.RequireFromString("0")); err != nil {
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
	if !got["team-alpha"].Equal(decimal.RequireFromString("12.345")) {
		t.Errorf("team-alpha after reopen = %v, want 12.345", got["team-alpha"])
	}
	if !got["team-beta"].Equal(decimal.Zero) {
		t.Errorf("team-beta after reopen = %v, want 0", got["team-beta"])
	}
}

// TestLoadRejectsCorruptValue proves a corrupted stored value (written
// directly, bypassing Save) surfaces as a clear error from Load, never a
// silent zero or a panic.
func TestLoadRejectsCorruptValue(t *testing.T) {
	s, _ := openTestStore(t)

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte("team-corrupt"), []byte("not-a-decimal"))
	})
	if err != nil {
		t.Fatalf("writing a corrupt value directly: %v", err)
	}

	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("Load with a corrupt stored value returned nil error, want an error")
	}
}
