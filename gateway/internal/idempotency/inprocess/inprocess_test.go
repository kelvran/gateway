package inprocess

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/idempotency"
)

func fp(b byte) [32]byte {
	var f [32]byte
	f[0] = b
	return f
}

func TestClaimGrantsOwnershipOnFirstCall(t *testing.T) {
	s := New()
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if result.State != idempotency.StateNew {
		t.Errorf("State = %v, want StateNew", result.State)
	}
	if result.Token == 0 {
		t.Error("Token = 0, want a real, non-zero token for a StateNew claim")
	}
}

func TestClaimReturnsInFlightForAConcurrentSecondCallerBeforeCompletion(t *testing.T) {
	s := New()
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if result.State != idempotency.StateInFlight {
		t.Fatalf("State = %v, want StateInFlight", result.State)
	}
	select {
	case <-result.Done:
		t.Error("Done is already closed, want it open until Complete/Fail is called")
	default:
	}
}

func TestClaimReturnsCompletedResponseForAReplayAfterCompletion(t *testing.T) {
	s := New()
	first, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	want := []byte(`{"id":"chatcmpl-1"}`)
	if err := s.Complete(context.Background(), "key-1", first.Token, want); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("replay Claim: %v", err)
	}
	if result.State != idempotency.StateCompleted {
		t.Fatalf("State = %v, want StateCompleted", result.State)
	}
	if string(result.Response) != string(want) {
		t.Errorf("Response = %q, want %q", result.Response, want)
	}
}

// TestClaimReturnsCompletedForAWaiterAfterTheInFlightCallCompletes proves
// the actual "wait, then get the real result" property, not just the two
// static Claim outcomes above in isolation.
func TestClaimReturnsCompletedForAWaiterAfterTheInFlightCallCompletes(t *testing.T) {
	s := New()
	first, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("waiter Claim: %v", err)
	}
	if result.State != idempotency.StateInFlight {
		t.Fatalf("State = %v, want StateInFlight", result.State)
	}

	want := []byte(`{"id":"chatcmpl-1"}`)
	go func() {
		_ = s.Complete(context.Background(), "key-1", first.Token, want)
	}()

	select {
	case <-result.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed after Complete")
	}

	final, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("post-wait Claim: %v", err)
	}
	if final.State != idempotency.StateCompleted || string(final.Response) != string(want) {
		t.Errorf("post-wait Claim = %+v, want StateCompleted with %q", final, want)
	}
}

func TestClaimRejectsAFingerprintMismatch(t *testing.T) {
	s := New()
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	_, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if !errors.Is(err, idempotency.ErrFingerprintMismatch) {
		t.Errorf("err = %v, want ErrFingerprintMismatch", err)
	}
}

func TestClaimExpiresAfterTTL(t *testing.T) {
	s := New()
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Millisecond); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	result, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("Claim after TTL expiry: %v", err)
	}
	if result.State != idempotency.StateNew {
		t.Errorf("State after TTL expiry = %v, want StateNew (an expired entry must not cause a fingerprint mismatch against a DIFFERENT new request)", result.State)
	}
}

func TestFailReleasesTheClaimForRetry(t *testing.T) {
	s := New()
	first, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := s.Fail(context.Background(), "key-1", first.Token); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	result, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("Claim after Fail: %v", err)
	}
	if result.State != idempotency.StateNew {
		t.Errorf("State after Fail = %v, want StateNew (a failed attempt releases the key for a fresh retry, even under a different fingerprint)", result.State)
	}
}

// TestFailWakesAConcurrentWaiter proves a waiter blocked on Done doesn't
// hang forever when the in-flight attempt fails instead of completing.
func TestFailWakesAConcurrentWaiter(t *testing.T) {
	s := New()
	first, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("waiter Claim: %v", err)
	}

	go func() {
		_ = s.Fail(context.Background(), "key-1", first.Token)
	}()

	select {
	case <-result.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed after Fail")
	}
}

func TestCompleteOnUnknownKeyReturnsAnError(t *testing.T) {
	s := New()
	if err := s.Complete(context.Background(), "never-claimed", 1, []byte("x")); err == nil {
		t.Error("Complete on an unclaimed key returned nil error, want an error")
	}
}

func TestFailOnUnknownKeyIsANoOp(t *testing.T) {
	s := New()
	if err := s.Fail(context.Background(), "never-claimed", 1); err != nil {
		t.Errorf("Fail on an unclaimed key = %v, want nil", err)
	}
}

// TestSweepWakesAnAbandonedWaiterInsteadOfLeavingItToHangUntilItsOwnContextDeadline
// is the regression proof for a live 2026-09-16 adversarial-audit finding:
// sweepExpiredLocked used to delete an expired, never-completed entry
// WITHOUT closing its done channel, so any caller already parked on
// ClaimResult.Done (via a StateInFlight Claim) would hang until its own
// ctx.Done() rather than being woken promptly to retry. This proves the
// waiter wakes almost immediately after the owner's ttl elapses, well
// before any generous context deadline would have fired instead.
func TestSweepWakesAnAbandonedWaiterInsteadOfLeavingItToHangUntilItsOwnContextDeadline(t *testing.T) {
	s := New()
	if _, err := s.Claim(context.Background(), "key-1", fp(1), 20*time.Millisecond); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	waiter, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("waiter Claim: %v", err)
	}
	if waiter.State != idempotency.StateInFlight {
		t.Fatalf("waiter State = %v, want StateInFlight", waiter.State)
	}

	// Claim itself never blocks (confirmed directly by reading the
	// source: its select always has a default case) — the sweep that
	// closes an abandoned entry's done only ever runs at the top of SOME
	// Claim call, on ANY key, per sweepExpiredLocked's own doc comment.
	// A real caller (dataplane.go's claimIdempotency) relies on some
	// OTHER concurrent request eventually making that next call; drive
	// the same thing here with an unrelated key.
	stopSweeping := make(chan struct{})
	defer close(stopSweeping)
	go func() {
		for {
			select {
			case <-stopSweeping:
				return
			default:
			}
			_, _ = s.Claim(context.Background(), "unrelated-key", fp(0), time.Minute)
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-waiter.Done:
		// Good — woken by the sweep, not by a context deadline.
	case <-time.After(2 * time.Second):
		t.Fatal("waiter's Done never closed after the original claim's ttl elapsed, even with a concurrent unrelated Claim call driving the sweep — an abandoned claim orphans every waiter until its own context deadline")
	}
}

// TestClaimRetriesInsteadOfFabricatingACompletedResponseAfterAnAbandonmentWake
// proves the OTHER half of the sweep-wakes-waiters fix: a Claim call woken
// by an abandonment (not a real Complete) must never report StateCompleted
// with a stale/empty response — it must retry and become the new owner.
func TestClaimRetriesInsteadOfFabricatingACompletedResponseAfterAnAbandonmentWake(t *testing.T) {
	s := New()
	if _, err := s.Claim(context.Background(), "key-1", fp(1), 20*time.Millisecond); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	waiter, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("waiter Claim: %v", err)
	}

	stopSweeping := make(chan struct{})
	defer close(stopSweeping)
	go func() {
		for {
			select {
			case <-stopSweeping:
				return
			default:
			}
			_, _ = s.Claim(context.Background(), "unrelated-key", fp(0), time.Minute)
			time.Sleep(time.Millisecond)
		}
	}()

	select {
	case <-waiter.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter's Done never closed")
	}

	// This is what a real caller (dataplane.go's claimIdempotency) does
	// immediately after Done closes: re-Claim with the SAME fingerprint
	// (the same request body) it always used. Must retry to a fresh
	// StateNew, never fabricate a StateCompleted with a stale/empty
	// response — no real Complete ever ran for this key.
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("re-Claim: %v", err)
	}
	if result.State != idempotency.StateNew {
		t.Fatalf("re-Claim State = %v (Response=%q), want StateNew", result.State, result.Response)
	}
}

// TestCompleteCalledTwiceIsANoOpNotAPanic is the regression proof for a
// live 2026-09-16 adversarial-audit finding: unlike Fail (which deletes
// its entry before closing done, making a repeat call safe), Complete
// used to leave the entry in place and call close(e.done) unconditionally
// — a second Complete call for the same still-present entry would panic
// with "close of closed channel."
func TestCompleteCalledTwiceIsANoOpNotAPanic(t *testing.T) {
	s := New()
	first, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := s.Complete(context.Background(), "key-1", first.Token, []byte("first")); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if err := s.Complete(context.Background(), "key-1", first.Token, []byte("second")); err != nil {
		t.Fatalf("second Complete (same token) = %v, want nil (a no-op, not a panic or an error)", err)
	}

	// The FIRST response must survive — a second Complete call must
	// never overwrite an already-resolved claim's own stored response.
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("replay Claim: %v", err)
	}
	if string(result.Response) != "first" {
		t.Errorf("Response = %q, want %q (the second Complete call must not have overwritten it)", result.Response, "first")
	}
}

// TestStaleTokenFromASweptClaimCannotResolveANewerClaimForTheSameKey is
// the regression proof for a live 2026-09-16 adversarial-audit finding:
// Complete/Fail used to look up an entry by its bare string key alone,
// with no ownership check — so a caller whose OWN claim had already been
// swept as abandoned and replaced by a completely different, newer claim
// for the same key could incorrectly resolve that unrelated newer claim.
func TestStaleTokenFromASweptClaimCannotResolveANewerClaimForTheSameKey(t *testing.T) {
	s := New()
	original, err := s.Claim(context.Background(), "key-1", fp(1), time.Millisecond)
	if err != nil {
		t.Fatalf("original Claim: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // let the original claim's ttl elapse

	newer, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("newer Claim: %v", err)
	}
	if newer.State != idempotency.StateNew {
		t.Fatalf("newer Claim State = %v, want StateNew (the original's ttl already elapsed)", newer.State)
	}
	if newer.Token == original.Token {
		t.Fatalf("newer.Token == original.Token (%d) — tokens must be distinct per claim", newer.Token)
	}

	// The ORIGINAL (now-stale) caller's own deferred Complete finally
	// runs, using its OWN (stale) token — this must NOT resolve the
	// newer claim.
	if err := s.Complete(context.Background(), "key-1", original.Token, []byte("stale-response")); err == nil {
		t.Error("stale Complete returned nil error, want an error (it must not silently resolve the newer claim)")
	}

	// The newer claim must be completely unaffected: still in-flight,
	// resolvable only by ITS OWN token.
	stillInFlight, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("re-Claim: %v", err)
	}
	if stillInFlight.State != idempotency.StateInFlight {
		t.Fatalf("State after the stale Complete = %v, want StateInFlight (the newer claim must be untouched)", stillInFlight.State)
	}

	if err := s.Complete(context.Background(), "key-1", newer.Token, []byte("real-response")); err != nil {
		t.Fatalf("Complete with the newer claim's own real token: %v", err)
	}
	final, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("final Claim: %v", err)
	}
	if string(final.Response) != "real-response" {
		t.Errorf("final Response = %q, want %q", final.Response, "real-response")
	}
}

// TestFailWithAStaleTokenIsANoOp mirrors the Complete-side proof above for
// Fail: a stale token must never delete/fail a different, newer claim for
// the same key.
func TestFailWithAStaleTokenIsANoOp(t *testing.T) {
	s := New()
	original, err := s.Claim(context.Background(), "key-1", fp(1), time.Millisecond)
	if err != nil {
		t.Fatalf("original Claim: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	newer, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("newer Claim: %v", err)
	}

	if err := s.Fail(context.Background(), "key-1", original.Token); err != nil {
		t.Fatalf("stale Fail = %v, want nil (a safe no-op)", err)
	}

	stillInFlight, err := s.Claim(context.Background(), "key-1", fp(2), time.Minute)
	if err != nil {
		t.Fatalf("re-Claim: %v", err)
	}
	if stillInFlight.State != idempotency.StateInFlight {
		t.Fatalf("State after the stale Fail = %v, want StateInFlight (the newer claim must be untouched)", stillInFlight.State)
	}
	_ = newer
}
