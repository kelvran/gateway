package boltstore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/ledgertest"
)

func TestBoltLedgerContract(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T, opts spendledger.Options) spendledger.Ledger {
		s, err := OpenWith(opts)(filepath.Join(t.TempDir(), "ledger.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestBoltLedgerSessionSetSurvivesReopen: the per-key session set is
// persisted, so the cap holds across a restart — the 3rd session after a
// reopen still folds into other when two are live.
func TestBoltLedgerSessionSetSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	opts := spendledger.Options{MaxSessionsPerKey: 2}
	at := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	s, err := OpenWith(opts)(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range []string{"a", "b"} {
		if err := s.Record(context.Background(), spendledger.Entry{Key: "k", Model: "m", Tool: "curl", Session: sess, At: at, SpendNanoUSD: 1, Requests: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWith(opts)(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Record(context.Background(), spendledger.Entry{Key: "k", Model: "m", Tool: "curl", Session: "c", At: at, SpendNanoUSD: 1, Requests: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Query(context.Background(), spendledger.Query{GroupBy: spendledger.GroupBySession, Since: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[string]int64{}
	for _, r := range got {
		sessions[r.Session] = r.Requests
	}
	if sessions["a"] != 1 || sessions["b"] != 1 || sessions[spendledger.OtherSession] != 1 || len(sessions) != 3 {
		t.Errorf("after reopen: %v", sessions)
	}
	if s.DB() == nil {
		t.Error("DB() must expose the handle for backup")
	}
}

// TestOpenRefusesALockedFile mirrors the other bbolt stores: a second Open
// on a file another process holds fails within the lock timeout instead of
// hanging the gateway's start.
func TestOpenRefusesALockedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	start := time.Now()
	if _, err := Open(path); err == nil {
		t.Fatal("second Open on a locked file succeeded")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("second Open took %v; the lock timeout must bound it", elapsed)
	}
}

// TestRecordFailsClosedOnACorruptRowCount: a rows-bucket value that is not a
// decimal count is an error, never read as zero (which would lift the cap).
func TestRecordFailsClosedOnACorruptRowCount(t *testing.T) {
	s, err := OpenWith(spendledger.Options{MaxRowsPerKey: 2})(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	at := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	if err := s.Record(context.Background(), spendledger.Entry{Key: "k1", Model: "m", Tool: "curl", Session: "s1", At: at, SpendNanoUSD: 1, Requests: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(rowsBucket)).Put([]byte("k1"), []byte("not-a-count"))
	}); err != nil {
		t.Fatal(err)
	}
	err = s.Record(context.Background(), spendledger.Entry{Key: "k1", Model: "m", Tool: "curl", Session: "s2", At: at, SpendNanoUSD: 1, Requests: 1})
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Record with a corrupt row count: err = %v, want a corrupt-value error", err)
	}
}

// TestDeleteDropsAnEmptiedHourBucket: when a delete removes the last row of an
// hour, the hour's bucket goes with it instead of lingering until Sweep.
func TestDeleteDropsAnEmptiedHourBucket(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	hourA := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	hourB := hourA.Add(time.Hour)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Record(context.Background(), spendledger.Entry{Key: "k1", Model: "m", Tool: "curl", Session: "s1", At: hourA, SpendNanoUSD: 1, Requests: 1}))
	must(s.Record(context.Background(), spendledger.Entry{Key: "k2", Model: "m", Tool: "curl", Session: "s1", At: hourB, SpendNanoUSD: 1, Requests: 1}))
	must(s.DeleteKey(context.Background(), "k1"))
	var names []string
	must(s.DB().View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(hoursBucket)).ForEachBucket(func(name []byte) error { names = append(names, string(name)); return nil })
	}))
	if len(names) != 1 || names[0] != string(hourName(spendledger.HourBucket(hourB))) {
		t.Fatalf("hour buckets after DeleteKey = %v, want only %s", names, hourName(spendledger.HourBucket(hourB)))
	}
}

// TestMaintenanceRemovesUndecodableStateAndReadsNeverLeakIt: a hour bucket
// whose name is not a Unix timestamp and a row whose key does not decode are
// garbage. Reads fail closed on them without quoting the row (an identifier
// could be inside), and Sweep removes them so retention never stalls.
func TestMaintenanceRemovesUndecodableStateAndReadsNeverLeakIt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	hourA := time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Record(ctx, spendledger.Entry{Key: "k1", Model: "m", Tool: "curl", Session: "s1", At: hourA, SpendNanoUSD: 1, Requests: 1}))
	must(s.DB().Update(func(tx *bolt.Tx) error {
		hours := tx.Bucket([]byte(hoursBucket))
		junk, err := hours.CreateBucket([]byte("not-a-number"))
		if err != nil {
			return err
		}
		if err := junk.Put([]byte("k1|m|curl|SECRET-SESSION-A"), []byte(`{"spend":1,"requests":1}`)); err != nil {
			return err
		}
		real := hours.Bucket(hourName(spendledger.HourBucket(hourA)))
		if err := real.Put([]byte("k1|m|curl|SECRET-SESSION-B%zz"), []byte(`{"spend":1,"requests":1}`)); err != nil {
			return err
		}
		return real.Put([]byte(spendledger.CellKey{Key: "k1", Model: "m", Tool: "curl", Session: "SECRET-SESSION-C"}.Encode()), []byte("not json"))
	}))
	q := spendledger.Query{GroupBy: spendledger.GroupBySession, Since: hourA.Add(-time.Hour), Until: hourA.Add(time.Hour)}
	_, err = s.Query(ctx, q)
	if err == nil {
		t.Fatal("Query over undecodable state must fail closed")
	}
	if msg := err.Error(); strings.Contains(msg, "SECRET-SESSION") {
		t.Fatalf("a read error quoted a session identifier: %s", msg)
	}
	must(s.Sweep(ctx, hourA.Add(time.Hour)))
	got, err := s.Query(ctx, q)
	if err != nil || len(got) != 1 || got[0].Session != "s1" {
		t.Fatalf("after Sweep: rows = %v, err = %v; want only s1", got, err)
	}
	var names []string
	must(s.DB().View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(hoursBucket)).ForEachBucket(func(name []byte) error { names = append(names, string(name)); return nil })
	}))
	if len(names) != 1 {
		t.Fatalf("hour buckets after Sweep = %v, want only the real hour", names)
	}
}
