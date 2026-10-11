package main

import (
	"net"
	"net/http"
	"time"
)

// defaultMetricsListenAddr is where the Prometheus pull listener binds when
// telemetry.prometheus.enabled is set without listen_addr: loopback, the
// conventional Prometheus port (owner gate G24 -- unauthenticated by design,
// so an operator chooses any non-loopback bind explicitly and fences it, per
// docs/how-to/deploy/kubernetes-kustomize.md's NetworkPolicy recipe).
const defaultMetricsListenAddr = "127.0.0.1:9464"

// The metrics server's timeouts: an unauthenticated listener gets a bounded
// read, a write bound above promhttp's own 10 s handler timeout, and a short
// idle period, so a slow or idle scraper cannot hold connections open.
const (
	metricsReadTimeout  = 10 * time.Second
	metricsWriteTimeout = 30 * time.Second
	metricsIdleTimeout  = 60 * time.Second
)

// newMetricsMux is the third listener's whole route table: GET /metrics
// serves the pull reader's handler (the method-bearing pattern also answers
// HEAD, as net/http does for every GET pattern); any other method on the path
// is net/http's plain-text 405 with Allow, any other path its plain-text 404.
// Nothing here is part of the OpenAPI document, which describes the data
// plane and the admin listener.
func newMetricsMux(h http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", h)
	return mux
}

// isLoopbackListenAddr reports whether addr binds loopback only: a loopback
// IP or "localhost" before the port. An empty host (":9464") binds every
// interface and an unparsable value is treated the same way, so the
// non-loopback warning fires for both.
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
