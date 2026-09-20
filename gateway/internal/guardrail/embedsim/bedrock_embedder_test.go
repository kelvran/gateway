package embedsim

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testEmbedderConfig(baseURL string) BedrockEmbedderConfig {
	return BedrockEmbedderConfig{
		Region:          "us-east-1",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		BaseURL:         baseURL,
	}
}

// TestEmbedReturnsTheModelsEmbeddingVector proves the real wire shape
// (request {"inputText": "..."}, response {"embedding": [...]}) round-trips
// correctly, mirroring bedrockguard_test.go's TestDetectNoIntervention.
func TestEmbedReturnsTheModelsEmbeddingVector(t *testing.T) {
	want := []float32{0.1, 0.2, 0.3}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req invokeModelRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decoding request: %v", err)
		}
		if req.InputText != "hello world" {
			t.Errorf("InputText = %q, want %q", req.InputText, "hello world")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(invokeModelResponse{Embedding: want})
	}))
	defer srv.Close()

	e := NewBedrockEmbedder(testEmbedderConfig(srv.URL), srv.Client())
	got, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Embed returned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Embed returned %v, want %v", got, want)
		}
	}
}

// TestEmbedErrorsOnNonOKStatus proves an upstream error status surfaces
// as a real Go error rather than a silently-empty embedding.
func TestEmbedErrorsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"access denied"}`))
	}))
	defer srv.Close()

	e := NewBedrockEmbedder(testEmbedderConfig(srv.URL), srv.Client())
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("Embed against a 403 response returned nil error, want an error")
	}
}

// TestEmbedErrorsOnEmptyEmbedding proves a 200 OK response with no
// embedding field is treated as an error, not a silent zero-length
// vector that would corrupt every downstream cosine-similarity query.
func TestEmbedErrorsOnEmptyEmbedding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(invokeModelResponse{})
	}))
	defer srv.Close()

	e := NewBedrockEmbedder(testEmbedderConfig(srv.URL), srv.Client())
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("Embed against a response with no embedding returned nil error, want an error")
	}
}
