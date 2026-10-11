package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// newAnthropicClient builds the official client against the gateway with the
// SDK's environment defaults switched off, so an ANTHROPIC_* variable in the
// runner's shell can neither add a header nor redirect a request.
func newAnthropicClient(opts ...option.RequestOption) anthropic.Client {
	base := []option.RequestOption{
		option.WithoutEnvironmentDefaults(),
		option.WithBaseURL(gatewayURL),
		option.WithMaxRetries(0),
	}
	return anthropic.NewClient(append(base, opts...)...)
}

// recordOutgoingHeaders captures the headers the SDK actually sent, so the
// credential-form tests assert the wire shape and not the SDK's intent.
func recordOutgoingHeaders(dst *http.Header) option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		*dst = req.Header.Clone()
		return next(req)
	})
}

// captureResponseBody tees the response bytes the SDK reads into dst, so a
// streaming test can assert the wire -- the relayed frames -- and not only
// what the SDK's decoder kept (it swallows ping events, for one).
func captureResponseBody(dst *bytes.Buffer) option.RequestOption {
	return option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		resp, err := next(req)
		if err != nil {
			return resp, err
		}
		resp.Body = teeBody{Reader: io.TeeReader(resp.Body, dst), Closer: resp.Body}
		return resp, nil
	})
}

type teeBody struct {
	io.Reader
	io.Closer
}

const (
	question           = "What's the weather in Boston?"
	expectedText       = "It's 72°F and sunny in Boston."
	expectedStreamText = "The sky is blue today."
	replayedSignature  = "EqQBCkYIBhgCKkD3sig=="
	replayedToolUseID  = "toolu_01WeatherBuf00000000"
)

var weatherTool = anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
	Name:        "get_weather",
	Description: anthropic.String("Get the current weather for a city"),
	InputSchema: anthropic.ToolInputSchemaParam{
		Properties: map[string]any{"city": map[string]any{"type": "string"}},
		Required:   []string{"city"},
	},
}}

func userTurn(text string) []anthropic.MessageParam {
	return []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(text))}
}

// anthropicEnvelope is Anthropic's error object as the gateway writes it:
// {"type":"error","error":{"type","message","code","param"}}.
type anthropicEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code"`
		Param   string `json:"param"`
	} `json:"error"`
}

func anthropicAPIError(t *testing.T, err error) (*anthropic.Error, anthropicEnvelope) {
	t.Helper()
	var apierr *anthropic.Error
	if !errors.As(err, &apierr) {
		t.Fatalf("want an *anthropic.Error, got %T: %v", err, err)
	}
	var env anthropicEnvelope
	if uerr := json.Unmarshal([]byte(apierr.RawJSON()), &env); uerr != nil {
		t.Fatalf("error body is not JSON (%v): %s", uerr, apierr.RawJSON())
	}
	return apierr, env
}

func TestAnthropicSDKBufferedMessageIsAnthropicsOwnBytes(t *testing.T) {
	anthropicMock.reset()
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	asked := uniqueQuestion(t)
	msg, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
		Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(asked),
	})
	if err != nil {
		t.Fatalf("Messages.New: %v", err)
	}
	// Anthropic's own id, model and usage reach the SDK: the deployment's
	// answer was relayed, not re-encoded.
	if msg.ID != "msg_01XFDUDYJgAACzvnptvVoYEL" || string(msg.Model) != "claude-opus-4-20250514" {
		t.Errorf("id/model = %q/%q, want the golden's own", msg.ID, msg.Model)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != expectedText {
		t.Errorf("content = %+v, want one text block %q", msg.Content, expectedText)
	}
	if string(msg.StopReason) != "end_turn" || msg.Usage.InputTokens != 58 || msg.Usage.OutputTokens != 12 {
		t.Errorf("stop_reason/usage = %q/%d/%d, want end_turn/58/12", msg.StopReason, msg.Usage.InputTokens, msg.Usage.OutputTokens)
	}
	// What the deployment saw: the body as sent with model rewritten, the
	// deployment's own credential, the SDK's anthropic-version.
	fwd := anthropicMock.last(t)
	if fwd.Path != "/v1/messages" || fwd.jsonBody(t)["model"] != upstreamModelID {
		t.Errorf("forwarded to %s with model %v, want /v1/messages with %q", fwd.Path, fwd.jsonBody(t)["model"], upstreamModelID)
	}
	if fwd.Header.Get("X-Api-Key") != upstreamCredential || fwd.Header.Get("Authorization") != "" {
		t.Errorf("upstream auth headers = x-api-key %q / authorization %q, want the deployment's credential alone", fwd.Header.Get("X-Api-Key"), fwd.Header.Get("Authorization"))
	}
	if fwd.Header.Get("Anthropic-Version") == "" {
		t.Error("the SDK's anthropic-version header did not reach the deployment")
	}
	// The SDK sends the turn as a text block; the gateway forwards the body
	// as sent with only model (and stream) rewritten.
	if !fwd.bodyHas(`"text":"`+asked+`"`) || !fwd.bodyHas(`"stream":false`) {
		t.Errorf("forwarded body does not carry the SDK's message verbatim:\n%s", fwd.Body)
	}
}

// The response cache is on for every operator. A second identical request is
// answered without an upstream call, and because the cache holds the canonical
// shadow rather than Anthropic's bytes, the replay is re-encoded: the first
// answer's id, text, usage and stop_reason under the canonical model name,
// where the relayed first answer carried the upstream's own model id. An SDK
// must parse both shapes; this pins the second one.
func TestAnthropicSDKRepeatedRequestIsServedFromCacheInCanonicalShape(t *testing.T) {
	anthropicMock.reset()
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t)),
	}
	first, err := client.Messages.New(testContext(t), params)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	second, err := client.Messages.New(testContext(t), params)
	if err != nil {
		t.Fatalf("second request: %v", err)
	}
	if anthropicMock.count() != 1 {
		t.Fatalf("the upstream saw %d requests, want 1 (the second is a cache hit)", anthropicMock.count())
	}
	if textOf(*second) != textOf(*first) || second.Usage.InputTokens != first.Usage.InputTokens || string(second.StopReason) != "end_turn" {
		t.Errorf("cache hit = %q %+v %q, want the first answer's text, usage and stop_reason", textOf(*second), second.Usage, second.StopReason)
	}
	if string(second.Model) != modelAnthropic || second.ID != first.ID || string(first.Model) != "claude-opus-4-20250514" {
		t.Errorf("cache hit id/model = %q/%q, want the first answer's id %q under the canonical model %q (the relayed first answer said model %q)", second.ID, second.Model, first.ID, modelAnthropic, first.Model)
	}
}

func TestAnthropicSDKStreamAccumulatesTheRelayedFrames(t *testing.T) {
	var wire bytes.Buffer
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential), captureResponseBody(&wire))
	stream := client.Messages.NewStreaming(testContext(t), anthropic.MessageNewParams{
		Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t)),
	})
	var acc anthropic.Message
	for stream.Next() {
		if err := acc.Accumulate(stream.Current()); err != nil {
			t.Fatalf("Accumulate: %v", err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got := textOf(acc); got != expectedStreamText {
		t.Errorf("accumulated text %q, want %q", got, expectedStreamText)
	}
	if string(acc.StopReason) != "end_turn" || acc.Usage.OutputTokens != 9 {
		t.Errorf("stop_reason/output_tokens = %q/%d, want end_turn/9 from the golden's message_delta", acc.StopReason, acc.Usage.OutputTokens)
	}
	if acc.ID != "msg_01Text0nly000000000000" || string(acc.Model) != "claude-opus-4-20250514" {
		t.Errorf("id/model = %q/%q, want the upstream's own from message_start", acc.ID, acc.Model)
	}
	// The wire is the upstream's frames byte for byte -- the ping event the
	// SDK swallows included. A re-encoded stream (a translate hop, a cache
	// hit) synthesises its own frames and would not match.
	if want := anthropicMock.streamWire(goldenStreamText); wire.String() != want {
		t.Errorf("relayed stream differs from the upstream's frames:\n--- got ---\n%s\n--- want ---\n%s", wire.String(), want)
	}
}

// RFC-1 §9's one verbatim exception: an anthropic deployment's own 400 (or
// 422) in Anthropic's envelope reaches the SDK byte for byte, relay headers
// included, so Claude Code's recovery matchers see Anthropic's wording. Every
// other failure is Kelvran's redacted envelope (the tests above).
func TestAnthropicSDKUpstream400IsRelayedVerbatim(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	_, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
		Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t) + " " + verbatim400Marker),
	})
	apierr, env := anthropicAPIError(t, err)
	if apierr.StatusCode != http.StatusBadRequest || apierr.RawJSON() != verbatim400Body {
		t.Errorf("got %d %s, want Anthropic's own 400 body byte for byte", apierr.StatusCode, apierr.RawJSON())
	}
	if env.Error.Code != "" || !strings.Contains(env.Error.Message, "max_tokens: 64 > 32") {
		t.Errorf("envelope %+v, want Anthropic's wording with no Kelvran code added", env)
	}
	if apierr.Response == nil || apierr.Response.Header.Get("X-Should-Retry") != "false" || apierr.Response.Header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Errorf("relay headers missing on the verbatim 400: %v", apierr.Response.Header)
	}
}

func textOf(msg anthropic.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// The tool loop as Claude Code runs it: a turn with tools and thinking
// enabled answers thinking + text + tool_use; the client replays that
// assistant turn (ToParam keeps the thinking block and its signature) plus a
// tool_result, and the deployment must receive the signature byte for byte --
// the passthrough path's reason to exist.
func TestAnthropicSDKToolLoopReplaysThinkingSignaturesByteForByte(t *testing.T) {
	anthropicMock.reset()
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(modelAnthropic),
		MaxTokens: 2048,
		Messages:  userTurn(uniqueQuestion(t)),
		Tools:     []anthropic.ToolUnionParam{weatherTool},
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(1024),
	}
	first, err := client.Messages.New(testContext(t), params)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if string(first.StopReason) != "tool_use" || len(first.Content) != 3 {
		t.Fatalf("first turn stop_reason/content = %q/%d blocks, want tool_use/3", first.StopReason, len(first.Content))
	}
	thinking, text, toolUse := first.Content[0], first.Content[1], first.Content[2]
	if thinking.Type != "thinking" || thinking.Signature != replayedSignature {
		t.Errorf("block 0 = %s signature %q, want thinking with the golden's signature", thinking.Type, thinking.Signature)
	}
	if text.Type != "text" || text.Text != "Checking the weather." {
		t.Errorf("block 1 = %s %q, want the golden's text", text.Type, text.Text)
	}
	if toolUse.Type != "tool_use" || toolUse.Name != "get_weather" || toolUse.ID != replayedToolUseID || !strings.Contains(string(toolUse.Input), "Boston") {
		t.Errorf("block 2 = %+v, want the golden's tool_use", toolUse)
	}
	if fwd := anthropicMock.last(t); !fwd.bodyHas(`"budget_tokens":1024`) || !fwd.bodyHas(`"name":"get_weather"`) {
		t.Errorf("first turn forwarded without the thinking config or the tool:\n%s", fwd.Body)
	}

	params.Messages = append(params.Messages, first.ToParam(),
		anthropic.NewUserMessage(anthropic.NewToolResultBlock(toolUse.ID, `{"temperature":72,"condition":"sunny"}`, false)))
	second, err := client.Messages.New(testContext(t), params)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if string(second.StopReason) != "end_turn" || textOf(*second) != expectedText {
		t.Errorf("second turn = %q %q, want the loop to end with the text golden", second.StopReason, textOf(*second))
	}
	fwd := anthropicMock.last(t)
	for _, want := range []string{
		`"signature":"` + replayedSignature + `"`,
		`"type":"thinking"`,
		`"tool_use_id":"` + replayedToolUseID + `"`,
		`"type":"tool_result"`,
	} {
		if !fwd.bodyHas(want) {
			t.Errorf("replayed turn forwarded without %s:\n%s", want, fwd.Body)
		}
	}
	if fwd.jsonBody(t)["model"] != upstreamModelID {
		t.Errorf("replayed turn forwarded with model %v, want %q", fwd.jsonBody(t)["model"], upstreamModelID)
	}
}

func TestAnthropicSDKStreamedToolUseWithThinkingAccumulates(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	stream := client.Messages.NewStreaming(testContext(t), anthropic.MessageNewParams{
		Model:     anthropic.Model(modelAnthropic),
		MaxTokens: 2048,
		Messages:  userTurn(uniqueQuestion(t)),
		Tools:     []anthropic.ToolUnionParam{weatherTool},
		Thinking:  anthropic.ThinkingConfigParamOfEnabled(1024),
	})
	var acc anthropic.Message
	for stream.Next() {
		if err := acc.Accumulate(stream.Current()); err != nil {
			t.Fatalf("Accumulate: %v", err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(acc.Content) != 2 || string(acc.StopReason) != "tool_use" {
		t.Fatalf("accumulated %d blocks, stop_reason %q; want 2 and tool_use", len(acc.Content), acc.StopReason)
	}
	if acc.Content[0].Type != "thinking" || acc.Content[0].Signature != "sig_abc123" || !strings.Contains(acc.Content[0].Thinking, "get_weather") {
		t.Errorf("block 0 = %+v, want the thinking block with its signature_delta applied", acc.Content[0])
	}
	var input map[string]any
	if err := json.Unmarshal(acc.Content[1].Input, &input); err != nil || input["city"] != "Boston" || acc.Content[1].ID != "toolu_01WeatherThink000000" {
		t.Errorf("block 1 = %+v (input %v, %v), want the tool_use with its input_json_delta reassembled", acc.Content[1], input, err)
	}
	if acc.Usage.OutputTokens != 12 {
		t.Errorf("output_tokens %d, want 12", acc.Usage.OutputTokens)
	}
}

func TestAnthropicSDKCountTokensIsTheDeploymentsOwnCount(t *testing.T) {
	anthropicMock.reset()
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	count, err := client.Messages.CountTokens(testContext(t), anthropic.MessageCountTokensParams{
		Model: anthropic.Model(modelAnthropic), Messages: userTurn(uniqueQuestion(t)),
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if count.InputTokens != 2095 {
		t.Errorf("input_tokens %d, want the deployment's own 2095", count.InputTokens)
	}
	fwd := anthropicMock.last(t)
	if fwd.Path != "/v1/messages/count_tokens" || fwd.jsonBody(t)["model"] != upstreamModelID {
		t.Errorf("forwarded to %s with model %v, want /v1/messages/count_tokens with %q", fwd.Path, fwd.jsonBody(t)["model"], upstreamModelID)
	}
	if _, present := fwd.jsonBody(t)["stream"]; present {
		t.Error("a count body must not gain a stream member")
	}
}

func TestAnthropicSDKModelsListNamesEveryModel(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	page, err := client.Models.List(testContext(t), anthropic.ModelListParams{})
	if err != nil {
		t.Fatalf("Models.List: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range page.Data {
		seen[m.ID] = true
		if string(m.Type) != "model" {
			t.Errorf("model %q has type %q, want model", m.ID, m.Type)
		}
	}
	for _, want := range []string{modelAnthropic, modelBedrock45, modelBedrock55} {
		if !seen[want] {
			t.Errorf("models.list lacks %q; got %v", want, seen)
		}
	}
	if page.HasMore {
		t.Error("has_more is true for a three-model catalogue")
	}
}

func TestAnthropicSDKErrorEnvelopeCarriesACode(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	t.Run("unknown model", func(t *testing.T) {
		anthropicMock.reset()
		_, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
			Model: "no-such-model", MaxTokens: 64, Messages: userTurn(uniqueQuestion(t)),
		})
		apierr, env := anthropicAPIError(t, err)
		if apierr.StatusCode != http.StatusBadRequest || env.Type != "error" || env.Error.Type != "invalid_request_error" || env.Error.Code != "model_not_found" {
			t.Errorf("got %d %+v, want 400 invalid_request_error with code model_not_found", apierr.StatusCode, env)
		}
		if anthropicMock.count() != 0 {
			t.Error("an unknown model reached the upstream")
		}
	})
	t.Run("forced tool the request does not define", func(t *testing.T) {
		anthropicMock.reset()
		_, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
			Model:      anthropic.Model(modelAnthropic),
			MaxTokens:  64,
			Messages:   userTurn(uniqueQuestion(t)),
			Tools:      []anthropic.ToolUnionParam{weatherTool},
			ToolChoice: anthropic.ToolChoiceParamOfTool("not_a_tool"),
		})
		apierr, env := anthropicAPIError(t, err)
		if apierr.StatusCode != http.StatusBadRequest || env.Error.Code != "invalid_tool_choice" || env.Error.Param != "tool_choice" {
			t.Errorf("got %d %+v, want 400 with code invalid_tool_choice and param tool_choice", apierr.StatusCode, env)
		}
		if anthropicMock.count() != 0 {
			t.Error("a rejected tool_choice reached the upstream")
		}
	})
}

// Failure messages here report booleans, never the header values: both forms
// carry the run's credential, and a CI log is no place for it even once.
func TestAnthropicSDKAcceptsBothCredentialHeaderForms(t *testing.T) {
	// One body per subtest: a shared body would be a cache hit from the second
	// subtest on, and there would be no upstream request to inspect.
	params := func(t *testing.T) anthropic.MessageNewParams {
		return anthropic.MessageNewParams{Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t))}
	}
	t.Run("x-api-key", func(t *testing.T) {
		anthropicMock.reset()
		var sent http.Header
		client := newAnthropicClient(option.WithAPIKey(gatewayCredential), recordOutgoingHeaders(&sent))
		if _, err := client.Messages.New(testContext(t), params(t)); err != nil {
			t.Fatalf("Messages.New: %v", err)
		}
		if sent.Get("X-Api-Key") != gatewayCredential || sent.Get("Authorization") != "" {
			t.Errorf("x-api-key is the credential: %t, authorization present: %t; this case must exercise x-api-key alone", sent.Get("X-Api-Key") == gatewayCredential, sent.Get("Authorization") != "")
		}
		assertClientCredentialNotForwarded(t, anthropicMock.last(t))
	})
	t.Run("bearer", func(t *testing.T) {
		anthropicMock.reset()
		var sent http.Header
		client := newAnthropicClient(option.WithAuthToken(gatewayCredential), recordOutgoingHeaders(&sent))
		if _, err := client.Messages.New(testContext(t), params(t)); err != nil {
			t.Fatalf("Messages.New: %v", err)
		}
		if sent.Get("Authorization") != "Bearer "+gatewayCredential || sent.Get("X-Api-Key") != "" {
			t.Errorf("authorization is the bearer credential: %t, x-api-key present: %t; this case must exercise the bearer alone", sent.Get("Authorization") == "Bearer "+gatewayCredential, sent.Get("X-Api-Key") != "")
		}
		assertClientCredentialNotForwarded(t, anthropicMock.last(t))
	})
	t.Run("wrong credential", func(t *testing.T) {
		client := newAnthropicClient(option.WithAPIKey("not-" + gatewayCredential))
		_, err := client.Messages.New(testContext(t), params(t))
		apierr, env := anthropicAPIError(t, err)
		if apierr.StatusCode != http.StatusUnauthorized || env.Error.Type != "authentication_error" || env.Error.Code == "" {
			t.Errorf("got %d %+v, want 401 authentication_error with a code", apierr.StatusCode, env)
		}
	})
	// The SDK sends both headers when both options are set. The gateway's
	// precedence: the bearer wins, and a failing bearer never falls through
	// to the x-api-key beside it.
	t.Run("both headers, failing bearer beside a valid x-api-key", func(t *testing.T) {
		var sent http.Header
		client := newAnthropicClient(option.WithAPIKey(gatewayCredential), option.WithAuthToken("not-"+gatewayCredential), recordOutgoingHeaders(&sent))
		_, err := client.Messages.New(testContext(t), params(t))
		if sent.Get("X-Api-Key") == "" || sent.Get("Authorization") == "" {
			t.Fatalf("the SDK sent only one header form (x-api-key present: %t, authorization present: %t); this case needs both", sent.Get("X-Api-Key") != "", sent.Get("Authorization") != "")
		}
		apierr, env := anthropicAPIError(t, err)
		if apierr.StatusCode != http.StatusUnauthorized || env.Error.Type != "authentication_error" {
			t.Errorf("got %d %+v, want 401: a failing bearer must not fall through to the x-api-key", apierr.StatusCode, env)
		}
	})
	t.Run("both headers, valid bearer beside a wrong x-api-key", func(t *testing.T) {
		anthropicMock.reset()
		var sent http.Header
		client := newAnthropicClient(option.WithAPIKey("not-"+gatewayCredential), option.WithAuthToken(gatewayCredential), recordOutgoingHeaders(&sent))
		msg, err := client.Messages.New(testContext(t), params(t))
		if sent.Get("X-Api-Key") == "" || sent.Get("Authorization") == "" {
			t.Fatalf("the SDK sent only one header form (x-api-key present: %t, authorization present: %t); this case needs both", sent.Get("X-Api-Key") != "", sent.Get("Authorization") != "")
		}
		if err != nil {
			t.Fatalf("Messages.New: %v; the bearer must win over a wrong x-api-key", err)
		}
		if textOf(*msg) != expectedText {
			t.Errorf("text %q, want the golden's", textOf(*msg))
		}
		// Neither the client's bearer nor its (wrong) x-api-key may travel on.
		assertClientCredentialNotForwarded(t, anthropicMock.last(t))
	})
}

// Every anthropic failure outside §9's exception -- a 5xx, a 429, a 401 or a
// 400 whose body is not an Anthropic envelope -- is Kelvran's redacted
// envelope: 502 api_error upstream_error with none of the upstream's wording,
// the relay headers still forwarded, and a Retry-After from the key's own
// retry backoff (integer seconds, doubling per failure) that the upstream's
// value floors on a 429. An upstream 401 is the operator's problem, never the
// client's.
func TestAnthropicSDKOtherUpstreamFailuresAreRedacted(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	cases := []struct {
		marker, wording string
		minRetryAfter   int
	}{
		{"[upstream-500]", "XYZZY", 1},
		{"[upstream-429]", "mock-org-42", 7},
		{"[upstream-401]", "deployment credential", 1},
		{"[upstream-400-plain]", "mock-plain-400", 1},
	}
	for _, tc := range cases {
		t.Run(tc.marker, func(t *testing.T) {
			_, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
				Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t) + " " + tc.marker),
			})
			apierr, env := anthropicAPIError(t, err)
			if apierr.StatusCode != http.StatusBadGateway || env.Type != "error" || env.Error.Type != "api_error" || env.Error.Code != "upstream_error" {
				t.Errorf("got %d %+v, want 502 api_error upstream_error", apierr.StatusCode, env)
			}
			assertRedacted(t, "error body", apierr.RawJSON(), tc.wording, "mock", "claude-primary", hostPort(anthropicMock.URL()), "http://")
			if apierr.Response == nil {
				t.Fatal("no response on the API error")
			}
			h := apierr.Response.Header
			if h.Get("X-Should-Retry") != "false" || h.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
				t.Errorf("relay headers missing on an anthropic deployment's failure: %v", h)
			}
			seconds, convErr := strconv.Atoi(h.Get("Retry-After"))
			if convErr != nil || seconds < tc.minRetryAfter {
				t.Errorf("Retry-After %q, want integer seconds >= %d (the key's backoff, floored by the upstream's value on a 429)", h.Get("Retry-After"), tc.minRetryAfter)
			}
		})
	}
}

// A streaming request whose upstream fails before the first frame takes the
// same envelope path as a buffered one: the verbatim exception and the
// redaction both hold on NewStreaming.
func TestAnthropicSDKStreamedRequestFailuresUseTheSameEnvelope(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	streamError := func(t *testing.T, marker string) (*anthropic.Error, anthropicEnvelope) {
		t.Helper()
		stream := client.Messages.NewStreaming(testContext(t), anthropic.MessageNewParams{
			Model: anthropic.Model(modelAnthropic), MaxTokens: 64, Messages: userTurn(uniqueQuestion(t) + " " + marker),
		})
		for stream.Next() {
		}
		return anthropicAPIError(t, stream.Err())
	}
	t.Run("verbatim 400", func(t *testing.T) {
		apierr, _ := streamError(t, verbatim400Marker)
		if apierr.StatusCode != http.StatusBadRequest || apierr.RawJSON() != verbatim400Body {
			t.Errorf("got %d %s, want Anthropic's own 400 body byte for byte", apierr.StatusCode, apierr.RawJSON())
		}
	})
	t.Run("upstream 500", func(t *testing.T) {
		apierr, env := streamError(t, "[upstream-500]")
		if apierr.StatusCode != http.StatusBadGateway || env.Error.Type != "api_error" || env.Error.Code != "upstream_error" {
			t.Errorf("got %d %+v, want 502 api_error upstream_error", apierr.StatusCode, env)
		}
		assertRedacted(t, "error body", apierr.RawJSON(), "XYZZY", "mock", "claude-primary", hostPort(anthropicMock.URL()), "http://")
	})
}

// RFC-1 line 191: on a bedrock deployment the adapter forwards a thinking type
// the served generation accepts, and Bedrock's 400 for it carries no Anthropic
// envelope, so §9's verbatim exception must not fire -- the client gets
// Kelvran's own envelope: the upstream-failure default (502 api_error with a
// code), carrying none of Bedrock's wording, the deployment's name or the
// upstream's address. A generation known to reject the type never sees it:
// the field is dropped and the turn succeeds.
func TestAnthropicSDKBedrockThinkingRejectionIsRedacted(t *testing.T) {
	client := newAnthropicClient(option.WithAPIKey(gatewayCredential))
	t.Run("forwarded type, upstream 400", func(t *testing.T) {
		bedrockMock.reset()
		_, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
			Model: anthropic.Model(modelBedrock45), MaxTokens: 1024, Messages: userTurn(uniqueQuestion(t)),
			Thinking: anthropic.ThinkingConfigParamOfEnabled(1024),
		})
		apierr, env := anthropicAPIError(t, err)
		fwd := bedrockMock.last(t)
		assertClientCredentialNotForwarded(t, fwd)
		if !fwd.bodyHas(`"thinking":{`) || !fwd.bodyHas(`"type":"enabled"`) {
			t.Fatalf("the adapter did not forward thinking.type enabled to the haiku-4-5 deployment:\n%s", fwd.Body)
		}
		if apierr.StatusCode != http.StatusBadGateway || env.Type != "error" || env.Error.Type != "api_error" || env.Error.Code != "upstream_error" {
			t.Errorf("got %d %+v, want 502 api_error upstream_error", apierr.StatusCode, env)
		}
		// Everything the operator knows and the client must not: Bedrock's
		// wording, the exception name, the deployment, the upstream model id
		// and its date stamp, AWS's own vocabulary, the region, the dial target.
		forbidden := []string{
			"The model returned", "Input should be", "ValidationException",
			deploymentBedrock45, bedrock45UpstreamID, "haiku-4-5", "20251001",
			"amzn", "amazon", "coral", "us-east-1", hostPort(bedrockMock.URL()), "http://", "/converse",
		}
		assertRedacted(t, "error body", apierr.RawJSON(), forbidden...)
		if apierr.Response == nil {
			t.Fatal("no response on the API error")
		}
		assertHeadersRedacted(t, apierr.Response.Header, forbidden...)
		// Relay headers are anthropic-only: the mock's x-should-retry on a
		// bedrock deployment's answer must not reach the client.
		if apierr.Response.Header.Get("X-Should-Retry") != "" {
			t.Errorf("x-should-retry relayed from a bedrock deployment: %v", apierr.Response.Header)
		}
	})
	t.Run("rejecting generation, type dropped", func(t *testing.T) {
		bedrockMock.reset()
		msg, err := client.Messages.New(testContext(t), anthropic.MessageNewParams{
			Model: anthropic.Model(modelBedrock55), MaxTokens: 1024, Messages: userTurn(uniqueQuestion(t)),
			Thinking: anthropic.ThinkingConfigParamOfEnabled(1024),
		})
		if err != nil {
			t.Fatalf("Messages.New: %v", err)
		}
		if textOf(*msg) != expectedText || string(msg.StopReason) != "end_turn" {
			t.Errorf("got %q %q, want the Converse golden re-encoded as an Anthropic message", textOf(*msg), msg.StopReason)
		}
		fwd := bedrockMock.last(t)
		assertClientCredentialNotForwarded(t, fwd)
		if fwd.bodyHas(`"thinking"`) {
			t.Errorf("thinking reached a generation the adapter knows rejects it:\n%s", fwd.Body)
		}
	})
}
