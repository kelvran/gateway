package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// newOpenAIClient builds the official client against the gateway's OpenAI
// surface. The anthropic deployment serves these turns through the translate
// path, so the same goldens come back in OpenAI's shape.
func newOpenAIClient(opts ...option.RequestOption) openai.Client {
	base := []option.RequestOption{
		option.WithBaseURL(gatewayURL + "/v1"),
		option.WithAPIKey(gatewayCredential),
		option.WithMaxRetries(0),
	}
	return openai.NewClient(append(base, opts...)...)
}

var openAIWeatherTool = openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
	Name:        "get_weather",
	Description: openai.String("Get the current weather for a city"),
	Parameters: openai.FunctionParameters{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
		"required":   []string{"city"},
	},
})

func openAIUserTurn(text string) []openai.ChatCompletionMessageParamUnion {
	return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(text)}
}

func openAIAPIError(t *testing.T, err error) *openai.Error {
	t.Helper()
	var apierr *openai.Error
	if !errors.As(err, &apierr) {
		t.Fatalf("want an *openai.Error, got %T: %v", err, err)
	}
	return apierr
}

func TestOpenAISDKBufferedChatCompletionOnAnAnthropicDeployment(t *testing.T) {
	anthropicMock.reset()
	client := newOpenAIClient()
	completion, err := client.Chat.Completions.New(testContext(t), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(modelAnthropic), Messages: openAIUserTurn(uniqueQuestion(t)),
	})
	if err != nil {
		t.Fatalf("Chat.Completions.New: %v", err)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != expectedText || completion.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v, want one stop choice saying %q", completion.Choices, expectedText)
	}
	if completion.Usage.PromptTokens != 58 || completion.Usage.CompletionTokens != 12 {
		t.Errorf("usage = %d/%d, want 58/12 from the golden", completion.Usage.PromptTokens, completion.Usage.CompletionTokens)
	}
	if completion.Model != modelAnthropic {
		t.Errorf("model %q, want the canonical %q the client asked for", completion.Model, modelAnthropic)
	}
	fwd := anthropicMock.last(t)
	if fwd.Path != "/v1/messages" || fwd.jsonBody(t)["model"] != upstreamModelID || fwd.Header.Get("X-Api-Key") != upstreamCredential {
		t.Errorf("translate hop forwarded to %s model %v x-api-key %q, want /v1/messages %q and the deployment's credential", fwd.Path, fwd.jsonBody(t)["model"], fwd.Header.Get("X-Api-Key"), upstreamModelID)
	}
}

func TestOpenAISDKStreamAccumulates(t *testing.T) {
	client := newOpenAIClient()
	stream := client.Chat.Completions.NewStreaming(testContext(t), openai.ChatCompletionNewParams{
		Model: shared.ChatModel(modelAnthropic), Messages: openAIUserTurn(uniqueQuestion(t)),
	})
	acc := openai.ChatCompletionAccumulator{}
	for stream.Next() {
		acc.AddChunk(stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(acc.Choices) != 1 || acc.Choices[0].Message.Content != expectedStreamText || acc.Choices[0].FinishReason != "stop" {
		t.Errorf("accumulated %+v, want one stop choice saying %q", acc.Choices, expectedStreamText)
	}
}

func TestOpenAISDKToolLoop(t *testing.T) {
	anthropicMock.reset()
	client := newOpenAIClient()
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(modelAnthropic),
		Messages: openAIUserTurn(uniqueQuestion(t)),
		Tools:    []openai.ChatCompletionToolUnionParam{openAIWeatherTool},
	}
	first, err := client.Chat.Completions.New(testContext(t), params)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if len(first.Choices) != 1 || first.Choices[0].FinishReason != "tool_calls" || len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("first turn = %+v, want one tool_calls choice with one call", first.Choices)
	}
	call := first.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "get_weather" || !strings.Contains(call.Function.Arguments, "Boston") || call.ID != replayedToolUseID {
		t.Errorf("tool call = %+v, want get_weather(Boston) with the golden's id", call)
	}
	params.Messages = append(params.Messages, first.Choices[0].Message.ToParam(),
		openai.ToolMessage(`{"temperature":72,"condition":"sunny"}`, call.ID))
	second, err := client.Chat.Completions.New(testContext(t), params)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if second.Choices[0].Message.Content != expectedText || second.Choices[0].FinishReason != "stop" {
		t.Errorf("second turn = %+v, want the loop to end with the text golden", second.Choices)
	}
	fwd := anthropicMock.last(t)
	if !fwd.bodyHas(`"tool_use_id":"`+replayedToolUseID+`"`) || !fwd.bodyHas(`"name":"get_weather"`) {
		t.Errorf("replayed turn translated without the tool_result or the tool:\n%s", fwd.Body)
	}
}

func TestOpenAISDKModelsListNamesEveryModel(t *testing.T) {
	client := newOpenAIClient()
	page, err := client.Models.List(testContext(t))
	if err != nil {
		t.Fatalf("Models.List: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range page.Data {
		seen[m.ID] = true
		if m.OwnedBy == "" || string(m.Object) != "model" {
			t.Errorf("model %+v lacks owned_by or object", m)
		}
	}
	for _, want := range []string{modelAnthropic, modelBedrock45, modelBedrock55} {
		if !seen[want] {
			t.Errorf("models.list lacks %q; got %v", want, seen)
		}
	}
}

func TestOpenAISDKErrorEnvelopeCarriesACode(t *testing.T) {
	t.Run("unknown model", func(t *testing.T) {
		anthropicMock.reset()
		client := newOpenAIClient()
		_, err := client.Chat.Completions.New(testContext(t), openai.ChatCompletionNewParams{
			Model: "no-such-model", Messages: openAIUserTurn(uniqueQuestion(t)),
		})
		apierr := openAIAPIError(t, err)
		if apierr.StatusCode != http.StatusBadRequest || apierr.Code != "model_not_found" || apierr.Type != "invalid_request_error" {
			t.Errorf("got %d code %q type %q, want 400 model_not_found invalid_request_error", apierr.StatusCode, apierr.Code, apierr.Type)
		}
		if anthropicMock.count() != 0 {
			t.Error("an unknown model reached the upstream")
		}
	})
	t.Run("wrong credential", func(t *testing.T) {
		client := newOpenAIClient(option.WithAPIKey("not-" + gatewayCredential))
		_, err := client.Chat.Completions.New(testContext(t), openai.ChatCompletionNewParams{
			Model: shared.ChatModel(modelAnthropic), Messages: openAIUserTurn(uniqueQuestion(t)),
		})
		apierr := openAIAPIError(t, err)
		if apierr.StatusCode != http.StatusUnauthorized || apierr.Code == "" {
			t.Errorf("got %d code %q, want 401 with a code", apierr.StatusCode, apierr.Code)
		}
	})
}
