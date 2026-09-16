package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrResponseFormatSchemaTooComplex is returned by
// ValidateResponseFormatSchema when a caller-supplied
// ResponseFormat.JSONSchema.Schema exceeds this gateway's structural
// complexity bound (nesting depth or total token count), per
// THREAT_MODEL.md's Gateway Denial-of-Service row (2026-09-15): the
// 32MiB whole-body cap (cmd/gateway/main.go's maxRequestBodyBytes) bounds
// total request bytes, but nothing bounded the schema's own nesting depth
// or structural complexity until this check existed.
var ErrResponseFormatSchemaTooComplex = errors.New("adapter: response_format.json_schema.schema exceeds this gateway's structural complexity bound")

const (
	// maxJSONSchemaDepth bounds ResponseFormat.JSONSchema.Schema's own
	// nesting depth (counting every "{"/"[" open). 32 levels is far
	// beyond any realistic structured-output schema -- this codebase's
	// own adapter fixtures (e.g. bedrock_test.go's) never nest past a
	// handful of levels -- while still bounding a pathologically
	// deep-looking input well short of the 32MiB whole-body cap.
	maxJSONSchemaDepth = 32
	// maxJSONSchemaTokens bounds the schema's total JSON token count, as
	// counted by json.Decoder.Token() (every delimiter, key, and scalar
	// value). 10,000 is far beyond any realistic schema -- a few hundred
	// tokens at most in this codebase's own fixtures -- while still
	// bounding a pathologically wide (many-property) schema.
	maxJSONSchemaTokens = 10000
)

// ValidateResponseFormatSchema bounds a caller-supplied
// ResponseFormat.JSONSchema.Schema's structural complexity (nesting depth
// and total token count) -- a check that sits underneath, and is
// independent of, the whole-request-body byte cap
// (cmd/gateway/main.go's maxRequestBodyBytes). Every adapter's ToProvider
// either passes the raw schema through unparsed (openai/openaicompat) or
// json.Unmarshals it into a map[string]any (anthropic/gemini/bedrock),
// erroring only on malformed JSON -- never on depth or property count --
// and that unmarshal can run more than once per request (the first
// attempt plus each fallback hop that reaches a different adapter).
//
// Uses streaming tokenization (json.Decoder.Token(), one token at a
// time) rather than json.Unmarshal into a map first -- unmarshaling first
// would pay exactly the parsing/allocation cost this check exists to
// avoid before the check could ever reject anything. Fails fast: returns
// ErrResponseFormatSchemaTooComplex the instant either bound is exceeded,
// without decoding the rest of an already-rejected schema.
//
// A nil rf, nil rf.JSONSchema, or empty Schema is a no-op returning nil,
// matching every other optional-field convention in this codebase --
// ChatRequest.ResponseFormat itself is commonly nil (see its own doc
// comment). Malformed JSON in Schema returns a real, wrapped decoder
// error -- a secondary concern to the complexity bound itself, which is
// the actual security property this function adds.
func ValidateResponseFormatSchema(rf *ResponseFormat) error {
	if rf == nil || rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 {
		return nil
	}
	if err := validateJSONComplexity(rf.JSONSchema.Schema); err != nil {
		return fmt.Errorf("adapter: response_format.json_schema.schema: %w", err)
	}
	return nil
}

// validateJSONComplexity is ValidateResponseFormatSchema's own
// depth/token-counting walk, extracted so ValidateToolDefs below can
// apply the identical bound to ToolDef.ParametersJSON — the same kind of
// caller-supplied JSON Schema content, with the same complexity-DoS
// shape. Returns ErrResponseFormatSchemaTooComplex (the shared sentinel;
// the wrapping message text at each call site names which field
// actually exceeded it) or a wrapped decode error for malformed JSON.
func validateJSONComplexity(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth := 0
	tokens := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("not valid JSON: %w", err)
		}

		tokens++
		if tokens > maxJSONSchemaTokens {
			return fmt.Errorf("%w: exceeds %d tokens", ErrResponseFormatSchemaTooComplex, maxJSONSchemaTokens)
		}

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > maxJSONSchemaDepth {
					return fmt.Errorf("%w: exceeds %d levels of nesting", ErrResponseFormatSchemaTooComplex, maxJSONSchemaDepth)
				}
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

// ErrTooManyMessages is returned by ValidateMessageCount when a
// caller-supplied ChatRequest.Messages exceeds maxMessagesPerRequest.
var ErrTooManyMessages = errors.New("adapter: messages exceeds this gateway's per-request message-count bound")

// maxMessagesPerRequest bounds ChatRequest.Messages's length, per
// THREAT_MODEL.md's Gateway Denial-of-Service row: the 32MiB whole-body
// cap (cmd/gateway/main.go's maxRequestBodyBytes) bounds total request
// bytes, but a request built almost entirely of many tiny messages could
// still carry tens of thousands of them within that same byte budget --
// each one flowing into every downstream per-message pass (guardrail
// scanning, cache-key serialization, provider ToProvider translation)
// before authentication/budget/rate-limit are ever checked. 2,000 is far
// beyond any realistic conversation length (this codebase's own fixtures
// never exceed a handful) while still bounding a pathologically
// many-message input well short of the byte cap.
const maxMessagesPerRequest = 2000

// ValidateMessageCount bounds ChatRequest.Messages's length -- see
// maxMessagesPerRequest's own doc comment for the resource-exhaustion
// rationale. A nil or empty Messages is not this function's concern
// (dataplane's own downstream checks already reject an empty
// conversation for unrelated reasons); this only ever rejects an
// excessively LONG one.
func ValidateMessageCount(messages []Message) error {
	if len(messages) > maxMessagesPerRequest {
		return fmt.Errorf("%w: %d messages, max %d", ErrTooManyMessages, len(messages), maxMessagesPerRequest)
	}
	return nil
}

// ErrTooManyToolDefs is returned by ValidateToolDefs when a
// caller-supplied ChatRequest.Tools exceeds maxToolDefsPerRequest.
var ErrTooManyToolDefs = errors.New("adapter: tools exceeds this gateway's per-request tool-definition-count bound")

// maxToolDefsPerRequest mirrors maxMessagesPerRequest's own
// resource-exhaustion rationale, applied to ChatRequest.Tools: 256 is far
// beyond any realistic tool set (this codebase's own fixtures never
// exceed a handful) while still bounding a pathologically large one.
// Unlike Messages, ToolDef also carries its OWN unbounded-until-now
// complexity vector -- ParametersJSON, a caller-supplied JSON Schema
// structurally identical to (and unlike) ResponseFormat.JSONSchema.Schema
// had no bound at all before this function existed -- so ValidateToolDefs
// bounds both the count AND, per tool, ParametersJSON's own structural
// complexity via the same validateJSONComplexity walk
// ValidateResponseFormatSchema already uses.
const maxToolDefsPerRequest = 256

// ValidateToolDefs bounds ChatRequest.Tools's length and, per tool,
// ParametersJSON's structural complexity -- see maxToolDefsPerRequest's
// own doc comment. A nil or empty Tools, or a tool with empty
// ParametersJSON, is a no-op for the corresponding check, matching every
// other optional-field convention in this codebase.
func ValidateToolDefs(tools []ToolDef) error {
	if len(tools) > maxToolDefsPerRequest {
		return fmt.Errorf("%w: %d tools, max %d", ErrTooManyToolDefs, len(tools), maxToolDefsPerRequest)
	}
	for _, t := range tools {
		if len(t.ParametersJSON) == 0 {
			continue
		}
		if err := validateJSONComplexity(json.RawMessage(t.ParametersJSON)); err != nil {
			return fmt.Errorf("adapter: tools[%q].parameters: %w", t.Name, err)
		}
	}
	return nil
}

// ErrContentPartMIMEMismatch is returned by ValidateContentParts when a
// ContentPart's declared MediaType and its inline Data's actually-detected
// content type fall into different top-level categories (e.g. declared
// "image/png" but the bytes sniff as a PDF).
var ErrContentPartMIMEMismatch = errors.New("adapter: declared MediaType does not match the inline content's detected type")

// ValidateContentParts checks every inline (base64 Data-carrying)
// ContentPart across messages for a declared-vs-detected MIME-type
// mismatch, per docs/rfcs/2026-09-06-gateway-multimodal-content.md's own
// named gap and OWASP's File Upload Cheat Sheet ("a client-declared
// Content-Type... is trivial to spoof... should not be relied upon for
// security"). URL-referenced parts (Data == "") have no inline bytes to
// sniff and are skipped — Kelvran never fetches URLs itself, so there is
// nothing here to validate against.
//
// Only a definite cross-category mismatch is rejected (declared category
// vs. detected category, e.g. "image" vs "application" — not the exact
// subtype, since some legitimate document formats like docx/pptx are
// zip-based and would otherwise false-positive against a naive subtype
// comparison). net/http.DetectContentType's "application/octet-stream"
// fallback means "could not identify the content," which is inconclusive,
// never treated as evidence of a mismatch — a false negative here (missing
// a real spoof) costs nothing beyond what the hard-gate design elsewhere in
// this codebase already assumes providers will reject; a false positive
// would incorrectly block a legitimate request, the worse failure mode for
// this specific check.
func ValidateContentParts(messages []Message) error {
	for _, m := range messages {
		for _, part := range m.Parts {
			if part.Type == "text" || part.Data == "" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(part.Data)
			if err != nil {
				return fmt.Errorf("adapter: content part has invalid base64 data: %w", err)
			}
			declaredCategory, _, _ := strings.Cut(part.MediaType, "/")
			detected := http.DetectContentType(raw)
			if detected == "application/octet-stream" {
				continue
			}
			detectedCategory, _, _ := strings.Cut(detected, "/")
			if declaredCategory != "" && detectedCategory != declaredCategory {
				return fmt.Errorf("%w: declared %q, detected %q", ErrContentPartMIMEMismatch, part.MediaType, detected)
			}
		}
	}
	return nil
}
