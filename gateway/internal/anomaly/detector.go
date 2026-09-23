// Package anomaly flags a statistically meaningful shift in a per-virtual-key
// boolean signal, using only data the request pipeline already computes on
// every request -- no new upstream call, no new stored state beyond two
// small in-memory counters per key.
//
// Motivated by this session's own incident-postmortems-failure-taxonomy
// research: Anthropic's own September 2025 postmortem describes three
// infrastructure bugs that silently degraded output quality for a month
// while every request kept returning a normal HTTP 200 -- a failure class
// invisible to health/routing/circuit-breaker machinery that keys off HTTP
// status and latency alone. api/gatewayevents/v1.GatewayDecisionEvent
// carries FinishReason and FallbackHappened today but no response
// length/shape signal -- a true response-shape detector would need a new
// proto field (a cross-language contract change, out of scope here, see
// AGENTS.md's "Ask first" boundary on api/ changes). This package is
// deliberately scoped to what the existing event stream can support.
package anomaly

import "sync"

// Detector tracks one boolean signal (e.g. "was this request's finish
// reason length/content_filter," "did this request fall back") per key
// across two same-sized, non-overlapping windows: baseline (the
// window-before-last) and recent (the window in progress). Once a
// window fills, Observe compares recent's rate for the signal against
// baseline's rate before rotating recent into baseline and starting a
// fresh recent window -- so this never needs to store per-event history,
// only four integers per key, regardless of request volume.
type Detector struct {
	mu             sync.Mutex
	windowSize     int
	minFlaggedRate float64
	shiftFactor    float64
	perKey         map[string]*keyWindow
}

type keyWindow struct {
	baselineTotal, baselineFlagged int
	recentTotal, recentFlagged     int
}

// NewDetector constructs a Detector. windowSize is how many observations
// make up one window (baseline and recent are always the same size).
// minFlaggedRate is an absolute floor on recent's own rate below which
// an anomaly is never flagged, regardless of the relative shift --
// guards against noise on a baseline that's itself near zero (e.g.
// baseline 1/50 -> recent 3/50 is a real 3x shift but too small in
// absolute terms to page anyone over). shiftFactor is how many times
// higher than baseline's rate recent's rate must be to flag.
func NewDetector(windowSize int, minFlaggedRate, shiftFactor float64) *Detector {
	return &Detector{
		windowSize:     windowSize,
		minFlaggedRate: minFlaggedRate,
		shiftFactor:    shiftFactor,
		perKey:         make(map[string]*keyWindow),
	}
}

// Observe records one observation for key (flagged is this request's own
// value of the tracked boolean signal) and reports whether THIS
// observation completed a recent window that shows a statistically
// meaningful shift versus the immediately preceding baseline window.
// Returns false on every observation that doesn't complete a window, and
// on the very first window ever completed for a key (no baseline exists
// yet to compare against).
func (d *Detector) Observe(key string, flagged bool) (anomalous bool) {
	if key == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	kw := d.perKey[key]
	if kw == nil {
		kw = &keyWindow{}
		d.perKey[key] = kw
	}

	kw.recentTotal++
	if flagged {
		kw.recentFlagged++
	}
	if kw.recentTotal < d.windowSize {
		return false
	}

	if kw.baselineTotal >= d.windowSize {
		baselineRate := float64(kw.baselineFlagged) / float64(kw.baselineTotal)
		recentRate := float64(kw.recentFlagged) / float64(kw.recentTotal)
		if recentRate >= d.minFlaggedRate && recentRate >= baselineRate*d.shiftFactor {
			anomalous = true
		}
	}

	kw.baselineTotal, kw.baselineFlagged = kw.recentTotal, kw.recentFlagged
	kw.recentTotal, kw.recentFlagged = 0, 0
	return anomalous
}
