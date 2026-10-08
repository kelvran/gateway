package bench

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/benchupstream"
)

// TestRunAgainstTheMockUpstreamBuffered drives the open-loop runner straight
// at benchupstream (no gateway): every arrival must become a request, none
// may error, and the latency floor is the mock's configured latency.
func TestRunAgainstTheMockUpstreamBuffered(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{Latency: 20 * time.Millisecond, PromptTokens: 5, CompletionTokens: 5})
	srv := httptest.NewServer(up)
	defer srv.Close()

	res, err := Run(context.Background(), Config{
		Scenario: "test-buffered", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 50,
		Warmup: 200 * time.Millisecond, Duration: time.Second, Seed: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Requests < 25 || res.Requests > 90 {
		t.Errorf("requests = %d, want about 50 (50 rps for 1 s, warm-up excluded)", res.Requests)
	}
	if res.Errors != 0 || res.StatusCounts[200] != res.Requests {
		t.Errorf("errors = %d, statuses = %v, want none and all 200", res.Errors, res.StatusCounts)
	}
	if res.Latency.P50 < 20*time.Millisecond {
		t.Errorf("p50 latency = %s, want >= the mock's 20ms", res.Latency.P50)
	}
	if res.Latency.Count != res.Requests {
		t.Errorf("latency samples = %d, want one per request (%d)", res.Latency.Count, res.Requests)
	}
	if int64(res.Requests) > up.Calls() {
		t.Errorf("counted %d requests but the mock saw only %d", res.Requests, up.Calls())
	}
	if res.Overhead.Count != 0 {
		t.Errorf("overhead samples = %d, want 0 when the target sets no overhead header", res.Overhead.Count)
	}
}

func TestRunAgainstTheMockUpstreamStreaming(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{Latency: 15 * time.Millisecond, StreamChunks: 4, ChunkInterval: 5 * time.Millisecond})
	srv := httptest.NewServer(up)
	defer srv.Close()

	res, err := Run(context.Background(), Config{
		Scenario: "test-stream", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 20, Stream: true,
		Duration: time.Second, Seed: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Requests < 8 || res.Errors != 0 {
		t.Fatalf("requests/errors = %d/%d, want about 20 and none", res.Requests, res.Errors)
	}
	if res.TTFT.Count != res.Requests || res.TTFT.P50 < 15*time.Millisecond {
		t.Errorf("TTFT = %+v, want one sample per request and p50 >= the mock's 15ms", res.TTFT)
	}
	// role + 4 content + finish + usage = 7 data frames before [DONE].
	if res.FramesPerStream.P50 != 7 {
		t.Errorf("frames per stream p50 = %d, want 7", res.FramesPerStream.P50)
	}
	if res.InterChunk.Count == 0 || res.InterChunk.P50 < 4*time.Millisecond {
		t.Errorf("inter-chunk gaps = %+v, want samples with p50 >= ~5ms", res.InterChunk)
	}
	if res.IncompleteStreams != 0 {
		t.Errorf("incomplete streams = %d, want 0", res.IncompleteStreams)
	}
}

func TestRunCountsNon2xxAsErrors(t *testing.T) {
	srv := httptest.NewServer(benchupstream.New(benchupstream.Config{}))
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "bad-path", Target: srv.URL, Path: "/v1/embeddings", Bearer: "x", Model: "bench", RPS: 30, Duration: 500 * time.Millisecond, Seed: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Requests == 0 || res.Errors != res.Requests || res.StatusCounts[404] != res.Requests {
		t.Errorf("requests/errors/404s = %d/%d/%d, want every request counted as a 404 error", res.Requests, res.Errors, res.StatusCounts[404])
	}
}

func TestSampleRSSOfThisProcessIsPositive(t *testing.T) {
	rss, err := SampleRSS(os.Getpid())
	if err != nil {
		t.Skipf("RSS sampling unavailable on this platform: %v", err)
	}
	if rss <= 0 {
		t.Errorf("RSS = %d, want positive bytes", rss)
	}
}
