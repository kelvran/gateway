package dataplane

import (
	"context"
	"time"
)

// settlementTimeout bounds the deferred settlement calls a request makes
// after its outcome is decided: budget.Reconcile, limiter.ReconcileTPM and
// the two budget threshold checks (Redis reads and alert de-dup writes); all
// of them share the one window, so a hung backend holds the handler for at
// most this long in total.
// Five seconds comfortably covers a healthy Redis round trip and a go-redis
// dial failure, and keeps a hung backend from pinning the handler goroutine
// for longer than the request itself was allowed to take.
const settlementTimeout = 5 * time.Second

// settlementContext returns the context the deferred settlement writes run
// under: the request's values (trace context, baggage) without its
// cancellation, bounded by settlementTimeout.
//
// Why not the request context itself: a client that disconnects cancels it,
// and go-redis fails fast on a cancelled context. In Redis budget mode the
// pre-call reservation is the key's FULL remaining headroom, durably written,
// so a cancelled Reconcile left it debited and the key stayed locked at its
// cap for the rest of its window (for good, under a lifetime cap) -- with
// Redis perfectly healthy. Found 2026-10-08 while writing
// docs/operations/FAILURE-MODES.md; streaming requests, where a client
// disconnect after the first chunk is routine, were the realistic trigger.
// Redis-mode TPM has the same shape (the reservation decays within
// tpm_capacity / tpm_refill_per_second seconds rather than a budget window).
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settlementTimeout)
}
