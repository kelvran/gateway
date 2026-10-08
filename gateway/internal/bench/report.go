package bench

import (
	"fmt"
	"strings"
	"time"
)

// Result is one scenario's measurement. Durations inside Summary values are
// written as milliseconds; the top-level Duration and Warmup are
// nanoseconds (Go's default) and the markdown summary formats them.
type Result struct {
	Scenario  string        `json:"scenario"`
	Target    string        `json:"target"`
	Model     string        `json:"model"`
	Stream    bool          `json:"stream"`
	RPS       float64       `json:"offered_rps"`
	Duration  time.Duration `json:"duration_ns"`
	Warmup    time.Duration `json:"warmup_ns"`
	StartedAt time.Time     `json:"started_at"`

	// Requests is every request sent in the measured window; Errors counts
	// transport failures, non-2xx statuses, and streams that ended in an
	// error frame or without [DONE]; Dropped counts arrivals the in-flight
	// cap refused (never sent, never measured).
	Requests        int `json:"requests"`
	Errors          int `json:"errors"`
	TransportErrors int `json:"transport_errors"`
	Dropped         int `json:"dropped_arrivals"`
	// Cancelled counts requests the harness itself cut off because the run
	// was interrupted (SIGINT); they are in neither Requests nor Errors.
	Cancelled    int         `json:"cancelled_by_harness"`
	StatusCounts map[int]int `json:"status_counts"`
	// SentRPS is Requests per second of the measured window (what the
	// harness managed to send); CompletedRPS counts only the successes.
	SentRPS      float64 `json:"sent_rps"`
	CompletedRPS float64 `json:"completed_rps"`
	// ScheduleLateness is how long after its scheduled instant each request
	// left the harness (stamped at the instant the request is sent, where
	// the latency clock starts). Open-loop load is only honest while this stays
	// small: a large p99 here means the harness, not the gateway, fell
	// behind (coordinated omission), and the latencies are optimistic.
	ScheduleLateness Summary `json:"schedule_lateness"`

	// Latency and Overhead summarise successful requests only.
	Latency  Summary `json:"latency"`
	Overhead Summary `json:"overhead_header"`

	// TTFT and InterChunk summarise successful streams only; FramesPerStream
	// covers every stream that returned 200.
	TTFT              Summary      `json:"ttft"`
	InterChunk        Summary      `json:"inter_chunk"`
	FramesPerStream   CountSummary `json:"frames_per_stream"`
	IncompleteStreams int          `json:"incomplete_streams"`
	ErrorFrames       int          `json:"error_frames"`

	// CacheHitRatio is set when the scenario repeats prompts: the share of
	// responses whose completion id had already been seen, in the warm-up
	// or earlier in the window (a replay keeps the stored id, a fresh
	// upstream call mints a new one).
	CacheHitRatio *float64 `json:"cache_hit_ratio,omitempty"`
	RSS           *RSS     `json:"gateway_rss,omitempty"`

	Meta map[string]string `json:"meta,omitempty"`
}

// RSS is the gateway process's resident set: sampled once before this
// run's load starts (idle — for a sweep's later steps that is where the
// previous step ended), the largest one-second sample during it (peak), and
// once after the last response (end; 0 when that sample failed).
type RSS struct {
	IdleBytes int64 `json:"idle_bytes"`
	PeakBytes int64 `json:"peak_bytes"`
	EndBytes  int64 `json:"end_bytes"`
}

// Metric is one github-action-benchmark "customSmallerIsBetter" entry.
type Metric struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Value float64 `json:"value"`
	Extra string  `json:"extra,omitempty"`
}

// ToBenchmarkMetrics flattens results into the smaller-is-better metrics a
// trend chart tracks: latency and overhead percentiles, TTFT and
// inter-chunk gaps for streams, the error rate, and the completed rate
// expressed as its shortfall from the offered rate (so smaller is better).
func ToBenchmarkMetrics(results []Result) []Metric {
	var out []Metric
	add := func(name, unit string, v float64) { out = append(out, Metric{Name: name, Unit: unit, Value: v}) }
	for _, r := range results {
		p := r.Scenario + " "
		if r.Latency.Count > 0 {
			add(p+"latency p50", "ms", toMs(r.Latency.P50))
			add(p+"latency p95", "ms", toMs(r.Latency.P95))
			add(p+"latency p99", "ms", toMs(r.Latency.P99))
		}
		if r.Overhead.Count > 0 {
			add(p+"overhead p50", "ms", toMs(r.Overhead.P50))
			add(p+"overhead p99", "ms", toMs(r.Overhead.P99))
		}
		if r.TTFT.Count > 0 {
			add(p+"ttft p50", "ms", toMs(r.TTFT.P50))
			add(p+"ttft p99", "ms", toMs(r.TTFT.P99))
		}
		if r.InterChunk.Count > 0 {
			add(p+"inter-chunk p99", "ms", toMs(r.InterChunk.P99))
		}
		if r.Requests > 0 {
			add(p+"error rate", "%", 100*float64(r.Errors)/float64(r.Requests))
			if r.RPS > 0 {
				shortfall := r.RPS - r.CompletedRPS
				if shortfall < 0 {
					shortfall = 0
				}
				add(p+"rps shortfall", "req/s", shortfall)
			}
		}
	}
	return out
}

// MarkdownSummary renders one table row per scenario, for a step summary.
func MarkdownSummary(results []Result) string {
	var b strings.Builder
	b.WriteString("| Scenario | Offered rps | Sent/s | Completed/s | Requests | Errors | p50 | p95 | p99 | Overhead p99 | TTFT p50 | TTFT p99 | Inter-chunk p99 | Cache hit |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	cell := func(s Summary, d time.Duration) string {
		if s.Count == 0 {
			return "–"
		}
		return fmt.Sprintf("%.1f ms", toMs(d))
	}
	for _, r := range results {
		hit := "–"
		if r.CacheHitRatio != nil {
			hit = fmt.Sprintf("%.0f %%", 100**r.CacheHitRatio)
		}
		fmt.Fprintf(&b, "| %s | %.0f | %.0f | %.0f | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Scenario, r.RPS, r.SentRPS, r.CompletedRPS, r.Requests, r.Errors,
			cell(r.Latency, r.Latency.P50), cell(r.Latency, r.Latency.P95), cell(r.Latency, r.Latency.P99),
			cell(r.Overhead, r.Overhead.P99), cell(r.TTFT, r.TTFT.P50), cell(r.TTFT, r.TTFT.P99), cell(r.InterChunk, r.InterChunk.P99), hit)
	}
	return b.String()
}
