package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/shopspring/decimal"
	"go.yaml.in/yaml/v3"

	"github.com/kelvran/gateway/gateway/internal/gateway/controlplane"
	"github.com/kelvran/gateway/gateway/internal/gateway/dataplane"
)

const (
	dashboardJSONPath = "../../../docs/operations/grafana/dashboards/kelvran-overview.json"
	sloRulesPath      = "../../../docs/operations/grafana/prometheus/kelvran-slo-rules.yml"
)

// promQLToken matches an identifier and the character that follows it: a
// comparison marks a label name, anything else a metric name candidate.
var promQLToken = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)\s*(=~|!~|!=|=|\{|\[|\)|,|\s|$)`)

// promQLGrouping matches a grouping clause — `by (a, b)`, `without (…)`,
// `on (…)`, `ignoring (…)` — whose members are label names.
var promQLGrouping = regexp.MustCompile(`\b(by|without|on|ignoring|group_left|group_right)\s*\(([^)]*)\)`)

var promQLKeywords = map[string]bool{"sum": true, "rate": true, "irate": true, "increase": true, "by": true, "on": true, "ignoring": true, "group_left": true, "group_right": true, "and": true, "or": true, "unless": true, "without": true, "histogram_quantile": true, "le": true, "clamp_min": true, "clamp_max": true, "avg": true, "max": true, "min": true, "count": true, "label_replace": true, "vector": true, "absent": true, "changes": true}

// dashboardNames returns the gateway metric names and label names the
// overview dashboard's panels and the SLO rules' recording expressions use.
// Recording-rule outputs (`kelvran:slo_errors:*`, the alert rules' inputs)
// are not gateway metrics and are skipped.
func dashboardNames(t *testing.T) (metrics, labels []string) {
	t.Helper()
	var exprs []string
	raw, err := os.ReadFile(dashboardJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard any
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if e, ok := x["expr"].(string); ok {
				exprs = append(exprs, e)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(dashboard)
	rulesRaw, err := os.ReadFile(sloRulesPath)
	if err != nil {
		t.Fatal(err)
	}
	var rules struct {
		Groups []struct {
			Rules []struct {
				Record string `yaml:"record"`
				Expr   string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(rulesRaw, &rules); err != nil {
		t.Fatal(err)
	}
	for _, g := range rules.Groups {
		for _, r := range g.Rules {
			if r.Record != "" { // recording rules read gateway metrics; alert rules read the recordings
				exprs = append(exprs, r.Expr)
			}
		}
	}
	metricSet, labelSet := map[string]bool{}, map[string]bool{}
	for _, e := range exprs {
		for _, g := range promQLGrouping.FindAllStringSubmatch(e, -1) {
			for _, name := range strings.Split(g[2], ",") {
				if name = strings.TrimSpace(name); name != "" {
					labelSet[name] = true
				}
			}
		}
		e = promQLGrouping.ReplaceAllString(e, " ")
		for _, m := range promQLToken.FindAllStringSubmatch(e, -1) {
			tok, next := m[1], m[2]
			if promQLKeywords[tok] || strings.Contains(tok, ":") {
				continue
			}
			switch next {
			case "=~", "!~", "!=", "=":
				labelSet[tok] = true
			default:
				if strings.HasPrefix(tok, "kelvran_") || strings.HasPrefix(tok, "gen_ai_") {
					metricSet[tok] = true
				}
			}
		}
	}
	for k := range metricSet {
		metrics = append(metrics, k)
	}
	for k := range labelSet {
		labels = append(labels, k)
	}
	sort.Strings(metrics)
	sort.Strings(labels)
	if len(exprs) < 10 || len(metrics) < 4 || len(labels) < 6 {
		t.Fatalf("read %d expressions, %d metric names %v, %d label names %v; the dashboard or the rules moved", len(exprs), len(metrics), metrics, len(labels), labels)
	}
	return metrics, labels
}

// newFailingUpstream answers every request with a 500 so a deployment
// behind it fails on each call.
func newFailingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"upstream exploded","type":"server_error"}}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPrometheusEndpointExposesEveryDashboardMetricName drives one request
// of each kind the dashboard graphs through the real pipeline — a rescued
// fallback (the primary deployment fails, the chain's target serves), a
// cache hit (the same request again), and an upstream failure with no
// fallback — then scrapes the pull reader attached to the shared test
// provider and asserts every metric and label name the overview dashboard
// and the SLO recording rules use is present in the exposition under the
// pinned translation (`_total` on counters, `_seconds` on the duration
// histogram), and that every `kelvran_*` series carries service_name and
// service_instance_id. The exposition must also parse as Prometheus text.
func TestPrometheusEndpointExposesEveryDashboardMetricName(t *testing.T) {
	handler := cmdGatewayPrometheusHandlerForTest()
	metrics, labels := dashboardNames(t)

	good, _ := newMockUpstream(t)
	failing := newFailingUpstream(t)
	const keyEnv = "PROMETHEUS_DASHBOARD_TEST_UPSTREAM_KEY"
	t.Setenv(keyEnv, "fake-upstream-key-not-a-real-secret")
	cfg := &controlplane.Config{
		ListenAddr: ":0",
		VirtualKeys: []controlplane.VirtualKeyConfig{
			{Name: "prom-key", KeyHash: testKeyHash("prom-secret"), RateLimitBurst: 100, RateLimitRefill: 100},
		},
		Deployments: []controlplane.DeploymentConfig{
			{Name: "primary-broken", Model: "gpt-4o", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: failing.URL, APIKeyEnv: keyEnv,
				FallbackChains: map[string][]string{dataplane.FallbackClassGeneric: {"secondary-good"}}},
			{Name: "secondary-good", Model: "gpt-4o-fallback", Provider: "openai", UpstreamModel: "gpt-4o", BaseURL: good.URL, APIKeyEnv: keyEnv},
			{Name: "lonely-broken", Model: "m-fail", Provider: "openai", UpstreamModel: "m-fail", BaseURL: failing.URL, APIKeyEnv: keyEnv},
		},
		PriceTable: map[string]controlplane.ModelPriceConfig{
			"gpt-4o":          {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
			"gpt-4o-fallback": {PromptPerToken: decimal.RequireFromString("0.0000025"), CompletionPerToken: decimal.RequireFromString("0.00001")},
		},
	}
	srv := newIntegrationServerFromConfig(t, cfg)
	post := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer prom-secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	rescued := `{"model":"gpt-4o","messages":[{"role":"user","content":"prometheus dashboard rescue"}]}`
	if got := post(rescued); got != http.StatusOK {
		t.Fatalf("rescued request = %d, want 200", got)
	}
	if got := post(rescued); got != http.StatusOK {
		t.Fatalf("cache-hit request = %d, want 200", got)
	}
	if got := post(`{"model":"m-fail","messages":[{"role":"user","content":"prometheus dashboard failure"}]}`); got != http.StatusBadGateway {
		t.Fatalf("failing request = %d, want 502", got)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	body := rec.Body.Bytes()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the exposition does not parse as Prometheus text: %v", err)
	}
	if len(families) < 10 {
		t.Fatalf("only %d metric families exposed", len(families))
	}
	text := string(body)
	for _, name := range metrics {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `[{ ]`).MatchString(text) {
			t.Errorf("dashboard metric %q is not in the exposition", name)
		}
	}
	for _, label := range labels {
		if !strings.Contains(text, label+`="`) {
			t.Errorf("dashboard label %q is not in the exposition", label)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "kelvran_") {
			continue
		}
		if !strings.Contains(line, `service_name="kelvran-gateway"`) || !strings.Contains(line, `service_instance_id="`) {
			t.Errorf("series without the resource labels: %s", line)
		}
	}
	t.Logf("%d families; %d dashboard metrics %v and %d labels %v present", len(families), len(metrics), metrics, len(labels), labels)
}
