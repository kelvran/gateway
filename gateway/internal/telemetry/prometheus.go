package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// The Prometheus pull endpoint of
// docs/rfcs/2026-10-09-gateway-fallback-rescued-and-prometheus-pull.md
// (item 13c; owner gates G24-G26 decided 2026-10-10): a reader on the same
// MeterProvider as the push exporter, writing into a dedicated registry that
// also carries the Go runtime and process collectors, served by promhttp on a
// third listener cmd/gateway binds. The exposition names are pinned to
// UnderscoreEscapingWithSuffixes -- the v0.68.0 default, but a default the
// exporter documents as depending on prometheus/common's validation scheme and
// due to change -- so the `_total` and `_seconds` names the dashboards and the
// SLO rules use never move with a module default.

const (
	// prometheusMaxRequestsInFlight and prometheusHandlerTimeout are G24's
	// additions: an unauthenticated endpoint must not be a way to tie up the
	// process with concurrent scrapes or a slow reader.
	prometheusMaxRequestsInFlight = 4
	prometheusHandlerTimeout      = 10 * time.Second
)

// PrometheusReader pairs the OTel-to-Prometheus exporter (an sdkmetric.Reader
// the provider collects into) with the HTTP handler that serves its registry.
type PrometheusReader struct {
	reader  sdkmetric.Reader
	handler http.Handler
}

// NewPrometheusReader builds the dedicated registry with the Go and process
// collectors (G26, on by default, no switch; metric_relabel_configs is the
// documented opt-out), the exporter with the pinned translation and the
// resource's service.name and service.instance.id as constant labels on
// every series, and the promhttp handler with G24's limits. Init composes the
// reader into the gateway's provider; the cmd/gateway tests compose the same
// reader into their shared test provider, so the exposition they assert on is
// the one a running gateway serves.
func NewPrometheusReader() (*PrometheusReader, error) {
	reg := prometheus.NewRegistry()
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("telemetry: registering the Go runtime collector: %w", err)
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, fmt.Errorf("telemetry: registering the process collector: %w", err)
	}
	exporter, err := otelprom.New(
		otelprom.WithRegisterer(reg),
		otelprom.WithoutScopeInfo(),
		otelprom.WithResourceAsConstantLabels(attribute.NewAllowKeysFilter(resourceLabelKeys()...)),
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: constructing the Prometheus exporter: %w", err)
	}
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: prometheusMaxRequestsInFlight,
		Timeout:             prometheusHandlerTimeout,
	})
	return &PrometheusReader{reader: exporter, handler: handler}, nil
}

// Reader is the sdkmetric.Reader to attach to a MeterProvider; the provider's
// Shutdown covers it.
func (p *PrometheusReader) Reader() sdkmetric.Reader { return p.reader }

// Handler serves the registry in the Prometheus text exposition (version
// 0.0.4 by default; protobuf when the scraper negotiates it).
func (p *PrometheusReader) Handler() http.Handler { return p.handler }

// prometheusHandler is the handler of the reader the last Init composed, nil
// when that Init had Prometheus disabled -- package-level state of the same
// shape as InstanceID: written by Init, read once by cmd/gateway at startup
// before any server runs, never accessed concurrently.
var prometheusHandler http.Handler

// PrometheusHandler returns the pull endpoint's handler, or nil when the
// last Init did not enable Prometheus.
func PrometheusHandler() http.Handler { return prometheusHandler }

// resourceAttributes are the gateway's resource attributes: service.name and
// service.instance.id (the standard semantic-conventions attribute for
// exactly this purpose, per docs/rfcs/2026-09-07-cache-cross-instance-telemetry.md).
// NewResource sets exactly these and NewPrometheusReader exposes exactly their
// keys as constant labels, so an attribute added here reaches both places.
func resourceAttributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("service.name", "kelvran-gateway"),
		attribute.String("service.instance.id", InstanceID),
	}
}

// resourceLabelKeys are resourceAttributes' keys, for the exporter's filter.
func resourceLabelKeys() []attribute.Key {
	attrs := resourceAttributes()
	keys := make([]attribute.Key, 0, len(attrs))
	for _, kv := range attrs {
		keys = append(keys, kv.Key)
	}
	return keys
}

// NewResource is the gateway's OTel resource (resourceAttributes), so every
// exported span and metric carries them; the Prometheus reader turns them
// into constant labels on every OTel-exported series.
func NewResource(ctx context.Context) (*resource.Resource, error) {
	res, err := resource.New(ctx, resource.WithAttributes(resourceAttributes()...))
	if err != nil {
		return nil, fmt.Errorf("telemetry: building resource: %w", err)
	}
	return res, nil
}
