// Package grpcclient is the dormant network-adapter seam (adapter #3 in
// gateway/ARCHITECTURE.md's Cache Subsystem package layout) for a Cache
// consumer that talks to an extracted Cache service over gRPC instead of
// calling an in-process implementation directly.
//
// This is deliberately not a working feature this pass — see
// internal/cache/grpcserver's package doc and
// docs/decisions/0002-cache-embedded-in-gateway.md for why. Every method
// returns a clear "not implemented" error, and Client is written to
// satisfy cache.Cache's exact shape so it can be dropped in as a drop-in
// implementation the moment extraction is ever warranted.
package grpcclient

import (
	"context"
	"errors"
	"time"

	"github.com/kelvran/gateway/gateway/internal/cache"
)

// errNotImplemented is returned by every method on Client.
var errNotImplemented = errors.New("not implemented — dormant extraction seam, see docs/decisions/0002-cache-embedded-in-gateway.md")

// Client implements cache.Cache by (eventually) calling out to a
// grpcserver-hosted Cache service. Not implemented this pass.
type Client struct{}

// compile-time check that Client satisfies cache.Cache, proving the seam's
// shape is correct even though every method is unimplemented.
var _ cache.Cache = (*Client)(nil)

// New constructs a Client stub.
func New() *Client {
	return &Client{}
}

// Get implements cache.Cache. Not implemented.
func (c *Client) Get(_ context.Context, _ string) ([]byte, bool, error) {
	return nil, false, errNotImplemented
}

// Put implements cache.Cache. Not implemented.
func (c *Client) Put(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return errNotImplemented
}
