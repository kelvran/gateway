package openai

import (
	"testing"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// TestOpenAIToEmbeddingProviderTranslatesInputArray proves the canonical
// EmbeddingRequest's Input slice maps directly onto OpenAI's native
// batched input array.
func TestOpenAIToEmbeddingProviderTranslatesInputArray(t *testing.T) {
	a := New()
	native, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{
		Model: "text-embedding-3-small",
		Input: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("ToEmbeddingProvider: %v", err)
	}
	req, ok := native.(*EmbeddingRequest)
	if !ok {
		t.Fatalf("native = %T, want *EmbeddingRequest", native)
	}
	if req.Model != "text-embedding-3-small" {
		t.Errorf("Model = %q, want text-embedding-3-small", req.Model)
	}
	if len(req.Input) != 2 || req.Input[0] != "hello" || req.Input[1] != "world" {
		t.Errorf("Input = %v, want [hello world]", req.Input)
	}
}

// TestOpenAIToEmbeddingProviderThreadsCallerRequestedDimensions proves a
// caller-set canonical Dimensions is forwarded onto OpenAI's native
// dimensions field -- omitted entirely (via omitempty) when left unset,
// matching this adapter's behavior before that field existed.
func TestOpenAIToEmbeddingProviderThreadsCallerRequestedDimensions(t *testing.T) {
	a := New()
	native, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{
		Model:      "text-embedding-3-small",
		Input:      []string{"hello"},
		Dimensions: 256,
	})
	if err != nil {
		t.Fatalf("ToEmbeddingProvider: %v", err)
	}
	req, ok := native.(*EmbeddingRequest)
	if !ok {
		t.Fatalf("native = %T, want *EmbeddingRequest", native)
	}
	if req.Dimensions != 256 {
		t.Errorf("Dimensions = %d, want 256", req.Dimensions)
	}
}

// TestOpenAIToEmbeddingProviderRejectsEmptyInput proves an empty Input
// is a real, typed error rather than a silent zero-length upstream call.
func TestOpenAIToEmbeddingProviderRejectsEmptyInput(t *testing.T) {
	a := New()
	if _, err := a.ToEmbeddingProvider(adapter.EmbeddingRequest{Model: "text-embedding-3-small"}); err == nil {
		t.Fatal("ToEmbeddingProvider with empty Input: got nil error, want a real error")
	}
}

// TestOpenAIFromEmbeddingProviderDecodesResponse proves the native
// response's Data/Usage fields map correctly onto the canonical shape,
// preserving index order.
func TestOpenAIFromEmbeddingProviderDecodesResponse(t *testing.T) {
	a := New()
	native := &EmbeddingResponseWire{
		Object: "list",
		Model:  "text-embedding-3-small",
		Data: []EmbeddingDataWire{
			{Object: "embedding", Index: 0, Embedding: []float64{0.1, 0.2}},
			{Object: "embedding", Index: 1, Embedding: []float64{0.3, 0.4}},
		},
		Usage: EmbeddingUsageWire{PromptTokens: 8, TotalTokens: 8},
	}
	resp, err := a.FromEmbeddingProvider(native)
	if err != nil {
		t.Fatalf("FromEmbeddingProvider: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].Index != 0 || resp.Data[0].Embedding[0] != 0.1 {
		t.Errorf("Data[0] = %+v, want Index=0, Embedding[0]=0.1", resp.Data[0])
	}
	if resp.Data[1].Index != 1 || resp.Data[1].Embedding[0] != 0.3 {
		t.Errorf("Data[1] = %+v, want Index=1, Embedding[0]=0.3", resp.Data[1])
	}
	if resp.Usage.PromptTokens != 8 || resp.Usage.TotalTokens != 8 {
		t.Errorf("Usage = %+v, want PromptTokens=8 TotalTokens=8", resp.Usage)
	}
	if resp.Usage.CompletionTokens != 0 {
		t.Errorf("Usage.CompletionTokens = %d, want 0 (embeddings have no completion)", resp.Usage.CompletionTokens)
	}
}

// TestOpenAIFromEmbeddingProviderRejectsWrongType proves the type
// assertion is real, not a silent zero-value fallback.
func TestOpenAIFromEmbeddingProviderRejectsWrongType(t *testing.T) {
	a := New()
	if _, err := a.FromEmbeddingProvider("not the right type"); err == nil {
		t.Fatal("FromEmbeddingProvider with a wrong-typed argument: got nil error, want a real error")
	}
}
