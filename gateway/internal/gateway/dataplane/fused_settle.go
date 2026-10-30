package dataplane

import (
	"context"
	"errors"

	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/budget"
	"github.com/kelvran/gateway/gateway/internal/identity"
	"github.com/kelvran/gateway/gateway/internal/ratelimit"
	"github.com/kelvran/gateway/gateway/internal/redisfused"
	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

// FusedSettler settles a finished chat request's three Redis writes -- the
// budget adjust, the token settlement and the ledger record -- in one Redis
// command (round-5 item 10 phase 2c-2, internal/redisfused). Optional: nil
// (the default) keeps the three standalone calls. cmd/gateway builds one only
// when the rate limiter, the budget and the ledger share one Redis client, and
// NewPipeline refuses one over a limiter or tracker that is not Redis-backed. Embeddings,
// mid-stream top-ups and the alert marks never use it.
//
// Settle never returns a call-level error: every failure is on the part it
// belongs to, and a request is never failed by a lost settlement.
type FusedSettler interface {
	Settle(ctx context.Context, req redisfused.Request) redisfused.Result
	// Close releases the settler's hold on the shared Redis client.
	Close() error
}

// fusedInputs is what finalize knows when the request is over, for settleFused.
type fusedInputs struct {
	vk *identity.VirtualKey
	// reqModel is the model the client asked for (the rate limiter's per-model
	// key); servedModel the one that served it (the ledger's cell).
	reqModel, servedModel string
	// cost is the priced cost of the response (the ledger entry's spend).
	cost decimal.Decimal
	// realCost is non-nil exactly when the request is billable; realTokens
	// likewise.
	realCost    *decimal.Decimal
	realTokens  *float64
	attribution telemetry.Attribution

	budgetReserved      bool
	budgetReservedUSD   decimal.Decimal
	budgetEpoch         int64
	tpmReserved         bool
	tpmReservedTokens   float64
	tpmReservationEpoch int64
	tpd                 tokenReservation
}

// settleFused is finalize's three standalone settlement calls as one
// FusedSettler call, with each part gated and mapped exactly as it was:
//
//   - budget: runs when a reservation is held or the request billed; a lost
//     write is counted and logged (NoteAdjustFailure) and leaves spendKnown
//     false, which skips the threshold checks; the spend the settlement reports
//     is what they read.
//   - tokens: the dimensions KeyLimiter.PlanSettle plans; a failure is silent,
//     as SettleTokens's error always was.
//   - ledger: one entry for a billable request, planned here (once, so a
//     dropped session is counted once); a failure is counted and logged
//     (noteLedgerFailure).
//
// ledgerHandled reports that the ledger entry travelled with the call, so
// finalize does not record it a second time. A failed call is never retried
// on the standalone scripts: an error cannot prove nothing was applied.
func (p *Pipeline) settleFused(ctx context.Context, in fusedInputs) (settledSpend decimal.Decimal, spendKnown, ledgerHandled bool) {
	vk := in.vk
	var req redisfused.Request
	if in.budgetReserved || in.realCost != nil {
		delta, billed := budget.PlanReconcile(in.budgetReservedUSD, in.realCost)
		req.Budget = &redisfused.BudgetPart{KeyID: vk.ID, DeltaNanoUSD: delta, Epoch: in.budgetEpoch, Billed: billed}
	}
	req.Tokens = p.limiter.PlanSettle(vk.ID, in.reqModel,
		ratelimit.TokenReservation{Reserved: in.tpmReserved, Tokens: in.tpmReservedTokens, Epoch: in.tpmReservationEpoch},
		ratelimit.TokenReservation{Reserved: in.tpd.reserved, Tokens: in.tpd.tokens, Epoch: in.tpd.epoch},
		in.realTokens)
	if in.realCost != nil && p.ledger != nil {
		entry := p.ledgerEntry(ctx, vk.ID, in.servedModel, in.attribution, in.cost)
		req.Ledger = &entry
		ledgerHandled = true
	}

	res := p.fused.Settle(ctx, req)

	if req.Budget != nil {
		if res.Budget.Status == redisfused.StatusApplied {
			settledSpend, spendKnown = budget.NanoUSDToUSD(res.SpentAfterNanoUSD), true
		} else {
			p.budget.NoteAdjustFailure(ctx, vk.ID, partError(res.Budget, "budget"))
		}
	}
	if req.Ledger != nil && res.Ledger.Status != redisfused.StatusApplied {
		p.noteLedgerFailure(ctx, vk.ID, partError(res.Ledger, "ledger"))
	}
	return settledSpend, spendKnown, ledgerHandled
}

// partError is the error of a part that did not apply; a settler that reports
// a requested part as failed or skipped without saying why still gets one.
func partError(p redisfused.Part, name string) error {
	if p.Err != nil {
		return p.Err
	}
	return errors.New("fused settlement did not apply the " + name + " part")
}

// budgetThresholdChecks runs the warn and alert-ladder checks on the spend a
// settlement just reported. They run only for a billable request whose
// settlement reported a spend; a lost settlement (spendKnown false, logged and
// counted where it was lost) skips both for this request: the spend is unknown
// and the next billable request evaluates them. The checks read the reported
// spend, not Redis (round-5 item 10 phase 2a, 2026-10-16); the marks they set
// stay standalone scripts, one round trip each, over the settlement context so
// a client disconnect did not read 0.
func (p *Pipeline) budgetThresholdChecks(ctx context.Context, vk *identity.VirtualKey, realCost *decimal.Decimal, settledSpend decimal.Decimal, spendKnown bool) {
	if realCost != nil && spendKnown {
		p.checkBudgetWarnThreshold(ctx, vk, settledSpend)
		p.checkBudgetAlertLadder(ctx, vk, settledSpend)
	}
	if realCost != nil && !spendKnown {
		// FAILURE-MODES R7's greppable twin: the settlement was lost, so
		// the thresholds were not evaluated for this request.
		p.logger.Info("budget_threshold_checks_skipped", append(traceLogFields(ctx), "key_id", vk.ID, "reason", "settlement_lost")...)
	}
}
