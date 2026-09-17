// Package inprocess implements idempotency.Store with a mutex-guarded
// map — a single-instance-only primitive, matching this codebase's
// documented, disclosed scope: real cross-instance idempotency (the
// gateway2 multi-instance Compose profile) would need a Redis-backed
// implementation, named as a real, not-yet-built extension, not this
// package's job.
package inprocess

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kelvran/gateway/gateway/internal/idempotency"
)

type entry struct {
	fingerprint [32]byte
	done        chan struct{}
	resp        []byte
	expiresAt   time.Time
	// token is this claim's own identity, checked by Complete/Fail
	// against the token their caller was actually granted — see
	// idempotency.Token's own doc comment for why: without it, a stale
	// caller (its own claim already swept as abandoned and replaced by a
	// DIFFERENT claim for the same key) could otherwise resolve that
	// unrelated newer claim via a bare key-only lookup. Never the zero
	// value for a real entry — see nextToken below.
	token idempotency.Token
	// completed is true only once Complete has actually run for this
	// entry — distinguishes "done closed because Complete recorded a
	// real response" from "done closed because sweepExpiredLocked
	// abandoned this claim" for any Claim call currently parked in its
	// own select on this exact done channel (see Claim's own retry-loop
	// comment), and separately guards Complete itself against being
	// called twice for the same still-present entry (a second
	// close(e.done) would otherwise panic).
	completed bool
}

// Store is a mutex-guarded, TTL-swept idempotency.Store. The zero value
// is not usable; construct with New.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
	// nextToken is a monotonically increasing counter, incremented under
	// s.mu, that mints each new claim's own idempotency.Token — starts
	// at 1 (post-increment from the zero value), so 0 stays reserved as
	// "no real claim," matching idempotency.Token's own doc comment.
	// Unique across this Store's whole lifetime, which is stronger than
	// the actual requirement (unique only among claims for the SAME
	// key), but a single shared counter is simpler than a per-key one
	// and costs nothing extra.
	nextToken idempotency.Token
}

// New constructs an empty Store.
func New() *Store {
	return &Store{entries: map[string]*entry{}}
}

// sweepExpiredLocked removes every entry whose ttl has elapsed,
// regardless of whether it ever resolved — an unusually long-running
// claim that outlives its own ttl is treated as abandoned, letting a
// later Claim start fresh rather than staying permanently stuck.
// Closes done for any entry that was NEVER completed before removing it
// — waking any Claim call already parked on that exact channel (via its
// own StateInFlight ClaimResult.Done, or another goroutine's Claim
// itself blocked in its select, per Claim's own retry-loop) promptly,
// rather than leaving it to hang until its own ctx.Done() instead. Never
// closes an already-completed entry's done a second time — Complete
// itself already closed it once; sweeping it later is pure map cleanup,
// not a second resolution. Callers must already hold s.mu.
func (s *Store) sweepExpiredLocked() {
	now := time.Now()
	for key, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, key)
			if !e.completed {
				close(e.done)
			}
		}
	}
}

// Claim implements idempotency.Store.
func (s *Store) Claim(_ context.Context, key string, fingerprint [32]byte, ttl time.Duration) (idempotency.ClaimResult, error) {
	if ttl <= 0 {
		return idempotency.ClaimResult{}, idempotency.ErrNonPositiveTTL
	}
	for {
		s.mu.Lock()
		s.sweepExpiredLocked()
		e, ok := s.entries[key]
		if !ok {
			s.nextToken++
			token := s.nextToken
			s.entries[key] = &entry{fingerprint: fingerprint, done: make(chan struct{}), expiresAt: time.Now().Add(ttl), token: token}
			s.mu.Unlock()
			return idempotency.ClaimResult{State: idempotency.StateNew, Token: token}, nil
		}
		if e.fingerprint != fingerprint {
			s.mu.Unlock()
			return idempotency.ClaimResult{}, idempotency.ErrFingerprintMismatch
		}
		done := e.done
		s.mu.Unlock()

		select {
		case <-done:
			// Happens-after Complete's own close(e.done) (a real
			// completion) OR sweepExpiredLocked's/Fail's (an abandoned
			// claim, never completed) — e.completed (read under the
			// lock below) is what actually distinguishes the two; done
			// alone closing is not proof of a real result.
			s.mu.Lock()
			completed := e.completed
			resp := e.resp
			s.mu.Unlock()
			if !completed {
				// Abandoned, not completed — retry from the top rather
				// than fabricating a StateCompleted with no real
				// response. By the time the lock is re-acquired, the map
				// may already hold a fresh entry for this key (another
				// caller's Claim won the race) or be empty again;
				// either way, re-running the whole check is simplest and
				// correct, mirroring Fail's own "whichever caller calls
				// Claim first after the release becomes the new owner"
				// resolution.
				continue
			}
			return idempotency.ClaimResult{State: idempotency.StateCompleted, Response: resp}, nil
		default:
			return idempotency.ClaimResult{State: idempotency.StateInFlight, Done: done}, nil
		}
	}
}

// Complete implements idempotency.Store.
func (s *Store) Complete(_ context.Context, key string, token idempotency.Token, resp []byte) error {
	s.mu.Lock()
	e, ok := s.entries[key]
	if !ok || e.token != token {
		s.mu.Unlock()
		return fmt.Errorf("idempotency: Complete called for unknown, expired, or superseded key %q", key)
	}
	if e.completed {
		// Already resolved by an earlier Complete call for this exact
		// entry — a no-op, per this method's own documented contract,
		// not a second close(e.done) (which would panic).
		s.mu.Unlock()
		return nil
	}
	e.resp = resp
	e.completed = true
	s.mu.Unlock()
	close(e.done)
	return nil
}

// Fail implements idempotency.Store. Deletes the map entry BEFORE
// closing done — a waiter woken by the close, or the original caller
// itself retrying, calls Claim again and finds no entry at all, getting
// a fresh StateNew (see idempotency.Store.Fail's own doc comment for why
// this is the correct, if not perfectly fair under true concurrency,
// resolution: whichever caller calls Claim first after the release
// becomes the new owner). A stale token (the entry currently at key
// belongs to a DIFFERENT, newer claim than the one this caller was
// granted) is treated exactly like an unknown key — a safe no-op, never
// deleting or resolving a claim this caller was never granted.
func (s *Store) Fail(_ context.Context, key string, token idempotency.Token) error {
	s.mu.Lock()
	e, ok := s.entries[key]
	if ok && e.token != token {
		ok = false
	}
	if ok {
		delete(s.entries, key)
	}
	s.mu.Unlock()
	if ok {
		close(e.done)
	}
	return nil
}
