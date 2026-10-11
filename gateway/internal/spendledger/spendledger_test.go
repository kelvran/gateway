package spendledger_test

import (
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/spendledger/ledgertest"
)

// TestHourBucketFloorsToUTC: the ledger's unit is the UTC hour; a time in
// any zone floors to the same instant.
func TestHourBucketFloorsToUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	at := time.Date(2026, 10, 11, 23, 45, 12, 0, ist) // 18:15:12 UTC
	got := spendledger.HourBucket(at)
	want := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("HourBucket(%v) = %v, want %v UTC", at, got, want)
	}
	if spendledger.HourBucket(want) != want {
		t.Error("an hour boundary floors to itself")
	}
}

// TestIsSessionIdentifier: the identifier grammar the ledger keys and the
// erase route share — 1 to 128 bytes of [A-Za-z0-9._:-]; the delimiters
// the stores use (`|`, `#`) and anything non-ASCII are refused.
func TestIsSessionIdentifier(t *testing.T) {
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	cases := map[string]bool{
		"sess-1": true, "a.b:c_d-E9": true, string(long[:128]): true, "other": true,
		"": false, string(long): false, "with space": false, "pipe|y": false, "hash#n": false, "ünïcode": false, "tab\tx": false,
	}
	for in, want := range cases {
		if got := spendledger.IsSessionIdentifier(in); got != want {
			t.Errorf("IsSessionIdentifier(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestOptionsDefaults: absent options take the RFC-2 / G29 defaults. The leaf
// substitutes the default for a zero or negative value; the config layer
// (slice 3) is what rejects `retention_hours: 0` at load time.
func TestOptionsDefaults(t *testing.T) {
	o := spendledger.Options{}.WithDefaults()
	if o.Retention != 168*time.Hour || o.MaxSessionsPerKey != 2000 || o.MaxRowsPerKey != 50000 {
		t.Errorf("defaults = %+v", o)
	}
	o = spendledger.Options{Retention: 2 * time.Hour, MaxSessionsPerKey: 3, MaxRowsPerKey: 10}.WithDefaults()
	if o.Retention != 2*time.Hour || o.MaxSessionsPerKey != 3 || o.MaxRowsPerKey != 10 {
		t.Errorf("explicit options changed: %+v", o)
	}
}

// TestCellKeyEncodingRoundTripsAndEscapesTheDelimiter: a `|` inside a model
// name cannot forge a different row.
func TestCellKeyEncodingRoundTripsAndEscapesTheDelimiter(t *testing.T) {
	in := spendledger.CellKey{Key: "k", Model: "m|odel", Tool: "curl", Session: "s"}
	enc := in.Encode()
	if enc != "k|m%7Codel|curl|s" {
		t.Errorf("Encode = %q", enc)
	}
	out, err := spendledger.DecodeCellKey(enc)
	if err != nil || out != in {
		t.Errorf("DecodeCellKey = %+v, %v", out, err)
	}
	if _, err := spendledger.DecodeCellKey("too|few|parts"); err == nil {
		t.Error("a malformed key must not decode")
	}
}

// TestMemoryLedgerContract runs the backend-independent contract against
// the in-memory ledger.
func TestMemoryLedgerContract(t *testing.T) {
	ledgertest.Run(t, func(_ *testing.T, opts spendledger.Options) spendledger.Ledger { return spendledger.NewMemory(opts) })
}
