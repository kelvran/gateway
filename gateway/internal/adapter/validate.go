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

	dec := json.NewDecoder(bytes.NewReader(rf.JSONSchema.Schema))
	depth := 0
	tokens := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("adapter: response_format.json_schema.schema is not valid JSON: %w", err)
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
