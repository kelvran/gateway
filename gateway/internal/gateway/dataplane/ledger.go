package dataplane

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/spendledger"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// The spend ledger's write hook (RFC-2 item 13d, slice 3). Every billable
// chat completion and embeddings call records one Entry -- virtual key,
// served model, client tool, session, nano-USD, one request -- under the
// exact `billable` gate that already feeds kelvran.llm.spend_usd, so a cache
// hit or a coalesced singleflight follower never writes a row for spend it
// did not incur. A failed write is one Warn plus
// kelvran.persistence.failed{store_kind=ledger}; it never fails the request.

const (
	// ledgerStoreKind is the kelvran.persistence.store_kind value.
	ledgerStoreKind = "ledger"
	// ledgerSweepTimeout bounds one Sweep; a Redis sweep reads every hour
	// hash in the store.
	ledgerSweepTimeout = 2 * time.Minute
	// agentRunIDDropHeader names the Baggage member in the dropped counter
	// when its value fails the session grammar; not a request header, so
	// the `baggage.` prefix keeps it apart from the x-claude-code-* names.
	agentRunIDDropHeader = "baggage.agent_run_id"
	// sessionIDHeader is the Claude Code header the session comes from.
	sessionIDHeader = "x-claude-code-session-id"
)

// recordLedgerEntry writes one billable call to the ledger. ctx should be a
// settlement context (detached from the client's cancellation).
func (p *Pipeline) recordLedgerEntry(ctx context.Context, keyID, model string, a telemetry.Attribution, cost decimal.Decimal) {
	if p.ledger == nil {
		return
	}
	if err := p.ledger.Record(ctx, p.ledgerEntry(ctx, keyID, model, a, cost)); err != nil {
		p.noteLedgerFailure(ctx, keyID, err)
	}
}

// ledgerEntry is the row one billable request writes: the served model, the
// client tool, the session identifier (dropped when it is not a valid one,
// counted on kelvran.attribution.dropped), the request's time and its cost in
// nano-USD. recordLedgerEntry records it; the fused settlement (settleFused,
// round-5 item 10 phase 2c-2) sends it in one command with the budget's and
// the token settlements instead.
func (p *Pipeline) ledgerEntry(ctx context.Context, keyID, model string, a telemetry.Attribution, cost decimal.Decimal) spendledger.Entry {
	return spendledger.Entry{
		Key:          keyID,
		Model:        model,
		Tool:         a.ClientTool,
		Session:      p.sessionFor(ctx, a),
		At:           p.now(),
		SpendNanoUSD: budget.USDToNanoUSD(cost),
		Requests:     1,
	}
}

// noteLedgerFailure counts and logs a lost ledger write; it never fails the
// request.
func (p *Pipeline) noteLedgerFailure(ctx context.Context, keyID string, err error) {
	telemetry.RecordPersistenceFailed(ctx, ledgerStoreKind, keyID)
	p.logger.Warn("ledger_record_failed", append(traceLogFields(ctx), "key_id", keyID, "error", err.Error())...)
}

// sessionFor is the ledger's session dimension: the Claude Code session id
// when the request carried one, else the agent_run_id Baggage member when it
// satisfies the identifier grammar, else none. A value the ledger cannot
// store by name -- outside the grammar, or equal to the fold target's name
// `other`, which only a fold may write -- is dropped and counted under the
// name it came from, never truncated into a different identifier (a
// truncation could collide with a real session of that name and silently
// attribute one session's spend to another).
func (p *Pipeline) sessionFor(ctx context.Context, a telemetry.Attribution) string {
	if a.SessionID != "" {
		if a.SessionID != spendledger.OtherSession && spendledger.IsSessionIdentifier(a.SessionID) {
			return a.SessionID
		}
		telemetry.RecordAttributionDropped(ctx, sessionIDHeader)
		return ""
	}
	if id := telemetry.AgentRunIDFromContext(ctx); id != "" {
		if id != spendledger.OtherSession && spendledger.IsSessionIdentifier(id) {
			return id
		}
		telemetry.RecordAttributionDropped(ctx, agentRunIDDropHeader)
	}
	return ""
}

// deleteLedgerKey erases a revoked key's rows and derived state on this
// replica (the Redis backend is shared, so there the erasure is global and
// idempotent). Same posture as the budget cascade beside it: a failure is
// logged and counted, never returned. Like budget.Delete it runs under the
// virtual-key mutation lock, so other key mutations wait for it -- bounded
// by settlementTimeout, and in Redis one SCAN of the hour hashes plus
// chunked DELs; an admin operation, not a request-path one.
func (p *Pipeline) deleteLedgerKey(keyID string) {
	if p.ledger == nil {
		return
	}
	ctx, cancel := settlementContext(context.Background())
	defer cancel()
	if err := p.ledger.DeleteKey(ctx, keyID); err != nil {
		telemetry.RecordPersistenceFailed(ctx, ledgerStoreKind, keyID)
		p.logger.Warn("ledger_delete_key_failed", "key_id", keyID, "error", err.Error())
	}
}

// RunLedgerSweepLoop sweeps the ledger once at start (a restart after days
// must drop expired hours at once) and then every interval until ctx ends;
// a no-op with no ledger or a non-positive interval. cmd/gateway runs it
// hourly beside the health-probe loop. A failed sweep is one Warn plus the
// persistence counter (no key); the next tick retries.
func (p *Pipeline) RunLedgerSweepLoop(ctx context.Context, interval time.Duration) {
	if p.ledger == nil || interval <= 0 {
		return
	}
	p.sweepLedger(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweepLedger(ctx)
		}
	}
}

func (p *Pipeline) sweepLedger(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, ledgerSweepTimeout)
	defer cancel()
	if err := p.ledger.Sweep(sweepCtx, p.now()); err != nil {
		telemetry.RecordPersistenceFailed(ctx, ledgerStoreKind, "")
		p.logger.Warn("ledger_sweep_failed", "error", err.Error())
	}
}

// ErrNoSpendLedger is QuerySpend's and EraseSpendSession's answer when the
// pipeline has no ledger (cmd/gateway always wires one; tests may not).
var ErrNoSpendLedger = errors.New("dataplane: no spend ledger is configured")

// SpendRow is one aggregate of the spend ledger in USD: the leaf stores
// nano-USD, and the admin package may not import budget, so the conversion
// happens here, in the package that owns the nano-USD rule's other half.
type SpendRow struct {
	Key, Model, Tool, Session string
	SpendUSD                  decimal.Decimal
	Requests                  int64
}

// QuerySpend answers GET /admin/spend: the ledger's rows for q, converted to
// USD. The window, the group_by and the key are the caller's; the ledger
// validates group_by.
func (p *Pipeline) QuerySpend(ctx context.Context, q spendledger.Query) ([]SpendRow, error) {
	if p.ledger == nil {
		return nil, ErrNoSpendLedger
	}
	rows, err := p.ledger.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]SpendRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, SpendRow{Key: r.Key, Model: r.Model, Tool: r.Tool, Session: r.Session, SpendUSD: budget.NanoUSDToUSD(r.SpendNanoUSD), Requests: r.Requests})
	}
	return out, nil
}

// EraseSpendSession answers POST /admin/spend/erase: the session's cells go
// from every key and hour on this replica's ledger (the shared store in Redis
// mode); it returns how many went. The ledger refuses "" and `other`.
func (p *Pipeline) EraseSpendSession(ctx context.Context, session string) (int, error) {
	if p.ledger == nil {
		return 0, ErrNoSpendLedger
	}
	return p.ledger.DeleteSession(ctx, session)
}
