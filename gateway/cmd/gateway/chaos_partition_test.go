// See integration_test.go's own doc comment for why this lives in
// package main.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
)

// chaosNginxMockUpstreamConfig is a minimal nginx config returning a
// fixed, valid OpenAI-shaped chat-completion response for every request,
// regardless of path or method (nginx's `return` directive is a
// config-level canned response, never inspecting the request body) --
// this test's real, containerized "chaos" upstream needs a genuine
// Docker container for `docker pause`/`docker unpause` to act on, which
// an in-process httptest.Server (used everywhere else in this package)
// cannot provide at all.
const chaosNginxMockUpstreamConfig = `server {
	listen 80;
	location / {
		default_type application/json;
		return 200 '{"id":"chatcmpl-chaos-test","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}';
	}
}
`

// newChaosMockUpstreamContainer starts a real nginx:alpine container
// configured (via chaosNginxMockUpstreamConfig) to answer every request
// with a fixed, valid OpenAI chat-completion response. Returns its real
// container ID (for docker pause/unpause, shelled out directly -- neither
// testcontainers.Container's own interface nor this codebase's HTTP
// client exposes that operation) and the base URL the gateway's "chaos"
// deployment should point at.
//
// Uses `docker pause`/`docker unpause` rather than Stop/Start, per this
// repo's own already-established real-partition technique (see
// internal/configpropagation's
// TestSubscribeSurvivesARealRedisPartitionAndDeliversEventsAfterRecovery):
// pausing freezes the container's process via the cgroup freezer with its
// port mapping and network namespace fully intact -- every TCP
// connection to it goes silently unresponsive exactly like a real
// network partition, confirmed in that same prior test to NOT hold for
// Stop/Start (which can remap the host port on restart in this
// environment).
func newChaosMockUpstreamContainer(t *testing.T) (containerID, baseURL string) {
	t.Helper()
	ctx := context.Background()

	req := tc.ContainerRequest{
		Image:        "nginx:alpine",
		ExposedPorts: []string{"80/tcp"},
		Files: []tc.ContainerFile{
			{
				Reader:            strings.NewReader(chaosNginxMockUpstreamConfig),
				ContainerFilePath: "/etc/nginx/conf.d/default.conf",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/").WithPort("80/tcp"),
	}
	container, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		t.Fatalf("starting chaos mock upstream container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminating chaos mock upstream container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container.Host: %v", err)
	}
	port, err := container.MappedPort(ctx, "80/tcp")
	if err != nil {
		t.Fatalf("container.MappedPort: %v", err)
	}
	return container.GetContainerID(), fmt.Sprintf("http://%s:%s", host, port.Port())
}

// sendChaosTestBurst sends n sequential real HTTP client requests
// (distinct content per request, so none is ever served from cache)
// against gw, asserting every one gets a real 200 OK, and returns how
// many of them were served by the "stable" deployment specifically (per
// stableCalls' own delta across the call) -- the same observable-via-
// real-upstream-call-counts technique
// health_probe_integration_test.go's own
// TestIntegrationHealthProbeRoutesAroundConsistentlyFailingDeployment
// already established, extended here to report a count rather than
// assert a single fixed expectation, since this test's two call sites
// want different assertions on the same measurement.
func sendChaosTestBurst(t *testing.T, gw *httptest.Server, gatewayKey string, stableCalls interface{ Load() int64 }, n int, contentPrefix string) (stableDelta int64) {
	t.Helper()
	before := stableCalls.Load()
	client := &http.Client{}
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("%s-%d", contentPrefix, i)
		reqBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":%q}]}`, content)
		req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(reqBody))
		if err != nil {
			t.Fatalf("building request %d: %v", i, err)
		}
		req.Header.Set("Authorization", "Bearer "+gatewayKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, resp.StatusCode, body)
		}
	}
	return stableCalls.Load() - before
}

// TestIntegrationHealthProbeCircuitBreakerTripsAndRecoversAcrossARealPartition
// is item 8's chaos experiment: a real network partition (via docker
// pause/unpause against a genuine container, see
// newChaosMockUpstreamContainer's own doc comment) against one of two
// real deployments for the same model, proving the active-health-probing
// circuit breaker (router.HealthConfig's UnhealthyThreshold/
// HealthyThreshold, per
// docs/rfcs/2026-09-07-gateway-active-health-probing.md) both trips
// (routes every real client request around the partitioned deployment
// once the failure threshold is reached) and recovers (resumes routing
// real traffic to it once connectivity returns and the recovery
// threshold is reached) -- the two properties a circuit breaker exists
// to provide, proven end-to-end over real HTTP against a real container,
// not asserted from the router package's own unit tests alone.
//
// Configures RecoveryRampInitialPercent: 100 (a documented no-op ramp,
// per router.HealthConfig's own doc comment: "values above 100 are
// clamped to 100") specifically to keep the recovery assertion below
// deterministic: without this, a just-recovered deployment's effective
// weight ramps back up gradually across further probe outcomes (per
// docs/rfcs/2026-09-08-gateway-health-probe-backoff.md's thundering-herd
// mitigation), which is real, desirable production behavior but would
// make "does real traffic reach it again" depend on exactly how many
// probes ran and the WRR cursor's own internal phase -- neither of which
// this test needs to reason about to prove the two properties it's
// actually about.
//
// Uses pipeline.ProbeDeployments directly (deterministic, no real
// elapsed time) rather than RunHealthProbeLoop's real ticker, per
// health_probe_integration_test.go's own established precedent for
// exactly this reason.
func TestIntegrationHealthProbeCircuitBreakerTripsAndRecoversAcrossARealPartition(t *testing.T) {
	containerID, chaosBaseURL := newChaosMockUpstreamContainer(t)

	stableUpstream, stableCalls := newHealthProbeCountingUpstream(true)
	defer stableUpstream.Close()

	t.Setenv("KELVRAN_CHAOS_TEST_STABLE_KEY", "fake-upstream-key-not-a-real-secret")
	t.Setenv("KELVRAN_CHAOS_TEST_CHAOS_KEY", "fake-upstream-key-not-a-real-secret")

	gatewayKey := "test-gateway-key-chaos-partition"
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "test-key", KeyHash: testKeyHash(gatewayKey), RateLimitBurst: 1000, RateLimitRefill: 1000},
		},
		Deployments: []controlplane.DeploymentConfig{
			{
				Name:          "stable",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       stableUpstream.URL,
				APIKeyEnv:     "KELVRAN_CHAOS_TEST_STABLE_KEY",
			},
			{
				Name:          "chaos",
				Model:         "gpt-4o",
				Provider:      "openai",
				UpstreamModel: "gpt-4o",
				BaseURL:       chaosBaseURL,
				APIKeyEnv:     "KELVRAN_CHAOS_TEST_CHAOS_KEY",
			},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		HealthProbe: controlplane.HealthProbeConfig{
			UnhealthyThreshold:         3,
			HealthyThreshold:           2,
			RecoveryRampInitialPercent: 100,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline, err := buildPipeline(cfg, logger)
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	t.Cleanup(func() { _ = pipeline.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chatCompletionsHandler(pipeline))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	ctx := context.Background()

	// Baseline: prove "chaos" is genuinely live, over the real container,
	// before ever touching it.
	pipeline.ProbeDeployments(ctx)

	if out, err := exec.Command("docker", "pause", containerID).CombinedOutput(); err != nil {
		t.Fatalf("docker pause %s: %v (%s)", containerID, err, out)
	}
	// Always unpause before returning, even on a t.Fatalf above/below --
	// a paused container left behind would confuse any later local
	// re-run against the same Docker daemon.
	unpaused := false
	defer func() {
		if !unpaused {
			if out, err := exec.Command("docker", "unpause", containerID).CombinedOutput(); err != nil {
				t.Logf("docker unpause %s during cleanup: %v (%s)", containerID, err, out)
			}
		}
	}()

	// Trip: 3 consecutive failed probes (UnhealthyThreshold) while
	// "chaos" is genuinely unreachable. Each probe against the paused
	// container blocks until dataplane.healthProbeCallTimeout (5s) fires
	// -- a real, bounded wait, not a hang -- matching the already-
	// validated Redis-partition test's own documented "connections go
	// silently unresponsive" behavior for a paused container.
	//
	// Sanity-checked-by-breaking directly against this test: commenting
	// out these 3 calls (so the breaker never trips) makes the burst
	// below hang indefinitely instead of failing cleanly -- confirming
	// the assertion below is genuinely load-bearing, and surfacing a
	// real, disclosed-but-out-of-scope-for-this-item finding: the
	// buffered request path has no per-upstream-call timeout of its own
	// (unlike the streaming path's idleTimeout, dataplane.go's
	// NewHTTPUpstreamStreamCaller) -- a live client request against a
	// deployment health-probing hasn't yet marked unhealthy can hang
	// indefinitely against a genuinely unresponsive upstream. Not fixed
	// here -- this item is scoped to proving the EXISTING circuit
	// breaker works, not adding a new timeout mechanism.
	pipeline.ProbeDeployments(ctx)
	pipeline.ProbeDeployments(ctx)
	pipeline.ProbeDeployments(ctx)

	// The load-bearing "trip" assertion: with "chaos" excluded, every
	// real client request must land on "stable" -- deterministic,
	// zero ramp/WRR-cursor complexity involved, since an unhealthy
	// deployment is unconditionally rejected before any weight/ramp
	// logic ever runs.
	const burstSize = 20
	partitionedDelta := sendChaosTestBurst(t, gw, gatewayKey, stableCalls, burstSize, "chaos-partition-tripped")
	if partitionedDelta != int64(burstSize) {
		t.Errorf("stable deployment served %d/%d requests while \"chaos\" was partitioned and tripped, want all %d -- the circuit breaker did not exclude it", partitionedDelta, burstSize, burstSize)
	}

	if out, err := exec.Command("docker", "unpause", containerID).CombinedOutput(); err != nil {
		t.Fatalf("docker unpause %s: %v (%s)", containerID, err, out)
	}
	unpaused = true

	// Recover: 2 consecutive successful probes (HealthyThreshold) against
	// the now-reachable container.
	pipeline.ProbeDeployments(ctx)
	pipeline.ProbeDeployments(ctx)

	// The load-bearing "recover" assertion: with RecoveryRampInitialPercent
	// set to a no-op 100 (see this test's own doc comment), "chaos" is
	// immediately back at full WRR weight alongside "stable" -- real
	// traffic must reach BOTH across a 20-request burst (a strict
	// inequality, not an exact 50/50 split, since the WRR cursor's exact
	// phase entering this burst is an internal implementation detail this
	// test deliberately does not couple to).
	recoveredDelta := sendChaosTestBurst(t, gw, gatewayKey, stableCalls, burstSize, "chaos-partition-recovered")
	if recoveredDelta <= 0 || recoveredDelta >= int64(burstSize) {
		t.Errorf("stable deployment served %d/%d requests after \"chaos\" recovered, want strictly between 0 and %d -- real traffic must reach BOTH deployments again once the breaker recovers", recoveredDelta, burstSize, burstSize)
	}
}
