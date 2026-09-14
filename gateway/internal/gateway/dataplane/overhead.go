package dataplane

import (
	"context"
	"time"
)

// overheadContextKey is a private type so no other package can construct
// a colliding context key — the same convention every context-value key
// in this codebase already follows.
type overheadContextKey struct{}

// WithOverheadTracker returns a context carrying a pointer runMissPath
// writes the cumulative real-upstream-call duration into internally, per
// docs/rfcs/2026-09-14-gateway-overhead-duration-header.md. Call
// UpstreamDurationFromContext after HandleChatCompletion returns to read
// the final value — never during, since nothing synchronizes a
// concurrent read against runMissPath's own write (this is safe only
// because exactly one goroutine both creates the context and reads the
// result back, after the call that populates it has already returned).
//
// A cache hit never calls runMissPath at all, so the returned pointer
// stays at its zero value (0) — correctly reporting "no real upstream
// call happened for this request," not a fabricated measurement.
func WithOverheadTracker(ctx context.Context) (context.Context, *time.Duration) {
	d := new(time.Duration)
	return context.WithValue(ctx, overheadContextKey{}, d), d
}

// upstreamDurationPointerFromContext returns the pointer
// WithOverheadTracker stored on ctx, or nil if the caller never called
// WithOverheadTracker at all (e.g. a test driving HandleChatCompletion
// directly with a bare context) — runMissPath's own write is a no-op in
// that case, never a nil-pointer panic.
func upstreamDurationPointerFromContext(ctx context.Context) *time.Duration {
	d, _ := ctx.Value(overheadContextKey{}).(*time.Duration)
	return d
}
