package boltstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/budget"
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
	now := time.Now().Truncate(time.Second) // bbolt round-trips through JSON's RFC3339, sub-second precision isn't the point of this test
	want := budget.State{
		Spent:       decimal.RequireFromString("0.0000075"), // many decimal places on purpose
		PeriodStart: now,
		PeriodEpoch: 3,
		BilledCount: 2,
	}
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
	if !gotVal.Spent.Equal(want.Spent) {
		t.Errorf("round-tripped Spent = %v, want exactly %v (byte-for-byte decimal precision, no numeric coercion)", gotVal.Spent, want.Spent)
	}
	if !gotVal.PeriodStart.Equal(want.PeriodStart) {
		t.Errorf("round-tripped PeriodStart = %v, want %v", gotVal.PeriodStart, want.PeriodStart)
	}
	if gotVal.PeriodEpoch != want.PeriodEpoch {
		t.Errorf("round-tripped PeriodEpoch = %d, want %d", gotVal.PeriodEpoch, want.PeriodEpoch)
	}
	if gotVal.BilledCount != want.BilledCount {
		t.Errorf("round-tripped BilledCount = %d, want %d", gotVal.BilledCount, want.BilledCount)
	}
}

func TestSaveUpsertsRatherThanDuplicating(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, "team-alpha", budget.State{Spent: decimal.RequireFromString("1")}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := s.Save(ctx, "team-alpha", budget.State{Spent: decimal.RequireFromString("2")}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(Load()) = %d, want 1 (upsert, not a duplicate entry)", len(got))
	}
	if !got["team-alpha"].Spent.Equal(decimal.RequireFromString("2")) {
		t.Errorf("team-alpha = %v, want 2 (the latest Save, not the first)", got["team-alpha"].Spent)
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
	if err := s1.Save(ctx, "team-alpha", budget.State{Spent: decimal.RequireFromString("12.345")}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Save(ctx, "team-beta", budget.State{Spent: decimal.Zero}); err != nil {
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
	if !got["team-alpha"].Spent.Equal(decimal.RequireFromString("12.345")) {
		t.Errorf("team-alpha after reopen = %v, want 12.345", got["team-alpha"].Spent)
	}
	if !got["team-beta"].Spent.Equal(decimal.Zero) {
		t.Errorf("team-beta after reopen = %v, want 0", got["team-beta"].Spent)
	}
}

// TestLoadAcceptsLegacyBareDecimalStringFormat proves Load transparently
// reads a bucket entry written before this file understood budget.State
// — a bare decimal string (spent.String()), with no JSON object framing
// at all, exactly what every pre-migration Save call wrote.
func TestLoadAcceptsLegacyBareDecimalStringFormat(t *testing.T) {
	s, _ := openTestStore(t)

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte("team-legacy"), []byte("12.34"))
	})
	if err != nil {
		t.Fatalf("writing a legacy bare-decimal-string value directly: %v", err)
	}

	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state, ok := got["team-legacy"]
	if !ok {
		t.Fatal("Load did not return \"team-legacy\" at all")
	}
	if !state.Spent.Equal(decimal.RequireFromString("12.34")) {
		t.Errorf("Spent = %v, want 12.34", state.Spent)
	}
	if !state.PeriodStart.IsZero() {
		t.Errorf("PeriodStart = %v, want the zero value (legacy entry has no real window bookkeeping)", state.PeriodStart)
	}
	if state.PeriodEpoch != 0 || state.BilledCount != 0 {
		t.Errorf("PeriodEpoch/BilledCount = %d/%d, want 0/0 for a legacy entry", state.PeriodEpoch, state.BilledCount)
	}
}

// TestSaveRewritesLegacyEntryToNewJSONFormatOnNextSave proves the
// migration is lazy and automatic: once ANY Save happens for a
// legacy-shaped key, its entry is rewritten in the new JSON format and a
// subsequent Load returns the full State, not just the migrated Spent.
func TestSaveRewritesLegacyEntryToNewJSONFormatOnNextSave(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte("team-legacy"), []byte("5"))
	})
	if err != nil {
		t.Fatalf("writing a legacy bare-decimal-string value directly: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	if err := s.Save(ctx, "team-legacy", budget.State{Spent: decimal.RequireFromString("5"), PeriodStart: now, PeriodEpoch: 1, BilledCount: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	state := got["team-legacy"]
	if state.PeriodEpoch != 1 || state.BilledCount != 1 {
		t.Errorf("after rewrite, PeriodEpoch/BilledCount = %d/%d, want 1/1 — the legacy entry was not actually rewritten in the new format", state.PeriodEpoch, state.BilledCount)
	}
	if !state.PeriodStart.Equal(now) {
		t.Errorf("after rewrite, PeriodStart = %v, want %v", state.PeriodStart, now)
	}
}

// TestLoadRejectsCorruptValue proves a corrupted stored value (written
// directly, bypassing Save) surfaces as a clear error from Load, never a
// silent zero or a panic — neither the JSON decode nor the legacy
// bare-decimal-string fallback can parse it.
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
