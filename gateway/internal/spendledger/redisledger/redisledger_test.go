package redisledger

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/ledgertest"
)

// One real Redis container shared by every test in this file — per
// docs/testing/TESTING.md's "real Redis via testcontainers, never mocked at
// this layer" commitment, the same convention internal/budget/redisbudget's
// own test file establishes. The contract suite names fixed keys, so the
// factory below flushes the database before every case instead of
// generating unique keys.
var redisAddr string

func TestMain(m *testing.M) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		panic(fmt.Sprintf("redisledger: starting test Redis container: %v", err))
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		panic(fmt.Sprintf("redisledger: getting test Redis connection string: %v", err))
	}
	redisAddr = strings.TrimPrefix(connStr, "redis://")

	m.Run()
}

// flush empties the test database so a case starts from nothing; test-only.
func flush(t *testing.T) {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	if err := c.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("FLUSHDB: %v", err)
	}
}

func open(t *testing.T, opts spendledger.Options) *Store {
	t.Helper()
	s, err := Open(redis.Options{Addr: redisAddr}, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestRedisLedgerContract(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T, opts spendledger.Options) spendledger.Ledger {
		flush(t)
		return open(t, opts)
	})
}

var (
	at    = time.Date(2026, 10, 11, 10, 30, 0, 0, time.UTC)
	entry = func(key, model, tool, session string, at time.Time) spendledger.Entry {
		return spendledger.Entry{Key: key, Model: model, Tool: tool, Session: session, At: at, SpendNanoUSD: 1, Requests: 1}
	}
)

func sessionsOf(t *testing.T, l spendledger.Ledger, key string) string {
	t.Helper()
	rows, err := l.Query(context.Background(), spendledger.Query{GroupBy: spendledger.GroupBySession, Key: key, Since: at.Add(-time.Hour), Until: at.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s=%d", r.Session, r.Requests))
	}
	return strings.Join(parts, " ")
}

// TestDerivedStateSurvivesReopen: the per-key session set and cell count
// live in Redis, so the caps hold across a restart — the third session after
// a reopen still folds into other when two are live, and a third named cell
// still folds when two are counted.
func TestDerivedStateSurvivesReopen(t *testing.T) {
	flush(t)
	ctx := context.Background()
	opts := spendledger.Options{MaxSessionsPerKey: 2, MaxRowsPerKey: 2}
	s1 := open(t, opts)
	if err := s1.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	if err := s1.Record(ctx, entry("k1", "m", "curl", "s2", at)); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()
	s2 := open(t, opts)
	defer func() { _ = s2.Close() }()
	if err := s2.Record(ctx, entry("k1", "m", "curl", "s3", at)); err != nil { // over the session cap
		t.Fatal(err)
	}
	if err := s2.Record(ctx, entry("k1", "m2", "curl", "s1", at)); err != nil { // a live session, a third named cell: over the cell cap
		t.Fatal(err)
	}
	if got := sessionsOf(t, s2, "k1"); got != "other=2 s1=1 s2=1" {
		t.Fatalf("after reopen: %q, want %q", got, "other=2 s1=1 s2=1")
	}
}

// TestTwoClientsShareOneCap: the fold decision runs inside Redis, so two
// gateway replicas writing the same key share one session set and one
// cell count rather than each folding against a local view.
func TestTwoClientsShareOneCap(t *testing.T) {
	flush(t)
	ctx := context.Background()
	opts := spendledger.Options{MaxSessionsPerKey: 2}
	a, b := open(t, opts), open(t, opts)
	defer func() { _ = a.Close(); _ = b.Close() }()
	if err := a.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	if err := b.Record(ctx, entry("k1", "m", "curl", "s2", at)); err != nil {
		t.Fatal(err)
	}
	if err := a.Record(ctx, entry("k1", "m", "curl", "s3", at)); err != nil { // the third, through either client, folds
		t.Fatal(err)
	}
	if got := sessionsOf(t, b, "k1"); got != "other=1 s1=1 s2=1" {
		t.Fatalf("shared cap: %q, want %q", got, "other=1 s1=1 s2=1")
	}
}

// TestConcurrentWritersNeverExceedTheSessionCap: sixteen goroutines each
// recording ten distinct sessions of one key must leave exactly the cap's
// worth of named sessions and every other request under other — the check
// and the write are one Lua script, so no interleaving can admit an extra
// session.
func TestConcurrentWritersNeverExceedTheSessionCap(t *testing.T) {
	flush(t)
	ctx := context.Background()
	const cap, writers, each = 5, 16, 10
	s := open(t, spendledger.Options{MaxSessionsPerKey: cap})
	defer func() { _ = s.Close() }()
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.Record(ctx, entry("k1", "m", "curl", fmt.Sprintf("s%d-%d", w, i), at)); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := s.Query(ctx, spendledger.Query{GroupBy: spendledger.GroupBySession, Key: "k1", Since: at.Add(-time.Hour), Until: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	named, total := 0, int64(0)
	var other int64
	for _, r := range rows {
		total += r.Requests
		if r.Session == spendledger.OtherSession {
			other = r.Requests
		} else {
			named++
		}
	}
	if named != cap || total != writers*each || other != writers*each-cap {
		t.Fatalf("named=%d total=%d other=%d; want %d, %d, %d", named, total, other, cap, writers*each, writers*each-cap)
	}
}

// TestHashTTLIsSetOnceAsABackstop: an hour's hash carries a TTL of
// retention + 1 h from its first write (Sweep by the caller's clock is the
// primary retention; the TTL only catches a sweeper that never runs), and a
// later write to the same hash does not extend it: after a pause the TTL has
// only decayed.
func TestHashTTLIsSetOnceAsABackstop(t *testing.T) {
	flush(t)
	ctx := context.Background()
	retention := 2 * time.Hour
	s := open(t, spendledger.Options{Retention: retention})
	defer func() { _ = s.Close() }()
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	first, err := c.PTTL(ctx, hashKey(spendledger.HourBucket(at), "k1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if first <= retention || first > retention+time.Hour {
		t.Fatalf("PTTL after the first write = %v, want in (%v, %v]", first, retention, retention+time.Hour)
	}
	time.Sleep(30 * time.Millisecond)
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	second, err := c.PTTL(ctx, hashKey(spendledger.HourBucket(at), "k1")).Result()
	if err != nil {
		t.Fatal(err)
	}
	if second >= first { // a reset lands within the same millisecond as the first read, so equality is a reset too
		t.Fatalf("a second write reset or extended the TTL: %v after %v, want strictly less", second, first)
	}
}

// TestSweepRemovesGarbageFieldsAndReadsNeverLeakThem: a hash field that does
// not decode, whose value is not an integer, or whose requests field has no
// spend partner is garbage. For each kind on its own: Query fails closed
// without quoting the field (a field carries a session identifier), and
// Sweep removes the pair and leaves the real row.
func TestSweepRemovesGarbageFieldsAndReadsNeverLeakThem(t *testing.T) {
	garbage := map[string][]string{
		"an undecodable field":        {"m|curl|SECRET-SESSION-A%zz", "1", "m|curl|SECRET-SESSION-A%zz#n", "1"},
		"a non-integer spend":         {"m|curl|SECRET-SESSION-B", "not-a-number", "m|curl|SECRET-SESSION-B#n", "1"},
		"a non-integer request count": {"m|curl|SECRET-SESSION-C", "1", "m|curl|SECRET-SESSION-C#n", "x"},
		"an orphan requests field":    {"m|curl|SECRET-SESSION-D#n", "1"},
	}
	for name, fields := range garbage {
		t.Run(name, func(t *testing.T) {
			flush(t)
			ctx := context.Background()
			s := open(t, spendledger.Options{})
			defer func() { _ = s.Close() }()
			if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
				t.Fatal(err)
			}
			c := redis.NewClient(&redis.Options{Addr: redisAddr})
			defer func() { _ = c.Close() }()
			args := make([]interface{}, len(fields))
			for i, f := range fields {
				args[i] = f
			}
			if err := c.HSet(ctx, hashKey(spendledger.HourBucket(at), "k1"), args...).Err(); err != nil {
				t.Fatal(err)
			}
			q := spendledger.Query{GroupBy: spendledger.GroupBySession, Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}
			_, err := s.Query(ctx, q)
			if err == nil {
				t.Fatal("Query over a garbage pair must fail closed")
			}
			if msg := err.Error(); strings.Contains(msg, "SECRET-SESSION") || strings.Contains(msg, "not-a-number") {
				t.Fatalf("a read error quoted the field or its value: %s", msg)
			}
			if err := s.Sweep(ctx, at); err != nil {
				t.Fatal(err)
			}
			rows, err := s.Query(ctx, q)
			if err != nil || len(rows) != 1 || rows[0].Session != "s1" {
				t.Fatalf("after Sweep: rows = %v, err = %v; want only s1", rows, err)
			}
		})
	}
}

// TestOpenNeverFailsOnUnreachableAddr mirrors the sibling Redis stores:
// dialing is lazy, so the gateway can start while Redis is briefly down;
// the first operation reports the failure.
func TestOpenNeverFailsOnUnreachableAddr(t *testing.T) {
	s, err := Open(redis.Options{Addr: "127.0.0.1:1"}, spendledger.Options{})
	if err != nil {
		t.Fatalf("Open() on an unreachable address returned an error = %v, want nil (dialing is lazy)", err)
	}
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err == nil {
		t.Fatal("Record against an unreachable Redis address succeeded, want an error")
	}
}

// TestReadsAndDeletesAreChunked: a Query over many hashes and a Sweep that
// drops many hashes go to Redis in bounded pipelines, so a wide window or a
// long-unswept store never builds one command with hundreds of thousands of
// arguments. The chunk is shrunk to two so five hashes cross two boundaries.
func TestReadsAndDeletesAreChunked(t *testing.T) {
	flush(t)
	ctx := context.Background()
	saved := chunkSize
	chunkSize = 2
	t.Cleanup(func() { chunkSize = saved })
	s := open(t, spendledger.Options{Retention: time.Hour})
	defer func() { _ = s.Close() }()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, entry(fmt.Sprintf("k%d", i), "m", "curl", "s1", at)); err != nil {
			t.Fatal(err)
		}
	}
	q := spendledger.Query{GroupBy: spendledger.GroupByKey, Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}
	rows, err := s.Query(ctx, q)
	if err != nil || len(rows) != 5 {
		t.Fatalf("Query across chunks: %d rows, err %v; want 5", len(rows), err)
	}
	if err := s.Sweep(ctx, at.Add(3*time.Hour)); err != nil { // every hash is older than the retention
		t.Fatal(err)
	}
	rows, err = s.Query(ctx, q)
	if err != nil || len(rows) != 0 {
		t.Fatalf("Query after a chunked Sweep: %d rows, err %v; want none", len(rows), err)
	}
}

// TestSweepRebuildsAKeyWhoseHashesAreGone: when every hour hash of a key is
// already gone (the TTL backstop fired, or an operator DEL) but the key still
// owns derived state, Sweep removes that state, so the next session of the
// key is admitted by name instead of folding against ghosts.
func TestSweepRebuildsAKeyWhoseHashesAreGone(t *testing.T) {
	flush(t)
	ctx := context.Background()
	s := open(t, spendledger.Options{MaxSessionsPerKey: 1})
	defer func() { _ = s.Close() }()
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	if err := c.Del(ctx, hashKey(spendledger.HourBucket(at), "k1")).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sweep(ctx, at); err != nil {
		t.Fatal(err)
	}
	n, err := c.Exists(ctx, sessionsKey("k1"), cellsKey("k1")).Result()
	if err != nil || n != 0 {
		t.Fatalf("derived state after Sweep of a hash-less key: %d keys exist, err %v; want none", n, err)
	}
	if err := s.Record(ctx, entry("k1", "m", "curl", "s2", at)); err != nil {
		t.Fatal(err)
	}
	if got := sessionsOf(t, s, "k1"); got != "s2=1" {
		t.Fatalf("after Sweep freed the ghost slot: %q, want %q", got, "s2=1")
	}
}

// TestAForeignKeyUnderThePrefixIsIgnored: a key named like an hour hash but
// holding another type is not the store's, and neither is a hash whose key
// part carries a raw ':' (the store's key parts are query-escaped, so a raw
// ':' never occurs); reads skip both instead of failing or counting them,
// Sweep leaves them in place, and the real rows are unaffected.
func TestAForeignKeyUnderThePrefixIsIgnored(t *testing.T) {
	flush(t)
	ctx := context.Background()
	s := open(t, spendledger.Options{})
	defer func() { _ = s.Close() }()
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	foreign := hashKey(spendledger.HourBucket(at), "k2")
	if err := c.Set(ctx, foreign, "not a hash", 0).Err(); err != nil {
		t.Fatal(err)
	}
	colon := hashKey(spendledger.HourBucket(at), "k3") + ":x" // a hash, but its key part has a raw ':'
	if err := c.HSet(ctx, colon, "m|curl|s1", "1", "m|curl|s1#n", "1").Err(); err != nil {
		t.Fatal(err)
	}
	q := spendledger.Query{GroupBy: spendledger.GroupByKey, Since: at.Add(-time.Hour), Until: at.Add(time.Hour)}
	rows, err := s.Query(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].Key != "k1" {
		t.Fatalf("Query beside a foreign key: rows = %v, err = %v; want only k1", rows, err)
	}
	if err := s.Sweep(ctx, at); err != nil {
		t.Fatalf("Sweep beside a foreign key: %v", err)
	}
	if n, err := c.Exists(ctx, foreign, colon).Result(); err != nil || n != 2 {
		t.Fatalf("the foreign keys after Sweep: exists = %d, err %v; want both left in place", n, err)
	}
}

// TestRecordFailsClosedOnACorruptCellCount mirrors the bbolt test of the same
// shape: a cell count that is not an integer is an error on the one path that
// reads it (a new counting cell), never a lifted cap; a sessionless entry
// never reads it and still lands.
func TestRecordFailsClosedOnACorruptCellCount(t *testing.T) {
	flush(t)
	ctx := context.Background()
	s := open(t, spendledger.Options{})
	defer func() { _ = s.Close() }()
	if err := s.Record(ctx, entry("k1", "m", "curl", "s1", at)); err != nil {
		t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = c.Close() }()
	if err := c.Set(ctx, cellsKey("k1"), "not-a-count", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, entry("k1", "m2", "curl", "s1", at)); err == nil {
		t.Fatal("Record of a new counting cell with a corrupt cell count succeeded, want an error")
	}
	if err := s.Record(ctx, entry("k1", "m3", "curl", "", at)); err != nil {
		t.Fatalf("a sessionless entry never reads the cell count: %v", err)
	}
}
