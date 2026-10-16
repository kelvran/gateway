package telemetry

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// scrape serves one GET /metrics through h and returns the status, the
// Content-Type and the body.
func scrape(t *testing.T, h http.Handler, method string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/metrics", nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, rec.Header().Get("Content-Type"), string(body)
}

// TestPrometheusReaderExposesInstrumentsWithResourceLabelsAndCollectors:
// the reader built by NewPrometheusReader, attached to a provider carrying
// the gateway's resource, exposes an OTel counter under the
// UnderscoreEscapingWithSuffixes name (`_total`), carries service_name and
// service_instance_id as constant labels on it, and serves the Go runtime
// and process collectors on the same registry (G26); the exposition is the
// text format, version 0.0.4, asserted through mime.ParseMediaType because
// promhttp appends charset and escaping parameters.
func TestPrometheusReaderExposesInstrumentsWithResourceLabelsAndCollectors(t *testing.T) {
	pr, err := NewPrometheusReader()
	if err != nil {
		t.Fatalf("NewPrometheusReader: %v", err)
	}
	res, err := NewResource(context.Background())
	if err != nil {
		t.Fatalf("NewResource: %v", err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(pr.Reader()), sdkmetric.WithResource(res))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	counter, err := provider.Meter("prometheus_test").Int64Counter("kelvran.prometheus_test.events", metric.WithUnit("{event}"))
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(context.Background(), 3, metric.WithAttributes(attribute.String("kelvran.cache.layer", "L1")))

	status, contentType, body := scrape(t, pr.Handler(), http.MethodGet)
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, body %q", status, body)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "text/plain" || params["version"] != "0.0.4" {
		t.Errorf("Content-Type %q: media type %q, version %q (err %v); want text/plain version 0.0.4", contentType, mediaType, params["version"], err)
	}
	for _, want := range []string{
		"kelvran_prometheus_test_events_total{",
		`kelvran_cache_layer="L1"`,
		`service_name="kelvran-gateway"`,
		`service_instance_id="` + InstanceID + `"`,
		"go_goroutines ",
		"process_start_time_seconds ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q", want)
		}
	}
	if strings.Contains(body, "otel_scope_name") {
		t.Error("exposition carries otel_scope_* labels; WithoutScopeInfo is the design")
	}
}

// TestPrometheusHandlerFollowsInit: PrometheusHandler is nil after an Init
// without the pull reader and non-nil after one with it, including under
// exporter "none" — which still builds a meter provider when Prometheus is
// enabled, so a pull-only operator gets metrics without a push exporter.
func TestPrometheusHandlerFollowsInit(t *testing.T) {
	// Init sets the global MeterProvider, and otel's global package binds this
	// package's instruments to the FIRST provider a process sets. This file
	// sorts before result_test.go, so the shared ManualReader must be installed
	// here first or the Init calls below would take that binding and the
	// package's shared metrics test would read nothing.
	_ = cacheL3TelemetryMetricsReaderForTest()
	shutdown, err := Init(context.Background(), Config{Exporter: "none"})
	if err != nil {
		t.Fatalf("Init(none): %v", err)
	}
	if PrometheusHandler() != nil {
		t.Error("PrometheusHandler() non-nil after Init without Prometheus")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}

	shutdown, err = Init(context.Background(), Config{Exporter: "none", PrometheusEnabled: true})
	if err != nil {
		t.Fatalf("Init(none + prometheus): %v", err)
	}
	h := PrometheusHandler()
	if h == nil {
		t.Fatal("PrometheusHandler() nil after Init with Prometheus enabled")
	}
	status, _, body := scrape(t, h, http.MethodGet)
	if status != http.StatusOK || !strings.Contains(body, "go_goroutines ") {
		t.Errorf("GET /metrics = %d; body has go_goroutines: %v", status, strings.Contains(body, "go_goroutines "))
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	// A later Init without the reader clears the handler again: the getter
	// never serves a registry whose provider was shut down.
	shutdown, err = Init(context.Background(), Config{Exporter: "none"})
	if err != nil {
		t.Fatalf("Init(none) again: %v", err)
	}
	if PrometheusHandler() != nil {
		t.Error("PrometheusHandler() still non-nil after an Init without Prometheus")
	}
	_ = shutdown(context.Background())
}

// The bench harness (a stdlib leaf that cannot import this package) greps
// the exposition for kelvran_redis_round_trips_total and
// kelvran_redis_commands_total; this pins that the exporter's naming turns
// redis.go's instrument names and units (shared constants, so a unit change
// fails here) into exactly those series. The global meter is bound to a
// ManualReader once per test binary, so the instruments are re-declared on
// a Prometheus-backed provider from the same constants.
func TestPrometheusNamesOfTheRedisCountersMatchWhatTheBenchScrapes(t *testing.T) {
	pr, err := NewPrometheusReader()
	if err != nil {
		t.Fatal(err)
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(pr.Reader()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m := provider.Meter("prometheus_redis_test")
	rt, err := m.Int64Counter(redisRoundTripsName, metric.WithUnit(redisRoundTripsUnit))
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := m.Int64Counter(redisCommandsName, metric.WithUnit(redisCommandsUnit))
	if err != nil {
		t.Fatal(err)
	}
	rt.Add(context.Background(), 9, metric.WithAttributes(attribute.String(AttrKelvranRedisStore, "ratelimit"), attribute.String(AttrKelvranRedisKind, RedisKindCommand)))
	cmds.Add(context.Background(), 9, metric.WithAttributes(attribute.String(AttrKelvranRedisStore, "ratelimit"), attribute.String(AttrKelvranRedisCommand, "evalsha")))
	_, _, body := scrape(t, pr.Handler(), http.MethodGet)
	for _, want := range []string{
		`kelvran_redis_round_trips_total{kelvran_redis_kind="command",kelvran_redis_store="ratelimit"`,
		`kelvran_redis_commands_total{kelvran_redis_command="evalsha",kelvran_redis_store="ratelimit"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition lacks %q:\n%s", want, body)
		}
	}
}
