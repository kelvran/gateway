package bench

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kelvran/gateway/gateway/internal/benchupstream"
)

func TestSumPrometheusCounterAcrossLabelSets(t *testing.T) {
	body := "# HELP kelvran_redis_round_trips_total Redis round trips.\n" +
		"# TYPE kelvran_redis_round_trips_total counter\n" +
		"kelvran_redis_round_trips_total{kelvran_redis_kind=\"command\",kelvran_redis_store=\"budget\"} 4\n" +
		"kelvran_redis_round_trips_total{kelvran_redis_kind=\"command\",kelvran_redis_store=\"ratelimit\"} 6.5\n" +
		"kelvran_redis_round_trips_total_created{kelvran_redis_kind=\"command\",kelvran_redis_store=\"budget\"} 1.7e+09\n" +
		"kelvran_redis_commands_total{kelvran_redis_command=\"evalsha\",kelvran_redis_store=\"ratelimit\"} 12\n" +
		"kelvran_redis_round_trips_total5 5\n" + // a longer name that only the prefix guard can reject: its remainder parses as a number
		"other_total 99\n"
	if got, ok := sumPrometheusCounter(body, "kelvran_redis_round_trips_total"); !ok || got != 10.5 {
		t.Errorf("round trips = (%v, %v), want 10.5 across both label sets and not the _created series", got, ok)
	}
	if got, ok := sumPrometheusCounter(body, "kelvran_redis_commands_total"); !ok || got != 12 {
		t.Errorf("commands = (%v, %v), want 12", got, ok)
	}
	if _, ok := sumPrometheusCounter(body, "kelvran_redis_absent_total"); ok {
		t.Error("an absent counter must report not-found, never 0")
	}
}

func TestRunScrapesRedisRoundTripsPerRequestFromTheMetricsEndpoint(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{PromptTokens: 5, CompletionTokens: 5})
	srv := httptest.NewServer(up)
	defer srv.Close()
	// A stand-in for the gateway's Prometheus endpoint: 11 round trips and
	// 13 commands per request the mock has served so far.
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := up.Calls()
		_, _ = fmt.Fprintf(w, "kelvran_redis_round_trips_total{kelvran_redis_kind=\"command\",kelvran_redis_store=\"ratelimit\"} %d\n", 11*n)
		_, _ = fmt.Fprintf(w, "kelvran_redis_commands_total{kelvran_redis_command=\"evalsha\",kelvran_redis_store=\"ratelimit\"} %d\n", 13*n)
	}))
	defer metrics.Close()
	res, err := Run(context.Background(), Config{
		Scenario: "test-redis", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 50,
		Warmup: 300 * time.Millisecond, Duration: time.Second, Seed: 1, MetricsURLs: []string{metrics.URL + "/metrics"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RedisRoundTripsPerRequest == nil || math.Abs(*res.RedisRoundTripsPerRequest-11) > 0.5 {
		t.Errorf("redis round trips per request = %v, want about 11 (the window's counter delta over its successful requests)", res.RedisRoundTripsPerRequest)
	}
	if res.RedisCommandsPerRequest == nil || math.Abs(*res.RedisCommandsPerRequest-13) > 0.6 {
		t.Errorf("redis commands per request = %v, want about 13", res.RedisCommandsPerRequest)
	}
}

func TestRunWithoutMetricsURLsReportsNoRedisColumn(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{PromptTokens: 5, CompletionTokens: 5})
	srv := httptest.NewServer(up)
	defer srv.Close()
	res, err := Run(context.Background(), Config{Scenario: "test", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 50, Warmup: 100 * time.Millisecond, Duration: 300 * time.Millisecond, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.RedisRoundTripsPerRequest != nil || res.RedisCommandsPerRequest != nil {
		t.Errorf("no metrics endpoint means no Redis figures, not zeros: %+v %+v", res.RedisRoundTripsPerRequest, res.RedisCommandsPerRequest)
	}
}

func TestRunFailsLoudlyWhenAMetricsURLCannotBeScraped(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{PromptTokens: 5, CompletionTokens: 5})
	srv := httptest.NewServer(up)
	defer srv.Close()
	_, err := Run(context.Background(), Config{Scenario: "test", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 50, Warmup: 100 * time.Millisecond, Duration: 300 * time.Millisecond, Seed: 1, MetricsURLs: []string{"http://127.0.0.1:1/metrics"}})
	if err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Errorf("err = %v; want a scrape failure naming the metrics endpoint, never a silent dash", err)
	}
}

func TestReportCarriesRedisRoundTripsPerRequest(t *testing.T) {
	rt, cmds := 11.2, 13.0
	with := Result{Scenario: "S1r", RPS: 100, Requests: 100, Latency: Summary{Count: 100, P50: ms(1), P95: ms(2), P99: ms(3)}, RedisRoundTripsPerRequest: &rt, RedisCommandsPerRequest: &cmds}
	without := Result{Scenario: "S1a", RPS: 100, Requests: 100, Latency: Summary{Count: 100, P50: ms(1), P95: ms(2), P99: ms(3)}}
	names := map[string]Metric{}
	for _, m := range ToBenchmarkMetrics([]Result{with, without}) {
		names[m.Name] = m
	}
	if m, ok := names["S1r redis round trips per request"]; !ok || m.Unit != "rtt/req" || m.Value != 11.2 {
		t.Errorf("round-trip metric = %+v, %v", m, ok)
	}
	if m, ok := names["S1r redis commands per request"]; !ok || m.Unit != "cmd/req" || m.Value != 13 {
		t.Errorf("commands metric = %+v, %v", m, ok)
	}
	if _, ok := names["S1a redis round trips per request"]; ok {
		t.Error("a result without a Redis figure must not emit the metric")
	}
	md := MarkdownSummary([]Result{with, without})
	lines := strings.Split(strings.TrimSpace(md), "\n")
	if !strings.Contains(lines[0], "| Redis RTT/req |") {
		t.Errorf("header lacks the Redis column: %s", lines[0])
	}
	if !strings.Contains(lines[2], "| 11.2 |") {
		t.Errorf("S1r row lacks 11.2: %s", lines[2])
	}
	if !strings.HasSuffix(strings.TrimSpace(lines[3]), "| – |") {
		t.Errorf("S1a row must show a dash in the Redis column: %s", lines[3])
	}
}

func TestRunDividesRedisFiguresBySuccessfulRequests(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{PromptTokens: 5, CompletionTokens: 5})
	var served, failed atomic.Int64
	// Every other request fails at the mock; the gateway's counter would
	// still have moved for each of them (a refused request pays its pre-call
	// round trips), so the per-request figure must divide by the successes.
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1)%2 == 0 {
			failed.Add(1)
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		up.ServeHTTP(w, r)
	}))
	defer flaky.Close()
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "kelvran_redis_round_trips_total{kelvran_redis_kind=\"command\",kelvran_redis_store=\"ratelimit\"} %d\n", 11*served.Load())
	}))
	defer metrics.Close()
	res, err := Run(context.Background(), Config{Scenario: "test-redis-errors", Target: flaky.URL, Bearer: "unused", Model: "bench", RPS: 50, Warmup: 300 * time.Millisecond, Duration: time.Second, Seed: 1, MetricsURLs: []string{metrics.URL + "/metrics"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors == 0 {
		t.Fatal("the flaky mock produced no errors; the test cannot tell the divisor")
	}
	// 11 round trips per request served, over successes only: about 22.
	if res.RedisRoundTripsPerRequest == nil || math.Abs(*res.RedisRoundTripsPerRequest-22) > 1.5 {
		t.Errorf("redis round trips per request = %v, want about 22 (11 per request served, divided by the successes alone)", res.RedisRoundTripsPerRequest)
	}
}

func TestRunFailsWhenTheMetricsEndpointExposesNoRedisSeries(t *testing.T) {
	up := benchupstream.New(benchupstream.Config{PromptTokens: 5, CompletionTokens: 5})
	srv := httptest.NewServer(up)
	defer srv.Close()
	// A gateway without the hook, or one in memory mode, exposes no
	// kelvran_redis_round_trips_total at all: that is not 0.0 round trips.
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "go_goroutines 12\nother_total 99\n")
	}))
	defer metrics.Close()
	_, err := Run(context.Background(), Config{Scenario: "test", Target: srv.URL, Bearer: "unused", Model: "bench", RPS: 50, Warmup: 100 * time.Millisecond, Duration: 300 * time.Millisecond, Seed: 1, MetricsURLs: []string{metrics.URL + "/metrics"}})
	if err == nil || !strings.Contains(err.Error(), "exposes no kelvran_redis_round_trips_total") {
		t.Errorf("err = %v; want a loud refusal to report 0.0 for a gateway that counted nothing", err)
	}
}
