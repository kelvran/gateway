package bedrock

import (
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestBedrockToEmbeddingProviderTranslatesSingleInput proves the
// canonical EmbeddingRequest's single-entry Input maps onto Titan's
// native inputText field, with the fixed dimensions/normalize values
// this repo's own existing script already uses.
func TestBedrockToEmbeddingProviderTranslatesSingleInput(t *testing.T) {
	a := New()
	native, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{
		Model: "amazon.titan-embed-text-v2:0",
		Input: []string{"hello world"},
	})
	if err != nil {
		t.Fatalf("ToEmbeddingProvider: %v", err)
	}
	req, ok := native.(*EmbeddingRequest)
	if !ok {
		t.Fatalf("native = %T, want *EmbeddingRequest", native)
	}
	if req.InputText != "hello world" {
		t.Errorf("InputText = %q, want %q", req.InputText, "hello world")
	}
	if req.Dimensions != 1024 || !req.Normalize {
		t.Errorf("Dimensions/Normalize = %d/%v, want 1024/true", req.Dimensions, req.Normalize)
	}
}

// TestBedrockToEmbeddingProviderRejectsBatchInput is the real-API-
// constraint proof: Titan's InvokeModel embeddings contract accepts
// exactly one inputText per call, so a multi-input request must be
// rejected outright, never silently truncated to the first entry.
func TestBedrockToEmbeddingProviderRejectsBatchInput(t *testing.T) {
	a := New()
	_, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{
		Model: "amazon.titan-embed-text-v2:0",
		Input: []string{"one", "two"},
	})
	if !errors.Is(err, ErrBedrockEmbeddingBatchNotSupported) {
		t.Errorf("err = %v, want ErrBedrockEmbeddingBatchNotSupported", err)
	}
}

// TestBedrockToEmbeddingProviderRejectsEmptyInput mirrors the batch
// rejection for the zero-input edge case.
func TestBedrockToEmbeddingProviderRejectsEmptyInput(t *testing.T) {
	a := New()
	_, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{Model: "amazon.titan-embed-text-v2:0"})
	if !errors.Is(err, ErrBedrockEmbeddingBatchNotSupported) {
		t.Errorf("err = %v, want ErrBedrockEmbeddingBatchNotSupported", err)
	}
}

// TestBedrockFromEmbeddingProviderDecodesResponse proves Titan's native
// response maps onto the canonical shape as a single Index-0 entry.
func TestBedrockFromEmbeddingProviderDecodesResponse(t *testing.T) {
	a := New()
	native := &EmbeddingResponseWire{
		Embedding:           []float64{0.5, 0.6, 0.7},
		InputTextTokenCount: 3,
	}
	resp, err := a.FromEmbeddingProvider(native)
	if err != nil {
		t.Fatalf("FromEmbeddingProvider: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].Index != 0 {
		t.Fatalf("Data = %+v, want exactly one Index=0 entry", resp.Data)
	}
	if len(resp.Data[0].Embedding) != 3 || resp.Data[0].Embedding[0] != 0.5 {
		t.Errorf("Data[0].Embedding = %v, want [0.5 0.6 0.7]", resp.Data[0].Embedding)
	}
	if resp.Usage.PromptTokens != 3 || resp.Usage.TotalTokens != 3 {
		t.Errorf("Usage = %+v, want PromptTokens=3 TotalTokens=3", resp.Usage)
	}
}

// TestBedrockFromEmbeddingProviderRejectsWrongType mirrors OpenAI's own
// identical proof.
func TestBedrockFromEmbeddingProviderRejectsWrongType(t *testing.T) {
	a := New()
	if _, err := a.FromEmbeddingProvider("not the right type"); err == nil {
		t.Fatal("FromEmbeddingProvider with a wrong-typed argument: got nil error, want a real error")
	}
}
