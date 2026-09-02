package streaming

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// These benchmarks are the streaming transport's performance-regression
// baseline, mirroring internal/cache/inprocess's and internal/ratelimit's
// own bench_test.go files per docs/testing/TESTING.md §7 ("Track p50/p95/
// p99 latency ... as trend lines, not just pass/fail thresholds"). They
// are not a pass/fail gate on their own — go test -bench has no built-in
// threshold — but a recorded ns/op/allocs baseline, since both Reader and
// Writer sit directly in the hot loop of every SSE chunk forwarded to a
// client.

// BenchmarkReaderNext measures the cost of parsing one SSE event out of a
// realistic OpenAI-shaped stream.
func BenchmarkReaderNext(b *testing.B) {
	const event = `data: {"id":"chatcmpl-bench","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"

	r := NewReader(strings.NewReader(strings.Repeat(event, b.N)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Next(); err != nil {
			b.Fatalf("Next(): %v", err)
		}
	}
}

// BenchmarkWriteChunk measures the cost of encoding and framing one
// canonical chunk carrying a tool-call delta — a representative, non-empty
// shape rather than the cheapest all-blank case.
func BenchmarkWriteChunk(b *testing.B) {
	rec := httptest.NewRecorder()
	sw, err := NewWriter(rec)
	if err != nil {
		b.Fatalf("NewWriter: %v", err)
	}
	chunk := ChatCompletionChunk{
		ID:    "chunk-bench",
		Model: "gpt-4o",
		Choices: []ChunkChoice{
			{Index: 0, Delta: MessageDelta{
				ToolCalls: []ToolCallDelta{{Index: 0, ArgumentsJSON: `{"city":"Boston"}`}},
			}},
		},
	}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := sw.WriteChunk(chunk); err != nil {
			b.Fatalf("WriteChunk: %v", err)
		}
		// Reset the recorder's buffered body every iteration so a large
		// b.N doesn't accumulate unbounded memory — in production this
		// buffer never grows past one HTTP response's lifetime, so letting
		// it grow across the whole benchmark would measure something that
		// doesn't happen in practice.
		rec.Body.Reset()
	}
}
