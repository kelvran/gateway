package bedrock

import (
	"errors"
	"fmt"

	"github.com/kelvran/gateway/gateway/internal/adapter"
)

// ErrBedrockEmbeddingBatchNotSupported is returned by ToEmbeddingProvider
// when the canonical request carries more than one Input entry — Titan's
// real InvokeModel embeddings contract accepts exactly one inputText per
// call (confirmed directly against the request shape this repo's own
// evals/scripts/validate_embedding_gate.py already sends live), unlike
// OpenAI's array-batched /v1/embeddings. A real API constraint, not a
// design choice narrowed here — disclosed via a typed error rather than
// silently embedding only the first entry or truncating the rest.
var ErrBedrockEmbeddingBatchNotSupported = errors.New("bedrock: InvokeModel embeddings accepts exactly one input per call, batch requests are not supported")

// EmbeddingRequest is Titan's native InvokeModel embeddings request
// body, confirmed live (see ErrBedrockEmbeddingBatchNotSupported's own
// doc comment). Dimensions/Normalize are Titan V2-specific optional
// fields; fixed here to the same values this repo's own existing script
// already uses (1024 dimensions, normalized) rather than exposed as new
// per-request knobs this pass has no real demand signal for.
type EmbeddingRequest struct {
	InputText  string `json:"inputText"`
	Dimensions int    `json:"dimensions"`
	Normalize  bool   `json:"normalize"`
}

// EmbeddingResponseWire is Titan's native InvokeModel embeddings response
// body.
type EmbeddingResponseWire struct {
	Embedding           []float64 `json:"embedding"`
	InputTextTokenCount int       `json:"inputTextTokenCount"`
}

// ToEmbeddingProvider converts the canonical EmbeddingRequest into
// Titan's native InvokeModel shape. Rejects a batch request outright —
// see ErrBedrockEmbeddingBatchNotSupported.
func (a *Adapter) ToEmbeddingProvider(req adapter.EmbeddingRequest) (any, error) {
	if len(req.Input) != 1 {
		return nil, fmt.Errorf("%w: got %d inputs", ErrBedrockEmbeddingBatchNotSupported, len(req.Input))
	}
	return &EmbeddingRequest{InputText: req.Input[0], Dimensions: 1024, Normalize: true}, nil
}

// FromEmbeddingProvider converts Titan's native InvokeModel embeddings
// response back into the canonical shape — always exactly one
// EmbeddingData entry (Index 0), matching ToEmbeddingProvider's own
// single-input constraint.
func (a *Adapter) FromEmbeddingProvider(native any) (adapter.EmbeddingResponse, error) {
	resp, ok := native.(*EmbeddingResponseWire)
	if !ok {
		return adapter.EmbeddingResponse{}, fmt.Errorf("bedrock: FromEmbeddingProvider: expected *EmbeddingResponseWire, got %T", native)
	}
	return adapter.EmbeddingResponse{
		Data: []adapter.EmbeddingData{{Index: 0, Embedding: resp.Embedding}},
		Usage: adapter.Usage{
			PromptTokens: resp.InputTextTokenCount,
			TotalTokens:  resp.InputTextTokenCount,
		},
	}, nil
}
