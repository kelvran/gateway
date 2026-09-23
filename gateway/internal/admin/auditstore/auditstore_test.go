package auditstore

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppendThenQueryRoundTripsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Append("admin_virtual_key_upserted", "name", "team-alpha", "authorized_by", "admin"); err != nil {
		t.Fatalf("Append: %v", err)
	}

	entries, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Msg != "admin_virtual_key_upserted" {
		t.Errorf("entries[0].Msg = %q, want %q", entries[0].Msg, "admin_virtual_key_upserted")
	}
	if entries[0].Fields["name"] != "team-alpha" || entries[0].Fields["authorized_by"] != "admin" {
		t.Errorf("entries[0].Fields = %+v, want name=team-alpha authorized_by=admin", entries[0].Fields)
	}
}

func TestQueryFiltersByMsg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	_ = s.Append("admin_virtual_key_upserted", "name", "team-alpha")
	_ = s.Append("admin_virtual_key_deleted", "name", "team-beta")

	entries, err := Query(path, Filter{Msg: "admin_virtual_key_deleted"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 || entries[0].Fields["name"] != "team-beta" {
		t.Fatalf("Query(Msg=admin_virtual_key_deleted) = %+v, want exactly the team-beta delete entry", entries)
	}
}

func TestQueryFiltersByFieldKeyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	_ = s.Append("admin_virtual_key_upserted", "name", "team-alpha")
	_ = s.Append("admin_virtual_key_upserted", "name", "team-beta")
	_ = s.Append("admin_virtual_key_deleted", "name", "team-alpha")

	entries, err := Query(path, Filter{FieldKey: "name", FieldValue: "team-alpha"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (both team-alpha entries, upsert and delete)", len(entries))
	}
}

func TestQueryFiltersByTimeRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	_ = s.Append("event-1")
	time.Sleep(10 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(10 * time.Millisecond)
	_ = s.Append("event-2")

	entries, err := Query(path, Filter{Since: cutoff})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 || entries[0].Msg != "event-2" {
		t.Fatalf("Query(Since=cutoff) = %+v, want exactly event-2", entries)
	}

	entries, err = Query(path, Filter{Until: cutoff})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 1 || entries[0].Msg != "event-1" {
		t.Fatalf("Query(Until=cutoff) = %+v, want exactly event-1", entries)
	}
}

// TestQueryOnNeverCreatedFileReturnsEmptyNotError proves the "nothing
// recorded yet" case is never a caller-visible error.
func TestQueryOnNeverCreatedFileReturnsEmptyNotError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created.jsonl")
	entries, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query on a never-created file: %v, want nil error", err)
	}
	if len(entries) != 0 {
		t.Errorf("len(entries) = %d, want 0", len(entries))
	}
}

// TestAppendPersistsAcrossReopen proves the file is genuinely durable
// (append-only, never truncated) across a Close/re-Open, the real
// restart scenario this store exists to survive.
func TestAppendPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s1.Append("event-before-restart")
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	_ = s2.Append("event-after-restart")

	entries, err := Query(path, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2 (both pre- and post-restart events preserved)", len(entries))
	}
}
