// Package ledgertest is the contract every spendledger backend must meet:
// bucket math, the session and row caps, retention, queries, erasure. It is
// a non-test package so the memory and bbolt backends (Redis joins in
// slice 2) share one suite; only tests import it.
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
)

// Factory opens a fresh, empty ledger for one test with the given options.
type Factory func(t *testing.T, opts spendledger.Options) spendledger.Ledger

var (
	hour0 = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	hour1 = hour0.Add(time.Hour)
)

func entry(key, model, tool, session string, at time.Time, nano, requests int64) spendledger.Entry {
	return spendledger.Entry{Key: key, Model: model, Tool: tool, Session: session, At: at, SpendNanoUSD: nano, Requests: requests}
}

func rows(t *testing.T, l spendledger.Ledger, q spendledger.Query) []spendledger.Row {
	t.Helper()
	got, err := l.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query(%+v): %v", q, err)
	}
	return got
}

func describe(rs []spendledger.Row) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s/%s/%s/%s=%d:%d", r.Key, r.Model, r.Tool, r.Session, r.SpendNanoUSD, r.Requests))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// Run executes the contract against the backend f opens.
func Run(t *testing.T, f Factory) {
	ctx := context.Background()
	window := spendledger.Query{Since: hour0.Add(-time.Hour), Until: hour1.Add(2 * time.Hour)}

	t.Run("two writes in one hour sum, writes across the boundary split", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour0.Add(5*time.Minute), 100, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour0.Add(50*time.Minute), 250, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour1.Add(time.Minute), 7, 1)))
		q := window
		q.GroupBy = spendledger.GroupBySession
		got := describe(rows(t, l, q))
		if want := "k1///s1=357:3"; got != want {
			t.Errorf("session rows = %q, want %q", got, want)
		}
		// The boundary split is visible when the window ends before hour1.
		q.Until = hour1
		if got := describe(rows(t, l, q)); got != "k1///s1=350:2" {
			t.Errorf("first hour alone = %q, want k1///s1=350:2", got)
		}
	})

	t.Run("group_by key, model, tool and session aggregate the right dimension", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m1", "curl", "s1", hour0, 10, 1)))
		must(t, l.Record(ctx, entry("k1", "m2", "codex", "s2", hour0, 20, 1)))
		must(t, l.Record(ctx, entry("k2", "m1", "curl", "", hour0, 30, 1)))
		for groupBy, want := range map[string]string{
			spendledger.GroupByKey:     "k1///=30:2 k2///=30:1",
			spendledger.GroupByModel:   "/m1//=40:2 /m2//=20:1",
			spendledger.GroupByTool:    "//codex/=20:1 //curl/=40:2",
			spendledger.GroupBySession: "k1///s1=10:1 k1///s2=20:1 k2///=30:1",
		} {
			q := window
			q.GroupBy = groupBy
			if got := describe(rows(t, l, q)); got != want {
				t.Errorf("group_by %s = %q, want %q", groupBy, got, want)
			}
		}
		// key= narrows every group_by to one key.
		q := window
		q.GroupBy = spendledger.GroupByModel
		q.Key = "k1"
		if got := describe(rows(t, l, q)); got != "k1/m1//=10:1 k1/m2//=20:1" {
			t.Errorf("group_by model key=k1 = %q", got)
		}
	})

	t.Run("rows come sorted by spend, highest first", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m1", "curl", "", hour0, 5, 1)))
		must(t, l.Record(ctx, entry("k1", "m2", "curl", "", hour0, 50, 1)))
		must(t, l.Record(ctx, entry("k1", "m3", "curl", "", hour0, 20, 1)))
		q := window
		q.GroupBy = spendledger.GroupByModel
		got := rows(t, l, q)
		if len(got) != 3 || got[0].Model != "m2" || got[1].Model != "m3" || got[2].Model != "m1" {
			t.Errorf("order = %s", describe(got))
		}
	})

	t.Run("an unknown group_by is an error", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		if _, err := l.Query(ctx, spendledger.Query{GroupBy: "deployment", Since: hour0, Until: hour1}); err == nil {
			t.Error("Query(group_by=deployment) returned no error")
		}
	})

	t.Run("the window clamps to [since, until) by hour bucket", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m", "curl", "", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "", hour1, 2, 1)))
		q := spendledger.Query{GroupBy: spendledger.GroupByKey, Since: hour1, Until: hour1.Add(time.Hour)}
		if got := describe(rows(t, l, q)); got != "k1///=2:1" {
			t.Errorf("window [hour1, hour2) = %q", got)
		}
		q = spendledger.Query{GroupBy: spendledger.GroupByKey, Since: hour0, Until: hour1}
		if got := describe(rows(t, l, q)); got != "k1///=1:1" {
			t.Errorf("window [hour0, hour1) = %q", got)
		}
		q = spendledger.Query{GroupBy: spendledger.GroupByKey, Since: hour0.Add(20 * time.Minute), Until: hour1.Add(30 * time.Minute)}
		if got := describe(rows(t, l, q)); got != "k1///=3:2" {
			t.Errorf("mid-hour bounds select whole buckets: %q", got)
		}
		q = spendledger.Query{GroupBy: spendledger.GroupByKey, Since: hour0}
		if got := describe(rows(t, l, q)); got != "" {
			t.Errorf("a zero until selects nothing: %q", got)
		}
	})

	t.Run("the session after the cap folds into other and the first cap keep their rows", func(t *testing.T) {
		l := f(t, spendledger.Options{MaxSessionsPerKey: 3})
		defer func() { _ = l.Close() }()
		for _, s := range []string{"s1", "s2", "s3", "s4", "s5"} {
			must(t, l.Record(ctx, entry("k1", "m", "curl", s, hour0, 1, 1)))
		}
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour1, 1, 1))) // a known session keeps writing under its own name
		must(t, l.Record(ctx, entry("k2", "m", "curl", "t1", hour0, 1, 1))) // another key has its own budget of sessions
		q := window
		q.GroupBy = spendledger.GroupBySession
		got := describe(rows(t, l, q))
		if want := "k1///other=2:2 k1///s1=2:2 k1///s2=1:1 k1///s3=1:1 k2///t1=1:1"; got != want {
			t.Errorf("session cap = %q, want %q", got, want)
		}
	})

	t.Run("named cells beyond max_rows_per_key fold into the other session of their hour bucket", func(t *testing.T) {
		l := f(t, spendledger.Options{MaxRowsPerKey: 2, MaxSessionsPerKey: 2})
		defer func() { _ = l.Close() }()
		// A sessionless row takes no row slot (it is bounded by the model and
		// tool vocabularies, not by traffic), so recording it first must leave
		// both slots for the named rows that follow.
		must(t, l.Record(ctx, entry("k1", "m5", "curl", "", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m1", "curl", "s1", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m2", "curl", "s2", hour0, 1, 1)))  // the second named row: kept by name
		must(t, l.Record(ctx, entry("k1", "m6", "curl", "s9", hour0, 1, 1)))  // over the session cap: other, no row slot
		must(t, l.Record(ctx, entry("k1", "m3", "curl", "s1", hour0, 1, 1)))  // a live session, a third named row: folded
		must(t, l.Record(ctx, entry("k1", "m4", "codex", "s2", hour1, 1, 1))) // folded into the next hour's other
		must(t, l.Record(ctx, entry("k1", "m1", "curl", "s1", hour0, 1, 1)))  // an existing cell keeps summing
		must(t, l.Record(ctx, entry("k1", "m1", "curl", "s1", hour1, 1, 1)))  // the same row in a later hour is a new cell: folded
		q := window
		q.GroupBy = spendledger.GroupBySession
		got := describe(rows(t, l, q))
		if want := "k1///=1:1 k1///other=4:4 k1///s1=2:2 k1///s2=1:1"; got != want {
			t.Errorf("row cap = %q, want %q", got, want)
		}
		q.GroupBy = spendledger.GroupByModel
		if got := describe(rows(t, l, q)); got != "/m1//=3:3 /m2//=1:1 /m3//=1:1 /m4//=1:1 /m5//=1:1 /m6//=1:1" {
			t.Errorf("folded rows keep their model under other: %q", got)
		}
	})

	t.Run("Sweep removes hours older than the retention and frees their sessions", func(t *testing.T) {
		l := f(t, spendledger.Options{Retention: 2 * time.Hour, MaxSessionsPerKey: 2})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m", "curl", "old", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "keep", hour1.Add(time.Hour), 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "third", hour1.Add(time.Hour), 1, 1))) // over the cap while old is live
		// cutoff = HourBucket(now) - 2 h = hour1 + 1 h exactly: the bucket AT the
		// cutoff is kept (a Query with since = now - retention still reads it);
		// hour0, which starts before the cutoff, goes.
		now := hour1.Add(3*time.Hour + time.Minute)
		must(t, l.Sweep(ctx, now))
		q := spendledger.Query{GroupBy: spendledger.GroupBySession, Since: hour0.Add(-time.Hour), Until: now}
		if got := describe(rows(t, l, q)); got != "k1///keep=1:1 k1///other=1:1" {
			t.Errorf("after sweep = %q", got)
		}
		// The swept session's slot is free: a new session writes under its own name now.
		must(t, l.Record(ctx, entry("k1", "m", "curl", "fresh", now, 1, 1)))
		q.Until = now.Add(time.Hour)
		if got := describe(rows(t, l, q)); got != "k1///fresh=1:1 k1///keep=1:1 k1///other=1:1" {
			t.Errorf("slot freed = %q", got)
		}
	})

	t.Run("DeleteKey removes every row of the key across hours and leaves other keys", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s2", hour1, 1, 1)))
		must(t, l.Record(ctx, entry("k2", "m", "curl", "s1", hour0, 1, 1)))
		must(t, l.DeleteKey(ctx, "k1"))
		q := window
		q.GroupBy = spendledger.GroupBySession
		if got := describe(rows(t, l, q)); got != "k2///s1=1:1" {
			t.Errorf("after DeleteKey = %q", got)
		}
		must(t, l.DeleteKey(ctx, "absent")) // idempotent
	})

	t.Run("DeleteSession removes exactly that session across keys and hours; other is untouched", func(t *testing.T) {
		l := f(t, spendledger.Options{MaxSessionsPerKey: 2})
		defer func() { _ = l.Close() }()
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s1", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k2", "m", "curl", "s1", hour1, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s2", hour0, 1, 1)))
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s3", hour0, 1, 1))) // k1 is at its cap: folded into other
		n, err := l.DeleteSession(ctx, "s1")
		if err != nil || n != 2 {
			t.Fatalf("DeleteSession(s1) = %d, %v; want 2 rows", n, err)
		}
		q := window
		q.GroupBy = spendledger.GroupBySession
		if got := describe(rows(t, l, q)); got != "k1///other=1:1 k1///s2=1:1" {
			t.Errorf("after DeleteSession = %q", got)
		}
		must(t, l.Record(ctx, entry("k1", "m", "curl", "s4", hour0, 1, 1))) // the erased session freed its slot
		if got := describe(rows(t, l, q)); got != "k1///other=1:1 k1///s2=1:1 k1///s4=1:1" {
			t.Errorf("after DeleteSession freed a slot = %q", got)
		}
		if _, err := l.DeleteSession(ctx, spendledger.OtherSession); err == nil {
			t.Error("DeleteSession(other) must be refused")
		}
		if _, err := l.DeleteSession(ctx, ""); err == nil {
			t.Error("DeleteSession(\"\") must be refused")
		}
	})

	t.Run("a session outside the grammar, the reserved name other, a missing request and an over-long dimension are refused by Record", func(t *testing.T) {
		l := f(t, spendledger.Options{})
		defer func() { _ = l.Close() }()
		if err := l.Record(ctx, entry("k1", "m", "curl", "bad|session", hour0, 1, 1)); err == nil {
			t.Error("Record with a session outside the grammar must fail")
		}
		if err := l.Record(ctx, entry("", "m", "curl", "", hour0, 1, 1)); err == nil {
			t.Error("Record with an empty key must fail")
		}
		if err := l.Record(ctx, entry("k1", "m", "curl", spendledger.OtherSession, hour0, 1, 1)); !errors.Is(err, spendledger.ErrSessionReserved) {
			t.Errorf("Record naming the fold target as a session: err = %v, want ErrSessionReserved", err)
		}
		if err := l.Record(ctx, entry("k1", "m", "curl", "s1", hour0, 1, 0)); !errors.Is(err, spendledger.ErrInvalidEntry) {
			t.Errorf("Record with zero requests: err = %v, want ErrInvalidEntry", err)
		}
		if err := l.Record(ctx, entry("k1", strings.Repeat("m", spendledger.MaxDimensionBytes+1), "curl", "", hour0, 1, 1)); !errors.Is(err, spendledger.ErrInvalidEntry) {
			t.Errorf("Record with an over-long model: err = %v, want ErrInvalidEntry", err)
		}
		q := window
		q.GroupBy = spendledger.GroupByKey
		if got := describe(rows(t, l, q)); got != "" {
			t.Errorf("refused entries must leave nothing behind: %q", got)
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
