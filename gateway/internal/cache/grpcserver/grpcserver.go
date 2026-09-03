// Package grpcserver is the dormant network-adapter seam (adapter #2 in
// gateway/ARCHITECTURE.md's Cache Subsystem package layout) for exposing
// an in-process Cache over gRPC, should Cache ever be extracted into its
// own service.
//
// This is deliberately not a working feature this pass — it exists only
// so the extraction seam is visible and typed, per
// docs/decisions/0002-cache-embedded-in-gateway.md, which lists the
// specific evidence-based triggers required before extraction is even
// considered. Every method returns a clear "not implemented" error.
package grpcserver

import (
	"context"
	"errors"
	"time"

	"github.com/kelvran/gateway/gateway/internal/cache"
)

// errNotImplemented is returned by every method on Server. It is a
// package-level var (not a fresh errors.New per call) so callers can
// errors.Is against a stable sentinel if they ever need to.
var errNotImplemented = errors.New("not implemented — dormant extraction seam, see docs/decisions/0002-cache-embedded-in-gateway.md")

// Server is the gRPC-facing wrapper around a cache.Cache implementation.
// It is not wired to any real gRPC transport this pass — the
// cache.proto contract referenced in gateway/ARCHITECTURE.md is defined
// but unused until/unless Cache is ever extracted. Server itself
// implements cache.Cache's shape (rather than only some ad hoc RPC-handler
// signature) so it's a direct, typed stand-in for the interface once real
// gRPC wiring is added.
type Server struct{}

// compile-time check that Server satisfies cache.Cache, proving the seam's
// shape is correct even though every method is unimplemented.
var _ cache.Cache = (*Server)(nil)

// New constructs a Server stub.
func New() *Server {
	return &Server{}
}

// Get is the server-side handler for a Cache.Get RPC. Not implemented.
func (s *Server) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, errNotImplemented
}

// Put is the server-side handler for a Cache.Put RPC. Not implemented.
func (s *Server) Put(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return errNotImplemented
}
