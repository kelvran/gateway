// Package spendvelocity is a pure, standalone CUSUM (cumulative sum)
// change-point detector for spend-velocity anomaly detection, per
// docs/upgrade-research/llm-cost-optimization-finops-2026-09-14.md
// Finding 6: a real, decades-old, lightweight extension of the same
// statistical family Kelvran's Evals toolkit already uses (Wilson
// interval, mixture-SPRT) — applicable to gateway-side spend monitoring
// specifically.
//
// This package takes no live dependency on the running gateway process,
// budget.Tracker, or any storage backend — it operates purely on a
// caller-supplied sequence of observations via Observe. Mirrors
// telemetry/cachecorrelation's own precedent exactly: a standalone
// analysis unit, NOT wired into the live request pipeline this pass.
// The real trigger for wiring it in (e.g. a periodic sampler feeding it
// from budget.Tracker's own per-key spend, and a decision about where an
// alarm surfaces — a log line, an OTel span event, an admin endpoint) is
// a deliberate product choice the source research explicitly declined
// to make on its own ("the specific threshold needs a deliberate choice,
// not a default").
package spendvelocity

import "errors"

// errSigmaNotPositive is returned by New when sigma <= 0.
var errSigmaNotPositive = errors.New("spendvelocity: sigma must be positive")

// Detector is a two-sided CUSUM detector tracking cumulative deviation
// of a sequence of observations from Target. Detect small, sustained
// shifts rather than single-point outliers — the whole point of CUSUM
// over a simple z-score/threshold check, per the source research: it
// accumulates evidence across observations instead of alarming (or
// failing to alarm) on any one point in isolation.
//
// The three tuning knobs and their real tradeoff, per the source
// research's own cited numbers: at ThresholdSigma=4, the expected
// false-alarm interval (ARL0) is roughly 170 observations, with a 1-sigma
// shift caught in roughly 6; at ThresholdSigma=8, ARL0 rises to 4,000+
// observations at the cost of roughly 12 observations of added detection
// delay. There is no single correct default — SlackSigma/ThresholdSigma
// must be chosen deliberately for the real spend-velocity distribution
// being monitored, not copied from this doc comment.
type Detector struct {
	// Target is the reference mean this detector considers "normal" —
	// e.g. a rolling baseline spend rate computed by the (not yet built)
	// caller feeding this detector.
	Target float64
	// Sigma is the reference standard deviation of the monitored series
	// around Target — SlackSigma and ThresholdSigma below are both
	// expressed as multiples of this value, per the standard CUSUM
	// convention the source research's own ARL0 numbers are quoted in.
	// Must be > 0 — see New's own validation.
	Sigma float64
	// SlackSigma (often called "k" in CUSUM literature) is the allowed
	// drift tolerance before an observation contributes to the
	// cumulative sum at all, in units of Sigma — typically 0.5. Too
	// small and ordinary noise accumulates as if it were a real shift;
	// too large and a real shift takes longer to detect.
	SlackSigma float64
	// ThresholdSigma (often called "h") is the decision threshold the
	// cumulative sum must cross to alarm, in units of Sigma — see this
	// type's own doc comment for the ARL0-vs-detection-delay tradeoff at
	// two concrete example values (4 and 8).
	ThresholdSigma float64

	upper float64 // S+ — accumulates evidence of a sustained INCREASE
	lower float64 // S- — accumulates evidence of a sustained DECREASE
}

// New constructs a Detector, or returns an error if sigma is not
// positive — a zero or negative Sigma would make every SlackSigma/
// ThresholdSigma multiple meaningless (dividing by, or scaling by, a
// non-positive spread).
func New(target, sigma, slackSigma, thresholdSigma float64) (*Detector, error) {
	if sigma <= 0 {
		return nil, errSigmaNotPositive
	}
	return &Detector{
		Target:         target,
		Sigma:          sigma,
		SlackSigma:     slackSigma,
		ThresholdSigma: thresholdSigma,
	}, nil
}

// Direction reports which side of the reference mean an alarm fired on.
type Direction int

const (
	// DirectionNone is Observe's zero-value Direction when no alarm
	// fired — never a meaningful "no direction" alarm state.
	DirectionNone Direction = iota
	// DirectionIncrease means the cumulative sum crossed the upper
	// threshold — a sustained spend-rate INCREASE above Target.
	DirectionIncrease
	// DirectionDecrease means the cumulative sum crossed the lower
	// threshold — a sustained spend-rate DECREASE below Target. Real for
	// spend velocity specifically (unlike a pure fraud/abuse detector,
	// which would only ever care about increases): an unexpected spend
	// DROP can itself be a real signal — e.g. a fallback chain silently
	// routing every request to a free/misconfigured deployment, or a
	// guardrail over-blocking traffic that should be billing normally.
	DirectionDecrease
)

// Observe folds x into the running cumulative sums and reports whether
// this observation caused an alarm. alarmed is true exactly once per
// sustained shift — Observe resets both cumulative sums to zero
// immediately after any alarm, the standard CUSUM convention for being
// ready to detect the NEXT shift rather than continuing to alarm on
// every subsequent observation while the sum stays past threshold.
func (d *Detector) Observe(x float64) (alarmed bool, direction Direction) {
	slack := d.SlackSigma * d.Sigma
	threshold := d.ThresholdSigma * d.Sigma

	d.upper = max(0, d.upper+(x-d.Target-slack))
	d.lower = min(0, d.lower+(x-d.Target+slack))

	switch {
	case d.upper > threshold:
		d.Reset()
		return true, DirectionIncrease
	case d.lower < -threshold:
		d.Reset()
		return true, DirectionDecrease
	default:
		return false, DirectionNone
	}
}

// Reset zeroes both cumulative sums, discarding all accumulated
// evidence — called automatically by Observe after every alarm; exposed
// so a caller can also reset deliberately (e.g. after a known, expected
// spend-rate change such as a deliberate price-table update, which
// should not itself count as an anomaly).
func (d *Detector) Reset() {
	d.upper = 0
	d.lower = 0
}
