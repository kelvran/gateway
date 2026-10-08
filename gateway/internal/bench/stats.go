// Package bench is the load-generation and measurement core of
// cmd/kelvran-bench: open-loop Poisson arrivals against an OpenAI-shaped
// endpoint, per-request latency, the gateway's own overhead header, and for
// streams the time to first token and the inter-chunk gaps, summarised as
// nearest-rank percentiles and written as JSON a dashboard or
// github-action-benchmark can read. It is stdlib-only and knows nothing
// about the gateway's internals; the gateway is just the URL it targets.
package bench

import (
	"encoding/json"
	"math"
	"slices"
	"time"
)

// Summary is a nearest-rank percentile summary of durations.
type Summary struct {
	Count int
	Min   time.Duration
	Max   time.Duration
	Mean  time.Duration
	P50   time.Duration
	P95   time.Duration
	P99   time.Duration
}

// Summarize computes a Summary; an empty input yields the zero Summary.
func Summarize(samples []time.Duration) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return Summary{
		Count: len(sorted),
		Min:   sorted[0],
		Max:   sorted[len(sorted)-1],
		Mean:  total / time.Duration(len(sorted)),
		P50:   nearestRank(sorted, 50),
		P95:   nearestRank(sorted, 95),
		P99:   nearestRank(sorted, 99),
	}
}

// nearestRank is the classic nearest-rank percentile: the value at ordinal
// rank ceil(p/100 * n), never interpolated, so a reported p99 is always a
// latency some request actually saw.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type summaryJSON struct {
	Count  int     `json:"count"`
	MinMs  float64 `json:"min_ms"`
	MaxMs  float64 `json:"max_ms"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
}

func toMs(d time.Duration) float64    { return float64(d) / float64(time.Millisecond) }
func fromMs(ms float64) time.Duration { return time.Duration(ms * float64(time.Millisecond)) }

// MarshalJSON writes durations as fractional milliseconds so results.json is
// readable without a unit conversion.
func (s Summary) MarshalJSON() ([]byte, error) {
	return json.Marshal(summaryJSON{s.Count, toMs(s.Min), toMs(s.Max), toMs(s.Mean), toMs(s.P50), toMs(s.P95), toMs(s.P99)})
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (s *Summary) UnmarshalJSON(b []byte) error {
	var j summaryJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*s = Summary{j.Count, fromMs(j.MinMs), fromMs(j.MaxMs), fromMs(j.MeanMs), fromMs(j.P50Ms), fromMs(j.P95Ms), fromMs(j.P99Ms)}
	return nil
}

// CountSummary is the integer twin of Summary (for per-stream frame counts).
type CountSummary struct {
	Count int     `json:"count"`
	Min   int     `json:"min"`
	Max   int     `json:"max"`
	Mean  float64 `json:"mean"`
	P50   int     `json:"p50"`
	P99   int     `json:"p99"`
}

// SummarizeInts computes a CountSummary; an empty input yields zeros.
func SummarizeInts(samples []int) CountSummary {
	if len(samples) == 0 {
		return CountSummary{}
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	total := 0
	for _, v := range sorted {
		total += v
	}
	rank := func(p float64) int {
		idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		return sorted[idx]
	}
	return CountSummary{Count: len(sorted), Min: sorted[0], Max: sorted[len(sorted)-1], Mean: float64(total) / float64(len(sorted)), P50: rank(50), P99: rank(99)}
}
