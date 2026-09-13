// Package bedrockguard implements an optional guardrail.Detector backed by
// AWS Bedrock Guardrails' standalone ApplyGuardrail check, per
// docs/rfcs/2026-09-13-gateway-bedrock-guardrails-ml-detector-design.md.
//
// This lives OUTSIDE internal/guardrail deliberately: that package's own
// doc comment states it "never imports adapter, cache, or any
// provider-specific package" — a real AWS API call is unavoidably
// provider-specific, so the implementation must be a sibling package that
// merely satisfies guardrail.Detector, never code living inside guardrail
// itself. Mirrors internal/cache/grpcserver's own precedent for the same
// reason.
package bedrockguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/kelvran/gateway/gateway/internal/guardrail"
)

// bedrockSigningName mirrors dataplane.go's own identically-named constant
// exactly — confirmed live once already (that file's own comment: a prior
// guess, "amazonbedrockfrontendservice", produced a real 403 until fixed).
// ApplyGuardrail lives on the same bedrock-runtime host family as Converse,
// so the same SigV4 service-signing name applies; re-verify against a real
// call before trusting this for a differently-hosted future operation.
const bedrockSigningName = "bedrock"

// requestTimeout bounds the ApplyGuardrail call itself -- this runs on the
// guardrail hot path (pre-call AND post-call, buffered AND streaming), so
// an AWS outage must fail fast rather than hang the whole request.
const requestTimeout = 5 * time.Second

// Config configures one Detector instance. Every field is required --
// there is no zero-value-safe default the way GuardrailsConfig itself has,
// since a Detector with an empty GuardrailID/Region can never make a real
// call; construction-time validation belongs to whoever builds a Detector
// (cmd/gateway's own wiring), not to this package.
type Config struct {
	Region           string
	AccessKeyID      string
	SecretAccessKey  string
	SessionToken     string
	GuardrailID      string
	GuardrailVersion string
	// BaseURL overrides the real AWS host ("https://bedrock-runtime.
	// {Region}.amazonaws.com") entirely when non-empty -- test-only,
	// mirroring controlplane.DeploymentConfig.BaseURL's own convention,
	// so a test can point this at an httptest.Server instead of real AWS.
	// Production wiring (cmd/gateway) never sets this.
	BaseURL string
}

// Detector calls AWS Bedrock's standalone ApplyGuardrail check as an
// optional, ML-classifier second opinion behind promptinjection.go's own
// zero-latency regex heuristic -- never a replacement.
// guardrail.DefaultDetectors() is unconditionally unaffected; a *Detector
// is appended to the Engine's detector list only when an operator opts in
// via GuardrailsConfig.BedrockGuardrails.
//
// Fail-open/fail-closed is deliberately NOT this Detector's own concern:
// an AWS-call failure returns a non-nil error, and Engine.Check's existing
// ErrorActions[CategoryPromptInjection] policy decides Blocked from there,
// exactly like every other detector's error path already works -- no new
// per-detector failure-policy surface, per the RFC's own resolved
// Unresolved Question.
type Detector struct {
	cfg    Config
	client *http.Client
}

// New constructs a Detector. client is injectable so tests can point it at
// an httptest.Server instead of real AWS; production callers should pass
// nil to get a real *http.Client with requestTimeout applied.
func New(cfg Config, client *http.Client) *Detector {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &Detector{cfg: cfg, client: client}
}

func (d *Detector) Name() string { return "bedrock_guardrails_prompt_attack" }

// Category is CategoryPromptInjection unconditionally -- this Detector
// only wraps Bedrock Guardrails' PROMPT_ATTACK filter (see the RFC's own
// Alternatives Considered for why the broader content-filter/denied-topics
// SKU stays explicitly out of scope), matching promptinjection.go's own
// category for the same threat class.
func (d *Detector) Category() guardrail.Category { return guardrail.CategoryPromptInjection }

// applyGuardrailRequest/applyGuardrailResponse mirror AWS's real,
// live-confirmed ApplyGuardrail wire shape (docs.aws.amazon.com/bedrock/
// latest/APIReference/API_runtime_ApplyGuardrail.html) -- guardrailIdentifier/
// guardrailVersion are URI path params, never body fields.
type applyGuardrailRequest struct {
	Content []contentBlock `json:"content"`
	// Source is always "INPUT": the Detector interface (guardrail.Detector)
	// gives no signal distinguishing a pre-call from a post-call scan --
	// Engine.Check's own doc comment confirms one shared Engine instance
	// runs identically for both -- and PROMPT_ATTACK is fundamentally an
	// input-side concern (an attack targets the model's instructions, not
	// its output), matching promptinjection.go's own pre/post-agnostic
	// behavior for the same category.
	Source string `json:"source"`
}

type contentBlock struct {
	Text textBlock `json:"text"`
}

type textBlock struct {
	Text string `json:"text"`
}

type applyGuardrailResponse struct {
	Action       string       `json:"action"`
	ActionReason string       `json:"actionReason"`
	Assessments  []assessment `json:"assessments"`
}

type assessment struct {
	ContentPolicy contentPolicyAssessment `json:"contentPolicy"`
}

type contentPolicyAssessment struct {
	Filters []contentFilter `json:"filters"`
}

type contentFilter struct {
	Type     string `json:"type"`
	Detected bool   `json:"detected"`
}

// Detect implements guardrail.Detector.
func (d *Detector) Detect(ctx context.Context, text string) ([]guardrail.Finding, error) {
	reqBody := applyGuardrailRequest{
		Content: []contentBlock{{Text: textBlock{Text: text}}},
		Source:  "INPUT",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("bedrockguard: marshaling ApplyGuardrail request: %w", err)
	}

	base := d.cfg.BaseURL
	if base == "" {
		base = fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", d.cfg.Region)
	}
	url := fmt.Sprintf("%s/guardrail/%s/version/%s/apply", base, d.cfg.GuardrailID, d.cfg.GuardrailVersion)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bedrockguard: building ApplyGuardrail request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	payloadHash := sha256.Sum256(body)
	creds := aws.Credentials{
		AccessKeyID:     d.cfg.AccessKeyID,
		SecretAccessKey: d.cfg.SecretAccessKey,
		SessionToken:    d.cfg.SessionToken,
	}
	signer := v4.NewSigner()
	if err := signer.SignHTTP(ctx, creds, httpReq, hex.EncodeToString(payloadHash[:]), bedrockSigningName, d.cfg.Region, time.Now()); err != nil {
		return nil, fmt.Errorf("bedrockguard: signing ApplyGuardrail request: %w", err)
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("bedrockguard: calling ApplyGuardrail: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bedrockguard: reading ApplyGuardrail response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bedrockguard: ApplyGuardrail returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var out applyGuardrailResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("bedrockguard: decoding ApplyGuardrail response: %w", err)
	}

	if out.Action != "GUARDRAIL_INTERVENED" {
		return nil, nil
	}

	var findings []guardrail.Finding
	for _, a := range out.Assessments {
		for _, f := range a.ContentPolicy.Filters {
			if !f.Detected {
				continue
			}
			findings = append(findings, guardrail.Finding{
				Category: guardrail.CategoryPromptInjection,
				Detector: "bedrock_guardrails_" + strings.ToLower(f.Type),
			})
		}
	}
	// GUARDRAIL_INTERVENED with zero detected content-policy filters is a
	// real, disclosed gap: other Bedrock Guardrails policy types (word
	// filters, sensitive-information/PII, contextual grounding, topics)
	// are out of scope for this Detector -- see the RFC's own Alternatives
	// Considered. If a guardrail is ever configured with any of those
	// enabled, an intervention they trigger surfaces here as Action ==
	// GUARDRAIL_INTERVENED with no matching finding, which this Detector
	// deliberately does not report as a false Finding (there is nothing
	// concrete to attribute it to under this Detector's own
	// CategoryPromptInjection scope).
	return findings, nil
}
