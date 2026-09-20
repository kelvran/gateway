package embedsim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// bedrockSigningName mirrors bedrockguard.go's own identically-named
// constant and its own comment: Titan Text Embeddings' InvokeModel lives
// on the same bedrock-runtime host family as Converse/ApplyGuardrail, so
// the same SigV4 service-signing name applies.
const bedrockSigningName = "bedrock"

// requestTimeout bounds one InvokeModel call. This runs at Detector
// construction time (once per corpus entry) and on every guardrail
// check thereafter, so an AWS outage must fail fast.
const requestTimeout = 5 * time.Second

// defaultEmbeddingModelID is Amazon Titan Text Embeddings V2 -- confirmed
// against AWS's own current documentation (model-parameters-titan-embed-
// text.html): request {"inputText": "..."}, response {"embedding":
// [...], "inputTextTokenCount": N} when no embeddingTypes is specified.
const defaultEmbeddingModelID = "amazon.titan-embed-text-v2:0"

// BedrockEmbedderConfig configures a BedrockEmbedder. Every field is
// required except ModelID/SessionToken, mirroring bedrockguard.Config's
// own "no zero-value-safe default" convention -- construction-time
// validation belongs to whoever builds one (cmd/gateway's own wiring).
type BedrockEmbedderConfig struct {
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// ModelID overrides defaultEmbeddingModelID -- test-only in practice.
	ModelID string
	// BaseURL overrides the real AWS host entirely when non-empty --
	// test-only, mirroring bedrockguard.Config.BaseURL's own convention.
	BaseURL string
}

// BedrockEmbedder implements Embedder via AWS Bedrock's Titan Text
// Embeddings model, called through InvokeModel.
type BedrockEmbedder struct {
	cfg    BedrockEmbedderConfig
	client *http.Client
}

// NewBedrockEmbedder constructs a BedrockEmbedder. client is injectable
// so tests can point it at an httptest.Server; production callers
// should pass nil to get a real *http.Client with requestTimeout applied.
func NewBedrockEmbedder(cfg BedrockEmbedderConfig, client *http.Client) *BedrockEmbedder {
	if cfg.ModelID == "" {
		cfg.ModelID = defaultEmbeddingModelID
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &BedrockEmbedder{cfg: cfg, client: client}
}

type invokeModelRequest struct {
	InputText string `json:"inputText"`
}

type invokeModelResponse struct {
	Embedding []float32 `json:"embedding"`
}

// Embed implements Embedder.
func (e *BedrockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(invokeModelRequest{InputText: text})
	if err != nil {
		return nil, fmt.Errorf("embedsim: marshaling InvokeModel request: %w", err)
	}

	base := e.cfg.BaseURL
	if base == "" {
		base = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", e.cfg.Region)
	}
	url := fmt.Sprintf("%s/model/%s/invoke", base, e.cfg.ModelID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedsim: building InvokeModel request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	payloadHash := sha256.Sum256(body)
	creds := aws.Credentials{
		AccessKeyID:     e.cfg.AccessKeyID,
		SecretAccessKey: e.cfg.SecretAccessKey,
		SessionToken:    e.cfg.SessionToken,
	}
	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, httpReq, hex.EncodeToString(payloadHash[:]), bedrockSigningName, e.cfg.Region, time.Now()); err != nil {
		return nil, fmt.Errorf("embedsim: signing InvokeModel request: %w", err)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embedsim: calling InvokeModel: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("embedsim: reading InvokeModel response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedsim: InvokeModel returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var out invokeModelResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("embedsim: decoding InvokeModel response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("embedsim: InvokeModel response has no embedding")
	}
	return out.Embedding, nil
}
