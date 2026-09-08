package router

import "sync"

// SampleWindow tracks a rolling count of candidate-eligible real-request
// outcomes per deployment against a configurable minimum volume — the
// "traffic-volume floor" gate, per docs/upgrade-research/gateway-router-
// health-real-traffic-2026-09-09.md's Finding 4: every statistical/
// passive detector studied (Envoy's success_rate_request_volume, Linkerd's
// failure-accrual-success-rate-min-requests, Hystrix's
// requestVolumeThreshold, resilience4j's minimumNumberOfCalls, Polly's
// MinimumThroughput) withholds any statistical action below its own
// configured floor — not a separate, more-cautious mode, but no action at
// all, even at 100% observed failure below the floor.
//
// Deliberately clock-free and window-boundary-free, mirroring this
// package's own established "I/O-free, no wall-clock dependency" design
// (see the package doc comment, and HealthConfig.RecoveryRampSteps' own
// doc comment for why: probe-driven progression is preferred here over
// real-time windows for deterministic testing and zero clock dependency).
// A caller that wants an actual sliding TIME window (rather than a
// caller-decided reset boundary — e.g. once per probe interval, or once
// per N requests) composes that on top via Reset; this primitive itself
// only ever counts up.
//
// Deliberately NOT WIRED to Router.ReportProbeResult, or to any real
// request-handling call site, anywhere in this codebase — per that same
// research doc's own explicit recommendation: build the volume-floor
// mechanism now, as real, tested, inert code, but leave the decision of
// when (and at what real floor value) to connect it to live health state
// gated on Kelvran having actual production traffic to calibrate against,
// which it does not have yet.
type SampleWindow struct {
	mu     sync.Mutex
	min    int
	counts map[string]int
}

// NewSampleWindow builds a SampleWindow requiring minVolume candidate-
// eligible outcomes (see dataplane.isCandidateHealthFailure — the
// intended, though not yet wired, caller of Record) before
// HasEnoughSignal reports true for any given deployment name. minVolume
// <= 0 means every name always has enough signal, immediately — a
// deliberately permissive default matching this codebase's "0/unset
// means no additional restriction" convention elsewhere (e.g.
// ratelimit.ConcurrencyConfig.MaxInFlight), never the opposite
// (permanently withholding signal).
func NewSampleWindow(minVolume int) *SampleWindow {
	return &SampleWindow{min: minVolume, counts: make(map[string]int)}
}

// Record counts one more candidate-eligible real-request outcome toward
// name's rolling sample — success or failure both count equally toward
// the VOLUME floor; HasEnoughSignal answers only "is there enough
// traffic to trust a verdict," never "what the verdict is." A caller
// computes the actual failure-rate verdict itself, once HasEnoughSignal
// confirms it's safe to act on one at all.
func (w *SampleWindow) Record(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.counts[name]++
}

// HasEnoughSignal reports whether name has accumulated at least the
// configured minimum volume of candidate-eligible outcomes since
// construction or its last Reset. A name never Recorded at all has a
// count of zero, correctly reporting false whenever min > 0.
func (w *SampleWindow) HasEnoughSignal(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.min <= 0 {
		return true
	}
	return w.counts[name] >= w.min
}

// Count reports name's current rolling sample count — exported so tests
// (and a future admin-status endpoint) can observe state directly,
// mirroring ConcurrencyLimiter.InFlight's own precedent for exactly that
// reason.
func (w *SampleWindow) Count(name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.counts[name]
}

// Reset zeroes name's rolling sample count — the primitive a future
// caller composes a real time-boxed or request-boxed window out of
// (e.g. calling Reset once per health-probe interval), never something
// this type does on its own, per its own doc comment's "clock-free"
// design.
func (w *SampleWindow) Reset(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.counts, name)
}
