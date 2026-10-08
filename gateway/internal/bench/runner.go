package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config describes one scenario run.
type Config struct {
	Scenario string
	// Target is the gateway's base URL; Targets, when set, is a list the
	// requests round-robin over (several replicas); Path defaults to
	// /v1/chat/completions.
	Target  string
	Targets []string
	Path    string
	// Bearer is the virtual-key secret sent as Authorization: Bearer.
	Bearer string
	Model  string
	Stream bool
	// RPS is the offered rate; Warmup requests run but are not measured;
	// Duration is the measured window.
	RPS      float64
	Warmup   time.Duration
	Duration time.Duration
	Seed     uint64
	// Prompt is the user message. Every request's prompt carries a suffix
	// that names this Run (its start time) and the request, so neither an
	// earlier request nor an earlier Run against the same gateway can have
	// warmed the cache for it — unless PromptPool > 0, in which case the
	// request part is drawn Zipf-distributed from a pool of that size (a
	// cache hit-ratio scenario; the Run part still keeps consecutive Runs
	// apart).
	Prompt     string
	PromptPool int
	ZipfS      float64
	MaxTokens  int
	// MaxInFlight caps concurrently open requests so a stalled target cannot
	// exhaust the harness; arrivals beyond it are counted as Dropped, never
	// silently skipped. Default 4096.
	MaxInFlight int
	// Timeout bounds one request (default 60 s).
	Timeout time.Duration
	// GatewayPID, when set, samples the gateway's RSS before, during and
	// after the run.
	GatewayPID int
	// Meta is copied into the result.
	Meta map[string]string
}

func (c *Config) defaults() {
	if c.Path == "" {
		c.Path = "/v1/chat/completions"
	}
	if c.Prompt == "" {
		c.Prompt = "Say hello."
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 64
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 4096
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	if c.ZipfS <= 1 {
		c.ZipfS = 1.1
	}
}

type sample struct {
	status    int
	transport bool
	// cancelled marks a request the harness itself cut off (the run's
	// context was cancelled while it was in flight): the harness's doing,
	// so it is neither a request nor an error in the result.
	cancelled bool
	// lateness is how long after its scheduled instant the request left the
	// harness (the coordinated-omission check: a harness that cannot keep
	// up shows it here instead of hiding it in shorter latencies).
	lateness   time.Duration
	latency    time.Duration
	overhead   *time.Duration
	stream     *StreamTiming
	responseID string
}

// Run executes the scenario and returns its Result.
func Run(ctx context.Context, cfg Config) (Result, error) {
	cfg.defaults()
	if len(cfg.Targets) == 0 && cfg.Target != "" {
		cfg.Targets = []string{cfg.Target}
	}
	if len(cfg.Targets) == 0 || cfg.Model == "" || cfg.RPS <= 0 || cfg.Duration <= 0 {
		return Result{}, errors.New("bench: a target, Model, RPS and Duration are required")
	}
	if cfg.Target == "" {
		cfg.Target = strings.Join(cfg.Targets, ",")
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	arrivals := PoissonArrivals(rng, cfg.RPS, cfg.Warmup+cfg.Duration)
	var zipf *rand.Zipf
	if cfg.PromptPool > 0 {
		zipf = rand.NewZipf(rng, cfg.ZipfS, 1, uint64(cfg.PromptPool-1))
	}
	client := &http.Client{Transport: &http.Transport{
		MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096, IdleConnTimeout: 90 * time.Second, DisableCompression: true,
	}}

	var (
		mu        sync.Mutex
		samples   []sample
		warmupIDs = map[string]bool{}
		dropped   int
		inFlight  int
		wg        sync.WaitGroup
	)
	res := Result{Scenario: cfg.Scenario, Target: cfg.Target, Model: cfg.Model, Stream: cfg.Stream, RPS: cfg.RPS,
		Duration: cfg.Duration, Warmup: cfg.Warmup, StartedAt: time.Now(), StatusCounts: map[int]int{}, Meta: cfg.Meta}
	// The run nonce keeps this Run's prompts apart from any earlier Run's
	// against the same gateway (the saturation sweep runs several Runs in
	// one process; a repeated prompt would be served from the cache). It is
	// letters only: a digit run reads as a phone or card number to the
	// gateway's default PII detectors and would add a finding to every request.
	runNonce := lettersNonce(res.StartedAt.UnixNano())

	rss := newRSSSampler(cfg.GatewayPID)
	rssDone := make(chan struct{})
	rssStopped := make(chan struct{})
	go func() {
		defer close(rssStopped)
		rss.samplePeaks(rssDone)
	}()

	start := time.Now()
	for i, at := range arrivals {
		intended := start.Add(at)
		if err := sleepUntil(ctx, intended); err != nil {
			break
		}
		measured := at >= cfg.Warmup
		var prompt string
		if zipf != nil {
			prompt = fmt.Sprintf("%s (run %s prompt %d)", cfg.Prompt, runNonce, zipf.Uint64())
		} else {
			prompt = fmt.Sprintf("%s (run %s request %d)", cfg.Prompt, runNonce, i)
		}
		mu.Lock()
		if inFlight >= cfg.MaxInFlight {
			if measured {
				dropped++
			}
			mu.Unlock()
			continue
		}
		inFlight++
		mu.Unlock()
		target := cfg.Targets[i%len(cfg.Targets)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := doRequest(ctx, client, cfg, target, prompt, intended)
			mu.Lock()
			inFlight--
			switch {
			case measured:
				samples = append(samples, s)
			case s.responseID != "":
				// A warm-up fill is a cache entry the measured window can
				// hit; without this the first measured hit of every
				// entry would count as a miss.
				warmupIDs[s.responseID] = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(rssDone)
	<-rssStopped
	res.RSS = rss.finish()
	res.Dropped = dropped
	summarize(&res, samples, warmupIDs, cfg)
	return res, ctx.Err()
}

func sleepUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// doRequest sends one request. intended is the instant the schedule asked
// for; lateness is stamped at t0, the instant the request leaves the
// harness, so lateness and latency together cover the whole interval from
// the scheduled instant to the last byte. A request cut off by the run's
// own cancellation is marked cancelled rather than failed.
func doRequest(ctx context.Context, client *http.Client, cfg Config, target, prompt string, intended time.Time) sample {
	body, _ := json.Marshal(map[string]any{
		"model":      cfg.Model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": cfg.MaxTokens,
		"stream":     cfg.Stream,
	})
	rctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(target, "/")+cfg.Path, strings.NewReader(string(body)))
	if err != nil {
		return sample{transport: true, lateness: time.Since(intended)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Bearer)
	t0 := time.Now()
	s := sample{lateness: t0.Sub(intended)}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return sample{cancelled: true}
		}
		s.transport, s.latency = true, time.Since(t0)
		return s
	}
	defer func() { _ = resp.Body.Close() }()
	s.status = resp.StatusCode
	if cfg.Stream && resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		timing, _ := ReadSSE(resp.Body, t0)
		if !timing.Done && ctx.Err() != nil {
			return sample{cancelled: true}
		}
		s.stream = &timing
		s.latency = timing.Total
		return s
	}
	var out struct {
		ID string `json:"id"`
	}
	raw, readErr := io.ReadAll(resp.Body)
	s.latency = time.Since(t0)
	if readErr != nil {
		if ctx.Err() != nil {
			return sample{cancelled: true}
		}
		s.transport = true // the body was cut short by something other than us
		return s
	}
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, &out)
		s.responseID = out.ID
		if h := resp.Header.Get("X-Kelvran-Overhead-Duration-Ms"); h != "" {
			if ms, err := strconv.ParseFloat(h, 64); err == nil {
				d := time.Duration(ms * float64(time.Millisecond))
				s.overhead = &d
			}
		}
	}
	return s
}

// streamFailed reports a stream that ended in an in-band error frame or
// without the [DONE] sentinel: the client did not get a complete answer,
// so it is an error, not a latency sample.
func streamFailed(t *StreamTiming) bool { return t.ErrorFrame || !t.Done }

func summarize(res *Result, samples []sample, warmupIDs map[string]bool, cfg Config) {
	var latencies, overheads, ttfts, gaps, lateness []time.Duration
	var frames []int
	seen := map[string]int{}
	for _, s := range samples {
		if s.cancelled {
			res.Cancelled++
			continue
		}
		res.Requests++
		lateness = append(lateness, s.lateness)
		if s.transport {
			res.TransportErrors++
			res.Errors++
			continue
		}
		res.StatusCounts[s.status]++
		if s.status < 200 || s.status >= 300 {
			res.Errors++
			continue
		}
		if s.stream != nil {
			frames = append(frames, s.stream.Frames)
			if streamFailed(s.stream) {
				res.Errors++
				if s.stream.ErrorFrame {
					res.ErrorFrames++
				} else {
					res.IncompleteStreams++
				}
				continue
			}
			ttfts = append(ttfts, s.stream.TTFT)
			gaps = append(gaps, s.stream.ContentGaps...)
		}
		latencies = append(latencies, s.latency)
		if s.overhead != nil {
			overheads = append(overheads, *s.overhead)
		}
		if s.responseID != "" {
			seen[s.responseID]++
		}
	}
	res.Latency = Summarize(latencies)
	res.Overhead = Summarize(overheads)
	res.TTFT = Summarize(ttfts)
	res.InterChunk = Summarize(gaps)
	res.FramesPerStream = SummarizeInts(frames)
	res.ScheduleLateness = Summarize(lateness)
	if cfg.Duration > 0 {
		secs := cfg.Duration.Seconds()
		res.SentRPS = float64(res.Requests) / secs
		res.CompletedRPS = float64(res.Requests-res.Errors) / secs
	}
	if cfg.PromptPool > 0 && len(seen) > 0 {
		// A response is a hit when its id was already seen — in the
		// warm-up or earlier in the window; the misses are the distinct
		// ids the warm-up had not fetched.
		total, misses := 0, 0
		for id, n := range seen {
			total += n
			if !warmupIDs[id] {
				misses++
			}
		}
		ratio := 1 - float64(misses)/float64(total)
		res.CacheHitRatio = &ratio
	}
}

// rssSampler reads one process's RSS: once synchronously before the load
// (idle), every second during it (peak), and once after it (end). A nil
// sampler (no pid, or the first read failed) does nothing and reports nil.
type rssSampler struct {
	pid int
	rss *RSS
}

func newRSSSampler(pid int) *rssSampler {
	if pid <= 0 {
		return nil
	}
	idle, err := SampleRSS(pid)
	if err != nil {
		return nil
	}
	return &rssSampler{pid: pid, rss: &RSS{IdleBytes: idle, PeakBytes: idle}}
}

func (r *rssSampler) samplePeaks(done <-chan struct{}) {
	if r == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if v, err := SampleRSS(r.pid); err == nil && v > r.rss.PeakBytes {
				r.rss.PeakBytes = v
			}
		}
	}
}

// finish takes the end sample; call it only after samplePeaks has returned.
func (r *rssSampler) finish() *RSS {
	if r == nil {
		return nil
	}
	if v, err := SampleRSS(r.pid); err == nil {
		r.rss.EndBytes = v
		if v > r.rss.PeakBytes {
			r.rss.PeakBytes = v
		}
	}
	return r.rss
}

// lettersNonce encodes n as lower-case letters only (base 26, 'a'..'z'),
// so a prompt suffix can carry a run identifier without forming a digit run
// that the gateway's default PII detectors would read as a phone or card
// number (see prompt_pii_test.go).
func lettersNonce(n int64) string {
	if n < 0 {
		n = -n
	}
	var buf [16]byte
	i := len(buf)
	for {
		i--
		buf[i] = byte('a' + n%26)
		n /= 26
		if n == 0 {
			break
		}
	}
	return string(buf[i:])
}
