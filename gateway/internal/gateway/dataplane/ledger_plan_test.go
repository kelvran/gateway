package dataplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// Round-5 item 10 phase 2c-1: recordLedgerEntry is ledgerEntry (builds the
// row) plus noteLedgerFailure (counts and logs a lost write), so a fused
// settlement over a shared Redis client reuses both and the two-step path
// cannot drift from it.
func TestLedgerEntryIsTheRowTheLedgerRecords(t *testing.T) {
	at := time.Date(2026, 10, 23, 10, 30, 0, 0, time.UTC)
	p := &Pipeline{logger: discardLogger(), now: func() time.Time { return at }}
	a := telemetry.Attribution{ClientTool: "claude-code", SessionID: "sess-1"}
	got := p.ledgerEntry(context.Background(), "k", "gpt-4o", a, decimal.RequireFromString("0.5"))
	want := spendledger.Entry{Key: "k", Model: "gpt-4o", Tool: "claude-code", Session: "sess-1", At: at, SpendNanoUSD: 500_000_000, Requests: 1}
	if got != want {
		t.Fatalf("ledgerEntry = %+v, want %+v", got, want)
	}
	// An invalid session identifier is dropped, not recorded.
	bad := p.ledgerEntry(context.Background(), "k", "gpt-4o", telemetry.Attribution{SessionID: "has spaces!"}, decimal.NewFromInt(1))
	if bad.Session != "" {
		t.Fatalf("session = %q, want it dropped", bad.Session)
	}
}

func TestNoteLedgerFailureLogsTheFailure(t *testing.T) {
	var buf bytes.Buffer
	p := &Pipeline{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	p.noteLedgerFailure(context.Background(), "k", errors.New("boom"))
	if out := buf.String(); !strings.Contains(out, "ledger_record_failed") || !strings.Contains(out, "key_id=k") || !strings.Contains(out, "boom") {
		t.Fatalf("log = %q, want ledger_record_failed with the key and the error", out)
	}
}
