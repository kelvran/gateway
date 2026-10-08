package bench

import "time"

// Scenario is a named preset shared by the harness (what to send) and the
// mock upstream (how to answer), so `kelvran-bench upstream -scenario S2`
// and `kelvran-bench run -scenario S2` describe one experiment. What the
// recipe (scripts/bench-ci.sh) does around a preset — a second gateway
// replica for S4, the gateway pid for RSS sampling — is the recipe's own
// knowledge, keyed by the preset's name.
type Scenario struct {
	Name        string
	Description string
	Stream      bool
	// RPS is the default offered rate (overridable); RPSSteps, when set,
	// runs the scenario once per step (the saturation sweep) and the rate
	// override is ignored.
	RPS      float64
	RPSSteps []float64
	Duration time.Duration
	Warmup   time.Duration
	// PromptPool > 0 draws prompts Zipf-distributed from a pool so the
	// gateway's cache can hit; 0 makes every prompt unique.
	PromptPool int
	// Mock-side knobs.
	MockLatency       time.Duration
	MockChunks        int
	MockChunkInterval time.Duration
	MockPromptTokens  int
	MockOutputTokens  int
}

// Scenarios lists the presets docs/operations/BENCHMARKS.md describes.
func Scenarios() []Scenario {
	return []Scenario{
		{Name: "S1a", Description: "buffered chat, mock latency 0 ms: the gateway's own overhead with nothing to hide behind", RPS: 100, Duration: 120 * time.Second, Warmup: 60 * time.Second, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S1b", Description: "buffered chat, mock latency 200 ms: overhead must not grow with upstream time", RPS: 100, Duration: 120 * time.Second, Warmup: 60 * time.Second, MockLatency: 200 * time.Millisecond, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S2", Description: "streaming, 50 chunks at 10 ms: time to first token and inter-chunk latency added by the gateway", Stream: true, RPS: 100, Duration: 120 * time.Second, Warmup: 60 * time.Second, MockLatency: 50 * time.Millisecond, MockChunks: 50, MockChunkInterval: 10 * time.Millisecond, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S3", Description: "cache hit ratio and hit latency: Zipf-distributed repeats from a pool of 200 prompts against a 200 ms upstream", RPS: 100, Duration: 120 * time.Second, Warmup: 60 * time.Second, PromptPool: 200, MockLatency: 200 * time.Millisecond, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S4", Description: "two replicas sharing Redis (rate limits, budgets, identity, propagation), load round-robined; the recipe starts the second gateway", RPS: 100, Duration: 120 * time.Second, Warmup: 60 * time.Second, MockLatency: 50 * time.Millisecond, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S5", Description: "memory: the gateway's RSS idle, under load, and after a 10-minute soak", RPS: 1000, Duration: 10 * time.Minute, Warmup: 60 * time.Second, MockPromptTokens: 50, MockOutputTokens: 100},
		{Name: "S6", Description: "saturation knee: offered rate stepped 100, 500, 1000, 2000 rps (one result per step, named S6-<rate>rps; every step runs) to find where the error rate or p99 breaks", RPSSteps: []float64{100, 500, 1000, 2000}, Duration: 60 * time.Second, Warmup: 30 * time.Second, MockPromptTokens: 50, MockOutputTokens: 100},
	}
}

// LookupScenario finds a preset by name.
func LookupScenario(name string) (Scenario, bool) {
	for _, s := range Scenarios() {
		if s.Name == name {
			return s, true
		}
	}
	return Scenario{}, false
}
