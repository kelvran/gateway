package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

// Round-5 item 10 phase 2c-1: when rate_limit.redis_addr and budget.redis_addr
// are the same Redis (the ledger always follows the budget), the rate-limit,
// budget and ledger stores share ONE go-redis client -- one pool, one hook
// labelled "shared", one close -- which is what lets the later phases fuse
// their scripts into one round trip each side.

func TestSameRedisOptions(t *testing.T) {
	base := redis.Options{Addr: "r:6379", Username: "u", Password: "p"}
	withTLS := base
	withTLS.TLSConfig = redisTLSForTest()
	cases := []struct {
		name string
		a, b redis.Options
		want bool
	}{
		{"identical", base, base, true},
		{"different address", base, redis.Options{Addr: "r2:6379", Username: "u", Password: "p"}, false},
		{"loopback spelling differs", redis.Options{Addr: "127.0.0.1:6379"}, redis.Options{Addr: "localhost:6379"}, false},
		{"different username", base, redis.Options{Addr: "r:6379", Username: "v", Password: "p"}, false},
		{"different password", base, redis.Options{Addr: "r:6379", Username: "u", Password: "q"}, false},
		{"one side TLS", base, withTLS, false},
		{"both TLS", withTLS, withTLS, true},
		{"empty address is never shared", redis.Options{}, redis.Options{}, false},
	}
	for _, c := range cases {
		if got := sameRedisOptions(c.a, c.b); got != c.want {
			t.Errorf("%s: sameRedisOptions = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOpenSharedRedisEligibility(t *testing.T) {
	t.Setenv("KELVRAN_TEST_RL_PW", "secret")
	t.Setenv("KELVRAN_TEST_BUDGET_PW", "secret")
	t.Setenv("KELVRAN_TEST_OTHER_PW", "different")
	cases := []struct {
		name string
		cfg  controlplane.Config
		want bool
	}{
		{"both unset", controlplane.Config{}, false},
		{"only the limiter on Redis", controlplane.Config{RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1"}}, false},
		{"only the budget on Redis", controlplane.Config{Budget: controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1"}}, false},
		{"same address", controlplane.Config{RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1"}, Budget: controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1"}}, true},
		{"different address", controlplane.Config{RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1"}, Budget: controlplane.BudgetConfig{RedisAddr: "127.0.0.1:2"}}, false},
		{"same address, different usernames", controlplane.Config{
			RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{Username: "a"}},
			Budget:    controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{Username: "b"}}}, false},
		{"same address, different env names holding the same secret", controlplane.Config{
			RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{PasswordEnv: "KELVRAN_TEST_RL_PW"}},
			Budget:    controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{PasswordEnv: "KELVRAN_TEST_BUDGET_PW"}}}, true},
		{"same address, different secrets", controlplane.Config{
			RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{PasswordEnv: "KELVRAN_TEST_RL_PW"}},
			Budget:    controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{PasswordEnv: "KELVRAN_TEST_OTHER_PW"}}}, false},
		{"same address, one side TLS", controlplane.Config{
			RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1", Redis: controlplane.RedisAuthConfig{TLS: true}},
			Budget:    controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1"}}, false},
	}
	for _, c := range cases {
		cfg := c.cfg
		s := openSharedRedis(&cfg)
		if (s != nil) != c.want {
			t.Errorf("%s: openSharedRedis non-nil = %v, want %v", c.name, s != nil, c.want)
		}
		if s != nil {
			_ = s.client.Close()
		}
	}
}

func TestSharedRedisClosesTheClientOnlyAfterTheLastRelease(t *testing.T) {
	s := &sharedRedis{client: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})}
	r1, r2, r3 := s.acquire(), s.acquire(), s.acquire()

	closed := func() bool {
		err := s.client.Ping(context.Background()).Err()
		return errors.Is(err, redis.ErrClosed)
	}
	if err := r1(); err != nil || closed() {
		t.Fatalf("after the first release: err %v, closed %v; want the client still open", err, closed())
	}
	if err := r1(); err != nil || closed() {
		t.Fatalf("a repeated release of one handle must not count twice: err %v, closed %v", err, closed())
	}
	if err := r2(); err != nil || closed() {
		t.Fatalf("after the second release: err %v, closed %v; want the client still open", err, closed())
	}
	if err := r3(); err != nil {
		t.Fatalf("the last release must close the client without error, got %v", err)
	}
	if !closed() {
		t.Fatal("the client must be closed once the last holder released it")
	}
}

// The shared client carries exactly one hook, so a command is counted once,
// under the "shared" store label.
func TestSharedRedisCountsEachCommandOnceUnderTheSharedLabel(t *testing.T) {
	reader := cmdGatewayMetricsReaderForTest()
	before := redisRoundTripsByStore(t, reader)
	s := openSharedRedis(&controlplane.Config{
		RateLimit: controlplane.RateLimitConfig{RedisAddr: "127.0.0.1:1"},
		Budget:    controlplane.BudgetConfig{RedisAddr: "127.0.0.1:1"},
	})
	if s == nil {
		t.Fatal("same address must share")
	}
	defer func() { _ = s.client.Close() }()
	_ = s.client.Ping(context.Background()).Err() // fails to dial; the hook still counts the attempt
	after := redisRoundTripsByStore(t, reader)
	if d := after["shared"] - before["shared"]; d != 1 {
		t.Fatalf("round trips under \"shared\" = %d, want 1 (one hook, one count)", d)
	}
	for _, other := range []string{"ratelimit", "budget", "ledger"} {
		if after[other] != before[other] {
			t.Errorf("store %q moved on a shared client", other)
		}
	}
}

func redisRoundTripsByStore(t *testing.T, reader interface {
	Collect(context.Context, *metricdata.ResourceMetrics) error
}) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kelvran.redis.round_trips" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("kelvran.redis.round_trips is %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value("kelvran.redis.store"); ok {
					out[v.AsString()] += dp.Value
				}
			}
		}
	}
	return out
}

func newSharedRedisIntegrationServer(t *testing.T, upstreamURL, upstreamKeyEnvVar, rateLimitAddr, budgetAddr string, keys []controlplane.VirtualKeyConfig) (*httptest.Server, *dataplane.Pipeline) {
	t.Helper()
	t.Setenv(upstreamKeyEnvVar, "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr:  ":0",
		VirtualKeys: keys,
		Deployments: []controlplane.DeploymentConfig{{
			Name: "gpt4o-primary", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o",
			BaseURL: upstreamURL, APIKeyEnv: upstreamKeyEnvVar,
		}},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
		RateLimit: controlplane.RateLimitConfig{RedisAddr: rateLimitAddr},
		Budget:    controlplane.BudgetConfig{RedisAddr: budgetAddr},
	}
	pipeline, err := buildPipeline(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("buildPipeline: %v", err)
	}
	srv := httptest.NewServer(newDataPlaneMux(pipeline))
	t.Cleanup(srv.Close)
	return srv, pipeline
}

// doDistinctChatRequest sends a chat request whose content is unique to n, so
// no two requests share a cache entry: a repeated body would be a cache hit,
// which is not billable and writes no ledger row.
func doDistinctChatRequest(t *testing.T, gw *httptest.Server, secret string, n int) (int, string) {
	t.Helper()
	reqBody := fmt.Sprintf(`{"model":"gpt-4o","messages":[{"role":"user","content":"billable request number %d"}]}`, n)
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// chatRoundTripsOverFourWarmRequests sends one warm-up request (the first
// call of each script on a fresh Redis pays an EVALSHA miss) and then four
// sequential billable ones, returning the round trips each store label accrued.
func chatRoundTripsOverFourWarmRequests(t *testing.T, gw *httptest.Server, secret string) map[string]int64 {
	t.Helper()
	reader := cmdGatewayMetricsReaderForTest()
	if code, body := doDistinctChatRequest(t, gw, secret, 0); code != 200 {
		t.Fatalf("warm-up request = %d %s", code, body)
	}
	before := redisRoundTripsByStore(t, reader)
	for i := 1; i <= 4; i++ {
		if code, body := doDistinctChatRequest(t, gw, secret, i); code != 200 {
			t.Fatalf("request %d = %d %s", i, code, body)
		}
	}
	after := redisRoundTripsByStore(t, reader)
	delta := map[string]int64{}
	for k, v := range after {
		if d := v - before[k]; d != 0 {
			delta[k] = d
		}
	}
	return delta
}

func sharedRedisTestKeys(secret string) []controlplane.VirtualKeyConfig {
	return []controlplane.VirtualKeyConfig{{
		Name: "team-shared-client", KeyHash: testKeyHash(secret),
		RateLimitBurst: 1000, RateLimitRefill: 1000, TokensPerDay: 100_000_000, BudgetUSD: decimal.NewFromInt(1_000_000),
	}}
}

// TestIntegrationSharedRedisClientServesAllThreeStores is the end-to-end proof:
// with rate_limit.redis_addr and budget.redis_addr equal, every Redis round
// trip of a billable request goes through the one shared client (labelled
// "shared"), the count is still the 5.0 of phase 2b, and closing the pipeline
// closes the client exactly once without an error (the server sees the pool's
// connections go).
func TestIntegrationSharedRedisClientServesAllThreeStores(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	upstream, _ := newMockUpstream(t)
	gw, pipeline := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_SHARED", redisAddr, redisAddr, sharedRedisTestKeys("shared-client-secret"))

	got := chatRoundTripsOverFourWarmRequests(t, gw, "shared-client-secret")
	if len(got) != 1 || got["shared"] != 20 {
		t.Fatalf("round trips by store over 4 requests = %v, want only shared=20 (5.0 each: gate, budget reserve, budget adjust, token settle, ledger record)", got)
	}
	observer := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer func() { _ = observer.Close() }()
	before := connectedRedisClients(t, observer)
	if before < 2 {
		t.Fatalf("connected clients before Close = %d, want the observer plus the gateway's pooled connections", before)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatalf("closing a pipeline over one shared client = %v, want nil (three closes, one client: a second go-redis Close would return ErrClosed)", err)
	}
	// Closed once, not leaked: the gateway's pooled connections are gone from
	// the server (only the observer is left on this test's own container).
	deadline := time.Now().Add(3 * time.Second)
	after := connectedRedisClients(t, observer)
	for after != 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		after = connectedRedisClients(t, observer)
	}
	if after != 1 {
		t.Fatalf("connected clients after Close = %d (was %d), want 1 (the observer): the shared client must be closed exactly once, not leaked", after, before)
	}
}

// connectedRedisClients reads connected_clients from INFO clients.
func connectedRedisClients(t *testing.T, c *redis.Client) int {
	t.Helper()
	info, err := c.Info(context.Background(), "clients").Result()
	if err != nil {
		t.Fatalf("INFO clients: %v", err)
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "connected_clients:"); ok {
			n, convErr := strconv.Atoi(v)
			if convErr != nil {
				t.Fatalf("connected_clients %q: %v", v, convErr)
			}
			return n
		}
	}
	t.Fatalf("no connected_clients line in INFO clients: %q", info)
	return 0
}

// The same Redis spelled two ways is deliberately NOT shared: the match is
// conservative, so the stores keep their own clients and per-store labels.
func TestIntegrationDifferentAddressSpellingsKeepSeparateClients(t *testing.T) {
	redisAddr := openTestRedisForMain(t)
	host, port, err := splitHostPortForTest(redisAddr)
	if err != nil {
		t.Fatalf("splitting %q: %v", redisAddr, err)
	}
	// Two spellings of the one Redis: the host the container reported and its
	// loopback alias.
	var other string
	switch host {
	case "localhost":
		other = "127.0.0.1"
	case "127.0.0.1":
		other = "localhost"
	default:
		t.Skipf("the test Redis is at %q, which has no loopback alias to spell it two ways", host)
	}
	upstream, _ := newMockUpstream(t)
	gw, pipeline := newSharedRedisIntegrationServer(t, upstream.URL, "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_UNSHARED", host+":"+port, other+":"+port, sharedRedisTestKeys("unshared-client-secret"))

	got := chatRoundTripsOverFourWarmRequests(t, gw, "unshared-client-secret")
	if got["ratelimit"] != 8 || got["budget"] != 8 || got["ledger"] != 4 || got["shared"] != 0 || len(got) != 3 {
		t.Fatalf("round trips by store over 4 requests = %v, want ratelimit=8 (gate + token settle), budget=8 (reserve + adjust), ledger=4, no shared", got)
	}
	if err := pipeline.Close(); err != nil {
		t.Fatalf("pipeline.Close = %v, want nil", err)
	}
}
