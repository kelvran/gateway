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

// defaultTitanDimensions is Titan V2's own real default output size —
// used whenever the canonical request leaves Dimensions unset (0),
// preserving this adapter's pre-Dimensions-field behavior exactly.
const defaultTitanDimensions = 1024

// EmbeddingRequest is Titan's native InvokeModel embeddings request
// body, confirmed live (see ErrBedrockEmbeddingBatchNotSupported's own
// doc comment). Dimensions/Normalize are Titan V2-specific fields;
// Dimensions is now threaded from the canonical
// adapter.EmbeddingRequest.Dimensions (see ToEmbeddingProvider) rather
// than hardcoded — Titan V2 accepts one of {256, 512, 1024}, confirmed
// against AWS's own Titan Text Embeddings V2 model card. Normalize
// stays fixed to true: no real caller-facing knob for it exists in the
// canonical schema, and this repo's own existing script
// (evals/scripts/validate_embedding_gate.py) already always normalizes.
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
	dimensions := req.Dimensions
	if dimensions == 0 {
		dimensions = defaultTitanDimensions
	}
	return &EmbeddingRequest{InputText: req.Input[0], Dimensions: dimensions, Normalize: true}, nil
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
