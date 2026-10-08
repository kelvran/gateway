package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/benchupstream"
)

// promptRecorder answers like the mock but remembers every distinct prompt
// and, when replay is on, returns the same completion id for the same prompt
// (what a cache replay looks like from the harness's side).
type promptRecorder struct {
	mu      sync.Mutex
	prompts map[string]int
	ids     map[string]string
	replay  bool
	calls   int
}

func (p *promptRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	p.mu.Lock()
	p.calls++
	p.prompts[prompt]++
	id, ok := p.ids[prompt]
	if !ok || !p.replay {
		id = fmt.Sprintf("chatcmpl-%d", p.calls)
		p.ids[prompt] = id
	}
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%q,"object":"chat.completion","model":"bench","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, id)
}

func newPromptRecorder(replay bool) *promptRecorder {
	return &promptRecorder{prompts: map[string]int{}, ids: map[string]string{}, replay: replay}
}

// TestRunUsesPromptsNoEarlierRunInTheSameProcessSent is the S6 fix: the
// saturation sweep runs several Runs against one gateway, and a repeated
// prompt would be served from the gateway's cache on the later steps.
func TestRunUsesPromptsNoEarlierRunInTheSameProcessSent(t *testing.T) {
	rec := newPromptRecorder(false)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), Config{Scenario: "sweep", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 40, Duration: 500 * time.Millisecond, Seed: 1}); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for prompt, n := range rec.prompts {
		if n > 1 {
			t.Fatalf("prompt %q was sent %d times across two runs; every request must carry a prompt no earlier run sent", prompt, n)
		}
	}
	if rec.calls < 20 {
		t.Fatalf("calls = %d, want the two runs' requests", rec.calls)
	}
}

// TestCacheHitRatioCountsWarmupFillsAsHits: with one prompt and a replaying
// upstream, every measured request is a replay of the id the warm-up fetched,
// so the ratio must be exactly 1 — not 1 - 1/total, which is what counting
// the first measured sighting as a miss produces.
func TestCacheHitRatioCountsWarmupFillsAsHits(t *testing.T) {
	rec := newPromptRecorder(true)
	srv := httptest.NewServer(rec)
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "hits", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 50, Warmup: 300 * time.Millisecond, Duration: time.Second, Seed: 2, PromptPool: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheHitRatio == nil {
		t.Fatal("CacheHitRatio not set for a PromptPool scenario")
	}
	if *res.CacheHitRatio != 1 {
		t.Errorf("cache hit ratio = %.4f, want exactly 1 (the only miss happened during warm-up)", *res.CacheHitRatio)
	}
	if res.Requests < 20 {
		t.Errorf("requests = %d, want the measured window's requests", res.Requests)
	}
}

func TestStreamsEndingInAnErrorFrameOrWithoutDoneAreErrorsAndExcludedFromPercentiles(t *testing.T) {
	cases := map[string]struct {
		body           string
		wantErrorFrame bool
	}{
		"error frame": {"data: {\"id\":\"a\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"error\":{\"message\":\"upstream call failed\",\"type\":\"server_error\",\"code\":\"upstream_error\"}}\n\n", true},
		"no sentinel": {"data: {\"id\":\"a\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			res, err := Run(context.Background(), Config{Scenario: name, Target: srv.URL, Bearer: "x", Model: "bench", Stream: true, RPS: 30, Duration: 500 * time.Millisecond, Seed: 3})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Requests == 0 || res.Errors != res.Requests {
				t.Fatalf("requests/errors = %d/%d, want every stream counted as an error", res.Requests, res.Errors)
			}
			if res.Latency.Count != 0 || res.TTFT.Count != 0 {
				t.Errorf("latency/TTFT samples = %d/%d, want failed streams excluded from the success percentiles", res.Latency.Count, res.TTFT.Count)
			}
			if tc.wantErrorFrame && res.ErrorFrames != res.Requests {
				t.Errorf("error frames = %d, want %d", res.ErrorFrames, res.Requests)
			}
			if !tc.wantErrorFrame && res.IncompleteStreams != res.Requests {
				t.Errorf("incomplete streams = %d, want %d", res.IncompleteStreams, res.Requests)
			}
		})
	}
}

func TestRunWarmupIsSentButNotMeasured(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{})
	srv := httptest.NewServer(up)
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "warm", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 100, Warmup: 500 * time.Millisecond, Duration: 500 * time.Millisecond, Seed: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	unmeasured := int(up.Calls()) - res.Requests
	if unmeasured < 20 || unmeasured > 90 {
		t.Errorf("warm-up requests sent but not measured = %d (calls %d, measured %d), want about 50", unmeasured, up.Calls(), res.Requests)
	}
}

func TestRunRoundRobinsAcrossTargets(t *testing.T) {
	a, b := benchupstream.New(benchupstream.Config{}), benchupstream.New(benchupstream.Config{})
	sa, sb := httptest.NewServer(a), httptest.NewServer(b)
	defer sa.Close()
	defer sb.Close()
	res, err := Run(context.Background(), Config{Scenario: "rr", Targets: []string{sa.URL, sb.URL}, Bearer: "x", Model: "bench", RPS: 60, Duration: time.Second, Seed: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.Calls() == 0 || b.Calls() == 0 || a.Calls()+b.Calls() != int64(res.Requests) {
		t.Errorf("calls a/b = %d/%d for %d requests, want both targets used and every request accounted for", a.Calls(), b.Calls(), res.Requests)
	}
	if diff := a.Calls() - b.Calls(); diff > 1 || diff < -1 {
		t.Errorf("round robin uneven: a=%d b=%d", a.Calls(), b.Calls())
	}
}

func TestRunCountsDroppedArrivalsWhenTheInFlightCapIsHit(t *testing.T) {
	srv := httptest.NewServer(benchupstream.New(benchupstream.Config{Latency: 200 * time.Millisecond}))
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "cap", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 50, Duration: time.Second, Seed: 6, MaxInFlight: 1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Dropped == 0 {
		t.Errorf("dropped = 0 with MaxInFlight 1 against a 200 ms upstream at 50 rps, want arrivals beyond the cap counted as dropped")
	}
	if res.Requests+res.Dropped < 25 {
		t.Errorf("requests+dropped = %d, want about the 50 arrivals", res.Requests+res.Dropped)
	}
}

func TestRunCountsTransportErrorsWhenNothingListens(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens any more
	res, err := Run(context.Background(), Config{Scenario: "dead", Target: url, Bearer: "x", Model: "bench", RPS: 40, Duration: 500 * time.Millisecond, Seed: 7})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Requests == 0 || res.TransportErrors != res.Requests || res.Errors != res.Requests || res.Latency.Count != 0 {
		t.Errorf("requests/transport/errors/latency samples = %d/%d/%d/%d, want every request a transport error with no latency sample", res.Requests, res.TransportErrors, res.Errors, res.Latency.Count)
	}
}

func TestRunReportsSentAndCompletedRatesAndScheduleLateness(t *testing.T) {
	srv := httptest.NewServer(benchupstream.New(benchupstream.Config{}))
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "rates", Target: srv.URL, Bearer: "x", Model: "bench", RPS: 50, Duration: time.Second, Seed: 8})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SentRPS < 30 || res.SentRPS > 75 || res.CompletedRPS != res.SentRPS {
		t.Errorf("sent/completed rps = %.1f/%.1f, want about 50 and equal with no errors", res.SentRPS, res.CompletedRPS)
	}
	if res.ScheduleLateness.Count != res.Requests {
		t.Errorf("schedule lateness samples = %d, want one per request (%d)", res.ScheduleLateness.Count, res.Requests)
	}
}
