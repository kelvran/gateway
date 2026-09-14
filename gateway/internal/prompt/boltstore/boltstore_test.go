package boltstore

import (
	"context"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/prompt"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prompts.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func msgs(content string) []adapter.Message {
	return []adapter.Message{{Role: "user", Content: content}}
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
	want := []prompt.Prompt{
		{ID: "greeting", Version: 1, Messages: msgs("hi v1")},
		{ID: "greeting", Version: 2, Messages: msgs("hi v2")},
	}
	if err := s.Save(context.Background(), "greeting", want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	versions, ok := got["greeting"]
	if !ok {
		t.Fatal("Load did not return \"greeting\" at all")
	}
	if len(versions) != 2 || versions[1].Messages[0].Content != "hi v2" {
		t.Errorf("round-tripped versions = %+v, want 2 versions ending in %q", versions, "hi v2")
	}
}

func TestSaveWithEmptyVersionsDeletesTheEntry(t *testing.T) {
	s, _ := openTestStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, "greeting", []prompt.Prompt{{ID: "greeting", Version: 1, Messages: msgs("hi")}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Mirrors prompt.Store.Delete's own documented contract: Save(ctx, id,
	// nil) is the deletion signal.
	if err := s.Save(ctx, "greeting", nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}

	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, stillPresent := got["greeting"]; stillPresent {
		t.Error("\"greeting\" is still present after Save(ctx, id, nil) -- want the bucket entry removed, not stored as an empty array")
	}
}

// TestPersistsAcrossReopen is the load-bearing test for
// docs/upgrade-research/admin-operator-experience-2026-09-14.md's prompt-
// persistence Phase 5: data saved by one *Store instance must be readable
// by a brand-new *Store instance opened later against the same file path
// — mirroring identity/boltstore's and budget/boltstore's identical test.
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompts.db")
	ctx := context.Background()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if err := s1.Save(ctx, "greeting", []prompt.Prompt{{ID: "greeting", Version: 1, Messages: msgs("hi")}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second, simulating a restart): %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.Load(ctx)
	if err != nil {
		t.Fatalf("Load (second): %v", err)
	}
	versions, ok := got["greeting"]
	if !ok || len(versions) != 1 || versions[0].Messages[0].Content != "hi" {
		t.Errorf("greeting after reopen = %+v, want 1 version with content %q", versions, "hi")
	}
}

// TestLoadRejectsCorruptValue proves a corrupted stored value (written
// directly, bypassing Save) surfaces as a clear error from Load, never a
// silent empty slice or a panic.
func TestLoadRejectsCorruptValue(t *testing.T) {
	s, _ := openTestStore(t)

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		return b.Put([]byte("corrupt"), []byte("not-json-at-all"))
	})
	if err != nil {
		t.Fatalf("writing a corrupt value directly: %v", err)
	}

	if _, err := s.Load(context.Background()); err == nil {
		t.Fatal("Load with a corrupt stored value returned nil error, want an error")
	}
}
