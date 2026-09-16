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
}

// Store is a mutex-guarded, TTL-swept idempotency.Store. The zero value
// is not usable; construct with New.
type Store struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// New constructs an empty Store.
func New() *Store {
	return &Store{entries: map[string]*entry{}}
}

// sweepExpiredLocked removes every entry whose ttl has elapsed,
// regardless of whether it ever resolved — an unusually long-running
// claim that outlives its own ttl is treated as abandoned, letting a
// later Claim start fresh rather than staying permanently stuck. Callers
// must already hold s.mu.
func (s *Store) sweepExpiredLocked() {
	now := time.Now()
	for key, e := range s.entries {
		if now.After(e.expiresAt) {
			delete(s.entries, key)
		}
	}
}

// Claim implements idempotency.Store.
func (s *Store) Claim(_ context.Context, key string, fingerprint [32]byte, ttl time.Duration) (idempotency.ClaimResult, error) {
	s.mu.Lock()
	s.sweepExpiredLocked()
	e, ok := s.entries[key]
	if !ok {
		s.entries[key] = &entry{fingerprint: fingerprint, done: make(chan struct{}), expiresAt: time.Now().Add(ttl)}
		s.mu.Unlock()
		return idempotency.ClaimResult{State: idempotency.StateNew}, nil
	}
	if e.fingerprint != fingerprint {
		s.mu.Unlock()
		return idempotency.ClaimResult{}, idempotency.ErrFingerprintMismatch
	}
	done := e.done
	s.mu.Unlock()

	select {
	case <-done:
		// Happens-after Complete's own close(e.done) (or Fail's, which
		// deletes the map entry before closing it — see Fail's own doc
		// comment) — safe to read e.resp now, doubly so since the
		// following re-lock is independently synchronized on its own.
		s.mu.Lock()
		resp := e.resp
		s.mu.Unlock()
		return idempotency.ClaimResult{State: idempotency.StateCompleted, Response: resp}, nil
	default:
		return idempotency.ClaimResult{State: idempotency.StateInFlight, Done: done}, nil
	}
}

// Complete implements idempotency.Store.
func (s *Store) Complete(_ context.Context, key string, resp []byte) error {
	s.mu.Lock()
	e, ok := s.entries[key]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("idempotency: Complete called for unknown or expired key %q", key)
	}
	e.resp = resp
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
// becomes the new owner).
func (s *Store) Fail(_ context.Context, key string) error {
	s.mu.Lock()
	e, ok := s.entries[key]
	if ok {
		delete(s.entries, key)
	}
	s.mu.Unlock()
	if ok {
		close(e.done)
	}
	return nil
}
