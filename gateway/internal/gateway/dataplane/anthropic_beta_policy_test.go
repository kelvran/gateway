package dataplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/kelvran/gateway/gateway/internal/adapter"
	"github.com/kelvran/gateway/gateway/internal/adapter/bedrock"
)

// betaPolicyRun drives one /v1/messages-shaped request (a canonical
// request carrying Passthrough.ForwardHeaders) through a bedrock deployment
// with the given anthropic_beta_policy and returns the native Converse
// request the fake upstream captured plus the JSON log the pipeline wrote.
func betaPolicyRun(t *testing.T, policy string, withPassthrough bool, betas ...string) (*bedrock.Request, string) {
	t.Helper()
	var captured *bedrock.Request
	var logBuf bytes.Buffer
	p := newTestPipeline(t, func(_ context.Context, _ Deployment, req any) (any, error) {
		captured = req.(*bedrock.Request)
		return fakeBedrockResponse("ok"), nil
	}, []Deployment{{Name: "bedrock-1", Model: "claude", Provider: "bedrock", UpstreamModel: "global.anthropic.claude-sonnet-4-6", BaseURL: "http://unused", AnthropicBetaPolicy: policy}})
	p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
	p.adapters["bedrock"] = bedrock.New()
	req := adapter.ChatRequest{Model: "claude", Messages: []adapter.Message{{Role: "user", Content: "hi"}}}
	if withPassthrough {
		h := http.Header{}
		h.Set("Anthropic-Version", "2023-06-01")
		for _, b := range betas {
			h.Add("Anthropic-Beta", b)
		}
		req.Passthrough = &adapter.Passthrough{Format: "anthropic-messages", ForwardHeaders: h}
	}
	if _, err := p.HandleChatCompletion(context.Background(), "Bearer test-key", "", "", req, ""); err != nil {
		t.Fatalf("HandleChatCompletion(policy %q): %v", policy, err)
	}
	if captured == nil {
		t.Fatal("the fake upstream was never called")
	}
	return captured, logBuf.String()
}

func droppedBetaLines(logs string) []string {
	var lines []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, `"msg":"request_field_dropped"`) && strings.Contains(l, `"field":"anthropic-beta"`) {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestBedrockHopForwardsAllowListedBetasOnlyUnderForwardKnown is RFC-1 §5's
// upstream leg. Under the default `strip` a bedrock hop sends no
// anthropic_beta and logs nothing: stripping is the documented default.
// Under forward_known the allow-listed values
// (adapter.BedrockForwardKnownAnthropicBetas) reach Converse's
// additionalModelRequestFields.anthropic_beta in the normalised order the
// fingerprint uses (sorted, deduplicated), and every
// other value is dropped and the request gets one request_field_dropped
// line listing them (bounded) -- Converse answers 400 to an unknown value,
// so forwarding it would fail the whole request, and the header is client
// input, so the record is one line, never one per value. A request without Passthrough (the OpenAI route) has
// no beta headers and is untouched by the policy.
func TestBedrockHopForwardsAllowListedBetasOnlyUnderForwardKnown(t *testing.T) {
	const known, alsoKnown, unknown = "interleaved-thinking-2025-05-14", "claude-code-20250219", "prompt-caching-scope-2026-01-05"

	t.Run("strip (the default) sends nothing and logs nothing", func(t *testing.T) {
		captured, logs := betaPolicyRun(t, "", true, known, unknown)
		if _, ok := captured.AdditionalModelRequestFields["anthropic_beta"]; ok {
			t.Fatalf("strip forwarded anthropic_beta: %v", captured.AdditionalModelRequestFields)
		}
		if lines := droppedBetaLines(logs); len(lines) != 0 {
			t.Fatalf("strip logged %d request_field_dropped line(s) for anthropic-beta; stripping is the documented default:\n%s", len(lines), strings.Join(lines, "\n"))
		}
	})

	t.Run("forward_known forwards the allow-listed values, normalised, and drops the rest, logged once each", func(t *testing.T) {
		captured, logs := betaPolicyRun(t, "forward_known", true, known, unknown, alsoKnown, known)
		got, _ := captured.AdditionalModelRequestFields["anthropic_beta"].([]string)
		// sorted and deduplicated: alsoKnown < known, the repeat of known folded
		if want := []string{alsoKnown, known}; !reflect.DeepEqual(got, want) {
			t.Fatalf("anthropic_beta = %#v, want %#v (fields %v)", got, want, captured.AdditionalModelRequestFields)
		}
		lines := droppedBetaLines(logs)
		if len(lines) != 1 {
			t.Fatalf("want exactly one request_field_dropped line for the unknown beta, got %d:\n%s", len(lines), logs)
		}
		for _, want := range []string{`"deployment":"bedrock-1"`, `"model":"global.anthropic.claude-sonnet-4-6"`, `"field":"anthropic-beta"`, `"value":"` + unknown + `"`, `"reason":"`} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("request_field_dropped line lacks %s: %s", want, lines[0])
			}
		}
	})

	t.Run("a header of many junk values is one bounded line, not one per value", func(t *testing.T) {
		junk := make([]string, 0, 601)
		for i := range 600 {
			junk = append(junk, "junk-"+strconv.Itoa(100000 + i)[1:])
		}
		captured, logs := betaPolicyRun(t, "forward_known", true, append(junk, known)...)
		got, _ := captured.AdditionalModelRequestFields["anthropic_beta"].([]string)
		if want := []string{known}; !reflect.DeepEqual(got, want) {
			t.Fatalf("anthropic_beta = %#v, want %#v", got, want)
		}
		lines := droppedBetaLines(logs)
		if len(lines) != 1 {
			t.Fatalf("600 junk values produced %d request_field_dropped lines, want exactly one", len(lines))
		}
		if !strings.Contains(lines[0], `"value":"junk-00000,junk-00001,`) || !strings.Contains(lines[0], ",…+") {
			t.Fatalf("the one line must list the dropped values in order and end in the cut marker: %s", lines[0])
		}
		value := regexp.MustCompile(`"value":"([^"]*)"`).FindStringSubmatch(lines[0])
		if value == nil || len(value[1]) > maxModelForTelemetry {
			t.Fatalf("the value must be cut within maxModelForTelemetry (%d bytes): %q", maxModelForTelemetry, value)
		}
	})

	t.Run("forward_known with only unknown values sends nothing", func(t *testing.T) {
		captured, logs := betaPolicyRun(t, "forward_known", true, unknown)
		if _, ok := captured.AdditionalModelRequestFields["anthropic_beta"]; ok {
			t.Fatalf("an unknown beta reached Converse: %v", captured.AdditionalModelRequestFields)
		}
		if lines := droppedBetaLines(logs); len(lines) != 1 {
			t.Fatalf("want one request_field_dropped line, got %d", len(lines))
		}
	})

	t.Run("forward_known with no beta header is silent", func(t *testing.T) {
		captured, logs := betaPolicyRun(t, "forward_known", true)
		if _, ok := captured.AdditionalModelRequestFields["anthropic_beta"]; ok {
			t.Fatalf("anthropic_beta set with no header: %v", captured.AdditionalModelRequestFields)
		}
		if lines := droppedBetaLines(logs); len(lines) != 0 {
			t.Fatalf("logged %d line(s) with no beta header", len(lines))
		}
	})

	t.Run("the OpenAI route carries no betas and is untouched by forward_known", func(t *testing.T) {
		captured, logs := betaPolicyRun(t, "forward_known", false)
		if captured.AdditionalModelRequestFields != nil {
			t.Fatalf("additionalModelRequestFields = %v, want nil for a plain chat completion", captured.AdditionalModelRequestFields)
		}
		if lines := droppedBetaLines(logs); len(lines) != 0 {
			t.Fatalf("logged %d line(s) for a request with no Passthrough", len(lines))
		}
	})
}

// TestBedrockStreamHopForwardsAllowListedBetasUnderForwardKnown covers the
// third upstream-request build site, streamDeploymentBedrock's own
// ToProvider call: Claude Code streams by default, so the leg has to hold
// on the streaming path too. The fake stream upstream captures the native
// request and answers a real wire-encoded Converse stream.
func TestBedrockStreamHopForwardsAllowListedBetasUnderForwardKnown(t *testing.T) {
	const known, unknown = "interleaved-thinking-2025-05-14", "prompt-caching-scope-2026-01-05"
	wire := encodeBedrockWireFixture(t, []eventstream.Message{
		bedrockWireEvent("messageStart", `{"role":"assistant"}`),
		bedrockWireEvent("contentBlockStart", `{"contentBlockIndex":0,"start":{}}`),
		bedrockWireEvent("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"ok"}}`),
		bedrockWireEvent("contentBlockStop", `{"contentBlockIndex":0}`),
		bedrockWireEvent("messageStop", `{"stopReason":"end_turn"}`),
		bedrockWireEvent("metadata", `{"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}}`),
	})
	for _, tc := range []struct {
		name, policy string
		want         []string
		wantDropped  int
	}{
		{"strip sends nothing", "", nil, 0},
		{"forward_known forwards the allow-listed value and drops the unknown one", "forward_known", []string{known}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured *bedrock.Request
			var logBuf bytes.Buffer
			p := newStreamingTestPipeline(t, func(_ context.Context, _ Deployment, req any) (io.ReadCloser, error) {
				captured = req.(*bedrock.Request)
				return io.NopCloser(bytes.NewReader(wire)), nil
			}, []Deployment{{Name: "bedrock-1", Model: "claude", Provider: "bedrock", UpstreamModel: "global.anthropic.claude-sonnet-4-6", BaseURL: "http://unused", AnthropicBetaPolicy: tc.policy}},
				adapter.Registry{"bedrock": bedrock.New()})
			p.logger = slog.New(slog.NewJSONHandler(&logBuf, nil))
			h := http.Header{}
			h.Set("Anthropic-Version", "2023-06-01")
			h.Set("Anthropic-Beta", known+", "+unknown)
			req := adapter.ChatRequest{Model: "claude", Stream: true, Messages: []adapter.Message{{Role: "user", Content: "hi"}}, Passthrough: &adapter.Passthrough{Format: "anthropic-messages", ForwardHeaders: h}}
			rec := httptest.NewRecorder()
			if err := p.HandleChatCompletionStream(context.Background(), "Bearer test-key", "", "", req, rec, ""); err != nil {
				t.Fatalf("HandleChatCompletionStream(policy %q): %v", tc.policy, err)
			}
			if captured == nil {
				t.Fatal("the fake stream upstream was never called")
			}
			got, _ := captured.AdditionalModelRequestFields["anthropic_beta"].([]string)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("anthropic_beta = %#v, want %#v (fields %v)", got, tc.want, captured.AdditionalModelRequestFields)
			}
			if lines := droppedBetaLines(logBuf.String()); len(lines) != tc.wantDropped {
				t.Fatalf("request_field_dropped lines for anthropic-beta = %d, want %d:\n%s", len(lines), tc.wantDropped, strings.Join(lines, "\n"))
			}
		})
	}
}
