package bench

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestSummarizeUsesNearestRankPercentiles(t *testing.T) {
	var samples []time.Duration
	for i := 1; i <= 100; i++ {
		samples = append(samples, ms(i))
	}
	s := Summarize(samples)
	if s.Count != 100 || s.Min != ms(1) || s.Max != ms(100) {
		t.Fatalf("count/min/max = %d/%s/%s, want 100/1ms/100ms", s.Count, s.Min, s.Max)
	}
	if s.P50 != ms(50) || s.P95 != ms(95) || s.P99 != ms(99) {
		t.Errorf("p50/p95/p99 = %s/%s/%s, want 50ms/95ms/99ms (nearest rank)", s.P50, s.P95, s.P99)
	}
	if s.Mean != ms(50)+500*time.Microsecond {
		t.Errorf("mean = %s, want 50.5ms", s.Mean)
	}
}

func TestSummarizeOfNothingIsZero(t *testing.T) {
	s := Summarize(nil)
	if s.Count != 0 || s.P99 != 0 || s.Mean != 0 {
		t.Errorf("empty summary = %+v, want zeros", s)
	}
}

func TestSummarizeSingleSample(t *testing.T) {
	s := Summarize([]time.Duration{ms(7)})
	if s.P50 != ms(7) || s.P99 != ms(7) || s.Max != ms(7) || s.Count != 1 {
		t.Errorf("single-sample summary = %+v, want every percentile 7ms", s)
	}
}
