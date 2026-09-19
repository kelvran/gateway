package bedrock

import (
	"errors"
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestBedrockToEmbeddingProviderTranslatesSingleInput proves the
// canonical EmbeddingRequest's single-entry Input maps onto Titan's
// native inputText field, defaulting Dimensions to 1024 (Normalize
// stays fixed true) when the caller leaves the canonical Dimensions
// field unset -- byte-identical to this adapter's behavior before that
// field existed.
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

// TestBedrockToEmbeddingProviderThreadsCallerRequestedDimensions proves
// a caller-set canonical Dimensions overrides the 1024 default -- the
// real gap an audit found: this adapter used to hardcode 1024
// unconditionally, with no way for a caller to request Titan V2's other
// two supported sizes (256, 512).
func TestBedrockToEmbeddingProviderThreadsCallerRequestedDimensions(t *testing.T) {
	a := New()
	native, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{
		Model:      "amazon.titan-embed-text-v2:0",
		Input:      []string{"hello world"},
		Dimensions: 512,
	})
	if err != nil {
		t.Fatalf("ToEmbeddingProvider: %v", err)
	}
	req, ok := native.(*EmbeddingRequest)
	if !ok {
		t.Fatalf("native = %T, want *EmbeddingRequest", native)
	}
	if req.Dimensions != 512 {
		t.Errorf("Dimensions = %d, want 512 (the caller-requested value, not the 1024 default)", req.Dimensions)
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
