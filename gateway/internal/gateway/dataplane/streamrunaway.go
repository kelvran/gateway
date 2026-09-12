package dataplane

import (
	"github.com/shopspring/decimal"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/costaccounting"
	"github.com/kelvran/gateway/gateway/internal/identity"
)

// Mid-stream runaway-completion guard, per
// docs/rfcs/2026-09-08-gateway-streaming-runaway-completion-guard.md.
//
// dataplane.go's checkRateLimit/budget.Reserve and streaming.go's
// HandleChatCompletionStream call limiter.ReserveTPM/budget.Reserve exactly
// ONCE, before a streamed request begins, and finalize's ReconcileTPM/
// Reconcile (dataplane.go, run via defer) run exactly ONCE, after the
// entire stream has finished. Between those two points, a single streaming
// request could otherwise run for an arbitrarily long time and generate an
// arbitrarily large completion with zero mid-flight check against what was
// ever reserved for it — distinct from the concurrent-request TOCTOU races
// docs/rfcs/2026-09-08-gateway-budget-ratelimit-toctou-fix.md already
// closed (that fix bounds N concurrent requests against each other; this
// gap is about ONE already-approved request's own real cost vastly
// exceeding its own reservation).
//
// docs/rfcs/2026-09-05-gateway-tpm-rate-limit.md already established, and
// this file does not revisit, that no tokenizer or token-count estimator
// exists anywhere in this codebase — real usage is only known from a
// provider's own final usage frame, if one ever arrives. This guard
// therefore cannot count real tokens. It instead checks a conservative,
// provider-agnostic PROXY — accumulated output character length (see
// streamaccumulator.go's totalContentLen, the only thing genuinely
// available as chunks arrive) — against a character-length ceiling derived
// from information already known at request time.

const (
	// streamRunawayCharsPerToken is a conservative, provider-agnostic
	// PROXY for token density — explicitly NOT a real token count. ~4
	// characters per token is the commonly cited heuristic for English
	// text (e.g. OpenAI's own public tokenizer guidance: "a helpful rule
	// of thumb is that one token generally corresponds to ~4 characters
	// of text for common English text"). This constant carries the exact
	// same "proxy, not precision" caveat docs/rfcs/2026-09-05-gateway-
	// tpm-rate-limit.md already states for TPM — this guard does not
	// attempt to solve the missing-tokenizer problem any more precisely
	// than that RFC already declined to.
	streamRunawayCharsPerToken = 4

	// streamRunawayMaxTokensMultiplier is how far past
	// req.MaxTokens*streamRunawayCharsPerToken a streamed completion may
	// grow, when the client set MaxTokens at all, before the guard trips.
	// Deliberately generous — 10x, not 2x or 3x — for two independent
	// reasons, either of which alone would justify real slack:
	//
	//  1. The chars-per-token proxy is itself imprecise. Code-heavy,
	//     CJK, or otherwise non-average-English content can legitimately
	//     run at a very different density than 4 chars/token in either
	//     direction; a tight multiplier would false-positive on ordinary,
	//     legitimate traffic simply shaped differently than the
	//     heuristic assumes.
	//  2. A well-behaved upstream provider already enforces MaxTokens as
	//     ITS OWN hard cap on completion length — this guard is
	//     defense-in-depth against a misbehaving/malicious upstream (or a
	//     provider bug) that doesn't, not the primary enforcement
	//     mechanism for the ordinary case.
	//
	// 10x sits comfortably past any plausible combination of both slack
	// sources while still bounding a genuinely runaway stream to a small,
	// finite multiple of what was actually asked for, rather than leaving
	// it fully unbounded.
	streamRunawayMaxTokensMultiplier = 10

	// streamRunawayAbsoluteCharsCeiling is the fallback ceiling applied
	// when the client set no MaxTokens at all — there is then no
	// per-request baseline to multiply, so a fixed absolute floor is
	// used instead. Sized comfortably above the largest legitimate
	// SINGLE completion this gateway's configured providers can
	// plausibly produce — a large generated-code-file response, not
	// merely an "average" chat reply: as of this writing, the largest
	// publicly documented single-completion output-token limits among
	// providers this codebase has adapters for (openai, anthropic,
	// gemini, bedrock, openaicompat) sit in the ~100K-128K output-token
	// range (e.g. Anthropic's extended-output mode, OpenAI's larger
	// reasoning-output models). At streamRunawayCharsPerToken's 4
	// chars/token, 128,000 tokens is 512,000 characters; this constant
	// rounds up further, to 750,000 characters (~187,500 tokens at the
	// same proxy), so it sits comfortably above even that ceiling rather
	// than merely just above an "average" response.
	streamRunawayAbsoluteCharsCeiling = 750_000
)

// streamRunawayCharsCeiling computes the mid-stream runaway guard's
// character-length ceiling for one request, per the constants above.
// maxTokens is req.MaxTokens (adapter.ChatRequest's own field) — nil or
// <= 0 (the client set no completion-length cap) falls back to the fixed
// absolute ceiling; any positive value scales the ceiling to that specific
// request's own declared expectation instead.
func streamRunawayCharsCeiling(maxTokens *int) int {
	if maxTokens == nil || *maxTokens <= 0 {
		return streamRunawayAbsoluteCharsCeiling
	}
	return *maxTokens * streamRunawayCharsPerToken * streamRunawayMaxTokensMultiplier
}

// midStreamReservation bundles what checkMidStreamReservationTopup needs,
// threaded through the same streamDeploymentWithFallback/
// streamDeploymentWithCapacityCheck/streamDeployment/
// streamDeploymentBedrock call chain firstChunkSent/keyID already are —
// per docs/upgrade-research/gateway-streaming-concurrent-sibling-
// reservation-gap-2026-09-09.md, the fix for the gap this codebase's own
// TOCTOU-fix RFC (2026-09-08) had explicitly, honestly left open: a
// streaming request's real cost/tokens can grow well past its own
// initial reservation over the FULL DURATION of the stream, silently
// understating its true claim on the key's headroom to a concurrent
// sibling for far longer than one HTTP round-trip.
//
// BudgetReservedUSD/TPMReservedTokens are POINTERS into
// HandleChatCompletionStream's own local variables — already closed over
// by that function's deferred finalize call. Mutating them here (via a
// successful top-up) is what lets that same deferred call reconcile the
// FINAL, topped-up amount once the stream ends, never the stale
// pre-stream estimate; see budget.Tracker.IncreaseReservation's own doc
// comment for the "caller must use whichever value this returns" contract
// that makes this safe.
type midStreamReservation struct {
	vk                *identity.VirtualKey
	budgetReservedUSD *decimal.Decimal
	// budgetReservationEpoch is a pointer into HandleChatCompletionStream's
	// own budgetReservationEpoch local, mirroring budgetReservedUSD's
	// identical closed-over-pointer shape — updated on every successful
	// top-up (see budget.Tracker.IncreaseReservation's own epoch-contract
	// doc comment) so the eventual finalize/Reconcile call at stream end
	// uses whichever epoch the LAST top-up actually observed, never the
	// stale pre-stream one.
	budgetReservationEpoch *int64
	tpmReservedTokens      *float64
}

// checkMidStreamReservationTopup is the mid-stream reservation top-up
// check: converts accumulatedChars (the SAME character-length proxy
// streamRunawayCharsCeiling already uses — one shared, consistent proxy,
// never two independently-invented ones) into an estimated completion-
// token count, then attempts to raise msr's own outstanding TPM and
// budget reservations to match that estimate whenever it has grown past
// what's currently reserved.
//
// The budget dimension's estimate deliberately uses ONLY the completion-
// token estimate (costaccounting.Usage.PromptTokens left at its zero
// value) — the original pre-stream Reserve call already sized its
// reservation to cover a typical TOTAL cost (prompt included, via its
// own historical-average-or-full-headroom estimate); this top-up's job
// is narrower: catching OUTPUT length growing beyond what that original
// estimate anticipated, not re-deriving the whole request's cost from
// scratch. Comparing completion-only cost against the full prior
// reservation is a deliberately conservative (trips later, not earlier,
// than a "total cost so far" estimate would) choice, consistent with
// streamRunawayMaxTokensMultiplier's own "generous slack, real signal
// only" philosophy right above.
//
// Called once per decoded chunk batch, same position the runaway-
// completion-ceiling check already occupies — deliberately NOT rate-
// limited/throttled to less-than-every-chunk: IncreaseReservation/
// IncreaseReservationTPM are both cheap no-ops (no lock acquired) unless
// there's a genuine increase to apply, so the expensive, lock-acquiring
// path only actually runs when real growth has occurred, not on every
// chunk regardless. If this ever proves to be real, measured overhead
// for an unusually chunk-dense stream, that's a separate, evidence-driven
// follow-up — not preemptively optimized here.
//
// Returns true if the request may continue (nothing needed topping up
// yet, or every top-up attempted this call succeeded); false the moment
// either dimension's top-up is rejected (the delta would exceed the
// key's own remaining budget/TPM headroom) — the caller MUST then stop
// the stream exactly like a runaway-completion-guard trip: cancel
// upstreamCtx and finish gracefully via finishStreamedResponse, never as
// an error, since this request already legitimately passed its own
// initial admission check before the stream ever started.
func (p *Pipeline) checkMidStreamReservationTopup(dep Deployment, req adapter.ChatRequest, accumulatedChars int, msr midStreamReservation) bool {
	estimatedTokens := float64(accumulatedChars) / float64(streamRunawayCharsPerToken)

	if estimatedTokens > *msr.tpmReservedTokens {
		allowed, applied := p.limiter.IncreaseReservationTPM(msr.vk.ID, req.Model, *msr.tpmReservedTokens, estimatedTokens)
		*msr.tpmReservedTokens = applied
		if !allowed {
			return false
		}
	}

	estimatedCostUSD := p.costCalc.Calculate(realServingModel(dep, req.Model), costaccounting.Usage{
		CompletionTokens: int(estimatedTokens),
	})
	if estimatedCostUSD.GreaterThan(*msr.budgetReservedUSD) {
		allowed, applied, newEpoch := p.budget.IncreaseReservation(msr.vk.ID, msr.vk.BudgetUSD, *msr.budgetReservedUSD, estimatedCostUSD, *msr.budgetReservationEpoch, msr.vk.BudgetResetInterval)
		*msr.budgetReservedUSD = applied
		*msr.budgetReservationEpoch = newEpoch
		if !allowed {
			return false
		}
	}

	return true
}
