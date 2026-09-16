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
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	want := []byte(`{"id":"chatcmpl-1"}`)
	if err := s.Complete(context.Background(), "key-1", want); err != nil {
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
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
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
		_ = s.Complete(context.Background(), "key-1", want)
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
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := s.Fail(context.Background(), "key-1"); err != nil {
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
	if _, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	result, err := s.Claim(context.Background(), "key-1", fp(1), time.Minute)
	if err != nil {
		t.Fatalf("waiter Claim: %v", err)
	}

	go func() {
		_ = s.Fail(context.Background(), "key-1")
	}()

	select {
	case <-result.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed after Fail")
	}
}

func TestCompleteOnUnknownKeyReturnsAnError(t *testing.T) {
	s := New()
	if err := s.Complete(context.Background(), "never-claimed", []byte("x")); err == nil {
		t.Error("Complete on an unclaimed key returned nil error, want an error")
	}
}

func TestFailOnUnknownKeyIsANoOp(t *testing.T) {
	s := New()
	if err := s.Fail(context.Background(), "never-claimed"); err != nil {
		t.Errorf("Fail on an unclaimed key = %v, want nil", err)
	}
}
