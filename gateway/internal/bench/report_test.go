package bench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestResultRoundTripsThroughJSONAndConvertsToBenchmarkMetrics(t *testing.T) {
	r := Result{
		Scenario: "S1-buffered-0ms", Target: "http://127.0.0.1:8080", Model: "bench", RPS: 100, Duration: 30 * time.Second,
		Requests: 3000, Errors: 2, StatusCounts: map[int]int{200: 2998, 502: 2},
		Latency:  Summary{Count: 2998, P50: ms(3), P95: ms(6), P99: ms(9), Max: ms(20), Mean: ms(4)},
		Overhead: Summary{Count: 2998, P50: ms(1), P95: ms(2), P99: ms(3), Max: ms(5), Mean: ms(1)},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Scenario != r.Scenario || back.Requests != r.Requests || back.Latency.P99 != r.Latency.P99 || back.StatusCounts[502] != 2 {
		t.Errorf("round trip changed the result: %+v", back)
	}
	metrics := ToBenchmarkMetrics([]Result{r})
	if len(metrics) == 0 {
		t.Fatal("no metrics produced")
	}
	var sawP99, sawOverhead, sawErrors bool
	for _, m := range metrics {
		if m.Unit != "ms" && m.Unit != "%" && m.Unit != "req/s" {
			t.Errorf("metric %q has unit %q, want ms, %% or req/s", m.Name, m.Unit)
		}
		if strings.Contains(m.Name, "latency p99") && m.Value == 9 {
			sawP99 = true
		}
		if strings.Contains(m.Name, "overhead p99") && m.Value == 3 {
			sawOverhead = true
		}
		if strings.Contains(m.Name, "error rate") {
			sawErrors = true
		}
	}
	if !sawP99 || !sawOverhead || !sawErrors {
		t.Errorf("metrics missing expected entries: %+v", metrics)
	}
}

func TestMarkdownSummaryHasOneRowPerScenario(t *testing.T) {
	results := []Result{{Scenario: "S1", Requests: 10, Latency: Summary{Count: 10, P50: ms(1), P99: ms(2)}}, {Scenario: "S2", Stream: true, Requests: 5, TTFT: Summary{Count: 5, P50: ms(3), P99: ms(4)}}}
	md := MarkdownSummary(results)
	if strings.Count(md, "\n| S") != 2 {
		t.Errorf("markdown summary rows = %d, want 2:\n%s", strings.Count(md, "\n| S"), md)
	}
}
