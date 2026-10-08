package benchupstream

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestBufferedResponseIsOpenAIShaped(t *testing.T) {
	s := New(Config{PromptTokens: 11, CompletionTokens: 7})
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp := post(t, srv.URL+"/v1/chat/completions", `{"model":"bench-model","messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Object != "chat.completion" || got.Model != "bench-model" || !strings.HasPrefix(got.ID, "chatcmpl-bench-") {
		t.Errorf("envelope = %+v, want object chat.completion, model echoed, chatcmpl-bench- id", got)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Role != "assistant" || got.Choices[0].Message.Content == "" || got.Choices[0].FinishReason != "stop" {
		t.Errorf("choices = %+v, want one assistant message with content and finish_reason stop", got.Choices)
	}
	if got.Usage.PromptTokens != 11 || got.Usage.CompletionTokens != 7 || got.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v, want 11/7/18", got.Usage)
	}
	if s.Calls() != 1 || s.Streams() != 0 {
		t.Errorf("calls/streams = %d/%d, want 1/0", s.Calls(), s.Streams())
	}
}

func TestStreamingResponseCarriesConfiguredChunksAndSentinel(t *testing.T) {
	s := New(Config{StreamChunks: 3, PromptTokens: 5, CompletionTokens: 3})
	srv := httptest.NewServer(s)
	defer srv.Close()

	resp := post(t, srv.URL+"/v1/chat/completions", `{"model":"bench-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	var frames []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			frames = append(frames, strings.TrimPrefix(sc.Text(), "data: "))
		}
	}
	// role + 3 content + finish + usage + [DONE]
	if len(frames) != 7 {
		t.Fatalf("data frames = %d, want 7: %q", len(frames), frames)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}
	content := 0
	for _, f := range frames[:len(frames)-1] {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f), &chunk); err != nil {
			t.Fatalf("frame %q is not JSON: %v", f, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("frame object = %q, want chat.completion.chunk", chunk.Object)
		}
		if len(chunk.Choices) == 1 && chunk.Choices[0].Delta.Content != "" {
			content++
		}
	}
	if content != 3 {
		t.Errorf("content chunks = %d, want 3", content)
	}
	if s.Calls() != 1 || s.Streams() != 1 {
		t.Errorf("calls/streams = %d/%d, want 1/1", s.Calls(), s.Streams())
	}
}

func TestLatencyIsAppliedBeforeTheResponse(t *testing.T) {
	s := New(Config{Latency: 60 * time.Millisecond})
	srv := httptest.NewServer(s)
	defer srv.Close()

	start := time.Now()
	resp := post(t, srv.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`)
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 60*time.Millisecond {
		t.Errorf("buffered response took %s, want >= the configured 60ms latency", elapsed)
	}
}

func TestRejectsNonPostMalformedJSONAndUnknownPaths(t *testing.T) {
	srv := httptest.NewServer(New(Config{}))
	defer srv.Close()

	get, err := http.Get(srv.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", get.StatusCode)
	}
	bad := post(t, srv.URL+"/v1/chat/completions", `{not json`)
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want 400", bad.StatusCode)
	}
	other := post(t, srv.URL+"/v1/embeddings", `{"model":"m","input":["x"]}`)
	_ = other.Body.Close()
	if other.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", other.StatusCode)
	}
}
