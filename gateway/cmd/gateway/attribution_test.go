package main

// Tests for the attribution middleware (plan item 13a, per
// docs/rfcs/2026-10-09-gateway-attribution-and-spend-ledger.md): the
// User-Agent normalisation table, the header caps and vocabularies, route
// coverage of the mux-level capture, the identifier switch, the dropped
// counter, and the trace-parenting fix that comes with it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/kelvran/gateway/gateway/internal/telemetry"
)

func TestClientToolFromUserAgent(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		// Python SDKs, verified against evals/.venv (openai 3.14.1, anthropic
		// 1.6.0): the token is "{ClassName}/Python {version}".
		{"OpenAI/Python 3.14.1", telemetry.ClientToolOpenAIPython},
		{"AsyncOpenAI/Python 3.14.1", telemetry.ClientToolOpenAIPython},
		{"AzureOpenAI/Python 3.14.1", telemetry.ClientToolOpenAIPython},
		{"AsyncAzureOpenAI/Python 3.14.1", telemetry.ClientToolOpenAIPython},
		{"Anthropic/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AsyncAnthropic/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AnthropicBedrock/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AsyncAnthropicVertex/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AnthropicAWS/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AnthropicGoogleCloud/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		{"AsyncAnthropicBedrockMantle/Python 1.6.0", telemetry.ClientToolAnthropicPython},
		// Other Stainless languages follow the same product/trailer rule (the
		// wire values are not verified; the rule is).
		{"OpenAI/JS 6.1.0", telemetry.ClientToolOpenAINode},
		{"OpenAI/Go 1.2.3", telemetry.ClientToolOpenAIGo},
		{"Anthropic/JS 0.60.0", telemetry.ClientToolAnthropicNode},
		{"Anthropic/Go 1.0.0", telemetry.ClientToolAnthropicGo},
		{"OpenAI/Rust 1.0", telemetry.ClientToolOther}, // unknown language
		// Claude Code 2.1.295, captured live 2026-10-09 (clean HOME, local
		// listener): three distinct tokens on three request kinds.
		{"claude-cli/2.1.295 (external, sdk-cli)", telemetry.ClientToolClaudeCode}, // POST /v1/messages
		{"claude-cli/2.1.295 (external, cli)", telemetry.ClientToolClaudeCode},
		{"claude-code/2.1.295", telemetry.ClientToolClaudeCode}, // GET /v1/models?limit=1000
		{"Bun/1.4.3", telemetry.ClientToolOther},                // HEAD /api/hello warming probe
		// Single-token tools.
		{"codex/0.5.0", telemetry.ClientToolCodex},
		{"aider/0.86.1", telemetry.ClientToolAider},
		{"continue/1.0.0", telemetry.ClientToolContinue},
		{"litellm/1.77.0", telemetry.ClientToolLiteLLM},
		{"curl/8.7.1", telemetry.ClientToolCurl},
		{"CURL/8.7.1", telemetry.ClientToolCurl}, // case-insensitive
		{"  OpenAI/Python 3.14.1", telemetry.ClientToolOpenAIPython},
		// Hostile or unknown input.
		{"", telemetry.ClientToolOther},
		{"Mozilla/5.0 (X11; Linux x86_64)", telemetry.ClientToolOther},
		{strings.Repeat("a", 1024) + "/1.0", telemetry.ClientToolOther},
		{"OpénAI/Python 3.14.1", telemetry.ClientToolOther},
		{"OpenAI/Python\x00 3.14.1", telemetry.ClientToolOther},
		{"openai", telemetry.ClientToolOther}, // product with no trailer language
	}
	for _, tc := range cases {
		if got := clientToolFromUserAgent(tc.ua); got != tc.want {
			t.Errorf("clientToolFromUserAgent(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

func headersOf(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h.Set(pairs[i], pairs[i+1])
	}
	return h
}

func TestParseAttributionHeadersIdentifierGrammar(t *testing.T) {
	ok128 := strings.Repeat("s", 128)
	a, dropped := parseAttributionHeaders(headersOf("x-claude-code-session-id", ok128))
	if a.SessionID != ok128 || len(dropped) != 0 {
		t.Errorf("128-byte session id: SessionID len = %d, dropped = %v; want kept, none dropped", len(a.SessionID), dropped)
	}
	for name, bad := range map[string]string{
		"129 bytes":   strings.Repeat("s", 129),
		"space":       "sess 1",
		"pipe":        "sess|1",
		"hash":        "sess#1",
		"non-ascii":   "sessé",
		"empty-ish":   "   ",
		"semicolon":   "a;b",
		"percent":     "a%41",
		"slash":       "a/b",
		"quote":       "a\"b",
		"newline":     "a\nb",
		"equals":      "a=b",
		"backslash":   "a\\b",
		"comma":       "a,b",
		"underscore+": "ok_but-also.fine:yes", // positive control inside the loop below
	} {
		if name == "underscore+" {
			a, dropped := parseAttributionHeaders(headersOf("x-claude-code-agent-id", bad))
			if a.AgentID != bad || len(dropped) != 0 {
				t.Errorf("agent id %q should be accepted (dots, underscores, colons and hyphens are in the alphabet); got %q dropped=%v", bad, a.AgentID, dropped)
			}
			continue
		}
		a, dropped := parseAttributionHeaders(headersOf("x-claude-code-session-id", bad))
		if a.SessionID != "" {
			t.Errorf("session id %s (%q) should have been dropped, got %q", name, bad, a.SessionID)
		}
		if len(dropped) != 1 || dropped[0] != "x-claude-code-session-id" {
			t.Errorf("session id %s: dropped = %v, want [x-claude-code-session-id]", name, dropped)
		}
	}
	// Each identifier header is validated independently.
	a, dropped = parseAttributionHeaders(headersOf(
		"x-claude-code-session-id", "sess-1",
		"x-claude-code-agent-id", strings.Repeat("a", 129),
		"x-claude-code-parent-agent-id", "parent-1",
		"x-claude-code-prompt-id", "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
	))
	if a.SessionID != "sess-1" || a.AgentID != "" || a.ParentAgentID != "parent-1" || a.ClaudeCodePromptID != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" {
		t.Errorf("mixed identifiers: %+v", a)
	}
	if len(dropped) != 1 || dropped[0] != "x-claude-code-agent-id" {
		t.Errorf("dropped = %v, want [x-claude-code-agent-id]", dropped)
	}
}

func TestParseAttributionHeadersBoundedVocabularies(t *testing.T) {
	a, _ := parseAttributionHeaders(headersOf(
		"x-claude-code-request-class", "main",
		"x-claude-code-compaction", "auto",
		"x-claude-code-context-compacted", "reactive",
		"x-claude-code-agent-type", "Explore",
	))
	if a.RequestClass != "main" || a.Compaction != "auto" || a.ContextCompacted != "reactive" || a.AgentType != "Explore" {
		t.Errorf("valid vocabulary values not kept: %+v", a)
	}
	for _, class := range []string{"main", "subagent", "workflow", "compaction", "auxiliary"} {
		a, _ := parseAttributionHeaders(headersOf("x-claude-code-request-class", class))
		if a.RequestClass != class {
			t.Errorf("request-class %q not kept verbatim: %q", class, a.RequestClass)
		}
	}
	a, dropped := parseAttributionHeaders(headersOf(
		"x-claude-code-request-class", "weird",
		"x-claude-code-compaction", "sometimes",
		"x-claude-code-context-compacted", "yes",
	))
	if a.RequestClass != telemetry.RequestClassOther || a.Compaction != "other" || a.ContextCompacted != "other" {
		t.Errorf("unknown vocabulary values must normalise to other: %+v", a)
	}
	if len(dropped) != 0 {
		t.Errorf("a vocabulary violation is normalised, not dropped; dropped = %v", dropped)
	}
	a, _ = parseAttributionHeaders(http.Header{})
	if a.RequestClass != "" || a.Compaction != "" || a.ContextCompacted != "" {
		t.Errorf("absent hint headers must stay empty (metrics normalise to none later): %+v", a)
	}
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-agent-type", strings.Repeat("t", 65)))
	if a.AgentType != "" || len(dropped) != 1 || dropped[0] != "x-claude-code-agent-type" {
		t.Errorf("65-byte agent type must be dropped: %+v dropped=%v", a, dropped)
	}
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-agent-type", "general\x7fpurpose"))
	if a.AgentType != "" || len(dropped) != 1 {
		t.Errorf("non-printable agent type must be dropped: %+v dropped=%v", a, dropped)
	}
}

func TestParseAttributionHeadersPrevToolDurations(t *testing.T) {
	a, dropped := parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "Bash=742;Read=9"))
	if a.PrevToolCount != 2 || a.PrevToolTotalMS != 751 || a.PrevToolDurations != "Bash=742;Read=9" || len(dropped) != 0 {
		t.Errorf("simple value: %+v dropped=%v", a, dropped)
	}
	// Percent-encoded names decode; the kept raw value is the original wire form.
	a, _ = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "Bash%3Bsub=10;Read%20file=5"))
	if a.PrevToolCount != 2 || a.PrevToolTotalMS != 15 {
		t.Errorf("percent-encoded names: %+v", a)
	}
	// Malformed entries are skipped, well-formed neighbours kept.
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "Bash;Read=5;=3;Grep=abc;Edit=-4;Write=7"))
	if a.PrevToolCount != 2 || a.PrevToolTotalMS != 12 {
		t.Errorf("malformed entries must be skipped (want Read=5 and Write=7): %+v", a)
	}
	if len(dropped) != 0 {
		t.Errorf("a partially malformed value is not dropped wholesale: %v", dropped)
	}
	// 33 entries: the first 32 are kept.
	var parts []string
	for i := 0; i < 33; i++ {
		parts = append(parts, "T"+strings.Repeat("x", i%3)+"="+"1")
	}
	a, _ = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", strings.Join(parts, ";")))
	if a.PrevToolCount != 32 || a.PrevToolTotalMS != 32 || strings.Count(a.PrevToolDurations, ";") != 31 {
		t.Errorf("33 entries: count=%d total=%d raw-entries=%d, want 32/32/32", a.PrevToolCount, a.PrevToolTotalMS, strings.Count(a.PrevToolDurations, ";")+1)
	}
	// 5 KB: truncated at an entry boundary to at most 4096 bytes.
	long := strings.Repeat(strings.Repeat("n", 200)+"=1;", 30)
	a, _ = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", long))
	if len(a.PrevToolDurations) > 4096 || strings.HasSuffix(a.PrevToolDurations, ";") || strings.Contains(a.PrevToolDurations, ";;") {
		t.Errorf("5 KB value: raw kept length = %d (want <= 4096, cut at an entry boundary): %q", len(a.PrevToolDurations), a.PrevToolDurations[max(0, len(a.PrevToolDurations)-30):])
	}
	if a.PrevToolCount == 0 || a.PrevToolCount > 32 {
		t.Errorf("5 KB value: count = %d", a.PrevToolCount)
	}
	// Only garbage: nothing kept, header counted as dropped.
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", ";;;===;;"))
	if a.PrevToolCount != 0 || a.PrevToolDurations != "" || len(dropped) != 1 || dropped[0] != "x-claude-code-prev-tool-durations" {
		t.Errorf("garbage-only value: %+v dropped=%v", a, dropped)
	}
	a, _ = parseAttributionHeaders(http.Header{})
	if a.PrevToolCount != 0 || a.PrevToolTotalMS != 0 || a.PrevToolDurations != "" {
		t.Errorf("absent header must leave the fields zero: %+v", a)
	}
	// Bytes outside printable ASCII never reach a span attribute: Go's
	// server passes 0x80-0xFF header bytes through, and an entry carrying
	// them is skipped as malformed (a conforming entry is always printable
	// ASCII because names are percent-encoded). Before this check an
	// unauthenticated client could plant invalid UTF-8 on the chat span,
	// which makes the OTLP exporter's proto.Marshal fail for the whole batch.
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "Ba\xffsh=1;Read=2;Gr\x80ep=3"))
	if a.PrevToolCount != 1 || a.PrevToolTotalMS != 2 || a.PrevToolDurations != "Read=2" || len(dropped) != 0 {
		t.Errorf("non-ASCII entries must be skipped, keeping Read=2: %+v dropped=%v", a, dropped)
	}
	if !utf8.ValidString(a.PrevToolDurations) {
		t.Errorf("kept value is not valid UTF-8: %q", a.PrevToolDurations)
	}
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "a\xff=1"))
	if a.PrevToolCount != 0 || a.PrevToolDurations != "" || len(dropped) != 1 {
		t.Errorf("a value whose only entry is non-ASCII is dropped wholesale: %+v dropped=%v", a, dropped)
	}
	// Each duration is capped at one day so the total cannot wrap: two
	// MaxInt64 entries used to sum to -2 on the span.
	a, dropped = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "a=9223372036854775807;b=9223372036854775807"))
	if a.PrevToolCount != 0 || a.PrevToolTotalMS != 0 || len(dropped) != 1 {
		t.Errorf("over-cap durations must be skipped, and the header dropped when none remain: %+v dropped=%v", a, dropped)
	}
	a, _ = parseAttributionHeaders(headersOf("x-claude-code-prev-tool-durations", "day=86400000;over=86400001"))
	if a.PrevToolCount != 1 || a.PrevToolTotalMS != 86400000 || a.PrevToolDurations != "day=86400000" {
		t.Errorf("one day is the inclusive per-entry cap: %+v", a)
	}
}

// TestParsePrevToolDurationsDoesNotAllocateForOversizedGarbage pins the
// memory bound: the parser walks the value with strings.Cut and stops at
// the entry/byte caps, so a 1 MiB header of separators (http.Server's
// default MaxHeaderBytes) costs no allocation at all. The eager
// strings.Split it replaced allocated ~16x the header per request, before
// authentication, on every route.
func TestParsePrevToolDurationsDoesNotAllocateForOversizedGarbage(t *testing.T) {
	garbage := strings.Repeat(";", 1<<20)
	allocs := testing.AllocsPerRun(5, func() {
		raw, count, total := parsePrevToolDurations(garbage)
		if raw != "" || count != 0 || total != 0 {
			t.Fatalf("garbage parsed as %q/%d/%d", raw, count, total)
		}
	})
	if allocs != 0 {
		t.Errorf("parsePrevToolDurations allocated %.0f times on a 1 MiB separator-only value, want 0", allocs)
	}
}

// attributionProbe records the Attribution each route saw.
type attributionProbe struct {
	mu   sync.Mutex
	seen map[string]telemetry.Attribution
}

func (p *attributionProbe) handler(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen[path] = telemetry.AttributionFromContext(r.Context())
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

func newProbeServer(t *testing.T, captureIDs bool) (*httptest.Server, *attributionProbe) {
	t.Helper()
	probe := &attributionProbe{seen: map[string]telemetry.Attribution{}}
	mux := http.NewServeMux()
	for _, path := range dataPlaneRoutes {
		mux.HandleFunc(path, probe.handler(path))
	}
	var inFlight sync.WaitGroup
	srv := httptest.NewServer(dataPlaneHandler(mux, &inFlight, captureIDs))
	t.Cleanup(srv.Close)
	return srv, probe
}

func sendProbe(t *testing.T, srv *httptest.Server, method, path string, headers http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do %s %s: %v", method, path, err)
	}
	_ = resp.Body.Close()
}

func TestCaptureAttributionCoversEveryRoute(t *testing.T) {
	srv, probe := newProbeServer(t, true)
	h := headersOf(
		"User-Agent", "OpenAI/Python 3.14.1",
		"x-claude-code-session-id", "sess-route",
		"x-claude-code-request-class", "subagent",
	)
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodPost, "/v1/embeddings"},
		{http.MethodGet, "/v1/models"},
		{http.MethodGet, "/healthz"},
	} {
		sendProbe(t, srv, rt.method, rt.path, h)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	for path, a := range probe.seen {
		if a.ClientTool != telemetry.ClientToolOpenAIPython || a.SessionID != "sess-route" || a.RequestClass != "subagent" {
			t.Errorf("%s saw %+v; want the same captured attribution on every route through the shared wrapper", path, a)
		}
	}
	if len(probe.seen) != 4 {
		t.Errorf("routes probed = %d, want 4", len(probe.seen))
	}
}

func TestCaptureAttributionGlobalOffBlanksIdentifiersKeepsBounded(t *testing.T) {
	srv, probe := newProbeServer(t, false)
	sendProbe(t, srv, http.MethodPost, "/v1/chat/completions", headersOf(
		"User-Agent", "claude-cli/2.1.295 (external, cli)",
		"x-claude-code-session-id", "sess-off",
		"x-claude-code-agent-id", "agent-off",
		"x-claude-code-prompt-id", "prompt-off",
		"x-claude-code-agent-type", "Explore",
		"x-claude-code-request-class", "main",
		"x-claude-code-prev-tool-durations", "Bash=10",
	))
	probe.mu.Lock()
	a := probe.seen["/v1/chat/completions"]
	probe.mu.Unlock()
	if a.HasIdentifiers() {
		t.Errorf("identifier capture off: identifiers must be blank, got %+v", a)
	}
	if a.ClientTool != telemetry.ClientToolClaudeCode || a.RequestClass != "main" || a.PrevToolCount != 1 || a.PrevToolTotalMS != 10 {
		t.Errorf("identifier capture off must keep the bounded fields, got %+v", a)
	}
}

var (
	cmdGatewayMetricsOnce   sync.Once
	cmdGatewayMetricsReader *sdkmetric.ManualReader
)

// cmdGatewayMetricsReaderForTest installs, exactly once per test binary, a
// ManualReader as the global MeterProvider — the same one-delegation
// constraint internal/gateway/dataplane's
// dataplaneTelemetryMetricsReaderForTest documents (otel's global package
// binds every package-level instrument to the FIRST SetMeterProvider in the
// process). No test in this package calls run()/telemetry.Init, so this is
// the only MeterProvider the binary ever sees. The reader is cumulative
// across -count reruns, so callers assert deltas, never absolutes.
func cmdGatewayMetricsReaderForTest() *sdkmetric.ManualReader {
	cmdGatewayMetricsOnce.Do(func() {
		cmdGatewayMetricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(cmdGatewayMetricsReader)))
	})
	return cmdGatewayMetricsReader
}

// attributionDroppedSnapshot returns kelvran.attribution.dropped summed by
// header name, plus every "key=value" attribute pair seen on the counter.
func attributionDroppedSnapshot(t *testing.T) (byHeader map[string]int64, attrs map[string]bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := cmdGatewayMetricsReaderForTest().Collect(context.Background(), &rm); err != nil {
		t.Fatalf("reader.Collect: %v", err)
	}
	byHeader, attrs = map[string]int64{}, map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kelvran.attribution.dropped" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("kelvran.attribution.dropped is %T, want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)+"="+kv.Value.AsString()] = true
				}
				h, _ := dp.Attributes.Value(attribute.Key(telemetry.AttrKelvranAttributionHeader))
				byHeader[h.AsString()] += dp.Value
			}
		}
	}
	return byHeader, attrs
}

// TestCaptureAttributionCountsEachDroppedHeaderOnce is the RFC test plan's
// "a 129-byte or out-of-alphabet identifier increments
// kelvran.attribution.dropped{header=…}" through the real middleware: one
// increment per refused header, the header NAME as the only attribute,
// valid neighbours untouched on the carrier.
func TestCaptureAttributionCountsEachDroppedHeaderOnce(t *testing.T) {
	before, _ := attributionDroppedSnapshot(t)
	srv, probe := newProbeServer(t, true)
	longID := strings.Repeat("s", 129)
	sendProbe(t, srv, http.MethodPost, "/v1/chat/completions", headersOf(
		"x-claude-code-session-id", longID,
		"x-claude-code-agent-id", "agent-ok",
		// 0x80 is the byte class ATTR-1 is about: Go's client sends it and
		// Go's server passes it to handlers (0x00-0x1F and 0x7F are refused
		// by both), so this is a real over-the-wire non-printable value.
		"x-claude-code-agent-type", "general\x80purpose",
		"x-claude-code-prev-tool-durations", "Bash=1",
	))
	after, attrs := attributionDroppedSnapshot(t)
	for header, want := range map[string]int64{
		"x-claude-code-session-id":          1,
		"x-claude-code-agent-id":            0,
		"x-claude-code-agent-type":          1,
		"x-claude-code-prev-tool-durations": 0,
	} {
		if got := after[header] - before[header]; got != want {
			t.Errorf("kelvran.attribution.dropped{%s} delta = %d, want %d", header, got, want)
		}
	}
	if len(attrs) == 0 {
		t.Fatal("kelvran.attribution.dropped has no data points at all; the counter is not wired to the middleware")
	}
	for attr := range attrs {
		if !strings.HasPrefix(attr, telemetry.AttrKelvranAttributionHeader+"=x-claude-code-") {
			t.Errorf("kelvran.attribution.dropped carries attribute %q; the header name is the only allowed attribute", attr)
		}
		if strings.Contains(attr, longID) || strings.Contains(attr, "general") {
			t.Errorf("a header VALUE leaked onto the counter: %q", attr)
		}
	}
	probe.mu.Lock()
	a := probe.seen["/v1/chat/completions"]
	probe.mu.Unlock()
	if a.SessionID != "" || a.AgentType != "" || a.AgentID != "agent-ok" || a.PrevToolCount != 1 {
		t.Errorf("dropped headers must be absent from the carrier while valid neighbours survive: %+v", a)
	}
}

// TestChatSpanIsChildOfGatewayHTTPSpanWithClientTraceparent pins the
// re-parenting fix: before 13a the handlers re-extracted the client's
// traceparent with the composite propagator, which replaced the otelhttp
// server span context in ctx, so the chat span became a child of the
// CLIENT's span instead of the server span wrapHTTPServerSpan started. The
// server runs the real chain (dataPlaneHandler → chatCompletionsHandler →
// finalize), so the same request also proves the middleware's Attribution
// reaches the chat span through the real handler.
func TestChatSpanIsChildOfGatewayHTTPSpanWithClientTraceparent(t *testing.T) {
	before := len(spanRecorder.Ended())
	upstream, _ := newMockUpstream(t)
	gw := newIntegrationServerWithOtelHTTP(t, upstream.URL, "test-gateway-key", "KELVRAN_INTEGRATION_TEST_UPSTREAM_KEY_TP")
	httpReq, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"parent me"}]}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer test-gateway-key")
	const clientTrace = "0af7651916cd43dd8448eb211c80319c"
	const clientSpan = "b7ad6b7169203331"
	httpReq.Header.Set("traceparent", "00-"+clientTrace+"-"+clientSpan+"-01")
	httpReq.Header.Set("baggage", "agent_run_id=run-parent-001")
	httpReq.Header.Set("User-Agent", "claude-cli/2.1.295 (external, sdk-cli)")
	httpReq.Header.Set("x-claude-code-session-id", "sess-parent-001")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// otelhttp ends the server span after the handler returns; give it a
	// moment to land in the recorder. The server span is identified by kind
	// (SpanKindServer) — otelhttp names it "{METHOD} {route}", not by the
	// operation argument "gateway.http" — as
	// TestIntegrationOtelHTTPMiddlewareNestsGenAISpanAsChild does.
	var chat, server sdktrace.ReadOnlySpan
	for attempt := 0; attempt < 50 && (chat == nil || server == nil); attempt++ {
		chat, server = nil, nil
		for _, s := range spanRecorder.Ended()[before:] {
			switch {
			case strings.HasPrefix(s.Name(), "chat "):
				chat = s
			case s.SpanKind() == trace.SpanKindServer:
				server = s
			}
		}
		if chat == nil || server == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if chat == nil || server == nil {
		t.Fatalf("want both a chat span and an otelhttp server span; got chat=%v server=%v", chat != nil, server != nil)
	}
	if got := server.Parent().SpanID().String(); got != clientSpan {
		t.Errorf("server span parent span id = %s, want the client's %s (otelhttp must still honour the client traceparent)", got, clientSpan)
	}
	if got, want := chat.Parent().SpanID(), server.SpanContext().SpanID(); got != want {
		t.Errorf("chat span parent = %s, want the otelhttp server span %s (it was %s: the client's span, because the handler re-extracted traceparent)", got, want, chat.Parent().SpanID())
	}
	if chat.SpanContext().TraceID().String() != clientTrace {
		t.Errorf("chat span trace id = %s, want the client's %s", chat.SpanContext().TraceID(), clientTrace)
	}
	// Baggage still reaches the pipeline through the Baggage-only extraction,
	// and the attribution captured by the middleware reaches finalize.
	want := map[string]string{
		telemetry.AttrKelvranAgentRunID:          "run-parent-001",
		telemetry.AttrKelvranClientTool:          telemetry.ClientToolClaudeCode,
		telemetry.AttrKelvranClaudeCodeSessionID: "sess-parent-001",
	}
	got := map[string]string{}
	for _, kv := range chat.Attributes() {
		if _, wanted := want[string(kv.Key)]; wanted {
			got[string(kv.Key)] = kv.Value.AsString()
		}
	}
	for key, v := range want {
		if got[key] != v {
			t.Errorf("chat span %s = %q, want %q (Baggage extraction or the middleware→finalize attribution path regressed)", key, got[key], v)
		}
	}
}
