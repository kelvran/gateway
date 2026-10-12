package main

import (
	"net/http"
	"time"
)

// The admin server's timeouts. Until 2026-10-12 the admin listener set only
// ReadHeaderTimeout, so a slow or stalled client could hold a connection, and
// a stalled persistence disk could hold a response, for as long as it liked
// (docs/how-to/admin-api-rbac.md, "Before you rely on it"). Now: 10 s for the
// request header, 60 s for everything else -- the whole request (the write
// routes' JSON bodies have no size cap and may arrive over a slow link), the
// response (above every handler's own deadline: 10 s for GET /admin/spend,
// 2 s for the spend reads of ?include=spend; a backup whose file copies
// outlive the bound still completes on disk and only its response is lost,
// because a write deadline closes the connection without cancelling the
// handler) and an idle keep-alive connection. The metrics listener
// (metrics_server.go) has the same shape with a 10 s read and a 30 s write
// bound, which fits an unauthenticated scraper rather than an operator's
// uploads.
//
// The pprof routes are the one case that legitimately runs longer than the
// write bound: profile?seconds=, trace?seconds= and the delta profiles
// (heap?seconds= and friends) each run for `seconds`. net/http/pprof handles
// that itself: each of those handlers calls its configureWriteDeadline, which
// extends the connection's write deadline by WriteTimeout + seconds whenever
// the server has a WriteTimeout, so a 60 s profile completes under a 60 s
// bound. The library never bounds `seconds` itself; internal/admin caps it at
// 60 (limits.go, since 2026-10-12), so a pprof connection lives at most 120 s.
// Before the cap an Admin-tier caller could hold a connection, and the
// exclusive CPU profiler, for as long as it asked; before the timeouts every
// admin connection could, with no pprof call at all.
// TestAdminServerWriteTimeoutIsExtendedByPprof pins the extension in this
// toolchain against a control handler the same timeout does cut off.
const (
	adminReadHeaderTimeout = 10 * time.Second
	adminReadTimeout       = 60 * time.Second
	adminWriteTimeout      = 60 * time.Second
	adminIdleTimeout       = 60 * time.Second
)

// newAdminServer builds the admin listener's http.Server: the caller's address
// and handler under the timeouts above. TLS stays the caller's to add
// afterwards (admin.mtls), exactly as before this constructor existed.
func newAdminServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: adminReadHeaderTimeout,
		ReadTimeout:       adminReadTimeout,
		WriteTimeout:      adminWriteTimeout,
		IdleTimeout:       adminIdleTimeout,
	}
}
