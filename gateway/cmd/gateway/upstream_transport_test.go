package main

import (
	"net/http"
	"testing"
)

// TestNewUpstreamTransportRaisesMaxIdleConnsPerHost proves the one
// deliberate override newUpstreamTransport makes over
// http.DefaultTransport. Regression test for
// docs/upgrade-research/performance-latency-optimization-2026-09-14.md
// Finding 1 -- before this existed, both upstream http.Clients ran on
// Go stdlib's implicit 2-idle-connection-per-host cap.
func TestNewUpstreamTransportRaisesMaxIdleConnsPerHost(t *testing.T) {
	transport := newUpstreamTransport()
	if transport.MaxIdleConnsPerHost != upstreamMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", transport.MaxIdleConnsPerHost, upstreamMaxIdleConnsPerHost)
	}
}

// TestNewUpstreamTransportPreservesOtherStdlibDefaults proves this is a
// genuine http.DefaultTransport.Clone(), not a bare &http.Transport{} --
// Proxy is a real stdlib default (http.ProxyFromEnvironment) a
// zero-value Transport would leave nil, and IdleConnTimeout is a stdlib
// default this function deliberately leaves untouched.
func TestNewUpstreamTransportPreservesOtherStdlibDefaults(t *testing.T) {
	transport := newUpstreamTransport()
	if transport.Proxy == nil {
		t.Error("Proxy = nil, want http.DefaultTransport's own ProxyFromEnvironment default -- this should be a clone, not a bare &http.Transport{}")
	}
	defaultTransport := http.DefaultTransport.(*http.Transport)
	if transport.IdleConnTimeout != defaultTransport.IdleConnTimeout {
		t.Errorf("IdleConnTimeout = %v, want the stdlib default %v (left unchanged)", transport.IdleConnTimeout, defaultTransport.IdleConnTimeout)
	}
}

// TestNewUpstreamTransportReturnsFreshInstance proves each call returns
// its own *http.Transport, not a shared package-level value mutated in
// place -- callers rely on being able to hold their own instance safely.
func TestNewUpstreamTransportReturnsFreshInstance(t *testing.T) {
	a := newUpstreamTransport()
	b := newUpstreamTransport()
	if a == b {
		t.Error("two calls returned the same *http.Transport instance, want two distinct instances")
	}
}
