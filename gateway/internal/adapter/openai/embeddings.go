package openai

import (
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// EmbeddingRequest is OpenAI's native /v1/embeddings request shape —
// confirmed against OpenAI's own current API reference: input accepts
// either a single string or an array of strings, encoding_format
// defaults to "float" (the only format this adapter ever requests; the
// alternative "base64" exists purely as a payload-size optimization
// OpenAI's own SDKs use internally and has no bearing on the canonical
// []float64 shape adapter.EmbeddingData already commits to).
type EmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
	// Dimensions is threaded from the canonical
	// adapter.EmbeddingRequest.Dimensions -- omitempty so a caller that
	// never sets it gets OpenAI's own default (the model's native
	// dimensionality), byte-identical to this adapter's behavior before
	// this field existed.
	Dimensions int `json:"dimensions,omitempty"`
}

// EmbeddingResponseWire is OpenAI's native /v1/embeddings response shape.
type EmbeddingResponseWire struct {
	Object string              `json:"object"`
	Data   []EmbeddingDataWire `json:"data"`
	Model  string              `json:"model"`
	Usage  EmbeddingUsageWire  `json:"usage"`
}

// EmbeddingDataWire is one native embedding entry.
type EmbeddingDataWire struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// EmbeddingUsageWire is OpenAI's native embeddings usage shape —
// deliberately its own type, not the chat Usage struct: embeddings usage
// carries only prompt_tokens/total_tokens, with no completion_tokens
// field at all (confirmed against OpenAI's own API reference), so
// reusing the chat Usage type here would silently imply a
// completion_tokens field that never actually arrives on the wire.
type EmbeddingUsageWire struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ToEmbeddingProvider converts the canonical EmbeddingRequest into
// OpenAI's native shape — a direct field mapping, the same "near-identity
// adapter" shape this package's own chat ToProvider already has, since
// the canonical schema is itself OpenAI-shaped.
func (a *Adapter) ToEmbeddingProvider(req adapter.EmbeddingRequest) (any, error) {
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("openai: embedding request has no input")
	}
	return &EmbeddingRequest{Model: req.Model, Input: req.Input, Dimensions: req.Dimensions}, nil
}

// FromEmbeddingProvider converts OpenAI's native embeddings response back
// into the canonical shape.
func (a *Adapter) FromEmbeddingProvider(native any) (adapter.EmbeddingResponse, error) {
	resp, ok := native.(*EmbeddingResponseWire)
	if !ok {
		return adapter.EmbeddingResponse{}, fmt.Errorf("openai: FromEmbeddingProvider: expected *EmbeddingResponseWire, got %T", native)
	}

	data := make([]adapter.EmbeddingData, 0, len(resp.Data))
	for _, d := range resp.Data {
		data = append(data, adapter.EmbeddingData{Index: d.Index, Embedding: d.Embedding})
	}

	return adapter.EmbeddingResponse{
		Model: resp.Model,
		Data:  data,
		Usage: adapter.Usage{
			PromptTokens: resp.Usage.PromptTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		},
	}, nil
}
